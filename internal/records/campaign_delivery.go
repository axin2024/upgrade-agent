package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type CampaignExportInput struct {
	Expected  uint64 `json:"campaign_rev"`
	Candidate string `json:"candidate_id"`
	Rehearsal string `json:"rehearsal_id"`
}

type CampaignAdoptionInput struct {
	Expected uint64 `json:"campaign_rev"`
	Ref      string `json:"adoption_ref"`
	Observed string `json:"observed_snapshot"`
}

type CampaignBundle struct {
	ID string `json:"bundle_id"`
	WaveChange
	Wave              string   `json:"wave_id"`
	State             string   `json:"state"`
	Candidate         string   `json:"candidate_id"`
	CandidateRevision uint64   `json:"candidate_rev"`
	Rehearsal         string   `json:"rehearsal_id"`
	Evidence          []string `json:"evidence_refs"`
	Owner             string   `json:"owner"`
	Policy            uint64   `json:"policy_revision"`
	Digest            string   `json:"bundle_hash,omitempty"`
	Patch             string   `json:"patch,omitempty"`
	Rollback          string   `json:"rollback,omitempty"`
	AdoptionRef       string   `json:"adoption_ref,omitempty"`
	Observed          string   `json:"observed_snapshot,omitempty"`
	Current           string   `json:"current_snapshot,omitempty"`
	CampaignRevision  uint64   `json:"campaign_rev"`
}

var adoptionRef = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func readCampaignBundle(ctx context.Context, db rowScanner, i Identity, campaign, id string) (CampaignBundle, error) {
	var result CampaignBundle
	if !digest.MatchString(campaign) || !digest.MatchString(id) {
		return result, ErrMissing
	}
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT payload FROM campaign_bundles WHERE tenant=? AND workspace=? AND campaign=? AND id=?`,
		i.Tenant, i.Workspace, campaign, id).Scan(&raw)
	if err != nil {
		return result, missing(err)
	}
	return result, json.Unmarshal(raw, &result)
}

func (s *Store) CampaignBundle(ctx context.Context, i Identity, campaign, id string) (CampaignBundle, error) {
	out, err := readCampaignBundle(ctx, s.db, i, campaign, id)
	if err != nil {
		return out, err
	}
	repo, err := estateRepository(ctx, s.db, i, out.Repository)
	if err != nil {
		return CampaignBundle{}, err
	}
	if repo.Owner != i.Actor || repo.PolicyRevision != out.Policy {
		return CampaignBundle{}, ErrDenied
	}
	return out, nil
}

func (s *Store) ExportCampaignBundle(ctx context.Context, i Identity, key, id string, in CampaignExportInput) (CampaignBundle, error) {
	if !digest.MatchString(in.Candidate) || !digest.MatchString(in.Rehearsal) {
		return CampaignBundle{}, ErrInvalid
	}
	if _, err := s.requireCandidateOwner(ctx, i, id, in.Candidate); err != nil {
		return CampaignBundle{}, err
	}
	request := struct {
		ID string
		CampaignExportInput
	}{id, in}
	return mutate(ctx, s, i, key, "export-campaign-bundle", request, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (CampaignBundle, error) {
		c, err := readCampaign(ctx, tx, i, id)
		if err != nil {
			return CampaignBundle{}, err
		}
		if c.Revision != in.Expected || c.ActiveWave >= len(c.Waves) {
			return CampaignBundle{}, ErrConflict
		}
		item, _, err := readCampaignCandidate(ctx, tx, i, id, in.Candidate)
		if err != nil {
			return CampaignBundle{}, err
		}
		repo, err := estateRepository(ctx, tx, i, item.Repository)
		if err != nil {
			return CampaignBundle{}, err
		}
		if requireCampaignPolicy(c, repo) != nil || repo.Owner != i.Actor || repo.PolicyRevision != item.Policy {
			return CampaignBundle{}, ErrDenied
		}
		change, ok := campaignChange(c, item.Wave, item.Repository)
		if !ok || change != item.WaveChange || c.Waves[c.ActiveWave].ID != item.Wave || item.State != "ready" {
			return CampaignBundle{}, errWaveIncomplete
		}
		out := CampaignBundle{ID: snapshot.Digest([]byte(id + "\x00" + i.Actor + "\x00" + key)), WaveChange: item.WaveChange,
			Wave: item.Wave, Candidate: item.ID, CandidateRevision: item.Revision, Rehearsal: in.Rehearsal,
			Owner: repo.Owner, Policy: repo.PolicyRevision, State: "ready", CampaignRevision: c.Revision}
		if repo.Snapshot != item.Base {
			out.State, out.Current = "drifted", repo.Snapshot
			return out, audit(ctx, tx, i, "campaign-baseline-drifted", id, out)
		}
		var patch strings.Builder
		for _, hunk := range item.Hunks {
			if !hunk.Selected || hunk.Decision != "accept" || hunk.Reviewer == "" || !snapshot.Within(hunk.Path, repo.Policy.Export) {
				return CampaignBundle{}, errWaveIncomplete
			}
			patch.WriteString(hunk.Patch)
		}
		if patch.Len() == 0 || snapshot.Digest([]byte(patch.String())) != item.Digest {
			return CampaignBundle{}, errWaveIncomplete
		}
		if err := verifyWaveAuthorization(ctx, tx, i, c, item.Wave); err != nil {
			return CampaignBundle{}, err
		}
		rehearsal, err := readRehearsal(ctx, tx, i, id, in.Rehearsal)
		if err != nil {
			return CampaignBundle{}, err
		}
		if rehearsal.Wave != item.Wave || rehearsal.State != "passed" || rehearsal.Matrix != matrixDigest(planMatrix(c, item.Wave)) {
			return CampaignBundle{}, errWaveIncomplete
		}
		out.Patch, out.Evidence = patch.String(), rehearsal.EvidenceRefs
		out.Rollback = "Apply the patch in reverse only to the matching post-migration repository. For a merged commit, revert the recorded adoption ref in Git."
		out.Digest = bundleDigest(out)
		body, err := json.Marshal(out)
		if err != nil {
			return CampaignBundle{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO campaign_bundles(tenant,workspace,campaign,id,repository,wave_id,payload)
			VALUES(?,?,?,?,?,?,?)`, i.Tenant, i.Workspace, id, out.ID, out.Repository, out.Wave, body)
		if err != nil {
			return CampaignBundle{}, err
		}
		return out, audit(ctx, tx, i, "export-campaign-bundle", id, map[string]any{"bundle_id": out.ID, "bundle_hash": out.Digest, "evidence": out.Evidence})
	}, func(tx *sql.Tx, prior CampaignBundle) error {
		return checkBundleOwner(ctx, tx, i, prior)
	})
}

func checkBundleOwner(ctx context.Context, db rowScanner, i Identity, bundle CampaignBundle) error {
	repo, err := estateRepository(ctx, db, i, bundle.Repository)
	if err != nil {
		return err
	}
	if repo.Owner != i.Actor || repo.PolicyRevision != bundle.Policy {
		return ErrDenied
	}
	return nil
}

func bundleDigest(out CampaignBundle) string {
	data, _ := json.Marshal(struct {
		Change          WaveChange
		Candidate       string
		Evidence        []string
		Patch, Rollback string
	}{out.WaveChange, out.Candidate, out.Evidence, out.Patch, out.Rollback})
	return snapshot.Digest(data)
}

func (s *Store) requireCandidateOwner(ctx context.Context, i Identity, campaign, candidate string) (EstateRepository, error) {
	item, _, err := readCampaignCandidate(ctx, s.db, i, campaign, candidate)
	if err != nil {
		return EstateRepository{}, err
	}
	repo, err := estateRepository(ctx, s.db, i, item.Repository)
	if err != nil {
		return EstateRepository{}, err
	}
	if repo.Owner != i.Actor || repo.PolicyRevision != item.Policy || len(repo.Policy.Export) == 0 {
		return EstateRepository{}, ErrDenied
	}
	return repo, nil
}

func verifyWaveAuthorization(ctx context.Context, tx *sql.Tx, i Identity, c MigrationCampaign, wave string) error {
	for repoID := range c.Policies {
		repo, err := estateRepository(ctx, tx, i, repoID)
		if err != nil {
			return err
		}
		if err := validRunnerRepository(c, repo); err != nil {
			return err
		}
	}
	if c.Strategy != "one-cut" && wave == "W3" {
		return checkLegacyCalls(ctx, tx, i, c)
	}
	return nil
}

func (s *Store) AdoptCampaignBundle(ctx context.Context, i Identity, key, campaign, id string, in CampaignAdoptionInput) (CampaignBundle, error) {
	if !adoptionRef.MatchString(in.Ref) || !digest.MatchString(in.Observed) {
		return CampaignBundle{}, ErrInvalid
	}
	if _, err := s.CampaignBundle(ctx, i, campaign, id); err != nil {
		return CampaignBundle{}, err
	}
	request := struct {
		Campaign, ID string
		CampaignAdoptionInput
	}{campaign, id, in}
	return mutate(ctx, s, i, key, "adopt-campaign-bundle", request, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (CampaignBundle, error) {
		c, err := readCampaign(ctx, tx, i, campaign)
		if err != nil {
			return CampaignBundle{}, err
		}
		out, err := readCampaignBundle(ctx, tx, i, campaign, id)
		if err != nil {
			return out, err
		}
		repo, err := estateRepository(ctx, tx, i, out.Repository)
		if err != nil {
			return CampaignBundle{}, err
		}
		if repo.Owner != i.Actor || repo.PolicyRevision != out.Policy {
			return CampaignBundle{}, ErrDenied
		}
		if out.State == "adopted" && out.AdoptionRef == in.Ref && out.Observed == in.Observed {
			return out, nil
		}
		change, ok := campaignChange(c, out.Wave, out.Repository)
		if c.Revision != in.Expected || c.ActiveWave >= len(c.Waves) || out.Wave != c.Waves[c.ActiveWave].ID || out.State != "ready" ||
			!ok || change != out.WaveChange || out.Digest != bundleDigest(out) {
			return CampaignBundle{}, ErrConflict
		}
		if repo.Snapshot != out.Base {
			out.State, out.Current = "drifted", repo.Snapshot
			return out, audit(ctx, tx, i, "campaign-baseline-drifted", c.ID, out)
		}
		if err := verifyWaveAuthorization(ctx, tx, i, c, out.Wave); err != nil {
			return CampaignBundle{}, err
		}
		rehearsal, err := readRehearsal(ctx, tx, i, campaign, out.Rehearsal)
		if err != nil || rehearsal.State != "passed" || rehearsal.Matrix != matrixDigest(planMatrix(c, out.Wave)) {
			return CampaignBundle{}, errWaveIncomplete
		}
		item, _, err := readCampaignCandidate(ctx, tx, i, campaign, out.Candidate)
		if err != nil || item.Revision != out.CandidateRevision {
			return CampaignBundle{}, errWaveIncomplete
		}
		target, err := readEstateSource(ctx, tx, i, repo, out.Target)
		if err != nil {
			return CampaignBundle{}, err
		}
		observed, err := readEstateSource(ctx, tx, i, repo, in.Observed)
		if err != nil {
			return CampaignBundle{}, err
		}
		if observed.Commit != in.Ref || !slices.Equal(observed.Roots, target.Roots) || !slices.EqualFunc(observed.Files, target.Files, func(a, b snapshot.File) bool {
			return a.Path == b.Path && a.SHA256 == b.SHA256
		}) {
			return CampaignBundle{}, fmt.Errorf("%w: observed commit or source differs from the rehearsed target", ErrConflict)
		}
		var version string
		err = tx.QueryRowContext(ctx, `SELECT version_name FROM estate_snapshots WHERE tenant=? AND workspace=? AND repository=?
			AND digest=? AND policy_revision=?`, i.Tenant, i.Workspace, repo.ID, out.Target, out.Policy).Scan(&version)
		if err != nil {
			return CampaignBundle{}, missing(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE estate_repositories SET snapshot=?,version_name=?,revision=revision+1 WHERE tenant=? AND workspace=? AND id=?`,
			out.Target, version, i.Tenant, i.Workspace, repo.ID)
		if err != nil {
			return CampaignBundle{}, err
		}
		c.Revision++
		out.State, out.AdoptionRef, out.Observed, out.CampaignRevision = "adopted", in.Ref, in.Observed, c.Revision
		data, err := json.Marshal(out)
		if err != nil {
			return CampaignBundle{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE campaign_bundles SET adoption_ref=?,payload=? WHERE tenant=? AND workspace=? AND campaign=? AND id=?`,
			out.AdoptionRef, data, i.Tenant, i.Workspace, campaign, out.ID)
		if err != nil {
			return CampaignBundle{}, err
		}
		var shipped int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT repository) FROM campaign_bundles WHERE tenant=? AND workspace=? AND campaign=?
			AND wave_id=? AND adoption_ref IS NOT NULL`, i.Tenant, i.Workspace, campaign, out.Wave).Scan(&shipped)
		if err != nil {
			return CampaignBundle{}, err
		}
		c.State = "partial"
		if shipped == len(c.Waves[c.ActiveWave].Changes) {
			c.ActiveWave++
			if c.ActiveWave == len(c.Waves) {
				c.State = "delivered"
			}
		}
		return out, saveCampaign(ctx, tx, i, c, "adopt-campaign-bundle", map[string]any{"bundle_id": id, "bundle_hash": out.Digest,
			"adoption_ref": in.Ref, "observed_snapshot": in.Observed, "revision": c.Revision, "state": c.State, "active_wave": c.ActiveWave})
	}, func(tx *sql.Tx, prior CampaignBundle) error {
		return checkBundleOwner(ctx, tx, i, prior)
	})
}

func (s *Store) VerifyCampaignOwner(ctx context.Context, i Identity, repository string) error {
	repo, err := estateRepository(ctx, s.db, i, repository)
	if err != nil {
		return err
	}
	if repo.Owner != i.Actor {
		return fmt.Errorf("%w: repository ownership required", ErrDenied)
	}
	return nil
}
