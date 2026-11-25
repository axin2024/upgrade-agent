// Package impact locates narrowly scoped PHP migration rules without running PHP.
package impact

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type Rule struct {
	ID        string   `json:"id"`
	OldType   string   `json:"old_type"`
	OldMethod string   `json:"old_method"`
	NewType   string   `json:"new_type"`
	NewMethod string   `json:"new_method"`
	Guide     string   `json:"guide"`
	Risk      string   `json:"risk"`
	Requires  []string `json:"requires_targets,omitempty"`
	Verify    []string `json:"verify,omitempty"`
}

type Finding struct {
	Rule        string   `json:"rule"`
	Path        string   `json:"path"`
	FileSHA256  string   `json:"file_sha256"`
	Line        int      `json:"line"`
	First       int      `json:"first_byte"`
	Last        int      `json:"last_byte"`
	Original    string   `json:"original"`
	Replacement string   `json:"replacement,omitempty"`
	Source      string   `json:"source"`
	Function    string   `json:"enclosing_function,omitempty"`
	State       string   `json:"state"`
	Guide       string   `json:"guide"`
	Risk        string   `json:"risk"`
	Verify      []string `json:"verify,omitempty"`
}

var className = regexp.MustCompile(`^\\?[a-zA-Z_][a-zA-Z_0-9]*(\\[a-zA-Z_][a-zA-Z_0-9]*)*$`)
var methodName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]*$`)
var ruleID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

func Scan(s snapshot.Snapshot, rules []Rule) ([]Finding, error) {
	if len(rules) > 100 {
		return nil, fmt.Errorf("at most 100 migration rules are supported")
	}
	byMethod := make(map[string][]Rule)
	seen := make(map[string]bool)
	for _, rule := range rules {
		guide, err := url.Parse(rule.Guide)
		if !ruleID.MatchString(rule.ID) || seen[rule.ID] || !className.MatchString(rule.OldType) ||
			!className.MatchString(rule.NewType) || !methodName.MatchString(rule.OldMethod) ||
			!methodName.MatchString(rule.NewMethod) || err != nil || guide.Scheme != "https" ||
			guide.Host == "" || guide.User != nil || len(rule.Guide) > 512 ||
			len(rule.Requires) > 32 || len(rule.Verify) > 6 ||
			slices.ContainsFunc(rule.Verify, func(item string) bool { return len(item) > 256 }) ||
			!slices.Contains([]string{"low", "medium", "high"}, rule.Risk) {
			return nil, fmt.Errorf("invalid migration rule %q", rule.ID)
		}
		seen[rule.ID] = true
		key := strings.ToLower(rule.OldMethod)
		byMethod[key] = append(byMethod[key], rule)
	}
	findings := make([]Finding, 0)
	for _, file := range s.Files {
		if !strings.HasSuffix(strings.ToLower(file.Path), ".php") {
			continue
		}
		findings = append(findings, inspect(file, byMethod)...)
		if len(findings) > 5_000 {
			return nil, fmt.Errorf("snapshot exceeds 5000 rule findings")
		}
	}
	return findings, nil
}

type token struct {
	value            string
	start, end, line int
}

func inspect(file snapshot.File, rules map[string][]Rule) []Finding {
	tokens, safe := lex(file.Content)
	var findings []Finding
	ns, depth, scope := "", 0, 0
	aliases := make(map[string]string)
	importsOK := safe
	functions := make(map[int]string)
	pendingFunction := ""
	for i, current := range tokens {
		if current.value == "{" {
			depth++
			if pendingFunction != "" {
				functions[depth], pendingFunction = pendingFunction, ""
			}
			continue
		}
		if current.value == "}" {
			delete(functions, depth)
			depth--
			if depth < scope {
				ns, scope, importsOK = "", 0, safe
				clear(aliases)
			}
			continue
		}
		if current.value == ";" {
			pendingFunction = ""
		}
		if strings.EqualFold(current.value, "function") && i+2 < len(tokens) &&
			methodName.MatchString(tokens[i+1].value) && tokens[i+2].value == "(" {
			pendingFunction = tokens[i+1].value
		}
		if depth == 0 && strings.EqualFold(current.value, "namespace") && i+2 < len(tokens) {
			next := tokens[i+1].value
			ns, importsOK = "", safe
			clear(aliases)
			if next == "{" {
				scope = 1
			} else if className.MatchString(next) {
				ns = strings.TrimPrefix(next, "\\")
				if tokens[i+2].value == "{" {
					scope = 1
				}
			} else {
				importsOK = false
			}
		}
		if depth == scope && strings.EqualFold(current.value, "use") && i+1 < len(tokens) && tokens[i+1].value != "(" {
			// An unsupported grouped import might shadow any unqualified class name.
			importsOK = readImport(tokens[i+1:], aliases) && importsOK
		}
		if i == 0 || i+2 >= len(tokens) || (current.value != "::" && current.value != "->" && current.value != "?->") ||
			tokens[i+2].value != "(" {
			continue
		}
		left, method := tokens[i-1], tokens[i+1]
		literalType := i < 2 || !slices.Contains([]string{"::", "->", "?->", "$"}, tokens[i-2].value)
		for _, rule := range rules[strings.ToLower(method.value)] {
			enclosing := ""
			for scope := depth; scope > 0 && enclosing == ""; scope-- {
				enclosing = functions[scope]
			}
			finding := Finding{Rule: rule.ID, Path: file.Path, FileSHA256: file.SHA256, Line: method.line,
				First: left.start, Last: method.end, Original: file.Content[left.start:method.end],
				Source: "php-lexer", Function: enclosing, State: "verify", Guide: rule.Guide, Risk: rule.Risk, Verify: rule.Verify}
			if safe && literalType && current.value == "::" && className.MatchString(left.value) &&
				strings.EqualFold(resolve(left.value, ns, aliases, importsOK), strings.TrimPrefix(rule.OldType, "\\")) &&
				strings.TrimSpace(file.Content[left.end:current.start]) == "" &&
				strings.TrimSpace(file.Content[current.end:method.start]) == "" {
				finding.Replacement = "\\" + strings.TrimPrefix(rule.NewType, "\\") +
					file.Content[left.end:current.end] + file.Content[current.end:method.start] + rule.NewMethod
				finding.State = "exact"
			}
			findings = append(findings, finding)
		}
	}
	return findings
}

func readImport(tokens []token, aliases map[string]string) bool {
	if strings.EqualFold(tokens[0].value, "function") || strings.EqualFold(tokens[0].value, "const") {
		return true
	}
	for len(tokens) >= 2 {
		target := strings.TrimPrefix(tokens[0].value, "\\")
		if !className.MatchString(target) {
			return false
		}
		parts := strings.Split(target, "\\")
		alias := parts[len(parts)-1]
		tokens = tokens[1:]
		if len(tokens) >= 3 && strings.EqualFold(tokens[0].value, "as") {
			alias, tokens = tokens[1].value, tokens[2:]
		}
		key := strings.ToLower(alias)
		if prior := aliases[key]; !methodName.MatchString(alias) || prior != "" && !strings.EqualFold(prior, target) {
			return false
		}
		aliases[key] = target
		if tokens[0].value == ";" {
			return true
		}
		if tokens[0].value != "," {
			return false
		}
		tokens = tokens[1:]
	}
	return false
}

func resolve(name, namespace string, aliases map[string]string, importsOK bool) string {
	if strings.HasPrefix(name, "\\") {
		return name[1:]
	}
	first, rest, qualified := strings.Cut(name, "\\")
	if !importsOK || slices.Contains([]string{"self", "parent", "static"}, strings.ToLower(first)) {
		return ""
	}
	if qualified && strings.EqualFold(first, "namespace") {
		name = rest
	} else if alias, ok := aliases[strings.ToLower(first)]; ok {
		return strings.TrimSuffix(alias+"\\"+rest, "\\")
	}
	if namespace == "" {
		return name
	}
	return namespace + "\\" + name
}

func lex(input string) ([]token, bool) {
	var tokens []token
	line := 1
	active := false
	for i := 0; i < len(input); {
		current := input[i]
		tail := input[i:]
		switch {
		case !active:
			if len(tail) > 5 && strings.EqualFold(tail[:5], "<?php") && strings.ContainsRune(" \t\r\n", rune(tail[5])) {
				i += 5
				active = true
			} else if strings.HasPrefix(tail, "<?=") {
				i += 3
				active = true
			} else if strings.HasPrefix(tail, "<?") {
				return tokens, false
			} else {
				if current == '\n' {
					line++
				}
				i++
			}
		case strings.HasPrefix(tail, "?>"):
			tokens = append(tokens, token{"(close-tag)", i, i + 2, line})
			i += 2
			active = false
		case current == ' ' || current == '\r' || current == '\t' || current == '\n':
			if current == '\n' {
				line++
			}
			i++
		case strings.HasPrefix(tail, "/*"):
			end := strings.Index(tail[2:], "*/")
			if end < 0 {
				return tokens, false
			}
			width := end + 4
			line += strings.Count(tail[:width], "\n")
			i += width
		case strings.HasPrefix(tail, "//") || current == '#' && !strings.HasPrefix(tail, "#["):
			nl, closeTag := strings.IndexByte(tail, '\n'), strings.Index(tail, "?>")
			if closeTag >= 0 && (nl < 0 || closeTag < nl) {
				i += closeTag
			} else if nl >= 0 {
				i += nl
			} else {
				return tokens, true
			}
		case current == '"' || current == '\'' || current == '\x60':
			end := quotedEnd(tail, current)
			if end == 0 {
				return tokens, false
			}
			tokens = append(tokens, token{"(literal)", i, i + end, line})
			line += strings.Count(tail[:end], "\n")
			i += end
		case strings.HasPrefix(tail, "<<<") || strings.HasPrefix(strings.ToLower(tail[:min(len(tail), 16)]), "__halt_compiler"):
			return tokens, false
		default:
			width := 1
			switch {
			case strings.HasPrefix(tail, "?->"):
				width = 3
			case strings.HasPrefix(tail, "::"), strings.HasPrefix(tail, "->"):
				width = 2
			case nameByte(current) || current == '$':
				for width < len(tail) && nameByte(tail[width]) {
					width++
				}
			}
			tokens = append(tokens, token{tail[:width], i, i + width, line})
			i += width
		}
	}
	return tokens, true
}

func nameByte(c byte) bool {
	return c >= 128 || c == '_' || c == '\\' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func quotedEnd(text string, delimiter byte) int {
	for i := 1; i < len(text); i++ {
		if text[i] == '\\' {
			i++
		} else if text[i] == delimiter {
			return i + 1
		} else if delimiter != '\'' && (strings.HasPrefix(text[i:], "{$") || strings.HasPrefix(text[i:], "${")) {
			// Double quoted PHP strings can contain nested expressions and quotes.
			return 0
		}
	}
	return 0
}
