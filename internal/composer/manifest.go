package composer

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

var packageName = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)
var platform = regexp.MustCompile(`^(php(?:-64bit|-ipv6|-zts|-debug)?|hhvm|ext-[a-z0-9_-]+|lib-[a-z0-9_-]+|composer(?:-plugin-api|-runtime-api)?)$`)

type Manifest struct {
	Name         string                     `json:"name"`
	Require      map[string]string          `json:"require"`
	RequireDev   map[string]string          `json:"require-dev"`
	Conflict     map[string]string          `json:"conflict"`
	Replace      map[string]string          `json:"replace"`
	Provide      map[string]string          `json:"provide"`
	Stability    string                     `json:"minimum-stability"`
	PreferStable bool                       `json:"prefer-stable"`
	Scripts      map[string]json.RawMessage `json:"scripts"`
}

type Package struct {
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Require  map[string]string `json:"require,omitempty"`
	Conflict map[string]string `json:"conflict,omitempty"`
	Provide  map[string]string `json:"provide,omitempty"`
	Replace  map[string]string `json:"replace,omitempty"`
	Type     string            `json:"type,omitempty"`
}

type Lock struct {
	Hash     string    `json:"content-hash"`
	Packages []Package `json:"packages"`
	Dev      []Package `json:"packages-dev"`
}

type Edge struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Constraint string `json:"constraint"`
	Kind       string `json:"kind"`
}

type Inventory struct {
	Name          string            `json:"name"`
	Manifest      Manifest          `json:"manifest"`
	LockSHA       string            `json:"lock_sha256,omitempty"`
	LockState     string            `json:"lock_state"`
	Declarations  []Edge            `json:"constraints"`
	Locked        []Package         `json:"locked"`
	Platform      map[string]string `json:"platform"`
	ToolchainGaps []string          `json:"toolchain_gaps"`
}

func Read(s snapshot.Snapshot, toolchain map[string]string) (Inventory, error) {
	manifestFile, ok := s.Find("composer.json")
	if !ok {
		return Inventory{}, fmt.Errorf("include composer.json in the approved snapshot")
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(manifestFile.Content), &manifest); err != nil {
		return Inventory{}, fmt.Errorf("parse composer.json: %w", err)
	}
	for _, declarations := range []map[string]string{manifest.Require, manifest.RequireDev, manifest.Conflict, manifest.Provide, manifest.Replace} {
		if err := checkDeclarations(declarations); err != nil {
			return Inventory{}, err
		}
	}
	result := Inventory{Name: manifest.Name, Manifest: manifest, LockState: "missing", Platform: toolchain,
		Locked: make([]Package, 0), Declarations: make([]Edge, 0), ToolchainGaps: make([]string, 0)}
	result.Declarations = appendEdges(result.Declarations, "root", manifest.Require, "require")
	result.Declarations = appendEdges(result.Declarations, "root", manifest.RequireDev, "require-dev")
	result.Declarations = appendEdges(result.Declarations, "root", manifest.Conflict, "conflict")
	result.Declarations = appendEdges(result.Declarations, "root", manifest.Provide, "provide")
	result.Declarations = appendEdges(result.Declarations, "root", manifest.Replace, "replace")
	if lockFile, ok := s.Find("composer.lock"); ok {
		var locked Lock
		if err := json.Unmarshal([]byte(lockFile.Content), &locked); err != nil {
			return Inventory{}, fmt.Errorf("parse composer.lock: %w", err)
		}
		if len(locked.Packages)+len(locked.Dev) > 1_000 {
			return Inventory{}, fmt.Errorf("lock has more than 1000 packages")
		}
		result.LockState, result.LockSHA = "captured", lockFile.SHA256
		seen := make(map[string]bool)
		for _, list := range [][]Package{locked.Packages, locked.Dev} {
			for _, pkg := range list {
				if !packageName.MatchString(pkg.Name) || pkg.Version == "" || seen[pkg.Name] {
					return Inventory{}, fmt.Errorf("invalid or duplicate Composer lock entry %q", pkg.Name)
				}
				seen[pkg.Name] = true
				for _, declarations := range []map[string]string{pkg.Require, pkg.Conflict, pkg.Provide, pkg.Replace} {
					if err := checkDeclarations(declarations); err != nil {
						return Inventory{}, err
					}
				}
				result.Locked = append(result.Locked, pkg)
				result.Declarations = appendEdges(result.Declarations, pkg.Name, pkg.Require, "require")
				result.Declarations = appendEdges(result.Declarations, pkg.Name, pkg.Conflict, "conflict")
				result.Declarations = appendEdges(result.Declarations, pkg.Name, pkg.Provide, "provide")
				result.Declarations = appendEdges(result.Declarations, pkg.Name, pkg.Replace, "replace")
			}
		}
	}
	slices.SortFunc(result.Locked, func(a, b Package) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(result.Declarations, func(a, b Edge) int {
		return strings.Compare(a.From+"\x00"+a.To+"\x00"+a.Kind, b.From+"\x00"+b.To+"\x00"+b.Kind)
	})
	for _, edge := range result.Declarations {
		if !platform.MatchString(edge.To) || (edge.Kind != "require" && edge.Kind != "require-dev") {
			continue
		}
		actual := toolchain[edge.To]
		c, err := parseConstraint(edge.Constraint)
		v, versionErr := readVersion(actual)
		if actual == "" || err != nil || versionErr != nil || !c.matches(v) {
			result.ToolchainGaps = append(result.ToolchainGaps,
				fmt.Sprintf("%s requires %s %s, observed %q", edge.From, edge.To, edge.Constraint, actual))
		}
	}
	return result, nil
}

func checkDeclarations(values map[string]string) error {
	if len(values) > 500 {
		return fmt.Errorf("too many package declarations")
	}
	for name, expression := range values {
		if (!packageName.MatchString(name) && !platform.MatchString(name)) || len(name) > 160 || len(expression) < 1 || len(expression) > 512 {
			return fmt.Errorf("invalid package or constraint %q", name)
		}
	}
	return nil
}

func appendEdges(edges []Edge, from string, requirements map[string]string, kind string) []Edge {
	for pkg, expression := range requirements {
		edges = append(edges, Edge{From: from, To: pkg, Constraint: expression, Kind: kind})
	}
	return edges
}

func (i Inventory) ShortestChain(target string) []Edge {
	type step struct {
		name string
		path []Edge
	}
	queue := []step{{name: "root"}}
	seen := map[string]bool{"root": true}
	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		for _, edge := range i.Declarations {
			if edge.From != head.name || edge.Kind != "require" && edge.Kind != "require-dev" || seen[edge.To] {
				continue
			}
			chain := append(slices.Clone(head.path), edge)
			if edge.To == target {
				return chain
			}
			seen[edge.To] = true
			queue = append(queue, step{edge.To, chain})
		}
	}
	return nil
}
