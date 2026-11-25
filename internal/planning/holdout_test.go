package planning_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/upgrade-agent/internal/composer"
	"github.com/axin2024/upgrade-agent/internal/impact"
	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type dependencyFixture struct {
	Name      string
	Target    string
	Expected  string
	Solver    string
	Packages  []composer.Package
	Extra     map[string]string
	Platform  map[string]string
	Conflicts map[string]string
}

var dependencyHoldout = []dependencyFixture{
	{Name: "caret-upper-bound", Target: "^2.0", Expected: "candidate", Packages: []composer.Package{pkg("bench/core", "2.4.2", nil), pkg("bench/core", "3.0.0", nil)}},
	{Name: "upper-bound-exclusive", Target: ">=2.0 <2.4", Expected: "blocked", Packages: []composer.Package{pkg("bench/core", "2.4.0", nil)}},
	{Name: "platform-php-minimum", Target: "^2.0", Expected: "blocked", Packages: []composer.Package{pkg("bench/core", "2.1.0", map[string]string{"php": ">=8.4"})}, Platform: map[string]string{"php": "8.2.30"}},
	{Name: "platform-extension-present", Target: "^2.0", Expected: "candidate", Packages: []composer.Package{pkg("bench/core", "2.1.0", map[string]string{"ext-json": "*"})}, Platform: map[string]string{"ext-json": "8.2.30"}},
	{Name: "platform-extension-missing", Target: "^2.0", Expected: "blocked", Solver: "needs-composer", Packages: []composer.Package{pkg("bench/core", "2.1.0", map[string]string{"ext-foo-absent": "*"})}},
	{Name: "transitive-shared-pin", Target: "^2.0", Expected: "candidate", Extra: map[string]string{"bench/addon": "^1.0"}, Packages: []composer.Package{
		pkg("bench/core", "2.0.0", map[string]string{"bench/bridge": "^2.0"}), pkg("bench/addon", "1.0.0", map[string]string{"bench/bridge": "~2.1.0"}), pkg("bench/bridge", "2.1.4", nil)}},
	{Name: "transitive-unsatisfied", Target: "^2.0", Expected: "blocked", Extra: map[string]string{"bench/addon": "^1.0"}, Packages: []composer.Package{
		pkg("bench/core", "2.0.0", map[string]string{"bench/bridge": "^2.0"}), pkg("bench/addon", "1.0.0", map[string]string{"bench/bridge": "^1.0"}), pkg("bench/bridge", "1.9.0", nil), pkg("bench/bridge", "2.1.4", nil)}},
	{Name: "root-conflict-exclusion", Target: "^2.0", Expected: "candidate", Conflicts: map[string]string{"bench/core": ">=2.2.0"}, Packages: []composer.Package{
		pkg("bench/core", "2.1.3", nil), pkg("bench/core", "2.2.0", nil)}},
	{Name: "alternative-constraint", Target: "~2.2.0 || ^4.0", Expected: "candidate", Packages: []composer.Package{pkg("bench/core", "4.0.8", nil), pkg("bench/core", "2.3.9", nil)}},
	{Name: "transitive-backtracking", Target: "^2.0", Expected: "candidate", Packages: []composer.Package{
		pkg("bench/core", "2.1.0", map[string]string{"bench/bridge": "^3.0"}), pkg("bench/core", "2.0.0", map[string]string{"bench/bridge": "^2.0"}), pkg("bench/bridge", "2.1.0", nil)}},
}

func pkg(name, version string, requires map[string]string) composer.Package {
	return composer.Package{Name: name, Version: version, Require: requires, Type: "metapackage"}
}

type phpFixture struct {
	Name       string
	Code       string
	Expression string
	Return     string
	Scope      string
	Wanted     []string
	Exact      int
	AllPass    bool
}

var phpHoldout = []phpFixture{
	{Name: "checkout-units", Code: `function cart_quantity($n) { return Adapter::convert($n); }`, Expression: `cart_quantity(8)`, Return: "8", Scope: "Replace only cart_quantity.", Wanted: []string{"cart_quantity"}, Exact: 1, AllPass: true},
	{Name: "signed-return-voucher", Code: `function sales_quantity($n) { return Adapter::convert($n); } function refunded_quantity($n) { return Adapter::convert($n); }`, Expression: `[sales_quantity(2),refunded_quantity(-2)]`, Return: "[2,-2]", Scope: "Replace sales_quantity. Keep legacy refunded_quantity to preserve a negative refund.", Wanted: []string{"sales_quantity"}, Exact: 2},
	{Name: "inventory-writeoff", Code: `function reserve_units($n) { return \Legacy\Converter::convert($n); } function release_units($n) { return \Legacy\Converter::convert($n); }`, Expression: `[reserve_units(4),release_units(-9)]`, Return: "[4,-9]", Scope: "Migrate reserve_units only. Keep negative inventory release unchanged.", Wanted: []string{"reserve_units"}, Exact: 2},
	{Name: "line-item-comment", Code: `function line_item() { /* Adapter::convert(-7); */ return Adapter::convert(7); }`, Expression: `line_item()`, Return: "7", Scope: "Replace the executable call in line_item; ignore commentary.", Wanted: []string{"line_item"}, Exact: 1, AllPass: true},
	{Name: "audit-literal", Code: `function audit_line() { return 'Adapter::convert(-9)'; } function usage_total() { return Adapter::convert(13); }`, Expression: `[audit_line(),usage_total()]`, Return: `["Adapter::convert(-9)",13]`, Scope: "Migrate usage_total; leave the audit label intact.", Wanted: []string{"usage_total"}, Exact: 1, AllPass: true},
	{Name: "unrelated-short-name", Code: `function report() { return \Other\Converter::convert(-13); }`, Expression: `report()`, Return: "-26", Scope: "The Other class is outside the migration.", Wanted: nil},
	{Name: "dynamic-class", Code: `function dynamic_adjustment() { $class = Adapter::class; return $class::convert(-3); }`, Expression: `dynamic_adjustment()`, Return: "-3", Scope: "The dynamic class target requires manual inspection.", Wanted: nil},
	{Name: "warehouse-receipt", Code: `function receipt_units() { return aDaPtEr::CoNvErT(15); }`, Expression: `receipt_units()`, Return: "15", Scope: "Migrate receipt_units, where quantities are always positive.", Wanted: []string{"receipt_units"}, Exact: 1, AllPass: true},
	{Name: "carrier-return", Code: `function delivery_units() { return Adapter::convert(6); } function damaged_return_units() { return Adapter::convert(-6); }`, Expression: `[delivery_units(),damaged_return_units()]`, Return: "[6,-6]", Scope: "Use the modern call in delivery_units. The damaged_return_units path must keep legacy signed amounts.", Wanted: []string{"delivery_units"}, Exact: 2},
	{Name: "work-order-reversal", Code: `function production_units() { return Adapter::convert(11); } function reverse_work_order() { return Adapter::convert(-4); }`, Expression: `[production_units(),reverse_work_order()]`, Return: "[11,-4]", Scope: "Migrate production_units; omit reverse_work_order because its negative result is intentional.", Wanted: []string{"production_units"}, Exact: 2},
}

func TestHoldout(t *testing.T) {
	php := os.Getenv("UPGRADE_TEST_PHP")
	if php == "" {
		t.Skip("set UPGRADE_TEST_PHP to evaluate the authored PHP fixtures")
	}
	phar := os.Getenv("UPGRADE_TEST_COMPOSER")
	var provider *model.Client
	if endpoint := os.Getenv("UPGRADE_TEST_MODEL_URL"); endpoint != "" {
		var err error
		provider, err = model.New(endpoint, os.Getenv("UPGRADE_TEST_MODEL"), os.Getenv("UPGRADE_TEST_MODEL_TOKEN"), 40*time.Second)
		if err != nil {
			t.Fatal(err)
		}
	}
	results := make([]map[string]any, 0, len(dependencyHoldout)+len(phpHoldout))
	for _, input := range dependencyHoldout {
		t.Run(input.Name, func(t *testing.T) {
			started := time.Now()
			requirements := map[string]string{"bench/core": input.Target}
			for key, value := range input.Extra {
				requirements[key] = value
			}
			inv := composer.Inventory{LockState: "captured", Manifest: composer.Manifest{Require: requirements, Conflict: input.Conflicts}}
			resolved, err := composer.Solve(inv, composer.Catalog{Packages: input.Packages, Platform: input.Platform}, map[string]string{"bench/core": input.Target})
			if err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"case": input.Name, "kind": "dependency", "expected": input.Expected, "state": resolved.State,
				"solver_steps": resolved.Steps, "analysis_ms": time.Since(started).Milliseconds()}
			results = append(results, row)
			expectedState := input.Expected
			if input.Solver != "" {
				expectedState = input.Solver
			}
			if resolved.State != expectedState || (resolved.State == "blocked" && len(resolved.Blockers) == 0) {
				t.Errorf("resolve %s, expected %s", resolved.State, expectedState)
			}
			if phar != "" {
				ok, duration := runComposer(t, php, phar, input, requirements)
				row["composer_resolved"], row["composer_ms"] = ok, duration
				if ok != (input.Expected == "candidate") {
					t.Errorf("Composer resolution %v, expected %s", ok, input.Expected)
				}
			}
		})
	}
	for _, input := range phpHoldout {
		t.Run(input.Name, func(t *testing.T) { results = append(results, runPHPCase(t, input, php, provider)) })
	}
	record, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("UPGRADE_TEST_EVIDENCE"); output != "" {
		if err := os.WriteFile(output, append(record, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("Executed %d frozen cases (%d dependency, %d PHP)", len(results), len(dependencyHoldout), len(phpHoldout))
}

func runComposer(t *testing.T, php, phar string, input dependencyFixture, requirements map[string]string) (bool, int64) {
	t.Helper()
	work := t.TempDir()
	repositories := []any{map[string]any{"type": "package", "package": input.Packages}, map[string]bool{"packagist.org": false}}
	root := map[string]any{"name": "bench/fixture", "license": "Apache-2.0", "description": "Offline holdout case", "require": requirements,
		"repositories": repositories}
	if len(input.Conflicts) != 0 {
		root["conflict"] = input.Conflicts
	}
	if len(input.Platform) != 0 {
		root["config"] = map[string]any{"platform": input.Platform}
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, work, "composer.json", string(encoded))
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	cmd := exec.CommandContext(ctx, php, phar, "update", "--dry-run", "--no-install", "--no-audit", "--no-interaction", "--no-plugins", "--no-scripts")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "COMPOSER_DISABLE_NETWORK=1", "COMPOSER_HOME="+filepath.Join(work, "composer-home"))
	started := time.Now()
	output, err := cmd.CombinedOutput()
	if err != nil && (ctx.Err() != nil || !strings.Contains(string(output), "could not be resolved")) {
		t.Fatalf("Composer fixture failed to execute: %v\n%s", err, output)
	}
	return err == nil, time.Since(started).Milliseconds()
}

func runPHPCase(t *testing.T, input phpFixture, php string, provider *model.Client) map[string]any {
	t.Helper()
	const classes = `<?php
namespace Legacy; class Converter { static function convert(int $n): int { return $n; } }
namespace Modern; class Converter { static function convertFast(int $n): int { return abs($n); } }
namespace Other; class Converter { static function convert(int $n): int { return $n * 2; } }
namespace Fixtures; use Legacy\Converter as Adapter;
`
	dir := t.TempDir()
	writeFixture(t, dir, "composer.json", `{"name":"bench/php-fixture"}`)
	writeFixture(t, dir, "subject.php", classes+input.Code+"\n")
	writeFixture(t, dir, "assertions.php", fmt.Sprintf("<?php namespace Fixtures; require __DIR__.'/subject.php'; echo json_encode(%s);\n", input.Expression))
	s, err := snapshot.Capture(dir, "", []string{"composer.json", "subject.php", "assertions.php"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	plan, err := planning.Build(s, planning.Input{RuleVersion: "holdout-v1", Rules: []impact.Rule{{
		ID: "converter", OldType: "Legacy\\Converter", OldMethod: "convert", NewType: "Modern\\Converter", NewMethod: "convertFast",
		Guide: "https://example.test/migrations/converter", Risk: "high", Verify: []string{input.Scope},
	}}, Tasks: []planning.Task{{ID: "lint", Kind: "php-lint"}, {ID: "regression", Kind: "php-test", Path: "assertions.php"}}, ModelBudget: 3000})
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"case": input.Name, "kind": "php", "expected_functions": input.Wanted, "analysis_ms": time.Since(start).Milliseconds()}
	batch, found := plan.Batch("rule-converter")
	row["findings_exact"] = len(batch.Findings)
	if len(batch.Findings) != input.Exact || found != (input.Exact > 0) {
		t.Errorf("got %d exact findings, want %d", len(batch.Findings), input.Exact)
	}
	if !phpOutput(php, dir, input.Return) {
		t.Fatal("the unchanged fixture did not match its authored expectation")
	}
	if !found {
		return row
	}
	index := make([]int, 0, input.Exact)
	for idx, finding := range batch.Findings {
		if slices.Contains(input.Wanted, finding.Function) {
			index = append(index, idx)
		}
	}
	if len(index) != len(input.Wanted) {
		t.Errorf("maintainer-selected functions do not match exact findings")
	}
	start = time.Now()
	row["rule_regression_passed"] = runCandidate(t, s, plan, dir, php, input.Return, "rule", nil)
	row["reviewed_regression_passed"] = runCandidate(t, s, plan, dir, php, input.Return, "reviewer", index)
	row["review_and_execution_ms"] = time.Since(start).Milliseconds()
	if row["rule_regression_passed"] != input.AllPass || row["reviewed_regression_passed"] != true {
		t.Errorf("regression gate did not match authored semantics: %+v", row)
	}
	if provider != nil {
		choices, tokens, _, err := model.Request(batch, plan.ModelBudget)
		if err != nil {
			t.Fatal(err)
		}
		guidance, usage, err := provider.Suggest(context.Background(), choices, tokens)
		row["model_usage"] = usage
		row["model_indices"] = guidance.Indices
		row["model_exact_selection"] = err == nil && slices.Equal(guidance.Indices, index)
		if err == nil && len(guidance.Indices) != 0 {
			row["model_regression_passed"] = runCandidate(t, s, plan, dir, php, input.Return, "model", guidance.Indices)
		} else if err != nil {
			row["model_error"] = err.Error()
		}
	}
	return row
}

func runCandidate(t *testing.T, s snapshot.Snapshot, plan planning.Plan, dir, php, wanted, source string, indices []int) bool {
	t.Helper()
	candidate, err := planning.CallCandidate(s, plan, "rule-converter", source, "holdout-v1", indices)
	if err != nil {
		t.Error(err)
		return false
	}
	for _, change := range candidate.Changes {
		writeFixture(t, dir, change.Path, change.Content)
	}
	return phpOutput(php, dir, wanted)
}

func phpOutput(php, dir, wanted string) bool {
	ctx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	lint := exec.CommandContext(ctx, php, "-n", "-l", "subject.php")
	lint.Dir = dir
	if err := lint.Run(); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, php, "-n", "assertions.php")
	cmd.Dir = dir
	data, err := cmd.Output()
	return err == nil && string(data) == wanted
}

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
