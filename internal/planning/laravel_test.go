package planning_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

func TestLaravelPricingPlan(t *testing.T) {
	fixture := filepath.Join("..", "..", "examples", "laravel-pricing")
	base, err := snapshot.Capture(fixture, "laravel-pricing-v1", []string{"app", "tests", "composer.json", "composer.lock"})
	if err != nil {
		t.Fatal(err)
	}
	options, err := os.ReadFile(filepath.Join(fixture, "plan-options.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input planning.Input
	if err := json.Unmarshal(options, &input); err != nil {
		t.Fatal(err)
	}
	plan, err := planning.Build(base, input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Resolution.State != "candidate" {
		t.Fatalf("Laravel dependency plan is %s: %+v", plan.Resolution.State, plan.Resolution.Blockers)
	}
	dependencies, found := plan.Batch("dependencies")
	if !found || dependencies.State != "ready" {
		t.Fatalf("dependency batch is not ready: %+v", dependencies)
	}
	calls, found := plan.Batch("rule-sale-amount")
	if !found || calls.State != "ready" || len(calls.Findings) != 2 ||
		calls.Findings[0].Function != "quote" || calls.Findings[1].Function != "credit" {
		t.Fatalf("expected two exact calls in their enclosing functions: %+v", calls)
	}
}
