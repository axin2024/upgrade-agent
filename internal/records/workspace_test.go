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
	bundle, err = store.Export(ctx, reviewer, "export-1", plan.Digest, snap.Digest)
	must(t, err)
	if len(bundle.Selections) != 1 || bundle.Selections[0].Batch != "rule-currency-display" {
		t.Fatalf("independent batch lost: %+v", bundle.Selections)
	}
	other, err := snapshot.Capture(root, "legacy-v2", snap.Roots)
	must(t, err)
	_, err = store.Pin(ctx, maintainer, "pin-2", BaselineInput{Expected: snap.Digest, Snapshot: other})
	must(t, err)
	_, err = store.Pin(ctx, maintainer, "pin-3", BaselineInput{Expected: other.Digest, Snapshot: snap})
	must(t, err)
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
	t.Logf("tool=%s schema=1→2 regression=%s patched=%s tenant-isolation=passed", tool.Identity(),
		rejected.State, saleReport.State)
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
