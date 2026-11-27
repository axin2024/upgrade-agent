package records

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/axin2024/upgrade-agent/internal/impact"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

type RehearsalInput struct {
	Expected    uint64   `json:"campaign_rev"`
	Wave        string   `json:"wave_id"`
	RepoDigests []string `json:"repo_digests"`
	ContractSet string   `json:"contract_set"`
	Runner      string   `json:"runner_profile"`
}

type MatrixPair struct {
	ID            string                    `json:"pair_id"`
	Combination   string                    `json:"combination"`
	Provider      EstatePin                 `json:"provider"`
	Consumer      EstatePin                 `json:"consumer"`
	ProviderTest  string                    `json:"provider_test"`
	ProviderRoute string                    `json:"provider_router,omitempty"`
	ConsumerTest  string                    `json:"consumer_test"`
	Language      string                    `json:"language"`
	State         string                    `json:"state"`
	Evidence      []validation.MatrixResult `json:"evidence,omitempty"`
}

type CampaignRehearsal struct {
	ID string `json:"rehearsal_id"`
	RehearsalInput
	Matrix       string       `json:"matrix_id"`
	State        string       `json:"state"`
	Pairs        []MatrixPair `json:"pairs,omitempty"`
	EvidenceRefs []string     `json:"evidence_refs"`
	FailedPairs  []string     `json:"failed_pairs"`
}

func planMatrix(c MigrationCampaign, wave string) []MatrixPair {
	type combination struct {
		label, provider string
		next            bool
	}
	var variants []combination
	if c.Strategy == "one-cut" {
		variants = append(variants, combination{"old-provider/old-consumer", c.Producer.Old, false},
			combination{"final-provider/new-consumer", c.Producer.Next, true})
	} else {
		if wave == "W1" {
			variants = append(variants, combination{"old-provider/old-consumer", c.Producer.Old, false})
		}
		if wave != "W3" {
			variants = append(variants, combination{"compatible-provider/old-consumer", c.Compatibility, false})
		}
		if wave != "W1" {
			variants = append(variants, combination{"compatible-provider/new-consumer", c.Compatibility, true},
				combination{"final-provider/new-consumer", c.Producer.Next, true})
		}
	}
	result := make([]MatrixPair, 0, len(variants)*len(c.Consumers))
	for _, variant := range variants {
		for _, consumer := range c.Consumers {
			revision := consumer.Old
			if variant.next {
				revision = consumer.Next
			}
			pair := MatrixPair{Combination: variant.label, Provider: EstatePin{c.Producer.Repository, variant.provider},
				Consumer: EstatePin{consumer.Repository, revision}, ProviderTest: c.Producer.Test, ConsumerTest: consumer.Test,
				ProviderRoute: c.ProviderRoute, Language: consumer.Language, State: "queued"}
			pair.ID = snapshot.Digest([]byte(c.ID + "\x00" + wave + "\x00" + variant.label + "\x00" + consumer.Repository))
			result = append(result, pair)
		}
	}
	return result
}

func matrixDigests(pairs []MatrixPair) []string {
	result := make([]string, 0, 2*len(pairs))
	for _, pair := range pairs {
		result = append(result, pair.Provider.Snapshot, pair.Consumer.Snapshot)
	}
	slices.Sort(result)
	return slices.Compact(result)
}

func matrixDigest(pairs []MatrixPair) string {
	data, _ := json.Marshal(pairs)
	return snapshot.Digest(data)
}

func validRunnerRepository(c MigrationCampaign, repo EstateRepository) error {
	if err := requireCampaignPolicy(c, repo); err != nil || !slices.Contains(repo.Policy.Runners, c.Runner) {
		return ErrDenied
	}
	return nil
}

func (s *Store) SubmitCampaignRehearsal(ctx context.Context, i Identity, key, id string, in RehearsalInput) (CampaignRehearsal, error) {
	if len(in.RepoDigests) < 2 || len(in.RepoDigests) > 100 || slices.ContainsFunc(in.RepoDigests, func(v string) bool { return !digest.MatchString(v) }) {
		return CampaignRehearsal{}, fmt.Errorf("%w: repo_digests", ErrInvalid)
	}
	in.RepoDigests = slices.Clone(in.RepoDigests)
	slices.Sort(in.RepoDigests)
	request := struct {
		ID string
		RehearsalInput
	}{id, in}
	return mutate(ctx, s, i, key, "submit-campaign-rehearsal", request, []string{"verifier"}, func(tx *sql.Tx, _ Workspace) (CampaignRehearsal, error) {
		c, err := readCampaign(ctx, tx, i, id)
		if err != nil {
			return CampaignRehearsal{}, err
		}
		if c.Revision != in.Expected || c.ActiveWave >= len(c.Waves) || c.Waves[c.ActiveWave].ID != in.Wave {
			return CampaignRehearsal{}, ErrConflict
		}
		if c.Runner != in.Runner || c.ContractSet != in.ContractSet {
			return CampaignRehearsal{}, fmt.Errorf("%w: runner_profile or contract_set differs from frozen campaign", ErrInvalid)
		}
		for repoID := range c.Policies {
			repo, err := estateRepository(ctx, tx, i, repoID)
			if err != nil {
				return CampaignRehearsal{}, err
			}
			if err := validRunnerRepository(c, repo); err != nil {
				return CampaignRehearsal{}, err
			}
		}
		if c.Strategy != "one-cut" && in.Wave == "W3" {
			if err := checkLegacyCalls(ctx, tx, i, c); err != nil {
				return CampaignRehearsal{}, err
			}
		}
		pairs := planMatrix(c, in.Wave)
		if !slices.Equal(in.RepoDigests, matrixDigests(pairs)) {
			return CampaignRehearsal{}, fmt.Errorf("%w: repo_digests differ from frozen pairs", ErrInvalid)
		}
		result := CampaignRehearsal{ID: snapshot.Digest([]byte(id + "\x00" + i.Actor + "\x00" + key)), RehearsalInput: in,
			Matrix: matrixDigest(pairs), State: "queued", EvidenceRefs: []string{}, FailedPairs: []string{}}
		body, err := json.Marshal(result)
		if err != nil {
			return result, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO campaign_rehearsals(tenant,workspace,campaign,id,wave_id,payload) VALUES(?,?,?,?,?,?)`,
			i.Tenant, i.Workspace, id, result.ID, in.Wave, body)
		if err != nil {
			return result, err
		}
		for _, pair := range pairs {
			data, err := json.Marshal(pair)
			if err != nil {
				return result, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO campaign_matrix_pairs(tenant,workspace,campaign,rehearsal,id,state,runner_profile,payload)
				VALUES(?,?,?,?,?,?,?,?)`, i.Tenant, i.Workspace, id, result.ID, pair.ID, pair.State, in.Runner, data); err != nil {
				return result, err
			}
		}
		result.Pairs = pairs
		return result, audit(ctx, tx, i, "submit-campaign-rehearsal", id, map[string]any{"rehearsal_id": result.ID, "matrix_id": result.Matrix, "pairs": len(pairs)})
	}, func(tx *sql.Tx, _ CampaignRehearsal) error {
		return checkRehearsalPolicies(ctx, tx, i, id)
	})
}

func checkLegacyCalls(ctx context.Context, db rowScanner, i Identity, c MigrationCampaign) error {
	producer, err := estateRepository(ctx, db, i, c.Producer.Repository)
	if err != nil {
		return err
	}
	old, err := readEstateCatalog(ctx, db, i, producer, c.Producer.Old)
	if err != nil {
		return err
	}
	final, err := readEstateCatalog(ctx, db, i, producer, c.Producer.Next)
	if err != nil {
		return err
	}
	var unresolvedNames []string
	for _, item := range c.LegacyTargets {
		if parts := strings.FieldsFunc(item, func(r rune) bool { return r == ':' || r == '.' || r == '\\' }); len(parts) != 0 {
			unresolvedNames = append(unresolvedNames, strings.ToLower(parts[len(parts)-1]))
		}
	}
	var catalogs []impact.ContractConsumer
	for _, consumer := range c.Consumers {
		repo, err := estateRepository(ctx, db, i, consumer.Repository)
		if err != nil {
			return err
		}
		if repo.Snapshot != consumer.Next {
			return fmt.Errorf("%w: consumer %s has not adopted the new baseline", ErrConflict, repo.ID)
		}
		catalog, err := readEstateCatalog(ctx, db, i, repo, repo.Snapshot)
		if err != nil {
			return err
		}
		for _, call := range catalog.Calls {
			if call.Confidence == "unknown" && (call.Target == "" || slices.Contains(unresolvedNames, strings.ToLower(call.Target))) {
				return fmt.Errorf("%w: unresolved legacy call in %s at %s:%d", ErrConflict, repo.ID, call.Location.Path, call.Location.Line)
			}
		}
		catalogs = append(catalogs, impact.ContractConsumer{Repository: repo.ID, Catalog: catalog})
	}
	impactReport, err := impact.CompareContracts(old, final, catalogs)
	if err != nil {
		return err
	}
	for _, change := range impactReport.Changes {
		if slices.Contains(c.LegacyTargets, change.Previous.Target) && len(change.ConsumerEdges) > 0 {
			return fmt.Errorf("%w: %s still has consumers in the new baseline", ErrConflict, change.Previous.Target)
		}
	}
	return nil
}

type estateQueryer interface {
	rowScanner
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRehearsal(ctx context.Context, db estateQueryer, i Identity, campaign, id string) (CampaignRehearsal, error) {
	var result CampaignRehearsal
	if !digest.MatchString(campaign) || !digest.MatchString(id) {
		return result, ErrMissing
	}
	var raw []byte
	err := db.QueryRowContext(ctx, `SELECT payload FROM campaign_rehearsals WHERE tenant=? AND workspace=? AND campaign=? AND id=?`,
		i.Tenant, i.Workspace, campaign, id).Scan(&raw)
	if err != nil {
		return result, missing(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	rows, err := db.QueryContext(ctx, `SELECT state,payload FROM campaign_matrix_pairs WHERE tenant=? AND workspace=? AND campaign=? AND rehearsal=? ORDER BY id`,
		i.Tenant, i.Workspace, campaign, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	result.State = "passed"
	var queued, running bool
	for rows.Next() {
		var pair MatrixPair
		var state string
		if err := rows.Scan(&state, &raw); err != nil {
			return result, err
		}
		if err := json.Unmarshal(raw, &pair); err != nil {
			return result, err
		}
		pair.State = state
		switch state {
		case "queued":
			queued = true
		case "running":
			running = true
		case "passed":
		default:
			result.FailedPairs = append(result.FailedPairs, pair.ID)
		}
		if len(pair.Evidence) > 0 {
			result.EvidenceRefs = append(result.EvidenceRefs, pair.Evidence[len(pair.Evidence)-1].Evidence)
		}
		result.Pairs = append(result.Pairs, pair)
	}
	switch {
	case len(result.Pairs) == 0:
		return result, errors.New("rehearsal has no frozen pairs")
	case running:
		result.State = "running"
	case queued:
		result.State = "queued"
	case len(result.FailedPairs) != 0:
		result.State = "blocked"
	}
	return result, rows.Err()
}

func (s *Store) CampaignRehearsal(ctx context.Context, i Identity, campaign, id string) (CampaignRehearsal, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (CampaignRehearsal, error) {
		if _, err := current(ctx, tx, i, true); err != nil {
			return CampaignRehearsal{}, err
		}
		if err := checkRehearsalPolicies(ctx, tx, i, campaign); err != nil {
			return CampaignRehearsal{}, err
		}
		return readRehearsal(ctx, tx, i, campaign, id)
	})
}

func checkRehearsalPolicies(ctx context.Context, tx *sql.Tx, i Identity, campaign string) error {
	c, err := readCampaign(ctx, tx, i, campaign)
	if err != nil {
		return err
	}
	return checkCampaignPolicies(ctx, tx, i, c)
}

func (s *Store) RetryCampaignRehearsal(ctx context.Context, i Identity, key, campaign, id string) (CampaignRehearsal, error) {
	return mutate(ctx, s, i, key, "retry-campaign-rehearsal", [2]string{campaign, id}, []string{"verifier"}, func(tx *sql.Tx, _ Workspace) (CampaignRehearsal, error) {
		c, err := readCampaign(ctx, tx, i, campaign)
		if err != nil {
			return CampaignRehearsal{}, err
		}
		out, err := readRehearsal(ctx, tx, i, campaign, id)
		if err != nil {
			return out, err
		}
		if out.State != "blocked" || c.ActiveWave >= len(c.Waves) || c.Waves[c.ActiveWave].ID != out.Wave ||
			out.Matrix != matrixDigest(planMatrix(c, out.Wave)) {
			return out, ErrConflict
		}
		for repoID := range c.Policies {
			repo, err := estateRepository(ctx, tx, i, repoID)
			if err != nil {
				return out, err
			}
			if err := validRunnerRepository(c, repo); err != nil {
				return out, err
			}
		}
		for index := range out.Pairs {
			pair := &out.Pairs[index]
			if pair.State == "passed" || len(pair.Evidence) >= 5 {
				continue
			}
			_, err := tx.ExecContext(ctx, `UPDATE campaign_matrix_pairs SET state='queued',queued_at=UTC_TIMESTAMP(6),lease=NULL,lease_until=NULL
				WHERE tenant=? AND workspace=? AND campaign=? AND rehearsal=? AND id=? AND state=?`,
				i.Tenant, i.Workspace, campaign, id, pair.ID, pair.State)
			if err != nil {
				return out, err
			}
			pair.State, out.State = "queued", "queued"
		}
		if out.State != "queued" {
			return out, fmt.Errorf("%w: failed pairs reached their retry limit", ErrConflict)
		}
		return out, audit(ctx, tx, i, "retry-campaign-rehearsal", campaign, map[string]any{"rehearsal_id": id, "previous_failures": out.FailedPairs})
	}, func(tx *sql.Tx, _ CampaignRehearsal) error {
		return checkRehearsalPolicies(ctx, tx, i, campaign)
	})
}

type matrixClaim struct {
	identity  Identity
	campaign  string
	rehearsal string
	lease     string
	pair      MatrixPair
}

func (s *Store) claimMatrixPair(ctx context.Context, profile string) (matrixClaim, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (matrixClaim, error) {
		var job matrixClaim
		var data []byte
		err := tx.QueryRowContext(ctx, `SELECT tenant,workspace,campaign,rehearsal,payload FROM campaign_matrix_pairs
			WHERE runner_profile=? AND (state='queued' OR (state='running' AND lease_until<=UTC_TIMESTAMP(6)))
			ORDER BY queued_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, profile).Scan(&job.identity.Tenant, &job.identity.Workspace, &job.campaign, &job.rehearsal, &data)
		if err != nil {
			return job, missing(err)
		}
		if err := json.Unmarshal(data, &job.pair); err != nil {
			return job, err
		}
		job.identity.Actor = "isolated-matrix-runner"
		job.lease = hex.EncodeToString(randBytes(16))
		_, err = tx.ExecContext(ctx, `UPDATE campaign_matrix_pairs SET state='running',lease=?,lease_until=UTC_TIMESTAMP(6)+INTERVAL 4 MINUTE
			WHERE tenant=? AND workspace=? AND campaign=? AND rehearsal=? AND id=?`,
			job.lease, job.identity.Tenant, job.identity.Workspace, job.campaign, job.rehearsal, job.pair.ID)
		return job, err
	})
}

func (s *Store) readMatrixSources(ctx context.Context, profile string, job matrixClaim) (validation.MatrixInput, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (validation.MatrixInput, error) {
		in := validation.MatrixInput{ProviderTest: job.pair.ProviderTest, ProviderRoute: job.pair.ProviderRoute,
			ConsumerTest: job.pair.ConsumerTest, Language: job.pair.Language}
		c, err := readCampaign(ctx, tx, job.identity, job.campaign)
		if err != nil || c.Runner != profile {
			return in, ErrDenied
		}
		providerRepo, err := estateRepository(ctx, tx, job.identity, job.pair.Provider.Repository)
		if err != nil {
			return in, err
		}
		consumerRepo, err := estateRepository(ctx, tx, job.identity, job.pair.Consumer.Repository)
		if err != nil {
			return in, err
		}
		if validRunnerRepository(c, providerRepo) != nil || validRunnerRepository(c, consumerRepo) != nil {
			return in, ErrDenied
		}
		in.Provider, err = readEstateSource(ctx, tx, job.identity, providerRepo, job.pair.Provider.Snapshot)
		if err != nil {
			return in, err
		}
		in.Consumer, err = readEstateSource(ctx, tx, job.identity, consumerRepo, job.pair.Consumer.Snapshot)
		return in, err
	})
}

func (s *Store) RunCampaignPair(ctx context.Context, profile string, runner validation.MatrixRunner) (bool, error) {
	if runner == nil || !identifier.MatchString(profile) {
		return false, ErrInvalid
	}
	job, err := s.claimMatrixPair(ctx, profile)
	if errors.Is(err, ErrMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	in, err := s.readMatrixSources(ctx, profile, job)
	var report validation.MatrixResult
	switch {
	case errors.Is(err, ErrDenied):
		report = validation.MatrixResult{Observation: validation.Observation{State: "rejected", ExitCode: -1}}
	case err != nil:
		report = validation.MatrixResult{Observation: validation.Observation{State: "unavailable", ExitCode: -1}}
	default:
		report = runner.RunMatrix(ctx, in)
		if report.Provider != in.Provider.Digest || report.Consumer != in.Consumer.Digest || report.DependencySHA != validation.MatrixDependencySHA(in) ||
			validation.SealMatrixResult(report).Evidence != report.Evidence || report.State == "passed" && report.ExitCode != 0 ||
			!slices.Contains([]string{"passed", "failed", "dependency-unavailable", "unavailable", "timeout", "resource-limit", "output-limit", "cancelled", "rejected"}, report.State) {
			report = validation.MatrixResult{Observation: validation.Observation{State: "unavailable", ExitCode: -1}, Output: "runner evidence did not match the frozen pair"}
		}
	}
	report.Provider, report.Consumer = job.pair.Provider.Snapshot, job.pair.Consumer.Snapshot
	report = validation.SealMatrixResult(report)
	job.pair.State = report.State
	job.pair.Evidence = append(job.pair.Evidence, report)
	exec, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	_, err = transaction(exec, s, func(tx *sql.Tx) (bool, error) {
		data, err := json.Marshal(job.pair)
		if err != nil {
			return false, err
		}
		updated, err := tx.ExecContext(exec, `UPDATE campaign_matrix_pairs SET state=?,payload=?,lease=NULL,lease_until=NULL
			WHERE tenant=? AND workspace=? AND campaign=? AND rehearsal=? AND id=? AND lease=? AND lease_until>UTC_TIMESTAMP(6)`,
			job.pair.State, data, job.identity.Tenant, job.identity.Workspace, job.campaign, job.rehearsal, job.pair.ID, job.lease)
		if err != nil {
			return false, err
		}
		count, err := updated.RowsAffected()
		if err != nil || count == 0 {
			return false, err
		}
		return true, audit(exec, tx, job.identity, "complete-campaign-pair", job.campaign, map[string]any{"pair_id": job.pair.ID, "state": report.State, "evidence": report.Evidence})
	})
	return true, err
}
