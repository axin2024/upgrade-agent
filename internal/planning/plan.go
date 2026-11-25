// Package planning ties upgrade batches to a captured baseline and approved tools.
package planning

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/composer"
	"github.com/axin2024/upgrade-agent/internal/impact"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type Task struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
}

type Input struct {
	Targets     map[string]string `json:"targets"`
	Catalog     composer.Catalog  `json:"catalog"`
	RuleVersion string            `json:"rule_version"`
	Rules       []impact.Rule     `json:"rules"`
	Tasks       []Task            `json:"tasks"`
	ModelBudget int               `json:"model_budget_tokens"`
}

type Batch struct {
	ID            string           `json:"id"`
	Kind          string           `json:"kind"`
	Prerequisites []string         `json:"prerequisites,omitempty"`
	Files         []string         `json:"expected_files"`
	Findings      []impact.Finding `json:"findings,omitempty"`
	Tasks         []Task           `json:"tasks"`
	State         string           `json:"state"`
}

type Plan struct {
	Digest      string             `json:"sha256"`
	Baseline    string             `json:"baseline"`
	RuleVersion string             `json:"rule_version"`
	Targets     map[string]string  `json:"targets"`
	Inventory   composer.Inventory `json:"inventory"`
	Resolution  composer.Result    `json:"resolution"`
	Batches     []Batch            `json:"batches"`
	Findings    []impact.Finding   `json:"findings"`
	ModelBudget int                `json:"model_budget_tokens"`
}

var tag = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func Build(s snapshot.Snapshot, input Input) (Plan, error) {
	if !tag.MatchString(input.RuleVersion) || input.ModelBudget < 0 || input.ModelBudget > 12_000 || len(input.Tasks) > 24 {
		return Plan{}, fmt.Errorf("invalid rule version, task list, or model budget")
	}
	tasks, err := checkTasks(s, input.Tasks)
	if err != nil {
		return Plan{}, err
	}
	inventory, err := composer.Read(s, input.Catalog.Platform)
	if err != nil {
		return Plan{}, err
	}
	findings, err := impact.Scan(s, input.Rules)
	if err != nil {
		return Plan{}, err
	}
	resolved := composer.Result{State: "unchanged"}
	if len(input.Targets) > 0 {
		resolved, err = composer.Solve(inventory, input.Catalog, input.Targets)
		if err != nil {
			return Plan{}, err
		}
	}
	plan := Plan{
		Baseline: s.Digest, RuleVersion: input.RuleVersion, Targets: maps.Clone(input.Targets),
		Inventory: inventory, Resolution: resolved, Findings: findings,
		Batches: make([]Batch, 0), ModelBudget: input.ModelBudget,
	}
	if len(input.Targets) > 0 {
		state := "blocked"
		if resolved.State == "candidate" && hasKind(tasks, "composer-lock") {
			state = "ready"
		}
		plan.Batches = append(plan.Batches, Batch{
			ID: "dependencies", Kind: "dependencies", State: state, Files: []string{"composer.json", "composer.lock"},
			Tasks: tasks,
		})
	}
	for _, rule := range input.Rules {
		batch := Batch{ID: "rule-" + rule.ID, Kind: "calls", Tasks: tasks, State: "ready"}
		for _, finding := range findings {
			if finding.Rule == rule.ID && finding.State == "exact" {
				batch.Findings = append(batch.Findings, finding)
				batch.Files = append(batch.Files, finding.Path)
			}
		}
		if len(batch.Files) == 0 {
			continue
		}
		slices.Sort(batch.Files)
		batch.Files = slices.Compact(batch.Files)
		if !hasKind(tasks, "php-lint") || !hasKind(tasks, "php-test") {
			batch.State = "blocked"
		}
		for _, required := range rule.Requires {
			if _, exists := input.Targets[required]; !exists || resolved.State != "candidate" ||
				!hasKind(tasks, "composer-lock") {
				batch.State = "blocked"
			}
		}
		if len(rule.Requires) > 0 && len(input.Targets) > 0 {
			batch.Prerequisites = []string{"dependencies"}
		}
		plan.Batches = append(plan.Batches, batch)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return Plan{}, err
	}
	plan.Digest = snapshot.Digest(encoded)
	return plan, nil
}

func (p Plan) Batch(id string) (Batch, bool) {
	for _, batch := range p.Batches {
		if batch.ID == id {
			return batch, true
		}
	}
	return Batch{}, false
}

func hasKind(tasks []Task, kind string) bool {
	return slices.ContainsFunc(tasks, func(task Task) bool { return task.Kind == kind })
}

func checkTasks(s snapshot.Snapshot, input []Task) ([]Task, error) {
	seen := make(map[string]bool)
	tasks := slices.Clone(input)
	for _, task := range tasks {
		if !tag.MatchString(task.ID) || seen[task.ID] {
			return nil, fmt.Errorf("invalid or duplicate task id %q", task.ID)
		}
		seen[task.ID] = true
		switch task.Kind {
		case "composer-lock", "php-lint":
			if task.Path != "" {
				return nil, fmt.Errorf("task %q does not accept a path", task.ID)
			}
		case "php-test":
			if _, ok := s.Find(task.Path); !ok || !strings.HasSuffix(task.Path, ".php") ||
				!snapshot.Within(task.Path, s.Roots) {
				return nil, fmt.Errorf("task %q must name an approved PHP test file", task.ID)
			}
		default:
			return nil, fmt.Errorf("unknown task kind %q", task.Kind)
		}
	}
	slices.SortFunc(tasks, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	return tasks, nil
}
