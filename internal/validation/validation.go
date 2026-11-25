// Package validation compares approved tasks on isolated copies of a baseline.
package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type Observation struct {
	State        string `json:"state"`
	ExitCode     int    `json:"exit_code"`
	Milliseconds int64  `json:"milliseconds"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
	OutputBytes  int    `json:"output_bytes"`
}

type Check struct {
	Task      string      `json:"task"`
	Kind      string      `json:"kind"`
	File      string      `json:"file,omitempty"`
	Baseline  Observation `json:"baseline"`
	Candidate Observation `json:"candidate"`
	State     string      `json:"state"`
}

type Report struct {
	Digest    string   `json:"sha256"`
	Baseline  string   `json:"baseline"`
	Plan      string   `json:"plan"`
	Candidate string   `json:"candidate"`
	Parents   []string `json:"prerequisite_candidates,omitempty"`
	Toolchain string   `json:"toolchain"`
	State     string   `json:"state"`
	Started   string   `json:"started_utc"`
	Checks    []Check  `json:"checks"`
}

type Backend interface {
	Identity() string
	Check(context.Context, string, string, string) Observation
	Regenerate(context.Context, string) (string, error)
}

func Verify(ctx context.Context, backend Backend, s snapshot.Snapshot, p planning.Plan,
	c planning.Candidate, prerequisites []planning.Candidate) (Report, error) {
	batch, ok := p.Batch(c.Batch)
	if !ok || p.Baseline != s.Digest || p.Digest != c.Plan || c.Baseline != s.Digest ||
		batch.State != "ready" || len(batch.Tasks) == 0 {
		return Report{}, errors.New("candidate has no current approved validation plan")
	}
	byID := make(map[string]planning.Candidate)
	for _, prerequisite := range prerequisites {
		if !slices.Contains(batch.Prerequisites, prerequisite.Batch) || prerequisite.Plan != p.Digest || byID[prerequisite.Batch].Digest != "" {
			return Report{}, errors.New("validation input has unrelated or repeated prerequisites")
		}
		byID[prerequisite.Batch] = prerequisite
	}
	parents := make([]string, 0, len(batch.Prerequisites))
	exact := make([]planning.Candidate, 0, len(batch.Prerequisites)+1)
	for _, required := range batch.Prerequisites {
		parent, found := byID[required]
		if !found {
			return Report{}, fmt.Errorf("missing prerequisite candidate %s", required)
		}
		parents, exact = append(parents, parent.Digest), append(exact, parent)
	}
	before, err := planning.Overlay(s, exact...)
	if err != nil {
		return Report{}, err
	}
	after, err := planning.Overlay(s, append(exact, c)...)
	if err != nil {
		return Report{}, err
	}
	oldDir, cleanOld, err := instance(s, before)
	if err != nil {
		return Report{}, err
	}
	defer cleanOld()
	newDir, cleanNew, err := instance(s, after)
	if err != nil {
		return Report{}, err
	}
	defer cleanNew()
	report := Report{
		Baseline: s.Digest, Plan: p.Digest, Candidate: c.Digest, Parents: parents,
		Toolchain: backend.Identity(), State: "passed", Started: time.Now().UTC().Format(time.RFC3339Nano),
		Checks: make([]Check, 0),
	}
	for _, task := range batch.Tasks {
		files := []string{task.Path}
		if task.Kind == "php-lint" {
			files = nil
			for _, file := range s.Files {
				if filepath.Ext(file.Path) == ".php" {
					files = append(files, file.Path)
				}
			}
		}
		for _, file := range files {
			old := backend.Check(ctx, oldDir, task.Kind, file)
			new := backend.Check(ctx, newDir, task.Kind, file)
			state := classify(old.State, new.State)
			if state != "passed" && state != "fixed" {
				report.State = "blocked"
			}
			report.Checks = append(report.Checks, Check{
				Task: task.ID, Kind: task.Kind, File: file, Baseline: old, Candidate: new, State: state,
			})
		}
	}
	if len(report.Checks) == 0 {
		report.State = "blocked"
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return Report{}, err
	}
	report.Digest = snapshot.Digest(raw)
	return report, nil
}

func classify(old, new string) string {
	if new != "passed" && new != "failed" {
		return "candidate-" + new
	}
	if old != "passed" && old != "failed" {
		return "baseline-" + old
	}
	if new == "passed" {
		if old == "failed" {
			return "fixed"
		}
		return "passed"
	}
	if old == "failed" {
		return "existing-failure"
	}
	return "regression"
}

func GenerateDependencies(ctx context.Context, backend Backend, s snapshot.Snapshot, p planning.Plan) (planning.Candidate, error) {
	manifest, err := planning.DependencyManifest(s, p)
	if err != nil {
		return planning.Candidate{}, err
	}
	directory, cleanup, err := instance(s, map[string]string{"composer.json": manifest})
	if err != nil {
		return planning.Candidate{}, err
	}
	defer cleanup()
	lock, err := backend.Regenerate(ctx, directory)
	if err != nil {
		return planning.Candidate{}, err
	}
	return planning.DependencyCandidate(s, p, lock, backend.Identity())
}

func instance(s snapshot.Snapshot, files map[string]string) (string, func(), error) {
	privateDir, err := os.MkdirTemp("", "upgrade-validation-")
	if err != nil {
		return "", nil, err
	}
	clean := func() { _ = os.RemoveAll(privateDir) }
	location := filepath.Join(privateDir, "work")
	if err := os.Mkdir(location, 0o755); err != nil {
		clean()
		return "", nil, err
	}
	if err := s.Materialize(location, files); err != nil {
		clean()
		return "", nil, err
	}
	return location, clean, nil
}
