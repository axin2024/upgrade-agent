package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

type BaselineInput struct {
	Expected string            `json:"expected_baseline"`
	Snapshot snapshot.Snapshot `json:"snapshot"`
}

type PathDelta struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

type BaselineResult struct {
	Workspace
	Previous string      `json:"previous,omitempty"`
	Delta    []PathDelta `json:"delta"`
}

const selectSnapshot = "SELECT payload FROM snapshots WHERE tenant=? AND workspace=? AND digest=?"
const selectPlan = "SELECT payload FROM plans WHERE tenant=? AND workspace=? AND digest=?"
const selectCandidate = "SELECT payload FROM candidates WHERE tenant=? AND workspace=? AND digest=?"
const selectReport = "SELECT payload FROM reports WHERE tenant=? AND workspace=? AND digest=?"

func (s *Store) Pin(ctx context.Context, i Identity, key string, in BaselineInput) (BaselineResult, error) {
	if err := in.Snapshot.Verify(); err != nil {
		return BaselineResult{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	signature := struct{ Expected, Proposed string }{in.Expected, in.Snapshot.Digest}
	return mutate(ctx, s, i, key, "pin-snapshot", signature, []string{"maintainer"}, func(tx *sql.Tx, ws Workspace) (BaselineResult, error) {
		out := BaselineResult{Workspace: ws, Previous: ws.Baseline, Delta: make([]PathDelta, 0)}
		if ws.Baseline == in.Snapshot.Digest {
			return out, nil
		}
		if ws.Baseline != in.Expected {
			return out, fmt.Errorf("%w: current baseline %.64s", ErrConflict, ws.Baseline)
		}
		var previous snapshot.Snapshot
		if ws.Baseline != "" {
			var err error
			previous, err = readJSON[snapshot.Snapshot](ctx, tx, i, ws.Baseline, selectSnapshot)
			if err != nil {
				return out, err
			}
		}
		out.Delta = baselineDelta(previous, in.Snapshot)
		body, err := json.Marshal(in.Snapshot)
		if err != nil {
			return out, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO snapshots(tenant,workspace,digest,payload) VALUES(?,?,?,?)
			ON DUPLICATE KEY UPDATE digest=digest`, i.Tenant, i.Workspace, in.Snapshot.Digest, body)
		if err != nil {
			return out, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE workspaces SET snapshot=?,revision=revision+1 WHERE tenant=? AND name=?`,
			in.Snapshot.Digest, i.Tenant, i.Workspace)
		if err != nil {
			return out, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM selections WHERE tenant=? AND workspace=?", i.Tenant, i.Workspace); err != nil {
			return out, err
		}
		out.Baseline, out.Revision = in.Snapshot.Digest, ws.Revision+1
		return out, audit(ctx, tx, i, "pin-snapshot", out.Baseline, out)
	})
}

func baselineDelta(old, new snapshot.Snapshot) []PathDelta {
	oldByPath := make(map[string]string, len(old.Files))
	delta := make([]PathDelta, 0)
	for _, file := range old.Files {
		oldByPath[file.Path] = file.SHA256
	}
	for _, file := range new.Files {
		switch before := oldByPath[file.Path]; before {
		case "":
			delta = append(delta, PathDelta{file.Path, "added"})
		case file.SHA256:
		default:
			delta = append(delta, PathDelta{file.Path, "modified"})
		}
		delete(oldByPath, file.Path)
	}
	for _, name := range slices.Sorted(maps.Keys(oldByPath)) {
		delta = append(delta, PathDelta{name, "removed"})
	}
	return delta
}

func (s *Store) Snapshot(ctx context.Context, i Identity, id string) (snapshot.Snapshot, error) {
	return readJSON[snapshot.Snapshot](ctx, s.db, i, id, selectSnapshot)
}

type PlanInput struct {
	Baseline string         `json:"baseline"`
	Options  planning.Input `json:"options"`
}

type Ref struct {
	Digest string `json:"sha256"`
}

func (s *Store) CreatePlan(ctx context.Context, i Identity, key string, in PlanInput) (Ref, error) {
	if !i.Can("maintainer") {
		return Ref{}, ErrDenied
	}
	repo, err := s.Snapshot(ctx, i, in.Baseline)
	if err != nil {
		return Ref{}, err
	}
	plan, err := planning.Build(repo, in.Options)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	return mutate(ctx, s, i, key, "create-plan", in, []string{"maintainer"}, func(tx *sql.Tx, ws Workspace) (Ref, error) {
		if ws.Baseline != plan.Baseline {
			return Ref{}, ErrConflict
		}
		payload, err := json.Marshal(plan)
		if err != nil {
			return Ref{}, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO plans(tenant,workspace,digest,baseline,payload,actor)
			VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE digest=digest`,
			i.Tenant, i.Workspace, plan.Digest, plan.Baseline, payload, i.Actor)
		if err != nil {
			return Ref{}, err
		}
		if rows, _ := result.RowsAffected(); rows > 0 {
			err = audit(ctx, tx, i, "create-plan", plan.Digest, map[string]string{"baseline": plan.Baseline})
		}
		return Ref{plan.Digest}, err
	})
}

func (s *Store) Plan(ctx context.Context, i Identity, id string) (planning.Plan, error) {
	return readJSON[planning.Plan](ctx, s.db, i, id, selectPlan)
}

func (s *Store) Candidate(ctx context.Context, i Identity, id string) (planning.Candidate, error) {
	return readJSON[planning.Candidate](ctx, s.db, i, id, selectCandidate)
}

func (s *Store) Report(ctx context.Context, i Identity, id string) (validation.Report, error) {
	return readJSON[validation.Report](ctx, s.db, i, id, selectReport)
}

type HistoryEntry struct {
	ID     uint64          `json:"id"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Object string          `json:"object"`
	Detail json.RawMessage `json:"detail"`
	When   time.Time       `json:"happened_at"`
}

func (s *Store) History(ctx context.Context, i Identity, after uint64) ([]HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,actor,action,object,detail,happened_at FROM audit_events
		WHERE tenant=? AND workspace=? AND id>? ORDER BY id LIMIT 100`, i.Tenant, i.Workspace, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]HistoryEntry, 0)
	for rows.Next() {
		var event HistoryEntry
		if err := rows.Scan(&event.ID, &event.Actor, &event.Action, &event.Object, &event.Detail, &event.When); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func saveCandidate(ctx context.Context, tx *sql.Tx, i Identity, value planning.Candidate) (Ref, error) {
	return saveArtifact(ctx, tx, i, value.Digest, value, "register-candidate",
		map[string]string{"plan": value.Plan, "batch": value.Batch, "source": value.Source},
		`INSERT INTO candidates(tenant,workspace,digest,payload,actor,plan,batch)
		 VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE digest=digest`, value.Plan, value.Batch)
}

func saveReport(ctx context.Context, tx *sql.Tx, i Identity, value validation.Report, revision uint64) (Ref, error) {
	return saveArtifact(ctx, tx, i, value.Digest, value, "record-validation",
		map[string]string{"candidate": value.Candidate, "state": value.State},
		`INSERT INTO reports(tenant,workspace,digest,payload,actor,candidate,verdict,baseline_revision)
		 VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE digest=digest`, value.Candidate, value.State, revision)
}

func saveArtifact(ctx context.Context, tx *sql.Tx, i Identity, id string, value any, event string, detail any,
	statement string, tail ...any) (Ref, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return Ref{}, err
	}
	result, err := tx.ExecContext(ctx, statement, append([]any{i.Tenant, i.Workspace, id, payload, i.Actor}, tail...)...)
	if err != nil {
		return Ref{}, err
	}
	if count, _ := result.RowsAffected(); count > 0 {
		err = audit(ctx, tx, i, event, id, detail)
	}
	return Ref{id}, err
}

func (s *Store) Patch(ctx context.Context, i Identity, candidate string) (string, error) {
	value, err := s.Candidate(ctx, i, candidate)
	if err != nil {
		return "", err
	}
	repo, err := s.Snapshot(ctx, i, value.Baseline)
	if err != nil {
		return "", err
	}
	return planning.Diff(repo, value)
}
