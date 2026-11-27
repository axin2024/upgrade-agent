package impact

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

type ContractSymbol struct {
	ID        string          `json:"symbol_id"`
	Target    string          `json:"target"`
	Signature string          `json:"signature,omitempty"`
	Required  []string        `json:"required_response_fields,omitempty"`
	Example   json.RawMessage `json:"response_example,omitempty"`
	Location  ContractSite    `json:"location"`
}

type ContractSite struct {
	Path   string `json:"path"`
	Line   int    `json:"line,omitempty"`
	First  int    `json:"first_byte,omitempty"`
	Last   int    `json:"last_byte,omitempty"`
	SHA256 string `json:"file_sha256"`
}

type ContractCall struct {
	Target     string          `json:"target,omitempty"`
	Confidence string          `json:"confidence"`
	Reason     string          `json:"reason,omitempty"`
	Fields     []string        `json:"response_fields,omitempty"`
	Example    json.RawMessage `json:"response_example,omitempty"`
	Location   ContractSite    `json:"location"`
}

type ContractCatalog struct {
	Snapshot   string           `json:"snapshot_id"`
	Symbols    []ContractSymbol `json:"symbols"`
	Calls      []ContractCall   `json:"calls"`
	Unresolved int              `json:"unresolved"`
}

type ContractConsumer struct {
	Repository string          `json:"repository"`
	Catalog    ContractCatalog `json:"catalog"`
}

type ContractEdge struct {
	Repository string       `json:"repository"`
	Call       ContractCall `json:"callsite"`
}

type ContractChange struct {
	Previous      ContractSymbol  `json:"previous"`
	Current       *ContractSymbol `json:"current,omitempty"`
	RemovedFields []string        `json:"removed_fields,omitempty"`
	ConsumerEdges []ContractEdge  `json:"consumer_edges"`
}

type ContractImpact struct {
	Changes       []ContractChange `json:"changes"`
	ConsumerCount int              `json:"consumer_count"`
	Unresolved    int              `json:"unresolved"`
}

func IndexContracts(repo snapshot.Snapshot) (ContractCatalog, error) {
	if err := repo.Verify(); err != nil {
		return ContractCatalog{}, err
	}
	catalog := ContractCatalog{Snapshot: repo.Digest, Symbols: []ContractSymbol{}, Calls: []ContractCall{}}
	module := goModule(repo)
	for _, file := range repo.Files {
		switch {
		case strings.HasSuffix(file.Path, ".php"):
			indexPHPContracts(&catalog, file)
		case strings.HasSuffix(file.Path, ".go") && !strings.HasSuffix(file.Path, "_test.go"):
			indexGoContracts(&catalog, file, module)
		case strings.HasPrefix(file.Path, "contracts/") && strings.HasSuffix(file.Path, ".http.json"):
			if err := indexHTTPContract(&catalog, repo, file); err != nil {
				return ContractCatalog{}, fmt.Errorf("%s: %w", file.Path, err)
			}
		}
		if len(catalog.Symbols) > 50_000 || len(catalog.Calls) > 100_000 {
			return ContractCatalog{}, fmt.Errorf("repository exceeds the contract index limit")
		}
	}
	return catalog, nil
}

func (c *ContractCatalog) declare(target, signature string, location ContractSite) int {
	id := snapshot.Digest([]byte(c.Snapshot + "\x00" + target + "\x00" + location.Path))
	c.Symbols = append(c.Symbols, ContractSymbol{ID: id, Target: target, Signature: signature, Location: location})
	return len(c.Symbols) - 1
}

func (c *ContractCatalog) uncertain(target, reason string, at ContractSite) {
	c.Unresolved++
	c.Calls = append(c.Calls, ContractCall{Target: target, Confidence: "unknown", Reason: reason, Location: at})
}

func (c *ContractCatalog) call(target string, at ContractSite) {
	c.Calls = append(c.Calls, ContractCall{Target: target, Confidence: "static", Location: at})
}

// CompareContracts expects consumers explicitly pinned to this producer by a campaign.
func CompareContracts(previous, current ContractCatalog, consumers []ContractConsumer) (ContractImpact, error) {
	latest := make(map[string]*ContractSymbol, len(current.Symbols))
	for index := range current.Symbols {
		symbol := &current.Symbols[index]
		if _, duplicate := latest[symbol.Target]; duplicate {
			return ContractImpact{}, fmt.Errorf("duplicate new contract %s", symbol.Target)
		}
		latest[symbol.Target] = symbol
	}
	result := ContractImpact{Changes: []ContractChange{}, ConsumerCount: len(consumers)}
	changed := make(map[string]int)
	seen := make(map[string]bool, len(previous.Symbols))
	for _, old := range previous.Symbols {
		if seen[old.Target] {
			return ContractImpact{}, fmt.Errorf("duplicate previous contract %s", old.Target)
		}
		seen[old.Target] = true
		next := latest[old.Target]
		var removed []string
		for _, field := range old.Required {
			if next == nil || !slices.Contains(next.Required, field) {
				removed = append(removed, field)
			}
		}
		if next == nil || next.Signature != old.Signature || len(removed) > 0 {
			changed[old.Target] = len(result.Changes)
			result.Changes = append(result.Changes, ContractChange{Previous: old, Current: next, RemovedFields: removed, ConsumerEdges: []ContractEdge{}})
		}
	}
	for _, consumer := range consumers {
		result.Unresolved += consumer.Catalog.Unresolved
		for _, call := range consumer.Catalog.Calls {
			idx, ok := changed[call.Target]
			if !ok || call.Confidence == "unknown" {
				continue
			}
			change := &result.Changes[idx]
			if change.Current != nil && change.Current.Signature == change.Previous.Signature && len(call.Fields) > 0 &&
				!slices.ContainsFunc(call.Fields, func(field string) bool { return slices.Contains(change.RemovedFields, field) }) {
				continue
			}
			change.ConsumerEdges = append(change.ConsumerEdges, ContractEdge{Repository: consumer.Repository, Call: call})
		}
	}
	return result, nil
}

type httpContract struct {
	Kind     string          `json:"kind"`
	Method   string          `json:"method"`
	Route    string          `json:"path"`
	Callsite string          `json:"callsite,omitempty"`
	Line     int             `json:"line,omitempty"`
	Required []string        `json:"required_response_fields"`
	Example  json.RawMessage `json:"response_example"`
}

var contractRoute = regexp.MustCompile(`^/[A-Za-z0-9_./{}-]*$`)
var contractField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

func indexHTTPContract(c *ContractCatalog, repo snapshot.Snapshot, file snapshot.File) error {
	var entry httpContract
	decoder := json.NewDecoder(strings.NewReader(file.Content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one HTTP contract object")
	}
	if !slices.Contains([]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}, entry.Method) ||
		!contractRoute.MatchString(entry.Route) || path.Clean(entry.Route) != entry.Route || len(entry.Route) > 240 {
		return fmt.Errorf("invalid HTTP method or path")
	}
	var example map[string]any
	if len(entry.Example) > 16_384 || json.Unmarshal(entry.Example, &example) != nil || example == nil || len(entry.Required) > 128 {
		return fmt.Errorf("expected a bounded response example and field list")
	}
	slices.Sort(entry.Required)
	for _, field := range entry.Required {
		if !contractField.MatchString(field) || !hasExampleField(example, field) {
			return fmt.Errorf("invalid or missing response field %q", field)
		}
	}
	entry.Required = slices.Compact(entry.Required)
	target := "http:" + entry.Method + " " + entry.Route
	at := ContractSite{Path: file.Path, SHA256: file.SHA256}
	switch entry.Kind {
	case "provider":
		if entry.Callsite != "" || entry.Line != 0 {
			return fmt.Errorf("provider examples cannot specify a consumer callsite")
		}
		idx := c.declare(target, target, at)
		c.Symbols[idx].Required, c.Symbols[idx].Example = entry.Required, entry.Example
	case "consumer":
		source, found := repo.Find(entry.Callsite)
		if !found || entry.Line < 1 || entry.Line > strings.Count(source.Content, "\n")+1 {
			return fmt.Errorf("consumer example needs a valid callsite and line")
		}
		c.Calls = append(c.Calls, ContractCall{Target: target, Confidence: "example", Fields: entry.Required, Example: entry.Example,
			Location: ContractSite{Path: entry.Callsite, Line: entry.Line, SHA256: source.SHA256}, Reason: file.Path})
	default:
		return fmt.Errorf("expected provider or consumer HTTP contract")
	}
	return nil
}

func hasExampleField(example map[string]any, field string) bool {
	if _, ok := example[field]; ok {
		return true
	}
	key, remainder, nested := strings.Cut(field, ".")
	child, ok := example[key].(map[string]any)
	return nested && ok && hasExampleField(child, remainder)
}
