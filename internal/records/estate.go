package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/impact"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type EstateRegistration struct {
	Package string `json:"package"`
	Origin  string `json:"origin"`
	Owner   string `json:"owner"`
	Version string `json:"version"`
}

type EstatePolicy struct {
	Index   []string `json:"allowed_paths"`
	Model   []string `json:"model_paths"`
	Export  []string `json:"export_paths"`
	Runners []string `json:"runner_profiles"`
}

type EstatePolicyInput struct {
	Expected uint64 `json:"expected_revision"`
	EstatePolicy
}

type EstateRepository struct {
	ID string `json:"repository_id"`
	EstateRegistration
	Policy         EstatePolicy `json:"policy"`
	PolicyRevision uint64       `json:"policy_revision"`
	Revision       uint64       `json:"revision"`
	Snapshot       string       `json:"snapshot_id,omitempty"`
	State          string       `json:"state"`
}

type EstateSnapshotInput struct {
	Expected uint64            `json:"expected_revision"`
	Version  string            `json:"version"`
	Snapshot snapshot.Snapshot `json:"snapshot"`
	Stage    bool              `json:"stage_only,omitempty"`
}

type EstatePin struct {
	Repository string `json:"repository_id"`
	Snapshot   string `json:"snapshot_id"`
}

type EstateImpactInput struct {
	Producer  string      `json:"producer_id"`
	Previous  string      `json:"previous_snapshot"`
	Current   string      `json:"current_snapshot"`
	Consumers []EstatePin `json:"consumers"`
}

var estatePackage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,159}$`)
var estateVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+/-]{0,95}$`)

const estateRepositorySelect = `SELECT id,package_name,origin,owner,version_name,policy,policy_revision,revision,COALESCE(snapshot,'')
	FROM estate_repositories WHERE tenant=? AND workspace=?`

type estateScanner interface{ Scan(...any) error }

func scanEstateRepository(row estateScanner) (EstateRepository, error) {
	var result EstateRepository
	var policy []byte
	if err := row.Scan(&result.ID, &result.Package, &result.Origin, &result.Owner, &result.Version,
		&policy, &result.PolicyRevision, &result.Revision, &result.Snapshot); err != nil {
		return result, missing(err)
	}
	result.State = "pending"
	if result.Snapshot != "" && len(policy) != 0 {
		result.State = "indexed"
	}
	return result, json.Unmarshal(policy, &result.Policy)
}

func estateRepository(ctx context.Context, db rowScanner, i Identity, id string) (EstateRepository, error) {
	if !digest.MatchString(id) {
		return EstateRepository{}, ErrMissing
	}
	return scanEstateRepository(db.QueryRowContext(ctx, estateRepositorySelect+" AND id=?", i.Tenant, i.Workspace, id))
}

func (s *Store) EstateRepositories(ctx context.Context, i Identity, after string) ([]EstateRepository, error) {
	if after != "" && !digest.MatchString(after) {
		return nil, ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, estateRepositorySelect+" AND id>? ORDER BY id LIMIT 100", i.Tenant, i.Workspace, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []EstateRepository{}
	for rows.Next() {
		repo, err := scanEstateRepository(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, repo)
	}
	return result, rows.Err()
}

func canonicalEstateOrigin(origin string) (string, bool) {
	parsed, err := url.Parse(origin)
	if err != nil || len(origin) > 512 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.ForceQuery ||
		!estatePackage.MatchString(strings.TrimPrefix(strings.TrimSuffix(parsed.Path, "/"), "/")) {
		return "", false
	}
	clean := strings.TrimSuffix(parsed.Path, "/")
	if path.Clean(clean) != clean {
		return "", false
	}
	parsed.Host, parsed.Path = strings.ToLower(parsed.Host), strings.TrimSuffix(clean, ".git")
	return parsed.String(), true
}

func (s *Store) RegisterEstateRepository(ctx context.Context, i Identity, key string, in EstateRegistration) (EstateRepository, error) {
	if in.Owner == "" {
		in.Owner = i.Actor
	}
	if i.Actor != in.Owner && i.Role != "admin" {
		return EstateRepository{}, ErrDenied
	}
	origin, valid := canonicalEstateOrigin(in.Origin)
	if !valid || !estatePackage.MatchString(in.Package) || !estateVersion.MatchString(in.Version) || !identifier.MatchString(in.Owner) {
		return EstateRepository{}, fmt.Errorf("%w: package, HTTPS repository origin, version or owner", ErrInvalid)
	}
	in.Origin = origin
	id := snapshot.Digest([]byte(in.Package + "\x00" + in.Origin))
	return mutate(ctx, s, i, key, "register-estate-repository", in, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (EstateRepository, error) {
		prior, err := estateRepository(ctx, tx, i, id)
		if err == nil {
			if prior.EstateRegistration == in {
				return prior, nil
			}
			return EstateRepository{}, ErrConflict
		}
		if !errors.Is(err, ErrMissing) {
			return EstateRepository{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO estate_repositories(tenant,workspace,id,package_name,origin,owner,version_name,policy)
			VALUES(?,?,?,?,?,?,?,JSON_OBJECT())`, i.Tenant, i.Workspace, id, in.Package, in.Origin, in.Owner, in.Version)
		if err != nil {
			return EstateRepository{}, err
		}
		value := EstateRepository{ID: id, EstateRegistration: in, Revision: 1, State: "pending"}
		return value, audit(ctx, tx, i, "register-estate-repository", id, value)
	})
}

func cleanPolicy(in EstatePolicy) (EstatePolicy, error) {
	sets := []*[]string{&in.Index, &in.Model, &in.Export, &in.Runners}
	for index, set := range sets {
		if len(*set) > 32 {
			return EstatePolicy{}, fmt.Errorf("%w: repository policy exceeds 32 entries per scope", ErrInvalid)
		}
		*set = slices.Clone(*set)
		slices.Sort(*set)
		*set = slices.Compact(*set)
		for _, value := range *set {
			if index == 3 && !identifier.MatchString(value) || index != 3 && !snapshot.SafePath(value) ||
				(index == 1 || index == 2) && !snapshot.Within(value, in.Index) {
				return EstatePolicy{}, fmt.Errorf("%w: repository scope %d contains an invalid path or profile", ErrInvalid, index+1)
			}
		}
	}
	return in, nil
}

func (s *Store) SetEstatePolicy(ctx context.Context, i Identity, key, id string, in EstatePolicyInput) (EstateRepository, error) {
	policy, err := cleanPolicy(in.EstatePolicy)
	if err != nil {
		return EstateRepository{}, err
	}
	in.EstatePolicy = policy
	signature := struct {
		ID string
		EstatePolicyInput
	}{id, in}
	return mutate(ctx, s, i, key, "authorize-estate-repository", signature, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (EstateRepository, error) {
		repo, err := estateRepository(ctx, tx, i, id)
		if err != nil {
			return repo, err
		}
		if i.Actor != repo.Owner {
			return repo, ErrDenied
		}
		if repo.Revision != in.Expected {
			return repo, ErrConflict
		}
		old, _ := json.Marshal(repo.Policy)
		data, err := json.Marshal(policy)
		if err != nil || string(old) == string(data) {
			return repo, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE estate_repositories SET policy=?, policy_revision=policy_revision+1,
			revision=revision+1, snapshot=NULL WHERE tenant=? AND workspace=? AND id=?`, data, i.Tenant, i.Workspace, id)
		if err != nil {
			return repo, err
		}
		repo.Policy, repo.PolicyRevision, repo.Revision, repo.Snapshot, repo.State = policy, repo.PolicyRevision+1, repo.Revision+1, "", "pending"
		return repo, audit(ctx, tx, i, "authorize-estate-repository", id, repo)
	})
}

func canPinEstate(i Identity, repo EstateRepository) error {
	if i.Actor != repo.Owner || !i.Can("maintainer") || len(repo.Policy.Index) == 0 {
		return ErrDenied
	}
	return nil
}

func (s *Store) CheckEstateSnapshotUpload(ctx context.Context, i Identity, id string) error {
	repo, err := estateRepository(ctx, s.db, i, id)
	if err != nil {
		return err
	}
	return canPinEstate(i, repo)
}

func (s *Store) PinEstateSnapshot(ctx context.Context, i Identity, key, id string, in EstateSnapshotInput) (EstateRepository, error) {
	signature := struct {
		ID, Version, Snapshot string
		Expected              uint64
		Stage                 bool
	}{id, in.Version, in.Snapshot.Digest, in.Expected, in.Stage}
	return mutate(ctx, s, i, key, "pin-estate-snapshot", signature, []string{"maintainer"}, func(tx *sql.Tx, _ Workspace) (EstateRepository, error) {
		repo, err := estateRepository(ctx, tx, i, id)
		if err != nil {
			return repo, err
		}
		if err := canPinEstate(i, repo); err != nil {
			return repo, err
		}
		if repo.Revision != in.Expected {
			return repo, ErrConflict
		}
		if !estateVersion.MatchString(in.Version) || slices.ContainsFunc(in.Snapshot.Roots, func(root string) bool {
			return !snapshot.Within(root, repo.Policy.Index)
		}) {
			return repo, fmt.Errorf("%w: version or unauthorized snapshot roots", ErrInvalid)
		}
		catalog, err := impact.IndexContracts(in.Snapshot)
		if err != nil {
			return repo, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		return saveEstateSnapshot(ctx, tx, i, repo, in.Snapshot, catalog, in.Version, in.Stage)
	})
}

func saveEstateSnapshot(ctx context.Context, tx *sql.Tx, i Identity, repo EstateRepository, source snapshot.Snapshot,
	catalog impact.ContractCatalog, version string, stage bool) (EstateRepository, error) {
	body, err := json.Marshal(source)
	if err != nil {
		return repo, err
	}
	index, err := json.Marshal(catalog)
	if err != nil {
		return repo, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO estate_snapshots(tenant,workspace,repository,digest,policy_revision,version_name,payload,contract_catalog)
		VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE version_name=version_name`, i.Tenant, i.Workspace, repo.ID,
		source.Digest, repo.PolicyRevision, version, body, index)
	if err != nil {
		return repo, err
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT version_name FROM estate_snapshots WHERE tenant=? AND workspace=? AND repository=?
		AND digest=? AND policy_revision=?`, i.Tenant, i.Workspace, repo.ID, source.Digest, repo.PolicyRevision).Scan(&stored)
	if err != nil {
		return repo, err
	}
	if stored != version {
		return repo, fmt.Errorf("%w: snapshot is pinned to version %s", ErrConflict, stored)
	}
	if stage {
		event := struct {
			Snapshot, Version string
		}{source.Digest, version}
		return repo, audit(ctx, tx, i, "stage-estate-snapshot", repo.ID, event)
	}
	if repo.Snapshot == source.Digest {
		return repo, nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE estate_repositories SET snapshot=?,version_name=?,revision=revision+1 WHERE tenant=? AND workspace=? AND id=?`,
		source.Digest, version, i.Tenant, i.Workspace, repo.ID)
	if err != nil {
		return repo, err
	}
	repo.Snapshot, repo.Version, repo.Revision, repo.State = source.Digest, version, repo.Revision+1, "indexed"
	return repo, audit(ctx, tx, i, "pin-estate-snapshot", repo.ID, repo)
}

func (s *Store) EstateCatalog(ctx context.Context, i Identity, id, revision string) (impact.ContractCatalog, error) {
	repo, err := estateRepository(ctx, s.db, i, id)
	if err != nil {
		return impact.ContractCatalog{}, err
	}
	return readEstateCatalog(ctx, s.db, i, repo, revision)
}

func readEstateCatalog(ctx context.Context, db rowScanner, i Identity, repo EstateRepository, revision string) (impact.ContractCatalog, error) {
	var result impact.ContractCatalog
	if len(repo.Policy.Index) == 0 {
		return result, ErrDenied
	}
	if revision == "" {
		revision = repo.Snapshot
	}
	if !digest.MatchString(revision) {
		return result, ErrMissing
	}
	var body []byte
	err := db.QueryRowContext(ctx, `SELECT x.contract_catalog FROM estate_snapshots x JOIN estate_repositories r
		ON r.tenant=x.tenant AND r.workspace=x.workspace AND r.id=x.repository AND r.policy_revision=x.policy_revision
		WHERE x.tenant=? AND x.workspace=? AND x.repository=? AND x.digest=? AND x.policy_revision=?`,
		i.Tenant, i.Workspace, repo.ID, revision, repo.PolicyRevision).Scan(&body)
	if err != nil {
		return result, missing(err)
	}
	return result, json.Unmarshal(body, &result)
}

func readEstateSource(ctx context.Context, db rowScanner, i Identity, repo EstateRepository, revision string) (snapshot.Snapshot, error) {
	var result snapshot.Snapshot
	if len(repo.Policy.Index) == 0 {
		return result, ErrDenied
	}
	if !digest.MatchString(revision) {
		return result, ErrMissing
	}
	var body []byte
	err := db.QueryRowContext(ctx, `SELECT x.payload FROM estate_snapshots x JOIN estate_repositories r
		ON r.tenant=x.tenant AND r.workspace=x.workspace AND r.id=x.repository AND r.policy_revision=x.policy_revision
		WHERE x.tenant=? AND x.workspace=? AND x.repository=? AND x.digest=? AND x.policy_revision=?`,
		i.Tenant, i.Workspace, repo.ID, revision, repo.PolicyRevision).Scan(&body)
	if err != nil {
		return result, missing(err)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	return result, result.Verify()
}

func (s *Store) EstateImpact(ctx context.Context, i Identity, in EstateImpactInput) (impact.ContractImpact, error) {
	if !digest.MatchString(in.Producer) || !digest.MatchString(in.Previous) || !digest.MatchString(in.Current) ||
		len(in.Consumers) == 0 || len(in.Consumers) > 99 {
		return impact.ContractImpact{}, fmt.Errorf("%w: expected producer snapshots and 1–99 consumer snapshots", ErrInvalid)
	}
	seen := map[string]bool{in.Producer: true}
	for _, pin := range in.Consumers {
		if seen[pin.Repository] || !digest.MatchString(pin.Repository) || !digest.MatchString(pin.Snapshot) {
			return impact.ContractImpact{}, fmt.Errorf("%w: duplicate or invalid consumer snapshot", ErrInvalid)
		}
		seen[pin.Repository] = true
	}

	return transaction(ctx, s, func(tx *sql.Tx) (impact.ContractImpact, error) {
		producer, err := estateRepository(ctx, tx, i, in.Producer)
		if err != nil {
			return impact.ContractImpact{}, err
		}
		previous, err := readEstateCatalog(ctx, tx, i, producer, in.Previous)
		if err != nil {
			return impact.ContractImpact{}, err
		}
		latest, err := readEstateCatalog(ctx, tx, i, producer, in.Current)
		if err != nil {
			return impact.ContractImpact{}, err
		}
		consumers := make([]impact.ContractConsumer, 0, len(in.Consumers))
		for _, pin := range in.Consumers {
			repo, err := estateRepository(ctx, tx, i, pin.Repository)
			if err != nil {
				return impact.ContractImpact{}, err
			}
			catalog, err := readEstateCatalog(ctx, tx, i, repo, pin.Snapshot)
			if err != nil {
				return impact.ContractImpact{}, err
			}
			consumers = append(consumers, impact.ContractConsumer{Repository: repo.ID, Catalog: catalog})
		}
		result, err := impact.CompareContracts(previous, latest, consumers)
		if err != nil {
			return result, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		return result, nil
	})
}
