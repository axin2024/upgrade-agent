package records

import (
	"context"
	"database/sql"
	"time"
)

type PlanMatch struct {
	Digest      string    `json:"sha256"`
	Baseline    string    `json:"baseline"`
	Repository  string    `json:"repository_ref"`
	RuleVersion string    `json:"rule_version"`
	Created     time.Time `json:"created_at"`
}

type PlanPage struct {
	Items []PlanMatch `json:"items"`
	Next  string      `json:"next,omitempty"`
}

func (s *Store) FindPlans(ctx context.Context, i Identity, name, version, after string) (PlanPage, error) {
	if len(name) == 0 || len(name) > 160 || len(version) == 0 || len(version) > 512 ||
		(after != "" && !digest.MatchString(after)) {
		return PlanPage{}, ErrInvalid
	}
	const pageSize = 25
	rows, err := s.db.QueryContext(ctx, `SELECT p.digest,p.baseline,
		COALESCE(JSON_UNQUOTE(w.payload->'$.commit'),''),JSON_UNQUOTE(p.payload->'$.rule_version'),p.created_at
		FROM plan_targets AS t
		JOIN plans AS p ON (p.tenant,p.workspace,p.digest)=(t.tenant,t.workspace,t.digest)
		JOIN snapshots AS w ON (w.tenant,w.workspace,w.digest)=(p.tenant,p.workspace,p.baseline)
		WHERE t.tenant=? AND t.workspace=? AND t.package_name=? AND t.version_digest=UNHEX(SHA2(?,256))
			AND t.digest>?
		ORDER BY t.digest LIMIT ?`, i.Tenant, i.Workspace, name, version, after, pageSize+1)
	if err != nil {
		return PlanPage{}, err
	}
	defer rows.Close()
	out := PlanPage{Items: make([]PlanMatch, 0, pageSize)}
	for rows.Next() {
		if len(out.Items) == pageSize {
			out.Next = out.Items[pageSize-1].Digest
			break
		}
		var match PlanMatch
		if err := rows.Scan(&match.Digest, &match.Baseline, &match.Repository, &match.RuleVersion, &match.Created); err != nil {
			return PlanPage{}, err
		}
		out.Items = append(out.Items, match)
	}
	return out, rows.Err()
}

type RetentionResult struct {
	Before    time.Time `json:"before"`
	Applied   bool      `json:"applied"`
	Snapshots []string  `json:"snapshots"`
	AuditIDs  []uint64  `json:"audit_ids"`
}

// Prune removes expired workspace audit events and unreferenced snapshots.
// The workspace lock keeps collection and pinning serial.
func (s *Store) Prune(ctx context.Context, tenant, workspace string, before time.Time, apply bool) (RetentionResult, error) {
	if !identifier.MatchString(tenant) || !identifier.MatchString(workspace) ||
		before.IsZero() || !before.Before(time.Now()) {
		return RetentionResult{}, ErrInvalid
	}
	i := Identity{Tenant: tenant, Workspace: workspace, Actor: "maintenance"}
	return transaction(ctx, s, func(tx *sql.Tx) (RetentionResult, error) {
		ws, err := current(ctx, tx, i, true)
		if err != nil {
			return RetentionResult{}, err
		}
		out := RetentionResult{Before: before.UTC(), Applied: apply, Snapshots: []string{}, AuditIDs: []uint64{}}
		out.Snapshots, err = retentionIDs[string](ctx, tx, `SELECT s.digest FROM snapshots AS s
			WHERE s.tenant=? AND s.workspace=? AND s.created_at<? AND s.digest<>?
			AND NOT EXISTS (SELECT 1 FROM plans AS p WHERE p.tenant=s.tenant AND p.workspace=s.workspace AND p.baseline=s.digest)
			ORDER BY s.created_at,s.digest LIMIT 250`, tenant, workspace, out.Before, ws.Baseline)
		if err != nil {
			return RetentionResult{}, err
		}
		out.AuditIDs, err = retentionIDs[uint64](ctx, tx, `SELECT id FROM audit_events
			WHERE tenant=? AND workspace=? AND happened_at<?
			ORDER BY happened_at,id LIMIT 250`, tenant, workspace, out.Before)
		if err != nil || !apply || len(out.Snapshots)+len(out.AuditIDs) == 0 {
			return out, err
		}
		for _, digest := range out.Snapshots {
			if _, err := tx.ExecContext(ctx, "DELETE FROM snapshots WHERE tenant=? AND workspace=? AND digest=?", tenant, workspace, digest); err != nil {
				return RetentionResult{}, err
			}
		}
		for _, id := range out.AuditIDs {
			if _, err := tx.ExecContext(ctx, "DELETE FROM audit_events WHERE tenant=? AND workspace=? AND id=?", tenant, workspace, id); err != nil {
				return RetentionResult{}, err
			}
		}
		return out, audit(ctx, tx, i, "retain-workspace", ws.Baseline, map[string]int{
			"snapshots": len(out.Snapshots), "audit_events": len(out.AuditIDs),
		})
	})
}

func retentionIDs[T string | uint64](ctx context.Context, tx *sql.Tx, statement string, args ...any) ([]T, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		var id T
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
