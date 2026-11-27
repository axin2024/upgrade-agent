package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

func TestWorkspaceLifecycle(t *testing.T) {
	dsn := os.Getenv("UPGRADE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("UPGRADE_TEST_MYSQL_DSN needs a disposable schema")
	}
	tool := fixtureBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	must(t, Migrate(dsn, 1))
	store, err := Open(ctx, dsn)
	must(t, err)
	defer store.Close()
	if err := store.Ready(ctx); err == nil {
		t.Fatal("schema v1 reported ready for a v2 operation")
	}
	maintainerGrant, err := store.Provision(ctx, "engineering", "pricing", "mira", "maintainer", time.Hour)
	must(t, err)
	must(t, Migrate(dsn, 1))
	if err := store.Ready(ctx); err == nil {
		t.Fatal("schema v2 reported ready for a v4 operation")
	}
	must(t, Migrate(dsn, 1))
	if err := store.Ready(ctx); err == nil {
		t.Fatal("schema v3 reported ready for a v4 operation")
	}
	must(t, Migrate(dsn, 0))
	must(t, store.Ready(ctx))
	maintainer, err := store.Identify(ctx, maintainerGrant.Token)
	must(t, err)
	reviewer := operator(t, ctx, store, "engineering", "pricing", "omar", "reviewer")
	verifier := operator(t, ctx, store, "engineering", "pricing", "lin", "verifier")
	outsider := operator(t, ctx, store, "other-tenant", "pricing", "mira", "maintainer")

	root := filepath.Join("..", "..", "examples", "legacy-pricing")
	snap, err := snapshot.Capture(root, "legacy-v1", []string{"composer.json", "composer.lock", "app", "tests"})
	must(t, err)
	pin, err := store.Pin(ctx, maintainer, "pin-1", BaselineInput{Snapshot: snap})
	must(t, err)
	if pin.Revision != 1 || len(pin.Delta) != 5 {
		t.Fatalf("first pin %+v", pin)
	}
	retry, err := store.Pin(ctx, maintainer, "pin-1", BaselineInput{Snapshot: snap})
	must(t, err)
	if retry.Revision != pin.Revision || !slices.Equal(retry.Delta, pin.Delta) {
		t.Fatal("baseline replay changed the receipt")
	}
	raw, err := os.ReadFile(filepath.Join(root, "plan-options.json"))
	must(t, err)
	var options planning.Input
	must(t, json.Unmarshal(raw, &options))
	planRef, err := store.CreatePlan(ctx, maintainer, "create-plan-1", PlanInput{Baseline: snap.Digest, Options: options})
	must(t, err)
	planAgain, err := store.CreatePlan(ctx, maintainer, "create-plan-repeat", PlanInput{Baseline: snap.Digest, Options: options})
	must(t, err)
	if planAgain != planRef {
		t.Fatalf("immutable baseline produced a different plan: %s and %s", planRef.Digest, planAgain.Digest)
	}
	plan, err := store.Plan(ctx, maintainer, planRef.Digest)
	must(t, err)
	sale, _ := plan.Batch("rule-sale-price")
	if plan.Resolution.State != "candidate" || len(plan.Resolution.Selected) != 4 ||
		len(sale.Findings) != 2 || sale.Findings[0].Function != "estimateSale" || sale.Findings[1].Function != "estimateCredit" {
		t.Fatalf("unexpected plan result: %+v exact %+v", plan.Resolution, sale.Findings)
	}
	if _, err := store.Plan(ctx, outsider, plan.Digest); !errors.Is(err, ErrMissing) {
		t.Fatalf("cross-tenant plan read: %v", err)
	}
	matches, err := store.FindPlans(ctx, reviewer, "upgrade-lab/framework", "^2.0", "")
	must(t, err)
	if len(matches.Items) != 1 || matches.Items[0].Digest != plan.Digest ||
		matches.Items[0].Repository != "legacy-v1" || matches.Next != "" {
		t.Fatalf("target lookup did not retrieve the immutable plan: %+v", matches)
	}
	for _, query := range []struct {
		identity Identity
		version  string
		after    string
	}{
		{outsider, "^2.0", ""},
		{reviewer, "^3.0", ""},
		{reviewer, "^2.0", plan.Digest},
	} {
		matches, err := store.FindPlans(ctx, query.identity, "upgrade-lab/framework", query.version, query.after)
		must(t, err)
		if len(matches.Items) != 0 {
			t.Fatalf("target lookup crossed a workspace or filter: %+v", matches)
		}
	}
	finding := sale.Findings[0]
	source, err := store.FindingSource(ctx, reviewer, plan.Digest, finding.Path, finding.Line)
	must(t, err)
	if source.Line != finding.Line || !strings.Contains(source.Lines[source.Line-source.FirstLine], finding.Original) {
		t.Fatalf("source view did not land on %s:%d: %+v", finding.Path, finding.Line, source)
	}
	if _, err := store.FindingSource(ctx, outsider, plan.Digest, finding.Path, finding.Line); !errors.Is(err, ErrMissing) {
		t.Fatalf("outside reviewer could read source: %v", err)
	}
	if _, err := store.FindingSource(ctx, reviewer, plan.Digest, "composer.json", 1); !errors.Is(err, ErrMissing) {
		t.Fatalf("source view returned a file outside the findings: %v", err)
	}
	if _, err := store.Pin(ctx, reviewer, "unauthorized", BaselineInput{Snapshot: snap}); !errors.Is(err, ErrDenied) {
		t.Fatalf("reviewer was allowed to repin: %v", err)
	}
	delete(options.Targets, "upgrade-lab/legacy-discounts")
	delete(options.Targets, "upgrade-lab/legacy-rounding")
	held, err := planning.Build(snap, options)
	must(t, err)
	var seenDiscount, seenRounding bool
	for _, blocker := range held.Resolution.Blockers {
		for _, edge := range blocker.Chain {
			seenDiscount = seenDiscount || edge.From == "upgrade-lab/legacy-discounts"
			seenRounding = seenRounding || edge.From == "upgrade-lab/legacy-rounding"
		}
	}
	if held.Resolution.State != "blocked" || !seenDiscount || !seenRounding {
		t.Fatalf("conflicting plugins were lost: %+v", held.Resolution)
	}
	noLock, err := snapshot.Capture(root, "no-lock", []string{"composer.json", "app", "tests"})
	must(t, err)
	missingLockPlan, err := planning.Build(noLock, options)
	must(t, err)
	if missingLockPlan.Resolution.State != "missing_lock" {
		t.Fatalf("unlocked repo reported %s", missingLockPlan.Resolution.State)
	}

	started, proceed := make(chan struct{}), make(chan struct{})
	blocking := &oneRegeneration{Backend: tool, entered: started, proceed: proceed}
	first := make(chan jobResult, 1)
	go func() {
		job, err := store.Generate(ctx, maintainer, "dependency-job", plan.Digest, "dependencies",
			CandidateInput{Source: "composer"}, blocking, nil)
		first <- jobResult{job, err}
	}()
	select {
	case <-started:
	case job := <-first:
		t.Fatalf("generation did not reach backend: %+v", job)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	parallel, err := store.Generate(ctx, maintainer, "dependency-job", plan.Digest, "dependencies",
		CandidateInput{Source: "composer"}, blocking, nil)
	close(proceed)
	must(t, err)
	if parallel.State != "running" {
		t.Fatalf("duplicate request got %+v", parallel)
	}
	dependency := <-first
	must(t, dependency.err)
	checkJob(t, dependency.Job, "completed")
	resumed, err := store.Job(ctx, maintainer, "dependency-job")
	must(t, err)
	if resumed.Artifact != dependency.Artifact || blocking.calls.Load() != 1 {
		t.Fatalf("duplicate dependency execution: %+v / %+v", resumed, dependency)
	}
	if _, err := store.Job(ctx, outsider, "dependency-job"); !errors.Is(err, ErrMissing) {
		t.Fatalf("cross-tenant request lookup: %v", err)
	}

	checkedDep := verify(t, ctx, store, verifier, tool, "verify-dependency", dependency.Artifact)
	if checkedDep.State != "passed" {
		t.Fatalf("Composer upgrade did not validate: %+v", checkedDep.Checks)
	}
	all, err := store.Generate(ctx, maintainer, "rule-all", plan.Digest, sale.ID, CandidateInput{Source: "rule"}, tool, nil)
	must(t, err)
	checkJob(t, all, "completed")
	ruleCandidate, err := store.Candidate(ctx, reviewer, all.Artifact)
	must(t, err)
	if len(ruleCandidate.Changes) != 1 || ruleCandidate.Changes[0].Path != finding.Path {
		t.Fatalf("candidate touched files beyond the approved path: %+v", ruleCandidate.Changes)
	}
	rejected := verify(t, ctx, store, verifier, tool, "verify-all", all.Artifact, dependency.Artifact)
	if rejected.State != "blocked" || !slices.ContainsFunc(rejected.Checks, func(c validation.Check) bool { return c.State == "regression" }) {
		t.Fatalf("credit regression was missed: %+v", rejected)
	}
	decide := func(key, candidate, action, evidence string, affected ...string) (Decision, error) {
		return store.Decide(ctx, reviewer, key, candidate, DecisionInput{Action: action, ExpectedBaseline: snap.Digest,
			Evidence: evidence, Reason: "pricing fixture review", ConfirmAffected: affected})
	}
	if _, err := decide("reject-regression-check", all.Artifact, "adopt", rejected.Digest); !errors.Is(err, ErrConflict) {
		t.Fatalf("invalid selection accepted: %v", err)
	}
	_, err = decide("reject-regression", all.Artifact, "reject", "")
	must(t, err)
	badModel, err := store.Generate(ctx, maintainer, "bad-replay", plan.Digest, sale.ID,
		CandidateInput{Source: "model-replay", Replay: json.RawMessage(`{"indices":[99]}`)}, tool, nil)
	must(t, err)
	if badModel.State != "failed" || badModel.Usage == nil || badModel.Usage.Mode != "replay" {
		t.Fatalf("invalid replay was accepted: %+v", badModel)
	}
	reviewed, err := store.Generate(ctx, reviewer, "selected-sale", plan.Digest, sale.ID,
		CandidateInput{Source: "reviewer", Indices: []int{0}}, tool, nil)
	must(t, err)
	checkJob(t, reviewed, "completed")
	saleReport := verify(t, ctx, store, verifier, tool, "verify-safe-sale", reviewed.Artifact, dependency.Artifact)
	if saleReport.State != "passed" {
		t.Fatalf("reviewed pricing did not pass: %+v", saleReport)
	}
	if _, err := decide("sale-too-early", reviewed.Artifact, "adopt", saleReport.Digest); !errors.Is(err, ErrConflict) {
		t.Fatalf("dependent adopted ahead of prerequisites: %v", err)
	}
	_, err = decide("accept-dependency", dependency.Artifact, "adopt", checkedDep.Digest)
	must(t, err)
	_, err = decide("accept-sale", reviewed.Artifact, "adopt", saleReport.Digest)
	must(t, err)
	currency, err := store.Generate(ctx, maintainer, "currency", plan.Digest, "rule-currency-display",
		CandidateInput{Source: "rule"}, tool, nil)
	must(t, err)
	checkJob(t, currency, "completed")
	missingTools := verify(t, ctx, store, verifier, nil, "unavailable", currency.Artifact)
	if missingTools.State != "blocked" {
		t.Fatal("unavailable tools counted as passing evidence")
	}
	currencyReport := verify(t, ctx, store, verifier, tool, "verify-currency", currency.Artifact)
	_, err = decide("accept-currency", currency.Artifact, "adopt", currencyReport.Digest)
	must(t, err)
	bundle, err := store.Export(ctx, reviewer, "export-3", plan.Digest, snap.Digest)
	must(t, err)
	if len(bundle.Selections) != 3 || !strings.Contains(bundle.Patch, "+++ b/composer.lock") ||
		snapshot.Digest([]byte(bundle.Patch)) != bundle.PatchSHA256 {
		t.Fatalf("unexpected export %+v", bundle.Selections)
	}
	applied := t.TempDir()
	must(t, snap.Materialize(applied, nil))
	git := exec.CommandContext(ctx, "git", "apply", "--check", "-")
	git.Dir, git.Stdin = applied, strings.NewReader(bundle.Patch)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git apply --check: %s: %v", out, err)
	}
	if _, err := decide("unconfirmed-withdraw", dependency.Artifact, "withdraw", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("withdraw did not require dependent confirmation: %v", err)
	}
	decision, err := decide("confirmed-withdraw", dependency.Artifact, "withdraw", "", sale.ID)
	must(t, err)
	if !slices.Equal(decision.Affected, []string{sale.ID}) {
		t.Fatalf("unexpected withdrawal %+v", decision)
	}
	_, err = decide("reject-withdrawn-prerequisite", dependency.Artifact, "reject", "")
	must(t, err)
	if _, err := decide("reject-dependent-adoption", reviewed.Artifact, "adopt", saleReport.Digest); !errors.Is(err, ErrConflict) {
		t.Fatalf("dependent adopted after a prerequisite was rejected: %v", err)
	}
	bundle, err = store.Export(ctx, reviewer, "export-1", plan.Digest, snap.Digest)
	must(t, err)
	if len(bundle.Selections) != 1 || bundle.Selections[0].Batch != "rule-currency-display" {
		t.Fatalf("independent batch lost: %+v", bundle.Selections)
	}
	batchInput := ReviewBatchInput{Revision: pin.Revision, Items: []ReviewBatchItemInput{
		{Candidate: all.Artifact, DecisionInput: DecisionInput{Action: "reject", ExpectedBaseline: snap.Digest, Reason: "keep credit regression blocked"}},
		{Candidate: currency.Artifact, DecisionInput: DecisionInput{Action: "reject", ExpectedBaseline: snap.Digest, Reason: "selected patch requires withdrawal"}},
		{Candidate: strings.Repeat("e", 64), DecisionInput: DecisionInput{Action: "reject", ExpectedBaseline: snap.Digest, Reason: "missing candidate"}},
	}}
	const batchKey = "review-mixed"
	prepared, err := store.PrepareReviewBatch(ctx, reviewer, batchKey, batchInput)
	must(t, err)
	if prepared.Completed != 0 || len(prepared.Items) != 3 {
		t.Fatalf("unexpected prepared review %+v", prepared)
	}
	if _, err := store.ReviewBatch(ctx, maintainer, batchKey); !errors.Is(err, ErrMissing) {
		t.Fatalf("review batch reached another actor: %v", err)
	}
	if _, err := store.ReviewBatch(ctx, outsider, batchKey); !errors.Is(err, ErrMissing) {
		t.Fatalf("review batch reached another tenant: %v", err)
	}
	if _, err := store.RunReviewBatch(ctx, verifier, batchKey); !errors.Is(err, ErrDenied) {
		t.Fatalf("verifier ran review batch: %v", err)
	}
	partial, err := store.reviewNext(ctx, reviewer, batchKey)
	must(t, err)
	if partial.Completed != 1 || partial.Items[0].Receipt == nil || partial.Items[0].Receipt.RequestID != batchKey ||
		partial.Items[1].State != "pending" || partial.Items[2].State != "pending" {
		t.Fatalf("unexpected interrupted review %+v", partial)
	}
	reopened, err := Open(ctx, dsn)
	must(t, err)
	defer reopened.Close()
	resumedReview, err := reopened.PrepareReviewBatch(ctx, reviewer, batchKey, batchInput)
	must(t, err)
	if resumedReview.Completed != 1 {
		t.Fatalf("identical replay returned %+v", resumedReview)
	}
	changed := batchInput
	changed.Items = slices.Clone(batchInput.Items)
	changed.Items[0].Reason = "changed request contents"
	if _, err := reopened.PrepareReviewBatch(ctx, reviewer, batchKey, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed batch preparation replay: %v", err)
	}
	ready, err := reopened.ReviewBatchIDs(ctx, reviewer)
	must(t, err)
	if !slices.Equal(ready, []string{batchKey}) {
		t.Fatalf("unexpected batch list %+v", ready)
	}
	type batchResult struct {
		batch ReviewBatch
		err   error
	}
	results := make(chan batchResult, 2)
	for _, connection := range []*Store{store, reopened} {
		go func() {
			batch, err := connection.RunReviewBatch(ctx, reviewer, batchKey)
			results <- batchResult{batch, err}
		}()
	}
	for range 2 {
		result := <-results
		must(t, result.err)
		batch := result.batch
		if batch.Completed != 3 || batch.Items[0].State != "applied" || batch.Items[1].State != "rejected" ||
			!strings.Contains(batch.Items[1].Error, "requires its own decision") || batch.Items[2].Error != ErrMissing.Error() {
			t.Fatalf("unexpected completed batch %+v", batch)
		}
	}
	var batchAudits int
	must(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant=? AND workspace=?
		AND actor=? AND JSON_UNQUOTE(JSON_EXTRACT(detail,'$.review_batch'))=?`,
		reviewer.Tenant, reviewer.Workspace, reviewer.Actor, batchKey).Scan(&batchAudits))
	if batchAudits != 1 {
		t.Fatalf("expected one decision audit, found %d", batchAudits)
	}
	_, err = store.PrepareReviewBatch(ctx, reviewer, "review-stale", batchInput)
	must(t, err)
	other, err := snapshot.Capture(root, "legacy-v2", snap.Roots)
	must(t, err)
	preview, err := store.PreviewPin(ctx, maintainer, noLock)
	must(t, err)
	if preview.Revision != pin.Revision || preview.Baseline != snap.Digest || preview.Proposed != noLock.Digest ||
		!slices.Equal(preview.Delta, []PathDelta{{Path: "composer.lock", State: "removed"}}) {
		t.Fatalf("incorrect read-only preview of a removed lock: %+v", preview)
	}
	_, err = store.Pin(ctx, maintainer, "pin-2", BaselineInput{Expected: snap.Digest, Snapshot: other})
	must(t, err)
	if _, err := store.Pin(ctx, maintainer, "preview-stale", BaselineInput{
		Expected: preview.Baseline, ExpectedRevision: &preview.Revision, Snapshot: noLock,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale baseline preview was written: %v", err)
	}
	preview, err = store.PreviewPin(ctx, maintainer, snap)
	must(t, err)
	if preview.Revision != pin.Revision+1 || preview.Baseline != other.Digest {
		t.Fatalf("fresh preview did not use the moved baseline: %+v", preview)
	}
	_, err = store.Pin(ctx, maintainer, "pin-3", BaselineInput{Expected: preview.Baseline, ExpectedRevision: &preview.Revision, Snapshot: snap})
	must(t, err)
	stale, err := reopened.RunReviewBatch(ctx, reviewer, "review-stale")
	must(t, err)
	for _, item := range stale.Items {
		if item.State != "rejected" || item.Error != ErrConflict.Error() || item.Receipt != nil {
			t.Fatalf("older review affected the new revision: %+v", item)
		}
	}
	if _, err := decide("old-evidence", currency.Artifact, "adopt", currencyReport.Digest); !errors.Is(err, ErrConflict) {
		t.Fatalf("historical evidence was adopted after a baseline round trip: %v", err)
	}
	if _, err := store.Patch(ctx, outsider, currency.Artifact); !errors.Is(err, ErrMissing) {
		t.Fatalf("cross-tenant patch read: %v", err)
	}
	outsiderEvents, err := store.History(ctx, outsider, 0)
	must(t, err)
	if len(outsiderEvents) != 0 {
		t.Fatalf("cross-tenant event read: %+v", outsiderEvents)
	}
	count, err := store.Revoke(ctx, reviewer.Tenant, reviewer.Workspace, reviewer.Actor)
	must(t, err)
	if count != 1 {
		t.Fatalf("expected one revoked credential, found %d", count)
	}
	if _, err := decide("revoked-decision", currency.Artifact, "reject", ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked reviewer retained write authority: %v", err)
	}
	before := time.Now().Add(-time.Hour).UTC()
	expired := before.Add(-time.Hour)
	_, err = store.db.ExecContext(ctx, `UPDATE snapshots SET created_at=? WHERE tenant=? AND workspace=?`,
		expired, maintainer.Tenant, maintainer.Workspace)
	must(t, err)
	_, err = store.db.ExecContext(ctx, `UPDATE audit_events SET happened_at=? WHERE tenant=? AND workspace=? AND action='pin-snapshot'`,
		expired, maintainer.Tenant, maintainer.Workspace)
	must(t, err)
	retention, err := store.Prune(ctx, maintainer.Tenant, maintainer.Workspace, before, false)
	must(t, err)
	if retention.Applied || !slices.Equal(retention.Snapshots, []string{other.Digest}) || len(retention.AuditIDs) != 3 {
		t.Fatalf("wrong retention preview: %+v", retention)
	}
	_, err = store.Snapshot(ctx, maintainer, other.Digest)
	must(t, err)
	pruned, err := store.Prune(ctx, maintainer.Tenant, maintainer.Workspace, before, true)
	must(t, err)
	if !pruned.Applied || !slices.Equal(pruned.Snapshots, retention.Snapshots) || !slices.Equal(pruned.AuditIDs, retention.AuditIDs) {
		t.Fatalf("cleanup failed to match its preview: %+v", pruned)
	}
	if _, err := store.Snapshot(ctx, maintainer, other.Digest); !errors.Is(err, ErrMissing) {
		t.Fatalf("expired snapshot payload survived cleanup: %v", err)
	}
	_, err = store.Patch(ctx, maintainer, currency.Artifact)
	must(t, err)
	_, err = store.Pin(ctx, maintainer, "pin-2", BaselineInput{Expected: snap.Digest, Snapshot: other})
	must(t, err)
	ws, err := store.Current(ctx, maintainer)
	must(t, err)
	if ws.Baseline != snap.Digest {
		t.Fatal("cleanup reset the original idempotency receipt")
	}
	t.Logf("tool=%s schema=1→2→3→4→5 regression=%s patched=%s tenant-isolation=passed batch-applied=1 batch-rejected=2 batch-audits=%d retention=passed", tool.Identity(),
		rejected.State, saleReport.State, batchAudits)
}

type jobResult struct {
	Job
	err error
}

type oneRegeneration struct {
	validation.Backend
	entered, proceed chan struct{}
	calls            atomic.Int32
}

func (o *oneRegeneration) Regenerate(ctx context.Context, directory string) (string, error) {
	if o.calls.Add(1) == 1 {
		close(o.entered)
	}
	select {
	case <-o.proceed:
		return o.Backend.Regenerate(ctx, directory)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func fixtureBackend(t *testing.T) validation.Backend {
	t.Helper()
	if binary, image := os.Getenv("UPGRADE_TEST_DOCKER"), os.Getenv("UPGRADE_TEST_IMAGE"); binary != "" && image != "" {
		tool, err := validation.NewDocker(binary, image, 30*time.Second)
		must(t, err)
		return tool
	}
	php, composer := os.Getenv("UPGRADE_TEST_PHP"), os.Getenv("UPGRADE_TEST_COMPOSER")
	if !filepath.IsAbs(php) || !filepath.IsAbs(composer) {
		t.Fatal("select Docker or absolute PHP and Composer executable paths for the authored fixture only")
	}
	return fixturePHP{php, composer}
}

// fixturePHP is test-only and runs the original repository-authored fixture.
type fixturePHP struct{ php, composer string }

func (f fixturePHP) Identity() string { return "trusted-test-fixture-php-composer" }

func (f fixturePHP) Check(ctx context.Context, directory, kind, file string) validation.Observation {
	start := time.Now()
	var out []byte
	var err error
	if kind == "composer-lock" {
		out, err = f.run(ctx, directory, f.composer, "validate", "--check-lock", "--no-check-publish", "--no-scripts", "--no-plugins", "--no-ansi")
		if err == nil {
			more, installErr := f.run(ctx, directory, f.composer, "install", "--dry-run", "--no-scripts", "--no-plugins", "--no-ansi")
			out, err = append(out, more...), installErr
		}
	} else {
		args := []string{file}
		if kind == "php-lint" {
			args = append([]string{"-l"}, args...)
		}
		out, err = f.run(ctx, directory, args...)
	}
	result := validation.Observation{State: "passed", Milliseconds: time.Since(start).Milliseconds(),
		OutputBytes: len(out), OutputSHA256: snapshot.Digest(out)}
	if err != nil {
		result.State, result.ExitCode = "failed", 1
	}
	return result
}

func (f fixturePHP) Regenerate(ctx context.Context, directory string) (string, error) {
	out, err := f.run(ctx, directory, f.composer, "update", "--no-install", "--no-scripts", "--no-plugins", "--no-ansi", "--no-progress")
	if err != nil {
		return "", fmt.Errorf("trusted fixture Composer: %w: %s", err, out)
	}
	file, err := os.ReadFile(filepath.Join(directory, "composer.lock"))
	return string(file), err
}

func (f fixturePHP) run(ctx context.Context, directory string, args ...string) ([]byte, error) {
	home, err := os.MkdirTemp("", "upgrade-test-composer-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	command := exec.CommandContext(ctx, f.php, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "COMPOSER_DISABLE_NETWORK=1", "COMPOSER_HOME="+home, "COMPOSER_NO_INTERACTION=1")
	return command.CombinedOutput()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func operator(t *testing.T, ctx context.Context, s *Store, tenant, ws, who, role string) Identity {
	t.Helper()
	grant, err := s.Provision(ctx, tenant, ws, who, role, time.Hour)
	must(t, err)
	i, err := s.Identify(ctx, grant.Token)
	must(t, err)
	return i
}

func checkJob(t *testing.T, job Job, state string) {
	t.Helper()
	if job.State != state || state == "completed" && job.Artifact == "" {
		t.Fatalf("expected %s job: %+v", state, job)
	}
}

func verify(t *testing.T, ctx context.Context, s *Store, identity Identity, tool validation.Backend, key, digest string,
	parents ...string) validation.Report {
	t.Helper()
	job, err := s.Verify(ctx, identity, key, digest, VerifyInput{Prerequisites: parents}, tool)
	must(t, err)
	checkJob(t, job, "completed")
	report, err := s.Report(ctx, identity, job.Artifact)
	must(t, err)
	return report
}
