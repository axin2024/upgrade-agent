package records

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

const jobDuration = 3 * time.Minute

type Job struct {
	RequestID string       `json:"request_id"`
	State     string       `json:"state"`
	Kind      string       `json:"kind"`
	Plan      string       `json:"plan"`
	Revision  uint64       `json:"revision"`
	Deadline  time.Time    `json:"deadline"`
	Artifact  string       `json:"artifact,omitempty"`
	Failure   string       `json:"failure,omitempty"`
	Usage     *model.Usage `json:"usage,omitempty"`
	owner     string
}

func (s *Store) Job(ctx context.Context, i Identity, key string) (Job, error) {
	if !requestID.MatchString(key) {
		return Job{}, ErrMissing
	}
	job, _, err := getJob(ctx, s.db, i, key)
	if err == nil && job.State == "running" && time.Now().UTC().After(job.Deadline) {
		job.State = "interrupted"
		job.Failure = "runner-did-not-record-a-result-before-deadline"
	}
	return job, err
}

func getJob(ctx context.Context, db rowScanner, i Identity, key string) (Job, string, error) {
	var payload []byte
	var signature string
	err := db.QueryRowContext(ctx, `SELECT request_digest,result FROM work_jobs
		WHERE tenant=? AND workspace=? AND actor=? AND request_id=?`, i.Tenant, i.Workspace, i.Actor, key).Scan(&signature, &payload)
	var job Job
	if err != nil {
		return job, "", missing(err)
	}
	err = json.Unmarshal(payload, &job)
	return job, signature, err
}

func (s *Store) beginJob(ctx context.Context, i Identity, key, kind string, plan planning.Plan, input any, reserve int) (Job, error) {
	if !requestID.MatchString(key) {
		return Job{}, fmt.Errorf("%w: idempotency key is required", ErrInvalid)
	}
	body, err := json.Marshal(input)
	if err != nil {
		return Job{}, err
	}
	signature := snapshot.Digest(append([]byte(kind+"\x00"), body...))
	return transaction(ctx, s, func(tx *sql.Tx) (Job, error) {
		ws, err := current(ctx, tx, i, true)
		if err != nil {
			return Job{}, err
		}
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_grants WHERE secret=? AND tenant=? AND workspace=?
			AND actor=? AND role=? AND revoked IS NULL AND expires>UTC_TIMESTAMP(6)`,
			i.secret[:], i.Tenant, i.Workspace, i.Actor, i.Role).Scan(&present); err != nil || present != 1 {
			if err == nil {
				err = ErrDenied
			}
			return Job{}, err
		}
		old, oldSignature, err := getJob(ctx, tx, i, key)
		if err == nil {
			if oldSignature != signature {
				return Job{}, ErrConflict
			}
			if old.State == "running" && time.Now().UTC().After(old.Deadline) {
				old.State, old.Failure = "interrupted", "runner-did-not-record-a-result-before-deadline"
			}
			return old, nil
		}
		if !errors.Is(err, ErrMissing) {
			return Job{}, err
		}
		if ws.Baseline != plan.Baseline {
			return Job{}, fmt.Errorf("%w: plan baseline has changed", ErrConflict)
		}
		var used, running int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_jobs WHERE tenant=? AND workspace=?
			AND state='running' AND deadline>UTC_TIMESTAMP(6)`, i.Tenant, i.Workspace).Scan(&running); err != nil {
			return Job{}, err
		}
		if running >= 4 {
			return Job{}, fmt.Errorf("%w: workspace has four active jobs", ErrConflict)
		}
		if reserve > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(budget_charge),0) FROM work_jobs WHERE tenant=? AND workspace=? AND plan=?`,
				i.Tenant, i.Workspace, plan.Digest).Scan(&used); err != nil {
				return Job{}, err
			}
			if used+reserve > plan.ModelBudget {
				return Job{}, fmt.Errorf("%w: plan model budget exhausted", ErrConflict)
			}
		}
		job := Job{RequestID: key, State: "running", Kind: kind, Plan: plan.Digest, Revision: ws.Revision,
			Deadline: time.Now().UTC().Add(jobDuration + 15*time.Second), owner: hex.EncodeToString(randBytes(16))}
		payload, err := json.Marshal(job)
		if err != nil {
			return Job{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO work_jobs
			(tenant,workspace,actor,request_id,request_digest,owner_mark,kind,plan,baseline_revision,state,budget_charge,deadline,result)
			VALUES(?,?,?,?,?,?,?,?,?,'running',?,?,?)`, i.Tenant, i.Workspace, i.Actor, key, signature, job.owner,
			kind, plan.Digest, ws.Revision, reserve, job.Deadline, payload)
		return job, err
	})
}

func (s *Store) finishJob(i Identity, job Job, baseline string, save func(context.Context, *sql.Tx) (Ref, error)) (Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return transaction(ctx, s, func(tx *sql.Tx) (Job, error) {
		ws, err := current(ctx, tx, i, true)
		if err != nil {
			return Job{}, err
		}
		var owner, state string
		var charge int
		err = tx.QueryRowContext(ctx, `SELECT owner_mark,state,budget_charge FROM work_jobs
			WHERE tenant=? AND workspace=? AND actor=? AND request_id=? FOR UPDATE`,
			i.Tenant, i.Workspace, i.Actor, job.RequestID).Scan(&owner, &state, &charge)
		if err != nil {
			return Job{}, err
		}
		if owner != job.owner || state != "running" {
			return Job{}, ErrConflict
		}
		if ws.Baseline != baseline || ws.Revision != job.Revision {
			job.Failure = "baseline-changed"
		}
		if job.Usage != nil && job.Usage.Metered && job.Usage.Mode == "live" {
			actual := job.Usage.PromptTokens + job.Usage.CompletionTokens
			if actual < 0 || actual > 1_000_000 {
				job.Failure = "invalid-provider-usage"
			} else if actual > charge {
				job.Failure = "provider-exceeded-budget"
				charge = actual
			} else {
				charge = actual
			}
		}
		if job.Failure == "" {
			ref, err := save(ctx, tx)
			if err != nil {
				return Job{}, err
			}
			job.Artifact, job.State = ref.Digest, "completed"
		} else {
			job.State = "failed"
		}
		payload, err := json.Marshal(job)
		if err != nil {
			return Job{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE work_jobs SET state=?,budget_charge=?,result=?
			WHERE tenant=? AND workspace=? AND actor=? AND request_id=?`, job.State, charge, payload,
			i.Tenant, i.Workspace, i.Actor, job.RequestID)
		if err != nil {
			return Job{}, err
		}
		return job, audit(ctx, tx, i, "job-"+job.State, job.Plan, job)
	})
}

type CandidateInput struct {
	Source  string          `json:"source"`
	Indices []int           `json:"indices,omitempty"`
	Replay  json.RawMessage `json:"recorded_guidance,omitempty"`
}

func (s *Store) Generate(ctx context.Context, i Identity, key, planID, batchID string, in CandidateInput,
	backend validation.Backend, provider *model.Client) (Job, error) {
	if !i.Can("maintainer", "reviewer") || in.Source == "model" && !i.Can("maintainer") {
		return Job{}, ErrDenied
	}
	plan, err := s.Plan(ctx, i, planID)
	if err != nil {
		return Job{}, err
	}
	batch, ok := plan.Batch(batchID)
	if !ok || batch.State != "ready" {
		return Job{}, fmt.Errorf("%w: batch is not available", ErrConflict)
	}
	if !slices.Contains([]string{"rule", "reviewer", "model", "model-replay", "composer"}, in.Source) ||
		(batch.Kind == "dependencies") != (in.Source == "composer") ||
		in.Indices != nil && in.Source != "reviewer" || len(in.Replay) != 0 && in.Source != "model-replay" {
		return Job{}, ErrInvalid
	}
	repo, err := s.Snapshot(ctx, i, plan.Baseline)
	if err != nil {
		return Job{}, err
	}
	var choices []model.Choice
	var maxTokens, reserve int
	if in.Source == "model" {
		choices, maxTokens, reserve, err = model.Request(batch, plan.ModelBudget)
		if err != nil {
			return Job{}, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		if provider == nil {
			reserve = 0
		}
	}
	signature := struct {
		Plan, Batch string
		Input       CandidateInput
	}{planID, batchID, in}
	job, err := s.beginJob(ctx, i, key, "candidate", plan, signature, reserve)
	if err != nil || job.owner == "" {
		return job, err
	}
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobDuration)
	defer cancel()
	var value planning.Candidate
	switch in.Source {
	case "composer":
		if backend == nil {
			job.Failure = "validation-backend-unavailable"
		} else {
			value, err = validation.GenerateDependencies(run, backend, repo, plan)
		}
	case "model":
		if provider == nil {
			job.Failure = "model-not-configured"
			break
		}
		var guidance model.Guidance
		var use model.Usage
		guidance, use, err = provider.Suggest(run, choices, maxTokens)
		job.Usage = &use
		if err == nil {
			value, err = planning.CallCandidate(repo, plan, batchID, in.Source, use.Model, guidance.Indices)
		}
	case "model-replay":
		guidance, parseErr := model.Decode(in.Replay, len(batch.Findings))
		job.Usage = &model.Usage{Mode: "replay", Provider: "recorded", Model: "fixture",
			ResponseSHA256: snapshot.Digest(in.Replay), State: "rejected"}
		err = parseErr
		if err == nil {
			value, err = planning.CallCandidate(repo, plan, batchID, in.Source, "recorded-fixture", guidance.Indices)
			if err == nil {
				job.Usage.State = "complete"
			}
		}
	default:
		value, err = planning.CallCandidate(repo, plan, batchID, in.Source, plan.RuleVersion, in.Indices)
	}
	if err != nil {
		job.Failure = "candidate-generation-failed"
		if job.Usage != nil && job.Usage.State != "complete" {
			job.Failure = "model-" + job.Usage.State
		}
	}
	return s.finishJob(i, job, plan.Baseline, func(ctx context.Context, tx *sql.Tx) (Ref, error) {
		return saveCandidate(ctx, tx, i, value)
	})
}

type VerifyInput struct {
	Prerequisites []string `json:"prerequisite_candidates"`
}

func (s *Store) Verify(ctx context.Context, i Identity, key, candidate string, in VerifyInput, backend validation.Backend) (Job, error) {
	if !i.Can("verifier", "maintainer") {
		return Job{}, ErrDenied
	}
	if len(in.Prerequisites) > 100 {
		return Job{}, ErrInvalid
	}
	value, err := s.Candidate(ctx, i, candidate)
	if err != nil {
		return Job{}, err
	}
	plan, err := s.Plan(ctx, i, value.Plan)
	if err != nil {
		return Job{}, err
	}
	repo, err := s.Snapshot(ctx, i, value.Baseline)
	if err != nil {
		return Job{}, err
	}
	parents := make([]planning.Candidate, 0, len(in.Prerequisites))
	for _, id := range in.Prerequisites {
		parent, err := s.Candidate(ctx, i, id)
		if err != nil {
			return Job{}, err
		}
		parents = append(parents, parent)
	}
	signature := struct {
		Candidate string
		Input     VerifyInput
	}{candidate, in}
	job, err := s.beginJob(ctx, i, key, "validation", plan, signature, 0)
	if err != nil || job.owner == "" {
		return job, err
	}
	if backend == nil {
		backend = validation.Unavailable{}
	}
	run, cancel := context.WithTimeout(context.WithoutCancel(ctx), jobDuration)
	defer cancel()
	report, err := validation.Verify(run, backend, repo, plan, value, parents)
	if err != nil {
		job.Failure = "validation-input-rejected"
	}
	return s.finishJob(i, job, plan.Baseline, func(ctx context.Context, tx *sql.Tx) (Ref, error) {
		return saveReport(ctx, tx, i, report, job.Revision)
	})
}
