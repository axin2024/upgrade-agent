package planning

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/composer"
	"github.com/axin2024/upgrade-agent/internal/impact"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type Change struct {
	Path    string `json:"path"`
	Before  string `json:"before_sha256"`
	After   string `json:"after_sha256"`
	Content string `json:"content"`
}

type Candidate struct {
	Digest   string   `json:"sha256"`
	Baseline string   `json:"baseline"`
	Plan     string   `json:"plan"`
	Batch    string   `json:"batch"`
	Source   string   `json:"source"`
	Tool     string   `json:"tool"`
	Changes  []Change `json:"changes"`
}

func DependencyManifest(s snapshot.Snapshot, p Plan) (string, error) {
	batch, ok := p.Batch("dependencies")
	file, captured := s.Find("composer.json")
	if !ok || !captured || batch.State != "ready" || p.Baseline != s.Digest {
		return "", fmt.Errorf("dependency batch is unavailable for this baseline")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(file.Content), &root); err != nil {
		return "", err
	}
	for _, field := range []string{"require", "require-dev"} {
		value, ok := root[field]
		if !ok {
			continue
		}
		var requirements map[string]string
		if err := json.Unmarshal(value, &requirements); err != nil {
			return "", err
		}
		changed := false
		for name, target := range p.Targets {
			if have, ok := requirements[name]; ok && have != target {
				requirements[name], changed = target, true
			}
		}
		if changed {
			value, err := json.Marshal(requirements)
			if err != nil {
				return "", err
			}
			root[field] = value
		}
	}
	body, err := json.MarshalIndent(root, "", "    ")
	if err != nil {
		return "", err
	}
	return preserveNewlines(file.Content, string(body)+"\n"), nil
}

func DependencyCandidate(s snapshot.Snapshot, p Plan, lock, tool string) (Candidate, error) {
	manifest, err := DependencyManifest(s, p)
	if err != nil {
		return Candidate{}, err
	}
	var replacement composer.Lock
	if err := json.Unmarshal([]byte(lock), &replacement); err != nil || len(replacement.Hash) != 32 {
		return Candidate{}, fmt.Errorf("candidate requires a Composer-generated lock file")
	}
	version := make(map[string]string)
	for _, packages := range [][]composer.Package{replacement.Packages, replacement.Dev} {
		for _, pkg := range packages {
			if _, duplicate := version[pkg.Name]; duplicate {
				return Candidate{}, fmt.Errorf("Composer lock contains duplicate package %q", pkg.Name)
			}
			version[pkg.Name] = pkg.Version
		}
	}
	for _, selected := range p.Resolution.Selected {
		if !composer.EqualVersion(version[selected.Name], selected.Version) {
			return Candidate{}, fmt.Errorf("Composer selected an unreviewed version of %q", selected.Name)
		}
		delete(version, selected.Name)
	}
	if len(version) != 0 {
		return Candidate{}, fmt.Errorf("Composer selected unreviewed packages")
	}
	return makeCandidate(s, p, "dependencies", "composer", tool, map[string]string{
		"composer.json": manifest,
		"composer.lock": lock,
	})
}

// CallCandidate only replaces calls whose type and source bytes are known.
// An optional set of finding indices lets a reviewer or model select a subset.
func CallCandidate(s snapshot.Snapshot, p Plan, batchID, source, tool string, indices []int) (Candidate, error) {
	batch, ok := p.Batch(batchID)
	if !ok || batch.Kind != "calls" || batch.State != "ready" || p.Baseline != s.Digest {
		return Candidate{}, fmt.Errorf("call batch is unavailable for this baseline")
	}
	if !slices.Contains([]string{"rule", "model", "model-replay", "reviewer"}, source) {
		return Candidate{}, fmt.Errorf("invalid call candidate source")
	}
	chosen := make(map[int]bool)
	for _, index := range indices {
		if index < 0 || index >= len(batch.Findings) || chosen[index] {
			return Candidate{}, fmt.Errorf("invalid or duplicate finding index %d", index)
		}
		chosen[index] = true
	}
	byPath := make(map[string][]impact.Finding)
	for i, finding := range batch.Findings {
		if indices == nil || chosen[i] {
			byPath[finding.Path] = append(byPath[finding.Path], finding)
		}
	}
	changed := make(map[string]string)
	for target, edits := range byPath {
		file, ok := s.Find(target)
		if !ok {
			return Candidate{}, fmt.Errorf("missing baseline file %s", target)
		}
		slices.SortFunc(edits, func(a, b impact.Finding) int { return a.First - b.First })
		var result strings.Builder
		cursor := 0
		for _, edit := range edits {
			if edit.State != "exact" || edit.FileSHA256 != file.SHA256 || edit.First < cursor ||
				edit.Last <= edit.First || edit.Last > len(file.Content) ||
				file.Content[edit.First:edit.Last] != edit.Original {
				return Candidate{}, fmt.Errorf("overlapping or stale call in %s at line %d", target, edit.Line)
			}
			result.WriteString(file.Content[cursor:edit.First])
			result.WriteString(edit.Replacement)
			cursor = edit.Last
		}
		result.WriteString(file.Content[cursor:])
		changed[target] = result.String()
	}
	return makeCandidate(s, p, batchID, source, tool, changed)
}

func makeCandidate(s snapshot.Snapshot, p Plan, id, source, tool string, contents map[string]string) (Candidate, error) {
	batch, ok := p.Batch(id)
	if !ok || tool == "" || len(tool) > 160 {
		return Candidate{}, fmt.Errorf("missing batch or tool version")
	}
	c := Candidate{Baseline: s.Digest, Plan: p.Digest, Batch: id, Source: source, Tool: tool}
	for _, name := range slices.Sorted(maps.Keys(contents)) {
		file, ok := s.Find(name)
		content := contents[name]
		if !ok || !snapshot.Within(name, s.Roots) || !slices.Contains(batch.Files, name) ||
			len(content) > 1<<20 || strings.ContainsRune(content, '\x00') {
			return Candidate{}, fmt.Errorf("change is outside the approved batch: %s", name)
		}
		after := snapshot.Digest([]byte(content))
		if after != file.SHA256 {
			c.Changes = append(c.Changes, Change{Path: name, Before: file.SHA256, After: after, Content: content})
		}
	}
	if len(c.Changes) == 0 {
		return Candidate{}, fmt.Errorf("candidate has no changes")
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return Candidate{}, err
	}
	c.Digest = snapshot.Digest(encoded)
	return c, nil
}

// Overlay refuses intersecting changes instead of applying a candidate to stale bytes.
func Overlay(s snapshot.Snapshot, candidates ...Candidate) (map[string]string, error) {
	result := make(map[string]string)
	for _, candidate := range candidates {
		if candidate.Baseline != s.Digest {
			return nil, fmt.Errorf("candidate %s has a stale baseline", candidate.Digest)
		}
		for _, change := range candidate.Changes {
			file, found := s.Find(change.Path)
			previous, already := result[change.Path]
			if !found || change.Before != file.SHA256 || snapshot.Digest([]byte(change.Content)) != change.After ||
				already && previous != change.Content {
				return nil, fmt.Errorf("conflicting or stale change to %s", change.Path)
			}
			result[change.Path] = change.Content
		}
	}
	return result, nil
}

func preserveNewlines(before, after string) string {
	if strings.Contains(before, "\r\n") && !strings.Contains(strings.ReplaceAll(before, "\r\n", ""), "\n") {
		after = strings.ReplaceAll(after, "\n", "\r\n")
	}
	return after
}
