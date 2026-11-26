package records

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

func TestLaravelPricingLifecycle(t *testing.T) {
	dsn, cache := os.Getenv("UPGRADE_TEST_LARAVEL_DSN"), os.Getenv("UPGRADE_TEST_LARAVEL_CACHE")
	if dsn == "" || cache == "" {
		t.Skip("requires a disposable MySQL schema and a Composer cache prepared for the real Laravel fixture")
	}
	php, composer := os.Getenv("UPGRADE_TEST_LARAVEL_PHP"), os.Getenv("UPGRADE_TEST_COMPOSER")
	if !filepath.IsAbs(php) || !filepath.IsAbs(composer) || !filepath.IsAbs(cache) {
		t.Fatal("select absolute PHP 8.4, Composer, and approved package cache paths")
	}
	tool := laravelFixturePHP{php: php, composer: composer, cache: cache, home: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	must(t, Migrate(dsn, 0))
	store, err := Open(ctx, dsn)
	must(t, err)
	defer store.Close()
	tenant := "laravel-" + hex.EncodeToString(randBytes(8))
	maintainer := operator(t, ctx, store, tenant, "pricing", "mira", "maintainer")
	reviewer := operator(t, ctx, store, tenant, "pricing", "omar", "reviewer")
	verifier := operator(t, ctx, store, tenant, "pricing", "lin", "verifier")

	fixture := filepath.Join("..", "..", "examples", "laravel-pricing")
	base, err := snapshot.Capture(fixture, "laravel-pricing-v1", []string{"app", "tests", "composer.json", "composer.lock"})
	must(t, err)
	_, err = store.Pin(ctx, maintainer, "pin", BaselineInput{Snapshot: base})
	must(t, err)
	raw, err := os.ReadFile(filepath.Join(fixture, "plan-options.json"))
	must(t, err)
	var options planning.Input
	must(t, json.Unmarshal(raw, &options))
	plan, err := store.CreatePlan(ctx, maintainer, "plan", PlanInput{Baseline: base.Digest, Options: options})
	must(t, err)

	dep, err := store.Generate(ctx, maintainer, "generate-deps", plan.Digest, "dependencies",
		CandidateInput{Source: "composer"}, tool, nil)
	must(t, err)
	checkJob(t, dep, "completed")
	depReport := verify(t, ctx, store, verifier, tool, "verify-deps", dep.Artifact)
	if depReport.State != "passed" {
		t.Fatalf("Laravel dependency update failed: %+v", depReport.Checks)
	}
	_, err = store.Decide(ctx, reviewer, "adopt-deps", dep.Artifact, DecisionInput{
		Action: "adopt", ExpectedBaseline: base.Digest, Evidence: depReport.Digest, Reason: "verified Laravel upgrade",
	})
	must(t, err)

	all, err := store.Generate(ctx, maintainer, "generate-all", plan.Digest, "rule-sale-amount",
		CandidateInput{Source: "rule"}, tool, nil)
	must(t, err)
	checkJob(t, all, "completed")
	regression := verify(t, ctx, store, verifier, tool, "verify-all", all.Artifact, dep.Artifact)
	if regression.State != "blocked" || !slices.ContainsFunc(regression.Checks, func(check validation.Check) bool {
		return check.Task == "pricing-contract" && check.State == "regression"
	}) {
		t.Fatalf("negative credit was not detected: %+v", regression.Checks)
	}
	_, err = store.Decide(ctx, reviewer, "adopt-regression", all.Artifact, DecisionInput{
		Action: "adopt", ExpectedBaseline: base.Digest, Evidence: regression.Digest, Reason: "exercise validation gate",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("workbench allowed failed candidate: %v", err)
	}
	_, err = store.Decide(ctx, reviewer, "reject-regression", all.Artifact, DecisionInput{
		Action: "reject", ExpectedBaseline: base.Digest, Reason: "use old behavior for credit",
	})
	must(t, err)
	reviewed, err := store.Generate(ctx, reviewer, "generate-reviewed", plan.Digest, "rule-sale-amount",
		CandidateInput{Source: "reviewer", Indices: []int{0}}, tool, nil)
	must(t, err)
	checkJob(t, reviewed, "completed")
	reviewedReport := verify(t, ctx, store, verifier, tool, "verify-reviewed", reviewed.Artifact, dep.Artifact)
	if reviewedReport.State != "passed" {
		t.Fatalf("reviewed Laravel candidate did not pass: %+v", reviewedReport.Checks)
	}
	_, err = store.Decide(ctx, reviewer, "adopt-reviewed", reviewed.Artifact, DecisionInput{
		Action: "adopt", ExpectedBaseline: base.Digest, Evidence: reviewedReport.Digest, Reason: "migrate only sale quote",
	})
	must(t, err)
	receipt, err := store.Export(ctx, reviewer, "export-reviewed", plan.Digest, base.Digest)
	must(t, err)
	if len(receipt.Selections) != 2 ||
		!slices.ContainsFunc(receipt.Selections, func(p Selection) bool { return p.Candidate == dep.Artifact }) ||
		!slices.ContainsFunc(receipt.Selections, func(p Selection) bool { return p.Candidate == reviewed.Artifact }) {
		t.Fatalf("export did not reflect the reviewed selection: %+v", receipt.Selections)
	}
	t.Logf("baseline=%s plan=%s dependency=%s rejected=%s reviewed=%s export=%s",
		base.Digest, plan.Digest, depReport.Digest, regression.Digest, reviewedReport.Digest, receipt.Digest)
}

// Runs only the repository-authored Laravel demonstration and its vetted
// dependencies. The production backend uses independent Docker containers.
type laravelFixturePHP struct{ php, composer, cache, home string }

func (f laravelFixturePHP) Identity() string { return "trusted-laravel-fixture-php-composer" }

func (f laravelFixturePHP) Check(ctx context.Context, directory, kind, file string) validation.Observation {
	start := time.Now()
	state := "passed"
	var out []byte
	var err error
	switch kind {
	case "composer-lock":
		out, err = f.run(ctx, directory, f.composer, "validate", "--check-lock", "--no-plugins", "--no-scripts")
	case "php-lint":
		out, err = f.run(ctx, directory, "-l", file)
	case "php-test":
		if _, err = os.Stat(filepath.Join(directory, "vendor", "autoload.php")); errors.Is(err, os.ErrNotExist) {
			out, err = f.run(ctx, directory, f.composer, "install", "--prefer-dist", "--no-plugins", "--no-scripts", "--no-progress")
		}
		if err != nil {
			state = "unavailable"
		} else {
			out, err = f.run(ctx, directory, file)
		}
	}
	var status int
	var exit *exec.ExitError
	if err != nil {
		if state == "passed" {
			state = "failed"
		}
		status = -1
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		}
	}
	return validation.Observation{State: state, ExitCode: status, Milliseconds: time.Since(start).Milliseconds(),
		OutputBytes: len(out), OutputSHA256: snapshot.Digest(out)}
}

func (f laravelFixturePHP) Regenerate(ctx context.Context, directory string) (string, error) {
	out, err := f.run(ctx, directory, f.composer, "update", "--no-install", "--no-plugins", "--no-scripts", "--no-progress")
	if err != nil {
		return "", fmt.Errorf("Laravel Composer: %w: %s", err, out)
	}
	lock, err := os.ReadFile(filepath.Join(directory, "composer.lock"))
	return string(lock), err
}

func (f laravelFixturePHP) run(ctx context.Context, directory string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, f.php, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "COMPOSER_CACHE_DIR="+f.cache, "COMPOSER_CACHE_READ_ONLY=1",
		"COMPOSER_HOME="+f.home, "COMPOSER_DISABLE_NETWORK=1", "COMPOSER_NO_INTERACTION=1")
	return command.CombinedOutput()
}
