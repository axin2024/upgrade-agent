package composer

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type Catalog struct {
	Packages []Package         `json:"packages"`
	Platform map[string]string `json:"platform"`
}

type Blocker struct {
	Package    string `json:"package"`
	Constraint string `json:"constraint"`
	Found      string `json:"found,omitempty"`
	Reason     string `json:"reason"`
	Chain      []Edge `json:"chain"`
}

type Result struct {
	State    string    `json:"state"`
	Selected []Package `json:"selected"`
	Blockers []Blocker `json:"blockers"`
	Steps    int       `json:"steps"`
}

type needed struct {
	name, description string
	chain             []Edge
}

type candidate struct {
	entry Package
	ver   version
}

type resolver struct {
	available  map[string][]candidate
	machine    map[string]string
	conflict   map[string]string
	cache      map[string]constraint
	failures   map[string]Blocker
	stability  int
	steps      int
	incomplete bool
	virtual    map[string]bool
}

// Solve checks an explicitly supplied local catalog. Composer validation is a
// separate release gate for replacements, other repositories, and the lock.
func Solve(inventory Inventory, index Catalog, targets map[string]string) (Result, error) {
	if inventory.LockState != "captured" {
		return Result{State: "missing_lock"}, nil
	}
	if len(targets) == 0 || len(targets) > 32 || len(index.Packages) > 3_000 {
		return Result{}, fmt.Errorf("expected a target and at most 3000 catalog entries")
	}
	if err := checkDeclarations(targets); err != nil {
		return Result{}, err
	}
	engine := &resolver{
		available: make(map[string][]candidate), machine: index.Platform, conflict: inventory.Manifest.Conflict,
		cache: make(map[string]constraint), failures: make(map[string]Blocker), stability: 4, virtual: make(map[string]bool),
	}
	switch strings.ToLower(inventory.Manifest.Stability) {
	case "", "stable":
	case "rc":
		engine.stability = 3
	case "beta":
		engine.stability = 2
	case "alpha":
		engine.stability = 1
	case "dev":
		engine.stability = 0
	default:
		return Result{}, fmt.Errorf("unknown Composer minimum-stability")
	}
	if err := engine.index(inventory, index); err != nil {
		return Result{}, err
	}
	reqs := make(map[string]string)
	maps.Copy(reqs, inventory.Manifest.Require)
	var root []needed
	for _, name := range slices.Sorted(maps.Keys(inventory.Manifest.RequireDev)) {
		previous := inventory.Manifest.RequireDev[name]
		if _, prod := reqs[name]; prod {
			if _, changed := targets[name]; !changed {
				root = append(root, needed{name, previous, []Edge{{"root", name, previous, "require-dev"}}})
			}
		} else {
			reqs[name] = previous
		}
	}
	for name, target := range targets {
		if _, declared := reqs[name]; !declared {
			return Result{}, fmt.Errorf("target %q is not a direct requirement", name)
		}
		reqs[name] = target
	}
	root = append(root, follow("root", reqs, nil)...)
	solution, solved := engine.search(root, make(map[string]candidate), make(map[string][]needed))
	result := Result{State: "blocked", Steps: engine.steps, Selected: make([]Package, 0), Blockers: make([]Blocker, 0)}
	if solved {
		result.State = "candidate"
		for _, selected := range solution {
			result.Selected = append(result.Selected, selected.entry)
		}
		slices.SortFunc(result.Selected, func(a, b Package) int { return strings.Compare(a.Name, b.Name) })
		return result, nil
	}
	if engine.incomplete || engine.steps > 12_000 {
		result.State = "needs-composer"
	}
	for _, failure := range engine.failures {
		result.Blockers = append(result.Blockers, failure)
	}
	slices.SortFunc(result.Blockers, func(a, b Blocker) int {
		if length := len(a.Chain) - len(b.Chain); length != 0 {
			return length
		}
		return strings.Compare(a.Package+a.Constraint+a.Found+a.Reason, b.Package+b.Constraint+b.Found+b.Reason)
	})
	return result, nil
}

func (r *resolver) index(inventory Inventory, index Catalog) error {
	for name := range inventory.Manifest.Provide {
		r.virtual[name] = true
	}
	for name := range inventory.Manifest.Replace {
		r.virtual[name] = true
	}
	dedup := make(map[string]Package)
	for _, candidates := range [][]Package{inventory.Locked, index.Packages} {
		for _, pkg := range candidates {
			if !packageName.MatchString(pkg.Name) || len(pkg.Name) > 160 {
				return fmt.Errorf("invalid catalog package %q", pkg.Name)
			}
			for _, declarations := range []map[string]string{pkg.Require, pkg.Conflict, pkg.Provide, pkg.Replace} {
				if err := checkDeclarations(declarations); err != nil {
					return err
				}
			}
			dedup[pkg.Name+"@"+pkg.Version] = pkg
		}
	}
	for _, pkg := range dedup {
		v, err := readVersion(pkg.Version)
		if err != nil {
			return err
		}
		r.available[pkg.Name] = append(r.available[pkg.Name], candidate{entry: pkg, ver: v})
		for name := range pkg.Provide {
			r.virtual[name] = true
		}
		for name := range pkg.Replace {
			r.virtual[name] = true
		}
	}
	for _, versions := range r.available {
		slices.SortFunc(versions, func(a, b candidate) int {
			if inventory.Manifest.PreferStable && (a.ver.stage >= 4) != (b.ver.stage >= 4) {
				if a.ver.stage >= 4 {
					return -1
				}
				return 1
			}
			if order := compare(b.ver, a.ver); order != 0 {
				return order
			}
			return strings.Compare(a.entry.Version, b.entry.Version)
		})
	}
	return nil
}

func follow(from string, requirements map[string]string, prefix []Edge) []needed {
	keys := slices.Sorted(maps.Keys(requirements))
	out := make([]needed, 0, len(keys))
	for _, name := range keys {
		exp := requirements[name]
		path := append(slices.Clone(prefix), Edge{from, name, exp, "require"})
		out = append(out, needed{name, exp, path})
	}
	return out
}

func (r *resolver) condition(req needed) (constraint, bool) {
	if value, exists := r.cache[req.description]; exists {
		return value, true
	}
	value, err := parseConstraint(req.description)
	if err != nil {
		r.incomplete = true
		r.block(req, "", err.Error())
		return nil, false
	}
	r.cache[req.description] = value
	return value, true
}

func (r *resolver) block(req needed, actual, message string) {
	key := req.name + "\x00" + req.description + "\x00" + actual + "\x00" + message
	if len(req.chain) > 0 {
		key += "\x00" + req.chain[len(req.chain)-1].From
	}
	if prior, found := r.failures[key]; found {
		if len(prior.Chain) <= len(req.chain) {
			return
		}
	} else if len(r.failures) >= 64 {
		return
	}
	r.failures[key] = Blocker{req.name, req.description, actual, message, req.chain}
}

func (r *resolver) search(queue []needed, selected map[string]candidate, constraints map[string][]needed) (map[string]candidate, bool) {
	r.steps++
	if r.steps > 12_000 || len(queue) > 5_000 {
		r.incomplete = true
		return nil, false
	}
	if len(queue) == 0 {
		return selected, true
	}
	req, rest := queue[0], queue[1:]
	condition, known := r.condition(req)
	if !known {
		return nil, false
	}
	if platform.MatchString(req.name) {
		actual := r.machine[req.name]
		v, err := readVersion(actual)
		if err != nil || !condition.matches(v) {
			if actual == "" {
				r.incomplete = true
			}
			r.block(req, actual, "platform requirement is not satisfied")
			return nil, false
		}
		return r.search(rest, selected, constraints)
	}
	if present, exists := selected[req.name]; exists {
		if !condition.matches(present.ver) {
			r.block(req, present.entry.Version, "selected version fails dependency constraint")
			for _, prior := range constraints[req.name] {
				r.block(prior, present.entry.Version, "active selection competes with another dependency")
			}
			for _, pending := range rest {
				if pending.name != req.name {
					continue
				}
				if expected, known := r.condition(pending); known && !expected.matches(present.ver) {
					r.block(pending, present.entry.Version, "selected version fails dependency constraint")
				}
			}
			return nil, false
		}
		return r.search(rest, selected, constraints)
	}
	hit := false
	if r.virtual[req.name] {
		r.incomplete = true
	}
	for _, next := range r.available[req.name] {
		if !condition.matches(next.ver) || next.ver.stage < r.stability && next.ver.branch == "" ||
			next.ver.branch != "" && r.stability > 0 {
			continue
		}
		hit = true
		if !r.compatible(next, selected, req) {
			continue
		}
		nextChoices := maps.Clone(selected)
		nextRequirements := maps.Clone(constraints)
		nextChoices[req.name] = next
		nextRequirements[req.name] = []needed{req}
		pending := append(slices.Clone(rest), follow(req.name, next.entry.Require, req.chain)...)
		if solution, solved := r.search(pending, nextChoices, nextRequirements); solved {
			return solution, true
		}
		if r.steps > 12_000 {
			break
		}
	}
	if !hit {
		message := "no version in the approved catalog satisfies the constraint"
		if r.virtual[req.name] {
			message = "virtual provider or replacement requires Composer verification"
		}
		r.block(req, "", message)
	}
	return nil, false
}

func (r *resolver) compatible(next candidate, choices map[string]candidate, req needed) bool {
	if expression, found := r.conflict[next.entry.Name]; found && r.matchesConflict(next, req, expression, "root") {
		return false
	}
	for _, name := range slices.Sorted(maps.Keys(choices)) {
		other := choices[name]
		if expression, found := next.entry.Conflict[name]; found && r.matchesConflict(other, req, expression, next.entry.Name) {
			return false
		}
		if expression, found := other.entry.Conflict[next.entry.Name]; found && r.matchesConflict(next, req, expression, other.entry.Name) {
			return false
		}
		if next.entry.Replace[name] != "" || other.entry.Replace[next.entry.Name] != "" {
			r.incomplete = true
			r.block(req, other.entry.Version, "installed and replaced packages need the Composer solver")
			return false
		}
	}
	return true
}

func (r *resolver) matchesConflict(pkg candidate, req needed, expression, from string) bool {
	check := needed{pkg.entry.Name, expression, append(slices.Clone(req.chain), Edge{from, pkg.entry.Name, expression, "conflict"})}
	condition, known := r.condition(check)
	if !known {
		return true
	}
	if !condition.matches(pkg.ver) {
		return false
	}
	r.block(check, pkg.entry.Version, "package conflict prevents simultaneous installation")
	return true
}
