package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type ReviewBatchItemInput struct {
	Candidate string `json:"candidate"`
	DecisionInput
}

type ReviewBatchInput struct {
	Revision uint64                 `json:"revision"`
	Items    []ReviewBatchItemInput `json:"items"`
}

type ReviewBatchItem struct {
	ReviewBatchItemInput
	State   string    `json:"state"`
	Receipt *Decision `json:"receipt,omitempty"`
	Error   string    `json:"error,omitempty"`
}

type ReviewBatch struct {
	RequestID string            `json:"request_id"`
	Baseline  string            `json:"baseline"`
	Revision  uint64            `json:"revision"`
	Completed int               `json:"completed"`
	Items     []ReviewBatchItem `json:"items"`
}

func (s *Store) PrepareReviewBatch(ctx context.Context, i Identity, key string, in ReviewBatchInput) (ReviewBatch, error) {
	if in.Revision == 0 || len(in.Items) == 0 || len(in.Items) > 20 {
		return ReviewBatch{}, ErrInvalid
	}
	baseline := in.Items[0].ExpectedBaseline
	seen := make(map[string]bool, len(in.Items))
	items := make([]ReviewBatchItem, 0, len(in.Items))
	in.Items = append([]ReviewBatchItemInput(nil), in.Items...)
	for index, item := range in.Items {
		if !digest.MatchString(item.Candidate) || seen[item.Candidate] || item.ExpectedBaseline != baseline {
			return ReviewBatch{}, ErrInvalid
		}
		decision, err := normalizeDecision(item.DecisionInput)
		if err != nil {
			return ReviewBatch{}, err
		}
		item.DecisionInput = decision
		in.Items[index] = item
		items = append(items, ReviewBatchItem{ReviewBatchItemInput: item, State: "pending"})
		seen[item.Candidate] = true
	}
	_, err := mutate(ctx, s, i, key, "prepare-review-batch", in, []string{"reviewer", "maintainer"},
		func(tx *sql.Tx, ws Workspace) (string, error) {
			if ws.Baseline != baseline || ws.Revision != in.Revision {
				return "", ErrConflict
			}
			batch := ReviewBatch{RequestID: key, Baseline: baseline, Revision: in.Revision, Items: items}
			payload, err := json.Marshal(batch)
			if err != nil {
				return "", err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO review_batches(tenant,workspace,actor,request_id,payload)
				VALUES(?,?,?,?,?)`, i.Tenant, i.Workspace, i.Actor, key, payload)
			return key, err
		})
	if err != nil {
		return ReviewBatch{}, err
	}
	return s.ReviewBatch(ctx, i, key)
}

func readReviewBatch(ctx context.Context, db rowScanner, i Identity, key string, lock bool) (ReviewBatch, error) {
	query := `SELECT payload FROM review_batches WHERE tenant=? AND workspace=? AND actor=? AND request_id=?`
	if lock {
		query += " FOR UPDATE"
	}
	var payload []byte
	err := db.QueryRowContext(ctx, query, i.Tenant, i.Workspace, i.Actor, key).Scan(&payload)
	if err != nil {
		return ReviewBatch{}, missing(err)
	}
	var batch ReviewBatch
	err = json.Unmarshal(payload, &batch)
	return batch, err
}

func (s *Store) ReviewBatch(ctx context.Context, i Identity, key string) (ReviewBatch, error) {
	if !i.Can("reviewer", "maintainer") {
		return ReviewBatch{}, ErrDenied
	}
	if !requestID.MatchString(key) {
		return ReviewBatch{}, ErrInvalid
	}
	return readReviewBatch(ctx, s.db, i, key, false)
}

func (s *Store) ReviewBatchIDs(ctx context.Context, i Identity) ([]string, error) {
	if !i.Can("reviewer", "maintainer") {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id FROM review_batches
		WHERE tenant=? AND workspace=? AND actor=? ORDER BY created_at DESC,request_id DESC LIMIT 25`,
		i.Tenant, i.Workspace, i.Actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) RunReviewBatch(ctx context.Context, i Identity, key string) (ReviewBatch, error) {
	if !i.Can("reviewer", "maintainer") {
		return ReviewBatch{}, ErrDenied
	}
	if !requestID.MatchString(key) {
		return ReviewBatch{}, ErrInvalid
	}
	for {
		batch, err := s.reviewNext(ctx, i, key)
		if err != nil || batch.Completed == len(batch.Items) {
			return batch, err
		}
	}
}

// Commit the decision and its cursor together, with the same workspace lock as a single decision.
func (s *Store) reviewNext(ctx context.Context, i Identity, key string) (ReviewBatch, error) {
	return transaction(ctx, s, func(tx *sql.Tx) (ReviewBatch, error) {
		ws, err := current(ctx, tx, i, true)
		if err != nil {
			return ReviewBatch{}, err
		}
		if err := checkGrant(ctx, tx, i); err != nil {
			return ReviewBatch{}, err
		}
		batch, err := readReviewBatch(ctx, tx, i, key, true)
		if err != nil || batch.Completed == len(batch.Items) {
			return batch, err
		}
		item := &batch.Items[batch.Completed]
		var decision Decision
		if ws.Revision != batch.Revision || ws.Baseline != batch.Baseline {
			err = ErrConflict
		} else {
			decision, err = decide(ctx, tx, i, ws, item.Candidate, item.DecisionInput, key)
		}
		switch {
		case err == nil:
			item.State, item.Receipt = "applied", &decision
		case errors.Is(err, ErrConflict), errors.Is(err, ErrMissing):
			item.State, item.Error = "rejected", err.Error()
		default:
			return ReviewBatch{}, err
		}
		batch.Completed++
		payload, err := json.Marshal(batch)
		if err != nil {
			return ReviewBatch{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE review_batches SET payload=?
			WHERE tenant=? AND workspace=? AND actor=? AND request_id=?`, payload, i.Tenant, i.Workspace, i.Actor, key)
		return batch, err
	})
}
