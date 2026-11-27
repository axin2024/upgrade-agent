package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

func TestMigrationCampaign(t *testing.T) {
	dsn := os.Getenv("UPGRADE_TEST_ESTATE_DSN")
	php := os.Getenv("UPGRADE_TEST_PHP")
	if dsn == "" || php == "" {
		t.Skip("requires a disposable estate schema and PHP")
	}
	ctx := t.Context()
	must(t, Migrate(dsn, 0))
	store, err := Open(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = store.Close() })
	must(t, store.Ready(ctx))
	identity := func(tenant, actor, role string) Identity {
		grant, err := store.Provision(ctx, tenant, "campaign", actor, role, time.Hour)
		must(t, err)
		value, err := store.Identify(ctx, grant.Token)
		must(t, err)
		return value
	}
	maintainer := identity("test-estate", "platform-owner", "maintainer")
	reviewer := identity("test-estate", "patch-reviewer", "reviewer")
	verifier := identity("test-estate", "test-verifier", "verifier")
	outsider := identity("outside-test-estate", "platform-owner", "admin")
	runner := "isolated-local"
	const previous = `<?php
namespace Retail;
class Quotes {
    public static function legacy(): array { return ['minor' => 1200]; }
}
`
	const compatibility = `<?php
namespace Retail;
class Quotes {
    public static function legacy(): array { return ['minor' => 1200]; }
    public static function current(): array { return ['amount_minor' => 1200]; }
}
`
	const final = `<?php
namespace Retail;
class Quotes {
    public static function current(): array { return ['amount_minor' => 1200]; }
}
`
	providerFiles := func(lib string, old, fresh bool) map[string]string {
		service := "<?php\nrequire __DIR__ . '/quotes.php';\nheader('Content-Type: application/json');\nswitch ($_SERVER['REQUEST_URI']) {\n"
		test := "<?php\nrequire __DIR__ . '/quotes.php';\n"
		files := map[string]string{"src/quotes.php": lib}
		if old {
			service += "case '/v1/quote': echo json_encode(\\Retail\\Quotes::legacy()); break;\n"
			test += "if (\\Retail\\Quotes::legacy()['minor'] !== 1200) exit(1);\n"
			files["contracts/quote-v1.http.json"] = contractFixture("provider", "/v1/quote", "minor", "")
		}
		if fresh {
			service += "case '/v2/quote': echo json_encode(\\Retail\\Quotes::current()); break;\n"
			test += "if (\\Retail\\Quotes::current()['amount_minor'] !== 1200) exit(1);\n"
			files["contracts/quote-v2.http.json"] = contractFixture("provider", "/v2/quote", "amount_minor", "")
		}
		service += "default: http_response_code(404); echo '{}';\n}\n"
		files["src/router.php"], files["src/test.php"] = service, test
		return files
	}
	phpFiles := func(route, field, method string) map[string]string {
		return map[string]string{
			"src/client.php": "<?php\nuse Retail\\Quotes as SharedQuotes;\nfunction localExample() { return SharedQuotes::" + method + "(); }\n" +
				"function remoteQuote() { return json_decode(file_get_contents(getenv('UPGRADE_PROVIDER_URL') . '" + route + "'), true, 512, JSON_THROW_ON_ERROR); }\n" +
				"function dynamicLookup(object $service) { return $service->subscribe(); }\n",
			"src/test.php":                 "<?php\nrequire __DIR__.'/client.php';\n$value=remoteQuote();\nif (($value['" + field + "'] ?? null) !== 1200) { echo json_encode($value); exit(1); }\n",
			"contracts/consumes.http.json": contractFixture("consumer", route, field, "src/client.php"),
		}
	}
	goFiles := func(route, field string) map[string]string {
		return map[string]string{
			"go.mod": "module example.test/quotes-client\n\ngo 1.26.0\n",
			"src/client.go": `package quotes
import ("encoding/json"; "net/http"; "os")
func Fetch() (map[string]int, error) {
  response, err := http.Get(os.Getenv("UPGRADE_PROVIDER_URL") + "` + route + `")
  if err != nil { return nil, err }; defer response.Body.Close()
  var result map[string]int
  err = json.NewDecoder(response.Body).Decode(&result)
  return result, err
}
`,
			"src/client_test.go": "package quotes\nimport \"testing\"\nfunc TestClient(t *testing.T) {\n" +
				"v, err := Fetch(); if err != nil || v[\"" + field + "\"] != 1200 { t.Fatalf(\"quote: %v, %v\", v, err) }\n}\n",
			"contracts/consumes.http.json": contractFixture("consumer", route, field, "src/client.go"),
		}
	}
	var req int
	key := func() string { req++; return fmt.Sprintf("estate-acceptance-%d", req) }
	register := func(origin, version string, roots []string) EstateRepository {
		repo, err := store.RegisterEstateRepository(ctx, maintainer, key(), EstateRegistration{Package: "example/quotes", Origin: "https://example.test/" + origin, Version: version})
		must(t, err)
		if repo.State != "pending" || repo.Snapshot != "" {
			t.Fatalf("unapproved repository: %+v", repo)
		}
		expect(t, store.CheckEstateSnapshotUpload(ctx, maintainer, repo.ID), ErrDenied)
		expect(t, store.CheckEstateSnapshotUpload(ctx, outsider, repo.ID), ErrMissing)
		repo, err = store.SetEstatePolicy(ctx, maintainer, key(), repo.ID, EstatePolicyInput{Expected: repo.Revision,
			EstatePolicy: EstatePolicy{Index: roots, Model: []string{"src"}, Export: roots, Runners: []string{runner}}})
		must(t, err)
		return repo
	}
	pin := func(repo EstateRepository, version string, files map[string]string, stage bool) (EstateRepository, snapshot.Snapshot) {
		source := captureEstateFixture(t, version, repo.Policy.Index, files)
		out, err := store.PinEstateSnapshot(ctx, maintainer, key(), repo.ID, EstateSnapshotInput{Expected: repo.Revision, Version: version, Snapshot: source, Stage: stage})
		must(t, err)
		return out, source
	}
	provider := register("provider", "1.0.0", []string{"src", "contracts"})
	phpClient := register("php-consumer", "1.4.0", []string{"src", "contracts"})
	goClient := register("go-consumer", "3.0.0", []string{"src", "contracts", "go.mod"})
	if provider.ID == phpClient.ID || phpClient.ID == goClient.ID || provider.Package != phpClient.Package || provider.Version == phpClient.Version {
		t.Fatalf("A1 origin/version isolation failed: %+v / %+v / %+v", provider, phpClient, goClient)
	}
	provider, old := pin(provider, "1.0.0", providerFiles(previous, true, false), false)
	_, bridge := pin(provider, "1.1.0", providerFiles(compatibility, true, true), true)
	_, next := pin(provider, "2.0.0", providerFiles(final, false, true), true)
	phpClient, phpOld := pin(phpClient, "1.4.0", phpFiles("/v1/quote", "minor", "legacy"), false)
	_, phpNew := pin(phpClient, "2.0.0", phpFiles("/v2/quote", "amount_minor", "current"), true)
	goClient, goOld := pin(goClient, "3.0.0", goFiles("/v1/quote", "minor"), false)
	_, goNew := pin(goClient, "3.1.0", goFiles("/v2/quote", "amount_minor"), true)
	listed, err := store.EstateRepositories(ctx, verifier, "")
	must(t, err)
	if len(listed) != 3 {
		t.Fatalf("expected exactly three repository registrations: %+v", listed)
	}
	impact, err := store.EstateImpact(ctx, maintainer, EstateImpactInput{Producer: provider.ID, Previous: old.Digest, Current: next.Digest,
		Consumers: []EstatePin{{phpClient.ID, phpOld.Digest}, {goClient.ID, goOld.Digest}}})
	must(t, err)
	var httpEdges int
	for _, changed := range impact.Changes {
		if changed.Previous.Target == "http:GET /v1/quote" {
			httpEdges = len(changed.ConsumerEdges)
			if len(changed.RemovedFields) != 1 || changed.RemovedFields[0] != "minor" || changed.ConsumerEdges[0].Call.Location.SHA256 == "" {
				t.Fatalf("A3 missing field/call evidence: %+v", changed)
			}
		}
	}
	if httpEdges != 2 || impact.Unresolved < 1 {
		t.Fatalf("A3/A4 HTTP links=%d unresolved=%d", httpEdges, impact.Unresolved)
	}
	campaign, err := store.CreateMigrationCampaign(ctx, maintainer, key(), CampaignInput{Name: "Retail quote migration", Producer: CampaignRepository{provider.ID, old.Digest, next.Digest, "src/test.php", "php"}, ProviderRoute: "src/router.php", Compatibility: bridge.Digest,
		Consumers:     []CampaignRepository{{phpClient.ID, phpOld.Digest, phpNew.Digest, "src/test.php", "php"}, {goClient.ID, goOld.Digest, goNew.Digest, "src/client_test.go", "go"}},
		LegacyTargets: []string{"http:GET /v1/quote", "php:retail\\quotes::legacy"}, ContractSet: "quote-v1-v2", Runner: runner, Strategy: "dual-version"})
	must(t, err)
	_, err = store.MigrationCampaign(ctx, outsider, campaign.ID)
	expect(t, err, ErrMissing)
	campaign, err = store.FreezeMigrationCampaign(ctx, maintainer, key(), campaign.ID, campaign.Revision)
	must(t, err)
	if len(campaign.Waves) != 3 || campaign.Waves[0].Changes[0].Target != bridge.Digest || len(campaign.Waves[1].Changes) != 2 ||
		campaign.Waves[2].Changes[0].Target != next.Digest || campaign.Choices[2].Available {
		t.Fatalf("A5 invalid release order or one-cut availability: %+v", campaign.Waves)
	}
	err = checkLegacyCalls(ctx, store.db, maintainer, campaign)
	expect(t, err, ErrConflict)
	verifyCampaignModel(t, store, maintainer, campaign, provider.ID, key)
	trusted := &trustedCampaignRuntime{php: php, injectDependency: true}
	var actual validation.MatrixRunner = trusted
	if image := os.Getenv("UPGRADE_TEST_MATRIX_IMAGE"); image != "" {
		container, err := validation.NewDocker(os.Getenv("UPGRADE_TEST_DOCKER"), image, 90*time.Second)
		must(t, err)
		trusted.docker = container
	}
	var reportIDs []string
	for index, wave := range campaign.Waves {
		if campaign.ActiveWave != index {
			t.Fatalf("wave advanced incorrectly: %+v", campaign)
		}
		request := RehearsalInput{Expected: campaign.Revision, Wave: wave.ID, RepoDigests: matrixDigests(planMatrix(campaign, wave.ID)),
			ContractSet: campaign.ContractSet, Runner: runner}
		rehearsalKey := key()
		result, err := store.SubmitCampaignRehearsal(ctx, verifier, rehearsalKey, campaign.ID, request)
		must(t, err)
		duplicate, err := store.SubmitCampaignRehearsal(ctx, verifier, rehearsalKey, campaign.ID, request)
		must(t, err)
		if duplicate.ID != result.ID || duplicate.Matrix != result.Matrix {
			t.Fatal("duplicate rehearsal created another matrix")
		}
		runAll := func() CampaignRehearsal {
			for {
				ran, err := store.RunCampaignPair(ctx, runner, actual)
				must(t, err)
				if !ran {
					break
				}
			}
			out, err := store.CampaignRehearsal(ctx, verifier, campaign.ID, result.ID)
			must(t, err)
			return out
		}
		result = runAll()
		if index == 0 {
			if result.State != "blocked" || len(result.FailedPairs) != 1 {
				t.Fatalf("A10 expected one unavailable dependency: %+v", result)
			}
			before := map[string]string{}
			for _, pair := range result.Pairs {
				if pair.State == "passed" {
					before[pair.ID] = pair.Evidence[0].Evidence
				} else if pair.State != "dependency-unavailable" {
					t.Fatalf("wrong matrix failure class: %+v", pair)
				}
			}
			_, err := store.RetryCampaignRehearsal(ctx, verifier, key(), campaign.ID, result.ID)
			must(t, err)
			result = runAll()
			for _, pair := range result.Pairs {
				if prior, ok := before[pair.ID]; ok && (len(pair.Evidence) != 1 || prior != pair.Evidence[0].Evidence) {
					t.Fatal("A12 completed pair was executed again")
				}
			}
		}
		if result.State != "passed" {
			t.Fatalf("A9 wave %s actual matrix did not pass: %+v", wave.ID, result)
		}
		reportIDs = append(reportIDs, result.EvidenceRefs...)
		var priorBundle CampaignBundle
		for j, change := range wave.Changes {
			candidate, err := store.GenerateCampaignCandidate(ctx, maintainer, key(), campaign.ID, CampaignCandidateInput{Expected: campaign.Revision,
				Wave: wave.ID, Repository: change.Repository, Mode: "rule"}, nil)
			must(t, err)
			_, err = store.ExportCampaignBundle(ctx, maintainer, key(), campaign.ID, CampaignExportInput{Expected: campaign.Revision, Candidate: candidate.ID, Rehearsal: result.ID})
			expect(t, err, ErrConflict)
			for _, hunk := range candidate.Hunks {
				candidate, err = store.ReviewCampaignHunk(ctx, reviewer, key(), campaign.ID, candidate.ID, hunk.ID,
					HunkDecision{Expected: candidate.Revision, Decision: "accept", Reason: "Matched the frozen migration"})
				must(t, err)
			}
			export := CampaignExportInput{Expected: campaign.Revision, Candidate: candidate.ID, Rehearsal: result.ID}
			if index == 1 && j == 1 {
				drift := goFiles("/v1/quote", "minor")
				drift["src/client.go"] += "\n// Unrelated commit from the Go owner.\n"
				freshGo, err := estateRepository(ctx, store.db, maintainer, goClient.ID)
				must(t, err)
				freshGo, _ = pin(freshGo, "3.0.1", drift, false)
				blocked, err := store.ExportCampaignBundle(ctx, maintainer, key(), campaign.ID, export)
				must(t, err)
				retained, err := store.CampaignBundle(ctx, maintainer, campaign.ID, priorBundle.ID)
				must(t, err)
				if blocked.State != "drifted" || blocked.Patch != "" || retained.AdoptionRef != priorBundle.AdoptionRef {
					t.Fatalf("A11 drift/receipt state mismatch: %+v / %+v", blocked, retained)
				}
				updated := goFiles("/v2/quote", "amount_minor")
				updated["src/client.go"] += "\n// Unrelated commit from the Go owner.\n"
				_, staged := pin(freshGo, "4.0.1", updated, true)
				campaign, err = store.RebaseCampaign(ctx, maintainer, key(), campaign.ID, CampaignRebaseInput{
					Expected: campaign.Revision, Repository: change.Repository, Current: freshGo.Snapshot, Target: staged.Digest})
				must(t, err)
				change = campaign.Waves[index].Changes[j]
				export.Expected = campaign.Revision
				_, err = store.ExportCampaignBundle(ctx, maintainer, key(), campaign.ID, export)
				expect(t, err, ErrConflict)
				candidate, err = store.GenerateCampaignCandidate(ctx, maintainer, key(), campaign.ID, CampaignCandidateInput{Expected: campaign.Revision,
					Wave: wave.ID, Repository: change.Repository, Mode: "rule"}, nil)
				must(t, err)
				for _, hunk := range candidate.Hunks {
					candidate, err = store.ReviewCampaignHunk(ctx, reviewer, key(), campaign.ID, candidate.ID, hunk.ID,
						HunkDecision{Expected: candidate.Revision, Decision: "accept", Reason: "Matched the rebased migration"})
					must(t, err)
				}
				export.Candidate = candidate.ID
				_, err = store.ExportCampaignBundle(ctx, maintainer, key(), campaign.ID, export)
				expect(t, err, ErrConflict)
				request.Expected, request.RepoDigests = campaign.Revision, matrixDigests(planMatrix(campaign, wave.ID))
				result, err = store.SubmitCampaignRehearsal(ctx, verifier, key(), campaign.ID, request)
				must(t, err)
				result = runAll()
				if result.State != "passed" {
					t.Fatalf("rebased matrix did not pass: %+v", result)
				}
				export.Rehearsal = result.ID
			}
			bundle, err := store.ExportCampaignBundle(ctx, maintainer, key(), campaign.ID, export)
			must(t, err)
			if bundle.Patch == "" || bundle.Digest == "" || bundle.State != "ready" {
				t.Fatalf("approved patch export missing data: %+v", bundle)
			}
			oldSource, err := readEstateSource(ctx, store.db, maintainer, estateForTest(t, store, maintainer, change.Repository), change.Base)
			must(t, err)
			observed := applyCampaignPatch(t, oldSource, bundle.Patch)
			repo := estateForTest(t, store, maintainer, change.Repository)
			targetSource, err := readEstateSource(ctx, store.db, maintainer, repo, change.Target)
			must(t, err)
			for _, file := range targetSource.Files {
				got, ok := observed.Find(file.Path)
				if !ok || file.SHA256 != got.SHA256 {
					t.Fatalf("Git produced different %s in wave %s: got %q, want %q", file.Path, change.Repository, got.Content, file.Content)
				}
			}
			_, err = store.PinEstateSnapshot(ctx, maintainer, key(), repo.ID, EstateSnapshotInput{Expected: repo.Revision, Version: "observed-" + observed.Commit[:12], Snapshot: observed, Stage: true})
			must(t, err)
			_, err = store.AdoptCampaignBundle(ctx, maintainer, key(), campaign.ID, bundle.ID,
				CampaignAdoptionInput{Expected: campaign.Revision, Ref: strings.Repeat("0", 40), Observed: observed.Digest})
			expect(t, err, ErrConflict)
			adopted, err := store.AdoptCampaignBundle(ctx, maintainer, key(), campaign.ID, bundle.ID,
				CampaignAdoptionInput{Expected: campaign.Revision, Ref: observed.Commit, Observed: observed.Digest})
			must(t, err)
			if adopted.State != "adopted" {
				t.Fatalf("approval receipt not read back: %+v", adopted)
			}
			priorBundle = adopted
			campaign, err = store.MigrationCampaign(ctx, verifier, campaign.ID)
			must(t, err)
		}
		for _, pair := range result.Pairs {
			proof := pair.Evidence[len(pair.Evidence)-1]
			if proof.Evidence == "" || proof.Provider != pair.Provider.Snapshot || proof.Consumer != pair.Consumer.Snapshot || proof.ExitCode != 0 {
				t.Fatalf("unbound matrix evidence: %+v", pair)
			}
		}
	}
	if campaign.State != "delivered" || len(reportIDs) != 14 {
		t.Fatalf("campaign outcome=%s proof=%d", campaign.State, len(reportIDs))
	}
	t.Logf("A1–A6, A9–A12: three scoped repositories, %d unresolved calls, %d frozen pair outputs, failed-only retry and drift-guarded adoption", impact.Unresolved, len(reportIDs))
}

func TestDockerCampaignMissingDependency(t *testing.T) {
	image, docker := os.Getenv("UPGRADE_TEST_MATRIX_IMAGE"), os.Getenv("UPGRADE_TEST_DOCKER")
	if image == "" || docker == "" {
		t.Skip("requires the pinned PHP and Go validation image")
	}
	backend, err := validation.NewDocker(docker, image, 90*time.Second)
	must(t, err)
	provider := captureEstateFixture(t, "provider", []string{"src"}, map[string]string{"src/test.php": "<?php exit(0);\n"})
	consumer := captureEstateFixture(t, "consumer", []string{"go.mod", "src"}, map[string]string{
		"go.mod":            "module example.invalid/consumer\n\ngo 1.26.0\n\nrequire example.invalid/locked v1.0.0\n",
		"src/quote_test.go": "package quote\nimport _ \"example.invalid/locked\"\n",
	})
	proof := backend.RunMatrix(t.Context(), validation.MatrixInput{Provider: provider, Consumer: consumer,
		ProviderTest: "src/test.php", ConsumerTest: "src/quote_test.go", Language: "go"})
	if proof.State != "dependency-unavailable" || proof.ExitCode != 78 || proof.Evidence == "" || proof.DependencySHA == "" {
		t.Fatalf("offline missing module was not classified as an execution error: %+v", proof)
	}
}

func verifyCampaignModel(t *testing.T, store *Store, owner Identity, campaign MigrationCampaign, provider string, key func() string) {
	t.Helper()
	request := CampaignCandidateInput{Expected: campaign.Revision, Wave: "W1", Repository: provider, Mode: "model", Budget: 6000,
		Review: map[string][]string{"src/quotes.php": {"Include the new current method. Keep legacy behavior."}}}
	if endpoint := os.Getenv("UPGRADE_TEST_CAMPAIGN_MODEL_URL"); endpoint != "" {
		client, err := model.New(endpoint, os.Getenv("UPGRADE_TEST_CAMPAIGN_MODEL_NAME"), "", 35*time.Second)
		must(t, err)
		out, err := store.GenerateCampaignCandidate(t.Context(), owner, key(), campaign.ID, request, client)
		must(t, err)
		selected := false
		for _, hunk := range out.Hunks {
			if hunk.Selected {
				selected = true
				if !strings.HasPrefix(hunk.Path, "src/") {
					t.Fatalf("A7 candidate selected unauthorized model context %s", hunk.Path)
				}
			}
		}
		if !selected || out.State != "ready" || out.Usage == nil || !out.Usage.Metered || out.Usage.RequestSHA256 == "" || out.Usage.ResponseSHA256 == "" {
			t.Fatalf("A7 real model did not provide a metered candidate: %+v", out.Usage)
		}
		value, err := json.Marshal(out.Usage)
		must(t, err)
		t.Logf("A7 local model evidence %s", value)
	}

	served := make(chan bool, 1)
	misbehaving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		decoded := json.NewDecoder(r.Body).Decode(&request) == nil
		served <- decoded && len(request.Messages) == 2 && strings.Contains(request.Messages[1].Content, "src/quotes.php") &&
			!strings.Contains(request.Messages[1].Content, "contracts/")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"indices\":[99]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":249,"completion_tokens":11}}`))
	}))
	t.Cleanup(misbehaving.Close)
	client, err := model.New(misbehaving.URL+"/v1/chat/completions", "adversarial-fixture", "", 5*time.Second)
	must(t, err)
	out, err := store.GenerateCampaignCandidate(t.Context(), owner, key(), campaign.ID, request, client)
	must(t, err)
	select {
	case allowed := <-served:
		if !allowed {
			t.Fatal("model payload contained a path outside the granted model scope")
		}
	default:
		t.Fatal("expected model response boundary to be exercised")
	}
	if out.State != "failed" || out.Usage == nil || !out.Usage.Metered || out.Usage.PromptTokens != 249 || out.Usage.CompletionTokens != 11 || out.Usage.State != "rejected" {
		t.Fatalf("A8 model escaped scope or rejected usage was lost: %+v", out.Usage)
	}
	t.Log("A8 unauthorized proposed index rejected and usage persisted")
}

func captureEstateFixture(t *testing.T, version string, roots []string, files map[string]string) snapshot.Snapshot {
	t.Helper()
	dir := t.TempDir()
	for path, contents := range files {
		name := filepath.Join(dir, filepath.FromSlash(path))
		must(t, os.MkdirAll(filepath.Dir(name), 0755))
		must(t, os.WriteFile(name, []byte(contents), 0644))
	}
	result, err := snapshot.Capture(dir, version, roots)
	must(t, err)
	return result
}

func estateForTest(t *testing.T, store *Store, i Identity, id string) EstateRepository {
	t.Helper()
	value, err := estateRepository(t.Context(), store.db, i, id)
	must(t, err)
	return value
}

func contractFixture(kind, route, field, callsite string) string {
	call := ""
	if callsite != "" {
		call = fmt.Sprintf(`,"callsite":%q,"line":3`, callsite)
	}
	return fmt.Sprintf(`{"kind":%q,"method":"GET","path":%q,"required_response_fields":[%q],"response_example":{%q:1200}%s}`, kind, route, field, field, call)
}

func applyCampaignPatch(t *testing.T, source snapshot.Snapshot, patch string) snapshot.Snapshot {
	t.Helper()
	dir := t.TempDir()
	must(t, source.Materialize(dir, nil))
	git := func(args ...string) string {
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir, command.Stdin = dir, strings.NewReader(patch)
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("Git %v: %v %s", args, err, data)
		}
		return strings.TrimSpace(string(data))
	}
	git("init", "-q")
	git("config", "core.autocrlf", "false")
	git("add", "-A")
	git("-c", "user.name=Campaign Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "Baseline")
	git("apply", "--check", "-")
	git("apply", "-")
	git("add", "-A")
	git("-c", "user.name=Campaign Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "Adopt reviewed changes")
	result, err := snapshot.Capture(dir, git("rev-parse", "HEAD"), source.Roots)
	must(t, err)
	return result
}

func expect(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got %v, expected %v", got, want)
	}
}

// The host runner below only executes the in-source synthetic test project.
type trustedCampaignRuntime struct {
	php              string
	injectDependency bool
	docker           validation.MatrixRunner
}

func (r *trustedCampaignRuntime) RunMatrix(ctx context.Context, in validation.MatrixInput) validation.MatrixResult {
	out := validation.MatrixResult{Observation: validation.Observation{State: "unavailable", ExitCode: -1}, Image: "trusted-fixture-host",
		Provider: in.Provider.Digest, Consumer: in.Consumer.Digest, DependencySHA: validation.MatrixDependencySHA(in), Command: "fixture PHP and Go regression"}
	if in.Language == "go" && r.injectDependency {
		r.injectDependency = false
		out.State, out.ExitCode, out.Output = "dependency-unavailable", 78, "injected missing dependency before the trusted fixture starts"
		return validation.SealMatrixResult(out)
	}
	if r.docker != nil {
		return r.docker.RunMatrix(ctx, in)
	}
	start := time.Now()
	root, err := os.MkdirTemp("", "upgrade-matrix-trusted-")
	if err != nil {
		return validation.SealMatrixResult(out)
	}
	defer os.RemoveAll(root)
	provider, consumer := filepath.Join(root, "provider"), filepath.Join(root, "consumer")
	for dir, value := range map[string]snapshot.Snapshot{provider: in.Provider, consumer: in.Consumer} {
		if err := os.Mkdir(dir, 0755); err != nil || value.Materialize(dir, nil) != nil {
			return validation.SealMatrixResult(out)
		}
	}
	address := ""
	if in.ProviderRoute != "" {
		local, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return validation.SealMatrixResult(out)
		}
		address = local.Addr().String()
		_ = local.Close()
		server := exec.CommandContext(ctx, r.php, "-S", address, in.ProviderRoute)
		server.Dir = provider
		if err := server.Start(); err != nil {
			return validation.SealMatrixResult(out)
		}
		defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
		for range 100 {
			connection, err := net.DialTimeout("tcp4", address, 40*time.Millisecond)
			if err == nil {
				_ = connection.Close()
				break
			}
			if ctx.Err() != nil {
				return validation.SealMatrixResult(out)
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	providerTest := exec.CommandContext(ctx, r.php, in.ProviderTest)
	providerTest.Dir = provider
	consumerTest := exec.CommandContext(ctx, r.php, in.ConsumerTest)
	if in.Language == "go" {
		consumerTest = exec.CommandContext(ctx, "go", "test", "-count=1", "-mod=readonly", "./src")
	}
	consumerTest.Dir = consumer
	consumerTest.Env = append(os.Environ(), "UPGRADE_PROVIDER_ROOT="+provider, "UPGRADE_PROVIDER_URL=http://"+address, "GOPROXY=off", "GOTOOLCHAIN=local")
	var output []byte
	for _, command := range []*exec.Cmd{providerTest, consumerTest} {
		data, err := command.CombinedOutput()
		output = append(output, data...)
		if err != nil {
			out.State, out.ExitCode, out.Output = "failed", 1, string(output)
			return validation.SealMatrixResult(out)
		}
	}
	out.State, out.ExitCode, out.OutputBytes, out.OutputSHA256 = "passed", 0, len(output), snapshot.Digest(output)
	out.Milliseconds = time.Since(start).Milliseconds()
	return validation.SealMatrixResult(out)
}
