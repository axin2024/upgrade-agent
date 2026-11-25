// Package composer reads Composer snapshots and explains bounded dependency resolutions.
package composer

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type version struct {
	parts     [4]int
	precision int
	stage     int
	stageNum  int
	branch    string
}

var release = regexp.MustCompile(`(?i)^v?([0-9]+(?:\.[0-9]+){0,3})(?:[.-]?(stable|patch|pl|p|rc|beta|b|alpha|a|dev)(?:[.-]?([0-9]+))?)?(?:\+[A-Za-z0-9.-]+)?$`)
var namedBranch = regexp.MustCompile(`(?i)^(?:dev-[A-Za-z0-9_./-]+|[v0-9x.*]+-dev)$`)
var operatorSpace = regexp.MustCompile(`(\^|~|>=|<=|>|<|==|=|!=|<>)\s+`)
var hyphenRange = regexp.MustCompile(`([^\s,]+)\s+-\s+([^\s,]+)`)

func readVersion(text string) (version, error) {
	match := release.FindStringSubmatch(text)
	if len(match) == 0 {
		if namedBranch.MatchString(text) {
			return version{branch: strings.ToLower(text)}, nil
		}
		return version{}, fmt.Errorf("unsupported version %q", text)
	}
	var v version
	for n, token := range strings.Split(match[1], ".") {
		part, err := strconv.Atoi(token)
		if err != nil || part > 1_000_000_000 {
			return version{}, fmt.Errorf("invalid version component %q", token)
		}
		v.parts[n] = part
		v.precision++
	}
	v.stage = 4
	switch strings.ToLower(match[2]) {
	case "dev":
		v.stage = 0
	case "a", "alpha":
		v.stage = 1
	case "b", "beta":
		v.stage = 2
	case "rc":
		v.stage = 3
	case "patch", "pl", "p":
		v.stage = 5
	}
	if match[3] != "" {
		suffix, err := strconv.Atoi(match[3])
		if err != nil {
			return version{}, fmt.Errorf("invalid stability counter %q", match[3])
		}
		v.stageNum = suffix
	}
	return v, nil
}

func compare(a, b version) int {
	if a.branch != "" || b.branch != "" {
		if a.branch == "" {
			return 1
		}
		if b.branch == "" {
			return -1
		}
		return strings.Compare(a.branch, b.branch)
	}
	for i := range a.parts {
		if n := cmp.Compare(a.parts[i], b.parts[i]); n != 0 {
			return n
		}
	}
	return cmp.Or(cmp.Compare(a.stage, b.stage), cmp.Compare(a.stageNum, b.stageNum))
}

func EqualVersion(left, right string) bool {
	a, leftErr := readVersion(left)
	b, rightErr := readVersion(right)
	return leftErr == nil && rightErr == nil && compare(a, b) == 0
}

type predicate struct {
	operator string
	want     version
}

type constraint [][]predicate

func (c constraint) matches(v version) bool {
	for _, all := range c {
		found := true
		for _, rule := range all {
			if (v.branch != "" || rule.want.branch != "") && rule.operator != "=" && rule.operator != "!=" {
				found = false
				break
			}
			n := compare(v, rule.want)
			if rule.operator == "=" && n != 0 || rule.operator == "!=" && n == 0 ||
				rule.operator == ">" && n <= 0 || rule.operator == ">=" && n < 0 ||
				rule.operator == "<" && n >= 0 || rule.operator == "<=" && n > 0 {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

func parseConstraint(text string) (constraint, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 512 {
		return nil, fmt.Errorf("empty or oversized version constraint")
	}
	text = strings.ReplaceAll(text, "||", "|")
	result := make(constraint, 0)
	for group := range strings.SplitSeq(text, "|") {
		group = operatorSpace.ReplaceAllString(group, "${1}")
		var failed error
		group = hyphenRange.ReplaceAllStringFunc(group, func(raw string) string {
			match := hyphenRange.FindStringSubmatch(raw)
			lo, err := readVersion(match[1])
			hi, hiErr := readVersion(match[2])
			if err != nil || hiErr != nil || lo.branch != "" || hi.branch != "" {
				failed = fmt.Errorf("unsupported Composer range %q", raw)
				return ""
			}
			if hi.precision >= 3 {
				return ">=" + match[1] + " <=" + match[2]
			}
			return ">=" + match[1] + " <" + upperText(hi, hi.precision-1)
		})
		if failed != nil {
			return nil, failed
		}
		tokens := strings.Fields(strings.ReplaceAll(group, ",", " "))
		if len(tokens) == 0 {
			return nil, fmt.Errorf("empty disjunction in %q", text)
		}
		predicates := make([]predicate, 0)
		for _, token := range tokens {
			if _, stability, found := strings.Cut(token, "@"); found {
				return nil, fmt.Errorf("stability override @%s requires Composer verification", stability)
			}
			items, err := parseTerm(token)
			if err != nil {
				return nil, err
			}
			predicates = append(predicates, items...)
		}
		result = append(result, predicates)
	}
	return result, nil
}

func upperText(v version, index int) string {
	top := upper(v, index)
	return fmt.Sprintf("%d.%d.%d.%d", top.parts[0], top.parts[1], top.parts[2], top.parts[3])
}

func upper(v version, index int) version {
	v.parts[index]++
	for n := index + 1; n < len(v.parts); n++ {
		v.parts[n] = 0
	}
	v.stage, v.stageNum = 0, 0
	return v
}

func parseTerm(term string) ([]predicate, error) {
	op, ver := "=", term
	for _, prefix := range []string{"<=", ">=", "!=", "<>", "==", "<", ">", "=", "^", "~"} {
		if rest, ok := strings.CutPrefix(term, prefix); ok {
			op, ver = prefix, rest
			break
		}
	}
	if op == "==" {
		op = "="
	}
	if op == "<>" {
		op = "!="
	}
	if ver == "*" || strings.EqualFold(ver, "x") {
		if op != "=" {
			return nil, fmt.Errorf("unsupported wildcard comparator %q", term)
		}
		return nil, nil
	}
	if !namedBranch.MatchString(ver) {
		isWildcard := func(s string) bool { return s == "*" || strings.EqualFold(s, "x") }
		parts := strings.Split(ver, ".")
		for i, value := range parts {
			if !isWildcard(value) {
				continue
			}
			if op != "=" || i < 1 || i > 3 {
				return nil, fmt.Errorf("unsupported wildcard comparator %q", term)
			}
			for _, remaining := range parts[i:] {
				if !isWildcard(remaining) {
					return nil, fmt.Errorf("unsupported wildcard %q", term)
				}
			}
			v, err := readVersion(strings.Join(parts[:i], "."))
			if err != nil {
				return nil, err
			}
			v.stage = 0
			return []predicate{{">=", v}, {"<", upper(v, i-1)}}, nil
		}
	}
	v, err := readVersion(ver)
	if err != nil {
		return nil, err
	}
	switch op {
	case "^", "~":
		if v.branch != "" {
			return nil, fmt.Errorf("unsupported branch range %q", term)
		}
		position := 0
		if op == "^" {
			for position < v.precision-1 && v.parts[position] == 0 {
				position++
			}
		} else if v.precision > 2 {
			position = v.precision - 2
		}
		return []predicate{{">=", v}, {"<", upper(v, position)}}, nil
	default:
		if v.branch != "" && op != "=" && op != "!=" {
			return nil, fmt.Errorf("unsupported branch comparison %q", term)
		}
		return []predicate{{op, v}}, nil
	}
}
