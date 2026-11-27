package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/impact"
	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type CampaignRepository struct {
	Repository string `json:"repository_id"`
	Old        string `json:"old_snapshot"`
	Next       string `json:"next_snapshot"`
	Test       string `json:"test_path"`
	Language   string `json:"language"`
}

type CampaignInput struct {
	Name          string               `json:"name"`
	Producer      CampaignRepository   `json:"producer"`
	ProviderRoute string               `json:"provider_router,omitempty"`
	Compatibility string               `json:"compatible_snapshot,omitempty"`
	Consumers     []CampaignRepository `json:"consumers"`
	LegacyTargets []string             `json:"legacy_targets"`
	ContractSet   string               `json:"contract_set"`
	Runner        string               `json:"runner_profile"`
	Strategy      string               `json:"strategy"`
}

type CampaignWave struct {
	ID          string       `json:"wave_id"`
	Description string       `json:"description"`
	Changes     []WaveChange `json:"repositories"`
}

type WaveChange struct {
	Repository string `json:"repository_id"`
	Base       string `json:"base_snapshot"`
	Target     string `json:"target_snapshot"`
}

type CampaignChoice struct {
	Strategy    string `json:"strategy"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason"`
	Deployments int    `json:"deployment_waves"`
}

type MigrationCampaign struct {
	ID string `json:"campaign_id"`
	CampaignInput
	State            string                `json:"state"`
	Revision         uint64                `json:"campaign_rev"`
	Impact           impact.ContractImpact `json:"impact"`
	Choices          []CampaignChoice      `json:"choices"`
	Waves            []CampaignWave        `json:"waves"`
	Policies         map[string]uint64     `json:"policy_revisions"`
	ActiveWave       int                   `json:"active_wave"`
	LegacyPlan       string                `json:"legacy_plan,omitempty"`
	LegacySelections []Selection           `json:"legacy_selections,omitempty"`
	ImpactRestricted bool                  `json:"impact_restricted,omitempty"`
}

func campaignRepositoryValid(item CampaignRepository, producer bool) bool {
	return digest.MatchString(item.Repository) && digest.MatchString(item.Old) && digest.MatchString(item.Next) &&
		item.Old != item.Next && snapshot.SafePath(item.Test) &&
		(item.Language == "php" && strings.HasSuffix(item.Test, ".php") || !producer && item.Language == "go" && strings.HasSuffix(item.Test, "_test.go"))
}

func validateCampaignInput(in CampaignInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 160 || strings.ContainsAny(in.Name, "\x00\r\n") ||
		!identifier.MatchString(in.ContractSet) || !identifier.MatchString(in.Runner) ||
		!campaignRepositoryValid(in.Producer, true) || len(in.Consumers) < 1 || len(in.Consumers) > 48 ||
		len(in.LegacyTargets) == 0 || len(in.LegacyTargets) > 100 ||
		in.ProviderRoute != "" && (!snapshot.SafePath(in.ProviderRoute) || !strings.HasSuffix(in.ProviderRoute, ".php")) {
		return fmt.Errorf("%w: campaign name, producer, consumers, runner or contract set", ErrInvalid)
	}
	validStrategy := slices.Contains([]string{"dual-version", "expand-contract", "one-cut"}, in.Strategy)
	validCompatibility := digest.MatchString(in.Compatibility) && in.Compatibility != in.Producer.Next && in.Compatibility != in.Producer.Old
	if !validStrategy || in.Strategy != "one-cut" && !validCompatibility || in.Strategy == "one-cut" && in.Compatibility != "" {
		return fmt.Errorf("%w: strategy or required compatibility snapshot", ErrInvalid)
	}
	seen := map[string]bool{in.Producer.Repository: true}
	for _, item := range in.Consumers {
		if seen[item.Repository] || !campaignRepositoryValid(item, false) || item.Language == "go" && in.ProviderRoute == "" {
			return fmt.Errorf("%w: duplicate or invalid consumer", ErrInvalid)
		}
		seen[item.Repository] = true
	}
	return nil
}

func readCampaign(ctx context.Context, db rowScanner, i Identity, id string) (MigrationCampaign, error) {
	return readJSON[MigrationCampaign](ctx, db, i, id, `SELECT payload FROM migration_campaigns WHERE tenant=? AND workspace=? AND id=?`)
}

func (s *Store) MigrationCampaign(ctx context.Context, i Identity, id string) (MigrationCampaign, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (MigrationCampaign, error) {
		if _, err := current(ctx, tx, i, true); err != nil {
			return MigrationCampaign{}, err
		}
		out, err := readCampaign(ctx, tx, i, id)
		if err != nil {
			return out, err
		}
		return campaignVisible(ctx, tx, i, out)
	})
}

func campaignVisible(ctx context.Context, db rowScanner, i Identity, c MigrationCampaign) (MigrationCampaign, error) {
	if err := checkCampaignPolicies(ctx, db, i, c); err != nil {
		if !errors.Is(err, ErrDenied) {
			return MigrationCampaign{}, err
		}
		c.Impact, c.ImpactRestricted = impact.ContractImpact{}, true
	}
	return c, nil
}

func checkCampaignPolicies(ctx context.Context, db rowScanner, i Identity, c MigrationCampaign) error {
	for repoID := range c.Policies {
		repo, err := estateRepository(ctx, db, i, repoID)
		if err != nil {
			return err
		}
		if err := requireCampaignPolicy(c, repo); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) MigrationCampaigns(ctx context.Context, i Identity, after string) ([]MigrationCampaign, error) {
	if after != "" && !digest.MatchString(after) {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM migration_campaigns WHERE tenant=? AND workspace=? AND id>?
		ORDER BY id LIMIT 20`, i.Tenant, i.Workspace, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []MigrationCampaign{}
	for rows.Next() {
		var campaign MigrationCampaign
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &campaign); err != nil {
			return nil, err
		}
		// The collection is a navigation summary. Fetch details under the workspace lock.
		campaign.Impact = impact.ContractImpact{}
		result = append(result, campaign)
	}
	return result, rows.Err()
}

func (s *Store) CreateMigrationCampaign(ctx context.Context, i Identity, key string, in CampaignInput) (MigrationCampaign, error) {
	if err := validateCampaignInput(in); err != nil {
		return MigrationCampaign{}, err
	}
	return mutate(ctx, s, i, key, "create-migration-campaign", in, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (MigrationCampaign, error) {
		id := snapshot.Digest([]byte(i.Tenant + "\x00" + i.Workspace + "\x00" + i.Actor + "\x00" + key))
		out := MigrationCampaign{ID: id, CampaignInput: in, State: "inventory", Revision: 1}
		return out, insertCampaign(ctx, tx, i, out, "create-migration-campaign")
	})
}

type LegacyCampaignInput struct {
	Name string `json:"name"`
	Plan string `json:"plan_id"`
}

func (s *Store) ImportLegacyCampaign(ctx context.Context, i Identity, key string, in LegacyCampaignInput) (MigrationCampaign, error) {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 160 || strings.ContainsAny(in.Name, "\x00\r\n") || !digest.MatchString(in.Plan) {
		return MigrationCampaign{}, ErrInvalid
	}
	return mutate(ctx, s, i, key, "import-legacy-campaign", in, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (MigrationCampaign, error) {
		plan, err := readJSON[planning.Plan](ctx, tx, i, in.Plan, selectPlan)
		if err != nil {
			return MigrationCampaign{}, err
		}
		adopted, err := selections(ctx, tx, i, in.Plan)
		if err != nil {
			return MigrationCampaign{}, err
		}
		out := MigrationCampaign{
			ID:            snapshot.Digest([]byte(i.Tenant + "\x00" + i.Workspace + "\x00legacy\x00" + in.Plan)),
			CampaignInput: CampaignInput{Name: in.Name, Strategy: "single-repository", Producer: CampaignRepository{Old: plan.Baseline, Language: "php"}},
			State:         "legacy", Revision: 1, LegacyPlan: in.Plan, LegacySelections: adopted,
		}
		if prior, err := readCampaign(ctx, tx, i, out.ID); err == nil {
			if prior.Name != in.Name {
				return MigrationCampaign{}, ErrConflict
			}
			return prior, nil
		} else if !errors.Is(err, ErrMissing) {
			return MigrationCampaign{}, err
		}
		return out, insertCampaign(ctx, tx, i, out, "import-legacy-campaign")
	})
}

func insertCampaign(ctx context.Context, tx *sql.Tx, i Identity, out MigrationCampaign, action string) error {
	body, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_campaigns(tenant,workspace,id,state,revision,payload) VALUES(?,?,?,?,?,?)`,
		i.Tenant, i.Workspace, out.ID, out.State, out.Revision, body)
	if err != nil {
		return err
	}
	return audit(ctx, tx, i, action, out.ID, map[string]any{"revision": out.Revision, "state": out.State, "legacy_plan": out.LegacyPlan})
}

func saveCampaign(ctx context.Context, tx *sql.Tx, i Identity, out MigrationCampaign, action string, detail any) error {
	data, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE migration_campaigns SET state=?,revision=?,payload=? WHERE tenant=? AND workspace=? AND id=?`,
		out.State, out.Revision, data, i.Tenant, i.Workspace, out.ID)
	if err != nil {
		return err
	}
	return audit(ctx, tx, i, action, out.ID, detail)
}

type CampaignRebaseInput struct {
	Expected   uint64 `json:"campaign_rev"`
	Repository string `json:"repository_id"`
	Current    string `json:"current_snapshot"`
	Target     string `json:"target_snapshot"`
}

// RebaseCampaign pins the pending repository to a new, owner-staged target.
// Previously adopted waves and their receipts stay bound to their original bytes.
func (s *Store) RebaseCampaign(ctx context.Context, i Identity, key, id string, in CampaignRebaseInput) (MigrationCampaign, error) {
	if in.Current == in.Target || !digest.MatchString(in.Repository) || !digest.MatchString(in.Current) || !digest.MatchString(in.Target) {
		return MigrationCampaign{}, ErrInvalid
	}
	request := struct {
		ID string
		CampaignRebaseInput
	}{id, in}
	return mutate(ctx, s, i, key, "rebase-migration-campaign", request, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (MigrationCampaign, error) {
		out, err := readCampaign(ctx, tx, i, id)
		if err != nil {
			return out, err
		}
		if out.Revision != in.Expected || out.ActiveWave >= len(out.Waves) {
			return out, ErrConflict
		}
		if err := checkCampaignPolicies(ctx, tx, i, out); err != nil {
			return out, err
		}
		wave := out.Waves[out.ActiveWave]
		change, ok := campaignChange(out, wave.ID, in.Repository)
		if !ok || change.Base == in.Current {
			return out, ErrConflict
		}
		repo, err := estateRepository(ctx, tx, i, in.Repository)
		if err != nil {
			return out, err
		}
		if repo.Owner != i.Actor || len(repo.Policy.Export) == 0 {
			return MigrationCampaign{}, ErrDenied
		}
		if repo.Snapshot != in.Current {
			return out, ErrConflict
		}
		var adopted int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM campaign_bundles WHERE tenant=? AND workspace=? AND campaign=?
			AND wave_id=? AND repository=? AND adoption_ref IS NOT NULL`, i.Tenant, i.Workspace, id, wave.ID, repo.ID).Scan(&adopted); err != nil {
			return out, err
		}
		if adopted != 0 {
			return out, ErrConflict
		}
		switch {
		case repo.ID != out.Producer.Repository:
			for index := range out.Consumers {
				if out.Consumers[index].Repository == repo.ID {
					out.Consumers[index].Old, out.Consumers[index].Next = in.Current, in.Target
				}
			}
		case out.Strategy == "one-cut":
			out.Producer.Old, out.Producer.Next = in.Current, in.Target
		case wave.ID == "W1":
			out.Producer.Old, out.Compatibility = in.Current, in.Target
		case wave.ID == "W3":
			out.Compatibility, out.Producer.Next = in.Current, in.Target
		default:
			return out, ErrConflict
		}
		if err := validateCampaignInput(out.CampaignInput); err != nil {
			return out, err
		}
		history := slices.Clone(out.Waves[:out.ActiveWave])
		if err := loadCampaignPlan(ctx, tx, i, &out, false); err != nil {
			return out, err
		}
		copy(out.Waves, history)
		out.Revision++
		return out, saveCampaign(ctx, tx, i, out, "rebase-migration-campaign", map[string]any{
			"revision": out.Revision, "wave_id": wave.ID, "repository_id": repo.ID, "prior_base": change.Base,
			"prior_target": change.Target, "current_snapshot": in.Current, "target_snapshot": in.Target,
		})
	}, func(tx *sql.Tx, prior MigrationCampaign) error {
		return checkCampaignPolicies(ctx, tx, i, prior)
	})
}

func (s *Store) FreezeMigrationCampaign(ctx context.Context, i Identity, key, id string, expected uint64) (MigrationCampaign, error) {
	request := struct {
		ID       string
		Expected uint64
	}{id, expected}
	return mutate(ctx, s, i, key, "freeze-migration-campaign", request, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (MigrationCampaign, error) {
		out, err := readCampaign(ctx, tx, i, id)
		if err != nil {
			return out, err
		}
		if out.State != "inventory" || out.Revision != expected {
			return out, ErrConflict
		}
		if err := freezeCampaign(ctx, tx, i, &out); err != nil {
			return out, err
		}
		out.State, out.Revision = "frozen", out.Revision+1
		return out, saveCampaign(ctx, tx, i, out, "freeze-migration-campaign", map[string]any{
			"revision": out.Revision, "state": out.State, "runner_profile": out.Runner, "waves": out.Waves,
		})
	}, func(tx *sql.Tx, prior MigrationCampaign) error {
		return checkCampaignPolicies(ctx, tx, i, prior)
	})
}

func freezeCampaign(ctx context.Context, tx *sql.Tx, i Identity, out *MigrationCampaign) error {
	return loadCampaignPlan(ctx, tx, i, out, true)
}

func loadCampaignPlan(ctx context.Context, tx *sql.Tx, i Identity, out *MigrationCampaign, enforceLiveBases bool) error {
	out.Policies = make(map[string]uint64)
	load := func(pin CampaignRepository) (EstateRepository, impact.ContractCatalog, impact.ContractCatalog, error) {
		repo, err := estateRepository(ctx, tx, i, pin.Repository)
		if err != nil {
			return repo, impact.ContractCatalog{}, impact.ContractCatalog{}, err
		}
		if !slices.Contains(repo.Policy.Runners, out.Runner) || !snapshot.Within(pin.Test, repo.Policy.Index) {
			return repo, impact.ContractCatalog{}, impact.ContractCatalog{}, ErrDenied
		}
		if enforceLiveBases && repo.Snapshot != pin.Old {
			return repo, impact.ContractCatalog{}, impact.ContractCatalog{}, fmt.Errorf("%w: repository %s drifted from its proposed base", ErrConflict, repo.ID)
		}
		out.Policies[repo.ID] = repo.PolicyRevision
		old, err := readEstateCatalog(ctx, tx, i, repo, pin.Old)
		if err != nil {
			return repo, old, impact.ContractCatalog{}, err
		}
		next, err := readEstateCatalog(ctx, tx, i, repo, pin.Next)
		return repo, old, next, err
	}
	producer, before, after, err := load(out.Producer)
	if err != nil {
		return err
	}
	if out.ProviderRoute != "" && !snapshot.Within(out.ProviderRoute, producer.Policy.Index) {
		return ErrDenied
	}
	var compat impact.ContractCatalog
	if out.Compatibility != "" {
		compat, err = readEstateCatalog(ctx, tx, i, producer, out.Compatibility)
		if err != nil {
			return err
		}
	}
	var consumers []impact.ContractConsumer
	for _, pin := range out.Consumers {
		repo, old, _, err := load(pin)
		if err != nil {
			return err
		}
		consumers = append(consumers, impact.ContractConsumer{Repository: repo.ID, Catalog: old})
	}
	out.Impact, err = impact.CompareContracts(before, after, consumers)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	return planCampaign(out, before, compat, after)
}

func planCampaign(out *MigrationCampaign, before, compatible, after impact.ContractCatalog) error {
	direct := out.Impact.Unresolved == 0 && !slices.ContainsFunc(out.Impact.Changes, func(change impact.ContractChange) bool {
		return len(change.ConsumerEdges) > 0
	})
	out.Choices = []CampaignChoice{
		{Strategy: "dual-version", Available: true, Reason: "Keep both API versions while consumers migrate", Deployments: 3},
		{Strategy: "expand-contract", Available: true, Reason: "Extend the provider contract before tightening it", Deployments: 3},
		{Strategy: "one-cut", Available: direct, Reason: "Requires that all indexed consumers are independent of the changed API", Deployments: 1},
	}
	find := func(c impact.ContractCatalog, target string) *impact.ContractSymbol {
		for index := range c.Symbols {
			if c.Symbols[index].Target == target {
				return &c.Symbols[index]
			}
		}
		return nil
	}
	seen := map[string]bool{}
	for _, target := range out.LegacyTargets {
		old, next := find(before, target), find(after, target)
		if old == nil || seen[target] || next != nil && next.Signature == old.Signature && slices.Equal(next.Required, old.Required) {
			return fmt.Errorf("%w: legacy target %q has no change between the pinned snapshots", ErrInvalid, target)
		}
		seen[target] = true
		if out.Strategy != "one-cut" {
			preserved := find(compatible, target)
			if preserved == nil || preserved.Signature != old.Signature ||
				slices.ContainsFunc(old.Required, func(field string) bool { return !slices.Contains(preserved.Required, field) }) {
				return fmt.Errorf("%w: compatibility snapshot breaks %s", ErrInvalid, target)
			}
		}
	}
	consumers := make([]WaveChange, 0, len(out.Consumers))
	for _, item := range out.Consumers {
		consumers = append(consumers, WaveChange{Repository: item.Repository, Base: item.Old, Target: item.Next})
	}
	provider := out.Producer
	if out.Strategy == "one-cut" {
		if !direct {
			return fmt.Errorf("%w: direct release has dependent or unresolved callsites", ErrConflict)
		}
		out.Waves = []CampaignWave{{ID: "W1", Description: "Deploy coordinated release", Changes: append(consumers,
			WaveChange{Repository: provider.Repository, Base: provider.Old, Target: provider.Next})}}
		return nil
	}
	out.Waves = []CampaignWave{
		{ID: "W1", Description: "Release the compatibility provider", Changes: []WaveChange{{provider.Repository, provider.Old, out.Compatibility}}},
		{ID: "W2", Description: "Migrate the consumers", Changes: consumers},
		{ID: "W3", Description: "Close the compatibility interval", Changes: []WaveChange{{provider.Repository, out.Compatibility, provider.Next}}},
	}
	return nil
}

var errWaveIncomplete = fmt.Errorf("%w: the active wave still has incomplete reviews, tests or adoption receipts", ErrConflict)

func requireCampaignPolicy(c MigrationCampaign, repo EstateRepository) error {
	value, known := c.Policies[repo.ID]
	if !known || value != repo.PolicyRevision {
		return fmt.Errorf("%w: repository authorization changed", ErrDenied)
	}
	return nil
}
