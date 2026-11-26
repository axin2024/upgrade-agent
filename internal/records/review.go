package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

type DecisionInput struct {
	Action           string   `json:"action"`
	ExpectedBaseline string   `json:"baseline"`
	Evidence         string   `json:"validation,omitempty"`
	Reason           string   `json:"reason"`
	ConfirmAffected  []string `json:"confirm_affected,omitempty"`
}

type Decision struct {
	Action    string   `json:"action"`
	Plan      string   `json:"plan"`
	Candidate string   `json:"candidate"`
	Batch     string   `json:"batch"`
	Baseline  string   `json:"baseline"`
	Revision  uint64   `json:"revision"`
	Evidence  string   `json:"validation,omitempty"`
	Affected  []string `json:"affected_batches,omitempty"`
	Reason    string   `json:"reason"`
	RequestID string   `json:"review_batch,omitempty"`
}

type Selection struct {
	Batch     string `json:"batch"`
	Candidate string `json:"candidate"`
	Evidence  string `json:"validation"`
}

type rowLister interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func selections(ctx context.Context, q rowLister, i Identity, plan string) ([]Selection, error) {
	rows, err := q.QueryContext(ctx, `SELECT batch,candidate,evidence FROM selections
		WHERE tenant=? AND workspace=? AND plan=? ORDER BY batch`, i.Tenant, i.Workspace, plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Selection, 0)
	for rows.Next() {
		var selection Selection
		if err := rows.Scan(&selection.Batch, &selection.Candidate, &selection.Evidence); err != nil {
			return nil, err
		}
		out = append(out, selection)
	}
	return out, rows.Err()
}

func latestReport(ctx context.Context, db rowScanner, i Identity, candidate string, revision uint64) (validation.Report, error) {
	var report validation.Report
	var payload []byte
	err := db.QueryRowContext(ctx, `SELECT payload FROM reports WHERE tenant=? AND workspace=? AND candidate=?
		AND baseline_revision=? ORDER BY created_at DESC,digest DESC LIMIT 1`,
		i.Tenant, i.Workspace, candidate, revision).Scan(&payload)
	if err != nil {
		return report, missing(err)
	}
	err = json.Unmarshal(payload, &report)
	return report, err
}

func normalizeDecision(in DecisionInput) (DecisionInput, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) < 3 || len(in.Reason) > 500 || !slices.Contains([]string{"adopt", "reject", "withdraw"}, in.Action) ||
		!digest.MatchString(in.ExpectedBaseline) || len(in.ConfirmAffected) > 100 {
		return DecisionInput{}, ErrInvalid
	}
	return in, nil
}

func (s *Store) Decide(ctx context.Context, i Identity, key, candidate string, in DecisionInput) (Decision, error) {
	in, err := normalizeDecision(in)
	if err != nil {
		return Decision{}, err
	}
	signature := struct {
		Candidate string
		Input     DecisionInput
	}{candidate, in}
	return mutate(ctx, s, i, key, "decide", signature, []string{"reviewer", "maintainer"}, func(tx *sql.Tx, ws Workspace) (Decision, error) {
		return decide(ctx, tx, i, ws, candidate, in, "")
	})
}

func decide(ctx context.Context, tx *sql.Tx, i Identity, ws Workspace, candidate string, in DecisionInput, batchKey string) (Decision, error) {
	c, err := readJSON[planning.Candidate](ctx, tx, i, candidate, selectCandidate)
	if err != nil {
		return Decision{}, err
	}
	if ws.Baseline != in.ExpectedBaseline || c.Baseline != ws.Baseline {
		return Decision{}, ErrConflict
	}
	p, err := readJSON[planning.Plan](ctx, tx, i, c.Plan, selectPlan)
	if err != nil {
		return Decision{}, err
	}
	available, err := selections(ctx, tx, i, p.Digest)
	if err != nil {
		return Decision{}, err
	}
	selected := make(map[string]Selection, len(available))
	for _, item := range available {
		selected[item.Batch] = item
	}
	out := Decision{Action: in.Action, Plan: p.Digest, Candidate: c.Digest, Batch: c.Batch, Baseline: ws.Baseline,
		Revision: ws.Revision, Reason: in.Reason, RequestID: batchKey}
	switch in.Action {
	case "adopt":
		if selected[c.Batch].Candidate != "" && selected[c.Batch].Candidate != c.Digest {
			return Decision{}, fmt.Errorf("%w: withdraw the existing selection first", ErrConflict)
		}
		if err := readyToAdopt(ctx, tx, i, ws, p, c, in.Evidence, selected); err != nil {
			return Decision{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO selections(tenant,workspace,plan,batch,candidate,evidence)
			VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE evidence=?`, i.Tenant, i.Workspace, p.Digest,
			c.Batch, c.Digest, in.Evidence, in.Evidence)
		out.Evidence = in.Evidence
	case "reject":
		if selected[c.Batch].Candidate == c.Digest {
			return Decision{}, fmt.Errorf("%w: withdrawing a selected candidate requires its own decision", ErrConflict)
		}
	case "withdraw":
		if selected[c.Batch].Candidate != c.Digest {
			return Decision{}, fmt.Errorf("%w: candidate is not selected", ErrConflict)
		}
		out.Affected = affectedBatches(p, c.Batch, selected)
		confirmed := slices.Clone(in.ConfirmAffected)
		slices.Sort(confirmed)
		if !slices.Equal(out.Affected, confirmed) {
			return Decision{}, fmt.Errorf("%w: confirm affected batches %s", ErrConflict, strings.Join(out.Affected, ","))
		}
		for _, batch := range append(slices.Clone(out.Affected), c.Batch) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM selections WHERE tenant=? AND workspace=? AND plan=? AND batch=?`,
				i.Tenant, i.Workspace, p.Digest, batch); err != nil {
				return Decision{}, err
			}
		}
	}
	if err != nil {
		return Decision{}, err
	}
	return out, audit(ctx, tx, i, in.Action, c.Digest, out)
}

func readyToAdopt(ctx context.Context, tx *sql.Tx, i Identity, ws Workspace, p planning.Plan,
	c planning.Candidate, evidence string, selected map[string]Selection) error {
	report, err := latestReport(ctx, tx, i, c.Digest, ws.Revision)
	if errors.Is(err, ErrMissing) || err == nil && (report.Digest != evidence || report.State != "passed") {
		return fmt.Errorf("%w: latest validation for this baseline revision must pass", ErrConflict)
	}
	if err != nil {
		return err
	}
	batch, _ := p.Batch(c.Batch)
	if len(report.Parents) != len(batch.Prerequisites) {
		return fmt.Errorf("%w: prerequisite evidence is incomplete", ErrConflict)
	}
	for n, parent := range batch.Prerequisites {
		parentSelection := selected[parent]
		if parentSelection.Candidate != report.Parents[n] {
			return fmt.Errorf("%w: prerequisite %s must be adopted", ErrConflict, parent)
		}
		checked, err := latestReport(ctx, tx, i, parentSelection.Candidate, ws.Revision)
		if errors.Is(err, ErrMissing) || err == nil && (checked.State != "passed" || checked.Digest != parentSelection.Evidence) {
			return fmt.Errorf("%w: prerequisite %s requires current evidence", ErrConflict, parent)
		}
		if err != nil {
			return err
		}
	}
	snap, err := readJSON[snapshot.Snapshot](ctx, tx, i, ws.Baseline, selectSnapshot)
	if err != nil {
		return err
	}
	combine := []planning.Candidate{c}
	for _, item := range selected {
		other, err := readJSON[planning.Candidate](ctx, tx, i, item.Candidate, selectCandidate)
		if err != nil {
			return err
		}
		combine = append(combine, other)
	}
	if _, err := planning.Overlay(snap, combine...); err != nil {
		return fmt.Errorf("%w: %s", ErrConflict, err)
	}
	return nil
}

func affectedBatches(plan planning.Plan, batch string, selected map[string]Selection) []string {
	affected := map[string]bool{batch: true}
	for _, candidate := range plan.Batches {
		if slices.ContainsFunc(candidate.Prerequisites, func(id string) bool { return affected[id] }) {
			affected[candidate.ID] = true
		}
	}
	result := make([]string, 0)
	for id := range selected {
		if id != batch && affected[id] {
			result = append(result, id)
		}
	}
	slices.Sort(result)
	return result
}

type Bundle struct {
	Ref
	Plan          string              `json:"plan"`
	Baseline      string              `json:"baseline"`
	Revision      uint64              `json:"revision"`
	RepositoryRef string              `json:"repository_ref,omitempty"`
	Selections    []Selection         `json:"selections"`
	Reports       []validation.Report `json:"reports"`
	Patch         string              `json:"patch"`
	PatchSHA256   string              `json:"patch_sha256"`
}

func (s *Store) Export(ctx context.Context, i Identity, key, id, baseline string) (Bundle, error) {
	signature := struct{ Plan, Baseline string }{id, baseline}
	return mutate(ctx, s, i, key, "export-bundle", signature, []string{"maintainer", "reviewer"}, func(tx *sql.Tx, ws Workspace) (Bundle, error) {
		plan, err := readJSON[planning.Plan](ctx, tx, i, id, selectPlan)
		if err != nil {
			return Bundle{}, err
		}
		if plan.Baseline != baseline || ws.Baseline != baseline {
			return Bundle{}, ErrConflict
		}
		picked, err := selections(ctx, tx, i, id)
		if err != nil {
			return Bundle{}, err
		}
		if len(picked) == 0 {
			return Bundle{}, fmt.Errorf("%w: no selected batches", ErrConflict)
		}
		snap, err := readJSON[snapshot.Snapshot](ctx, tx, i, baseline, selectSnapshot)
		if err != nil {
			return Bundle{}, err
		}
		bundle := Bundle{Plan: id, Baseline: baseline, Revision: ws.Revision, RepositoryRef: snap.Commit, Selections: picked}
		byBatch := make(map[string]Selection, len(picked))
		for _, selection := range picked {
			byBatch[selection.Batch] = selection
		}
		var candidates []planning.Candidate
		for _, batch := range plan.Batches {
			selection, ok := byBatch[batch.ID]
			if !ok {
				continue
			}
			report, err := latestReport(ctx, tx, i, selection.Candidate, ws.Revision)
			if errors.Is(err, ErrMissing) || err == nil && (report.State != "passed" || report.Digest != selection.Evidence) {
				return Bundle{}, fmt.Errorf("%w: evidence changed for %s", ErrConflict, batch.ID)
			}
			if err != nil {
				return Bundle{}, err
			}
			c, err := readJSON[planning.Candidate](ctx, tx, i, selection.Candidate, selectCandidate)
			if err != nil {
				return Bundle{}, err
			}
			candidates = append(candidates, c)
			bundle.Reports = append(bundle.Reports, report)
		}
		bundle.Patch, err = planning.Diff(snap, candidates...)
		if err != nil {
			return Bundle{}, fmt.Errorf("%w: %s", ErrConflict, err)
		}
		bundle.PatchSHA256 = snapshot.Digest([]byte(bundle.Patch))
		encoded, err := json.Marshal(bundle)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Digest = snapshot.Digest(encoded)
		return bundle, audit(ctx, tx, i, "export-bundle", bundle.Digest, map[string]string{"plan": id, "patch_sha256": bundle.PatchSHA256})
	})
}
