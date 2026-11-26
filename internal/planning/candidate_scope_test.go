package planning

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

func TestPatchRejectsConfigurationOutsideCallBatch(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "legacy-pricing")
	base, err := snapshot.Capture(root, "scope-fixture", []string{"app", "tests", "composer.json", "composer.lock"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "plan-options.json"))
	if err != nil {
		t.Fatal(err)
	}
	var options Input
	if err := json.Unmarshal(raw, &options); err != nil {
		t.Fatal(err)
	}
	plan, err := Build(base, options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := makeCandidate(base, plan, "rule-sale-price", "reviewer", "fixture", map[string]string{
		"composer.json": `{"name":"outside/patch"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "outside the approved batch: composer.json") || result.Digest != "" {
		t.Fatalf("unrelated config reached the patch: %+v, %v", result, err)
	}
}
