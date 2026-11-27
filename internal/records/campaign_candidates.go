package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type CampaignCandidateInput struct {
	Expected   uint64              `json:"campaign_rev"`
	Wave       string              `json:"wave_id"`
	Repository string              `json:"repository_id"`
	Mode       string              `json:"mode"`
	Budget     int                 `json:"token_budget,omitempty"`
	Review     map[string][]string `json:"review_notes,omitempty"`
}

type CampaignHunk struct {
	ID       string `json:"hunk_id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Patch    string `json:"patch"`
	Selected bool   `json:"suggested"`
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Reviewer string `json:"reviewer,omitempty"`
}

type CampaignCandidate struct {
	ID string `json:"candidate_id"`
	WaveChange
	Wave     string         `json:"wave_id"`
	Digest   string         `json:"candidate_hash,omitempty"`
	Mode     string         `json:"mode"`
	Actor    string         `json:"actor"`
	State    string         `json:"state"`
	Failure  string         `json:"failure,omitempty"`
	Revision uint64         `json:"revision"`
	Policy   uint64         `json:"policy_revision"`
	Deadline time.Time      `json:"deadline"`
	Hunks    []CampaignHunk `json:"hunks,omitempty"`
	Usage    *model.Usage   `json:"usage,omitempty"`
}

type HunkDecision struct {
	Expected uint64 `json:"candidate_rev"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func campaignChange(c MigrationCampaign, wave, repository string) (WaveChange, bool) {
	for index, step := range c.Waves {
		if step.ID != wave || index < c.ActiveWave {
			continue
		}
		for _, change := range step.Changes {
			if change.Repository == repository {
				return change, true
			}
		}
	}
	return WaveChange{}, false
}

func readCampaignCandidate(ctx context.Context, db rowScanner, i Identity, campaign, id string) (CampaignCandidate, string, error) {
	var result CampaignCandidate
	var signature string
	if !digest.MatchString(campaign) || !digest.MatchString(id) {
		return result, "", ErrMissing
	}
	var data []byte
	err := db.QueryRowContext(ctx, `SELECT payload,request_digest FROM campaign_candidates WHERE tenant=? AND workspace=? AND campaign=? AND id=?`,
		i.Tenant, i.Workspace, campaign, id).Scan(&data, &signature)
	if err != nil {
		return result, "", missing(err)
	}
	return result, signature, json.Unmarshal(data, &result)
}

func (s *Store) CampaignCandidate(ctx context.Context, i Identity, campaign, id string) (CampaignCandidate, error) {
	result, _, err := readCampaignCandidate(ctx, s.db, i, campaign, id)
	if err != nil {
		return result, err
	}
	repo, err := estateRepository(ctx, s.db, i, result.Repository)
	if err != nil {
		return CampaignCandidate{}, err
	}
	if repo.PolicyRevision != result.Policy || len(repo.Policy.Export) == 0 {
		return CampaignCandidate{}, ErrDenied
	}
	if result.State == "running" && time.Now().UTC().After(result.Deadline) {
		result.State, result.Failure = "interrupted", "model-lease-expired"
	}
	return result, nil
}

func candidateChoices(raw []planning.SnapshotHunk, repo EstateRepository, notes map[string][]string) ([]model.Choice, []int) {
	var choices []model.Choice
	var mapping []int
	for index, hunk := range raw {
		if !snapshot.Within(hunk.Path, repo.Policy.Model) || len(hunk.Before) > 300 || len(hunk.After) > 300 {
			continue
		}
		choices = append(choices, model.Choice{Index: len(choices), Before: hunk.Before, Proposed: hunk.After,
			Risk: "contract", Function: hunk.Path, Review: notes[hunk.Path]})
		mapping = append(mapping, index)
	}
	return choices, mapping
}

func validCandidateInput(in CampaignCandidateInput) bool {
	if !digest.MatchString(in.Repository) || len(in.Review) > 50 ||
		in.Mode != "rule" && in.Mode != "model" || in.Mode == "model" && (in.Budget < 256 || in.Budget > 12_000) ||
		in.Mode == "rule" && (in.Budget != 0 || len(in.Review) != 0) {
		return false
	}
	for path, notes := range in.Review {
		if !snapshot.SafePath(path) || len(notes) > 4 {
			return false
		}
		for _, note := range notes {
			if len(note) == 0 || len(note) > 200 || strings.ContainsAny(note, "\x00\r\n") {
				return false
			}
		}
	}
	return true
}

func auditCandidate(ctx context.Context, tx *sql.Tx, i Identity, campaign string, item CampaignCandidate) error {
	return audit(ctx, tx, i, "campaign-candidate-"+item.State, campaign, map[string]any{"candidate_id": item.ID,
		"repository": item.Repository, "candidate_hash": item.Digest, "state": item.State, "failure": item.Failure, "usage": item.Usage})
}

func (s *Store) GenerateCampaignCandidate(ctx context.Context, i Identity, key, campaign string, in CampaignCandidateInput,
	provider *model.Client) (CampaignCandidate, error) {
	if !i.Can("maintainer") {
		return CampaignCandidate{}, ErrDenied
	}
	if !requestID.MatchString(key) || !validCandidateInput(in) {
		return CampaignCandidate{}, ErrInvalid
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		return CampaignCandidate{}, err
	}
	signature := snapshot.Digest(encoded)
	id := snapshot.Digest([]byte(i.Actor + "\x00" + key + "\x00" + campaign))
	var choices []model.Choice
	var mapping []int
	var fresh bool
	item, err := transaction(ctx, s, func(tx *sql.Tx) (CampaignCandidate, error) {
		if _, err := current(ctx, tx, i, true); err != nil {
			return CampaignCandidate{}, err
		}
		if err := checkGrant(ctx, tx, i); err != nil {
			return CampaignCandidate{}, err
		}
		c, err := readCampaign(ctx, tx, i, campaign)
		if err != nil {
			return CampaignCandidate{}, err
		}
		repo, err := estateRepository(ctx, tx, i, in.Repository)
		if err != nil {
			return CampaignCandidate{}, err
		}
		if err := requireCampaignPolicy(c, repo); err != nil || len(repo.Policy.Export) == 0 || i.Actor != repo.Owner {
			return CampaignCandidate{}, ErrDenied
		}
		prior, signed, err := readCampaignCandidate(ctx, tx, i, campaign, id)
		if err == nil {
			if signed != signature {
				return CampaignCandidate{}, ErrConflict
			}
			if prior.State == "running" && time.Now().UTC().After(prior.Deadline) {
				prior.State, prior.Failure = "interrupted", "model-lease-expired"
			}
			return prior, nil
		}
		if !errors.Is(err, ErrMissing) {
			return CampaignCandidate{}, err
		}
		change, ok := campaignChange(c, in.Wave, in.Repository)
		if !ok || c.Revision != in.Expected || c.State == "inventory" || c.State == "delivered" {
			return CampaignCandidate{}, ErrConflict
		}
		old, err := readEstateSource(ctx, tx, i, repo, change.Base)
		if err != nil {
			return CampaignCandidate{}, err
		}
		next, err := readEstateSource(ctx, tx, i, repo, change.Target)
		if err != nil {
			return CampaignCandidate{}, err
		}
		raw, err := planning.SnapshotHunks(old, next, repo.Policy.Export)
		if err != nil || len(raw) == 0 || len(raw) > 100 {
			return CampaignCandidate{}, fmt.Errorf("%w: pinned revision has no exportable patch or exceeds 100 hunks", ErrInvalid)
		}
		result := CampaignCandidate{ID: id, WaveChange: change, Wave: in.Wave, Mode: in.Mode, Actor: i.Actor,
			State: "ready", Revision: 1, Policy: repo.PolicyRevision, Deadline: time.Now().UTC().Add(2 * time.Minute)}
		var combined strings.Builder
		for _, hunk := range raw {
			combined.WriteString(hunk.Patch)
			result.Hunks = append(result.Hunks, CampaignHunk{ID: snapshot.Digest([]byte(hunk.Patch)), Path: hunk.Path,
				Line: hunk.Line, Patch: hunk.Patch, Selected: in.Mode == "rule"})
		}
		result.Digest = snapshot.Digest([]byte(combined.String()))
		if in.Mode == "model" {
			choices, mapping = candidateChoices(raw, repo, in.Review)
			result.State = "running"
			if len(choices) == 0 || !provider.Local() {
				result.State, result.Failure = "failed", "no-authorized-context-or-local-model"
				result.Usage = &model.Usage{Mode: "live", State: "rejected"}
			}
		}
		data, err := json.Marshal(result)
		if err != nil {
			return CampaignCandidate{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO campaign_candidates(tenant,workspace,campaign,id,wave_id,repository,actor,request_digest,state,deadline,payload)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, i.Tenant, i.Workspace, campaign, result.ID, result.Wave, result.Repository, result.Actor,
			signature, result.State, result.Deadline, data)
		if err != nil {
			return CampaignCandidate{}, err
		}
		fresh = true
		return result, auditCandidate(ctx, tx, i, campaign, result)
	})
	if err != nil || !fresh || item.State != "running" {
		return item, err
	}
	run, stop := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer stop()
	guidance, use, providerErr := provider.SuggestContracts(run, choices, in.Budget)
	item.Usage = &use
	item.State = "ready"
	if providerErr != nil || use.Metered && (use.PromptTokens > in.Budget || use.CompletionTokens > in.Budget-use.PromptTokens) {
		item.State, item.Failure = "failed", "local-model-output-rejected"
	} else {
		for _, index := range guidance.Indices {
			item.Hunks[mapping[index]].Selected = true
		}
	}
	return s.finishCampaignCandidate(i, campaign, item)
}

func (s *Store) finishCampaignCandidate(i Identity, id string, item CampaignCandidate) (CampaignCandidate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return transaction(ctx, s, func(tx *sql.Tx) (CampaignCandidate, error) {
		if _, err := current(ctx, tx, i, true); err != nil {
			return item, err
		}
		repo, err := estateRepository(ctx, tx, i, item.Repository)
		if err != nil {
			return item, err
		}
		if repo.PolicyRevision != item.Policy || checkGrant(ctx, tx, i) != nil || time.Now().UTC().After(item.Deadline) {
			item.State, item.Failure = "failed", "authorization-changed-or-lease-expired"
			item.Hunks = nil
		}
		item.Revision++
		if err := saveCampaignCandidate(ctx, tx, i, id, item); err != nil {
			return item, err
		}
		return item, auditCandidate(ctx, tx, i, id, item)
	})
}

func saveCampaignCandidate(ctx context.Context, tx *sql.Tx, i Identity, campaign string, item CampaignCandidate) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE campaign_candidates SET state=?,payload=? WHERE tenant=? AND workspace=? AND campaign=? AND id=?`,
		item.State, data, i.Tenant, i.Workspace, campaign, item.ID)
	return err
}

func (s *Store) ReviewCampaignHunk(ctx context.Context, i Identity, key, campaign, candidate, hunk string, in HunkDecision) (CampaignCandidate, error) {
	if in.Decision != "accept" && in.Decision != "reject" && in.Decision != "reset" || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 {
		return CampaignCandidate{}, ErrInvalid
	}
	signature := struct {
		Campaign, Candidate, Hunk string
		HunkDecision
	}{campaign, candidate, hunk, in}
	return mutate(ctx, s, i, key, "review-campaign-hunk", signature, []string{"reviewer"}, func(tx *sql.Tx, _ Workspace) (CampaignCandidate, error) {
		item, _, err := readCampaignCandidate(ctx, tx, i, campaign, candidate)
		if err != nil {
			return item, err
		}
		if i.Actor == item.Actor {
			return CampaignCandidate{}, ErrDenied
		}
		c, err := readCampaign(ctx, tx, i, campaign)
		if err != nil {
			return item, err
		}
		repo, err := estateRepository(ctx, tx, i, item.Repository)
		if err != nil {
			return CampaignCandidate{}, err
		}
		if err := requireCampaignPolicy(c, repo); err != nil || item.Policy != repo.PolicyRevision {
			return CampaignCandidate{}, ErrDenied
		}
		if change, ok := campaignChange(c, item.Wave, item.Repository); !ok || change != item.WaveChange || item.State != "ready" || item.Revision != in.Expected {
			return item, ErrConflict
		}
		for index := range item.Hunks {
			value := &item.Hunks[index]
			if value.ID != hunk {
				continue
			}
			if in.Decision == "accept" && !value.Selected {
				return item, ErrConflict
			}
			value.Decision, value.Reviewer, value.Reason = in.Decision, i.Actor, in.Reason
			item.Revision++
			if err := saveCampaignCandidate(ctx, tx, i, campaign, item); err != nil {
				return item, err
			}
			return item, audit(ctx, tx, i, "review-campaign-hunk", campaign, map[string]any{"candidate_id": item.ID,
				"hunk_id": hunk, "decision": in.Decision, "reason": in.Reason, "revision": item.Revision})
		}
		return item, ErrMissing
	}, func(tx *sql.Tx, prior CampaignCandidate) error {
		repo, err := estateRepository(ctx, tx, i, prior.Repository)
		if err != nil {
			return err
		}
		if repo.PolicyRevision != prior.Policy || len(repo.Policy.Export) == 0 {
			return ErrDenied
		}
		return nil
	})
}
