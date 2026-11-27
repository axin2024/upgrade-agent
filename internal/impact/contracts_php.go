package impact

import (
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

func indexPHPContracts(c *ContractCatalog, file snapshot.File) {
	tokens, safe := lex(file.Content)
	if !safe {
		c.uncertain("", "PHP source uses unsupported syntax", ContractSite{Path: file.Path, SHA256: file.SHA256})
		return
	}
	location := func(first, last token) ContractSite {
		return ContractSite{Path: file.Path, Line: first.line, First: first.start, Last: last.end, SHA256: file.SHA256}
	}
	ns, owner := "", ""
	depth, nsDepth, classDepth := 0, 0, 0
	classAliases, funcAliases := map[string]string{}, map[string]string{}
	classImportsOK, funcImportsOK := true, true
	declarations := map[int]bool{}
	for i, item := range tokens {
		value := strings.ToLower(item.value)
		switch value {
		case "{":
			depth++
		case "}":
			depth--
			if classDepth > 0 && depth < classDepth {
				owner, classDepth = "", 0
			}
			if depth < nsDepth {
				ns, nsDepth = "", 0
				clear(classAliases)
				clear(funcAliases)
				classImportsOK, funcImportsOK = true, true
			}
		case "namespace":
			if depth == 0 && i+2 < len(tokens) {
				next := tokens[i+1].value
				switch {
				case next == "{":
					ns, nsDepth = "", 1
				case className.MatchString(next) && (tokens[i+2].value == "{" || tokens[i+2].value == ";"):
					ns, nsDepth = strings.TrimPrefix(next, "\\"), 0
					if tokens[i+2].value == "{" {
						nsDepth = 1
					}
				default:
					continue
				}
				clear(classAliases)
				clear(funcAliases)
				classImportsOK, funcImportsOK = true, true
			}
		case "use":
			if depth == nsDepth && i+1 < len(tokens) && tokens[i+1].value != "(" {
				switch strings.ToLower(tokens[i+1].value) {
				case "function":
					funcImportsOK = i+2 < len(tokens) && readImport(tokens[i+2:], funcAliases) && funcImportsOK
				case "const":
				default:
					classImportsOK = readImport(tokens[i+1:], classAliases) && classImportsOK
				}
			}
		case "class", "interface", "trait", "enum":
			if depth == nsDepth && i+1 < len(tokens) && methodName.MatchString(tokens[i+1].value) &&
				(i == 0 || tokens[i-1].value != "::" && !strings.EqualFold(tokens[i-1].value, "new")) {
				owner, classDepth = resolve(tokens[i+1].value, ns, nil, true), depth+1
			}
		case "function":
			name := i + 1
			if name < len(tokens) && tokens[name].value == "&" {
				name++
			}
			if name+1 >= len(tokens) || !methodName.MatchString(tokens[name].value) || tokens[name+1].value != "(" {
				continue
			}
			end := phpSignatureEnd(tokens, name+1)
			if end <= name {
				continue
			}
			for j := name; j <= end; j++ {
				declarations[j] = true
			}
			if depth == nsDepth || owner != "" && depth == classDepth && phpPublic(tokens[:i]) {
				target := resolve(tokens[name].value, ns, nil, true)
				if depth == classDepth && owner != "" {
					target = owner + "::" + tokens[name].value
				}
				var signature strings.Builder
				for _, part := range tokens[name+1 : end+1] {
					signature.WriteString(file.Content[part.start:part.end])
					signature.WriteByte(' ')
				}
				c.declare("php:"+strings.ToLower(target), strings.TrimSpace(signature.String()), location(tokens[name], tokens[end]))
			}
		}
		if declarations[i] || i == 0 || i+1 >= len(tokens) || tokens[i+1].value != "(" || phpCallKeyword(value) {
			continue
		}
		prev := tokens[i-1]
		switch {
		case prev.value == "::" || prev.value == "->" || prev.value == "?->":
			if i < 2 || !methodName.MatchString(value) && !strings.HasPrefix(value, "$") {
				continue
			}
			left := tokens[i-2]
			at := location(left, item)
			literal := i < 3 || !slices.Contains([]string{"::", "->", "?->", "$"}, tokens[i-3].value)
			if prev.value == "::" && className.MatchString(left.value) && methodName.MatchString(value) && literal {
				if target := resolve(left.value, ns, classAliases, classImportsOK); target != "" {
					c.call("php:"+strings.ToLower(target)+"::"+value, at)
					continue
				}
			}
			c.uncertain(value, "dynamic receiver, method or unresolved import", at)
		case strings.HasPrefix(value, "$"):
			c.uncertain(value, "variable function", location(item, item))
		case slices.Contains([]string{"call_user_func", "call_user_func_array", "forward_static_call", "forward_static_call_array"}, strings.TrimPrefix(value, "\\")):
			c.uncertain(value, "dynamic callback", location(item, item))
		case className.MatchString(value) && !strings.EqualFold(prev.value, "new"):
			if strings.HasPrefix(value, "\\") || strings.Contains(value, "\\") || funcImportsOK && funcAliases[value] != "" {
				if target := resolve(value, ns, funcAliases, funcImportsOK); target != "" {
					c.call("php:"+strings.ToLower(target), location(item, item))
				}
			}
		}
	}
}

func phpPublic(prefix []token) bool {
	for i := len(prefix) - 1; i >= 0; i-- {
		switch strings.ToLower(prefix[i].value) {
		case "private", "protected":
			return false
		case "public", "static", "abstract", "final":
		default:
			return true
		}
	}
	return true
}

func phpSignatureEnd(tokens []token, start int) int {
	nested := 0
	for i := start; i < len(tokens); i++ {
		switch tokens[i].value {
		case "(":
			nested++
		case ")":
			nested--
		case "{", ";", "(close-tag)":
			if nested == 0 {
				return i - 1
			}
		}
	}
	return -1
}

func phpCallKeyword(value string) bool {
	switch value {
	case "if", "for", "foreach", "while", "switch", "catch", "match", "function", "fn", "return", "array", "isset", "empty", "unset", "echo", "print", "include", "require":
		return true
	default:
		return false
	}
}
