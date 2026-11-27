package records

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/axin2024/upgrade-agent/internal/model"
	"github.com/axin2024/upgrade-agent/internal/planning"
	"github.com/axin2024/upgrade-agent/internal/snapshot"
	"github.com/axin2024/upgrade-agent/internal/validation"
)

type holdoutContract struct {
	Name, Method, LegacyField, NewField string
	Value                               int
	Keep                                bool
}

var campaignHoldout = []holdoutContract{
	{"tax-rounding", "taxQuote", "tax", "tax_minor", 7925, true},
	{"backorder-eta", "supplyEstimate", "eta", "eta_minutes", 270, false},
	{"shipment-proof", "shipmentProof", "proof", "evidence_count", 3, false},
	{"pick-weight", "pickWeight", "weight", "weight_grams", 734, false},
	{"point-expiry", "pointsExpiry", "expires", "expires_unix", 1920240000, true},
	{"commission-tiers", "commission", "tier", "tier_rank", 4, false},
	{"refund-line", "refundLine", "refund", "refund_minor", -2460, true},
	{"sku-damage", "damageCount", "broken", "damaged_units", 2, false},
	{"shipment-zone", "deliveryZone", "zone", "zone_id", 86, false},
	{"coupon-use", "couponCount", "used", "used_times", 19, false},
	{"bulk-invoice", "outstanding", "total", "total_minor", 72900, true},
	{"reserved-stock", "reserved", "qty", "reserved_units", 71, false},
	{"voucher-expiry", "voucherExpiry", "valid_to", "valid_until_utc", 1923333900, false},
	{"return-reason", "returnReason", "code", "reason_code", 12, false},
	{"settlement-rate", "settlementRate", "share", "share_bps", 725, true},
	{"partner-credit", "creditLimit", "credit", "limit_minor", 13200, false},
	{"replenishment", "reorderPoint", "minimum", "reorder_units", 14, false},
	{"checkout-deposit", "depositDue", "due", "due_minor", 10051, false},
	{"receiving-window", "receiveWindow", "slot", "window_minutes", 45, true},
	{"print-count", "labelCount", "count", "printed_labels", 28, false},
}

type holdoutOutcome struct {
	Valid        bool                    `json:"valid_patch"`
	Rejected     bool                    `json:"rejected"`
	BusinessPass bool                    `json:"business_pass"`
	Paths        []string                `json:"selected_paths"`
	Run          validation.MatrixResult `json:"run"`
	Usage        *model.Usage            `json:"model_usage,omitempty"`
}

func TestLiveCampaignHoldout(t *testing.T) {
	endpoint, report, php := os.Getenv("UPGRADE_TEST_CAMPAIGN_MODEL_URL"), os.Getenv("UPGRADE_TEST_CAMPAIGN_HOLDOUT"), os.Getenv("UPGRADE_TEST_PHP")
	if endpoint == "" || report == "" || php == "" {
		t.Skip("set model URL, holdout report path and PHP CLI")
	}
	client, err := model.New(endpoint, os.Getenv("UPGRADE_TEST_CAMPAIGN_MODEL_NAME"), "", 35*time.Second)
	must(t, err)
	type totals struct {
		Valid      int `json:"valid_patches"`
		Pass       int `json:"regressions_passed"`
		Rejected   int `json:"rejected"`
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
	}
	type evidence struct {
		CorpusSHA string           `json:"corpus_sha256"`
		Cases     int              `json:"cases"`
		MustKeep  int              `json:"must_keep_legacy"`
		Mode      string           `json:"runtime"`
		Rules     totals           `json:"rule"`
		Model     totals           `json:"model"`
		Results   []map[string]any `json:"results"`
	}
	encoded, err := json.Marshal(campaignHoldout)
	must(t, err)
	e := evidence{Cases: len(campaignHoldout), Mode: runtime.GOOS + " PHP trusted fixture, actual loopback model", CorpusSHA: snapshot.Digest(encoded)}
	count := func(to *totals, result holdoutOutcome) {
		if result.Valid {
			to.Valid++
		}
		if result.BusinessPass {
			to.Pass++
		}
		if result.Rejected {
			to.Rejected++
		}
		if result.Usage != nil {
			to.Prompt += result.Usage.PromptTokens
			to.Completion += result.Usage.CompletionTokens
		}
	}
	phpRunner := &trustedCampaignRuntime{php: php}
	for _, item := range campaignHoldout {
		if item.Keep {
			e.MustKeep++
		}
		packageName := "Shared" + strconv.Itoa(item.Value+4096)
		library := func(includeNew bool) string {
			newFunc := ""
			if includeNew {
				newFunc = fmt.Sprintf("\npublic static function %sCurrent(): array {return ['%s'=>%d];}", item.Method, item.NewField, item.Value)
			}
			return fmt.Sprintf("<?php\nclass %s { public static function %s(): array {return ['%s'=>%d];}%s }\n", packageName, item.Method, item.LegacyField, item.Value, newFunc)
		}
		providerFiles := func(includeNew bool) map[string]string {
			return map[string]string{"src/provider.php": library(includeNew), "src/ready.php": "<?php\nrequire __DIR__.'/provider.php';\n"}
		}
		baseProvider := captureEstateFixture(t, item.Name+"-provider-v1", []string{"src"}, providerFiles(false))
		newProvider := captureEstateFixture(t, item.Name+"-provider-compat", []string{"src"}, providerFiles(true))
		function := func(name, suffix string) string {
			return fmt.Sprintf("<?php\nfunction %s(): array { return %s::%s%s(); }\n", name, packageName, item.Method, suffix)
		}
		exportField := item.NewField
		if item.Keep {
			exportField = item.LegacyField
		}
		consumerTest := fmt.Sprintf(`<?php
require getenv('UPGRADE_PROVIDER_ROOT').'/src/provider.php';
require __DIR__.'/checkout.php';
require __DIR__.'/export.php';
if (checkoutValue() !== ['%s'=>%d]) { echo 'checkout-contract'; exit(1); }
if (warehouseExport() !== ['%s'=>%d]) { echo 'warehouse-wire-contract'; exit(1); }
`, item.NewField, item.Value, exportField, item.Value)
		baselineFiles := map[string]string{"src/checkout.php": function("checkoutValue", ""), "src/export.php": function("warehouseExport", ""), "src/test.php": consumerTest}
		offer := map[string]string{"src/checkout.php": function("checkoutValue", "Current"), "src/export.php": function("warehouseExport", "Current"), "src/test.php": consumerTest}
		base := captureEstateFixture(t, item.Name+"-consumer-v1", []string{"src"}, baselineFiles)
		candidate := captureEstateFixture(t, item.Name+"-consumer-proposed", []string{"src"}, offer)
		raw, err := planning.SnapshotHunks(base, candidate, []string{"src"})
		must(t, err)
		keep := "Replace warehouseExport with the current provider function."
		if item.Keep {
			keep = "Keep warehouseExport and its legacy return field unchanged. Omit this patch."
		}
		notes := map[string][]string{"src/checkout.php": {"Replace checkoutValue with the current provider function."}, "src/export.php": {keep}}
		choices, mapping := candidateChoices(raw, EstateRepository{Policy: EstatePolicy{Model: []string{"src"}}}, notes)
		if len(mapping) != 2 || len(mapping) != len(choices) {
			t.Fatalf("%s did not produce two grounded choices", item.Name)
		}
		run := func(indices []int, usage *model.Usage, modelErr error) holdoutOutcome {
			out := holdoutOutcome{Usage: usage, Paths: []string{}, Rejected: modelErr != nil}
			files := make(map[string]string, len(baselineFiles))
			for name, content := range baselineFiles {
				files[name] = content
			}
			for _, index := range indices {
				name := raw[mapping[index]].Path
				out.Paths = append(out.Paths, name)
				files[name] = offer[name]
			}
			out.Valid = slices.Contains(out.Paths, "src/checkout.php") && slices.Contains(out.Paths, "src/export.php") != item.Keep
			applied := captureEstateFixture(t, item.Name+"-consumer-review", []string{"src"}, files)
			out.Run = phpRunner.RunMatrix(t.Context(), validation.MatrixInput{Provider: newProvider, Consumer: applied,
				ProviderTest: "src/ready.php", ConsumerTest: "src/test.php", Language: "php"})
			out.BusinessPass = out.Run.State == "passed"
			if out.Run.State != "passed" && out.Run.State != "failed" {
				t.Fatalf("%s runtime unavailable: %+v", item.Name, out.Run)
			}
			out.Rejected = out.Rejected || !out.Valid
			return out
		}
		pure := run([]int{0, 1}, nil, nil)
		suggestion, usage, modelErr := client.SuggestContracts(t.Context(), choices, 7000)
		assisted := run(suggestion.Indices, &usage, modelErr)
		count(&e.Rules, pure)
		count(&e.Model, assisted)
		e.Results = append(e.Results, map[string]any{"name": item.Name, "preserve_legacy": item.Keep, "provider_old_snapshot": baseProvider.Digest,
			"provider_compatible_snapshot": newProvider.Digest, "consumer_old_snapshot": base.Digest, "consumer_candidate_snapshot": candidate.Digest,
			"rule": pure, "model": assisted})
		t.Logf("%s rule=%t model=%t tokens=%d", item.Name, pure.BusinessPass, assisted.BusinessPass, usage.PromptTokens+usage.CompletionTokens)
	}
	if e.Cases != 20 || e.MustKeep < 5 || e.Model.Prompt == 0 || e.Rules.Pass+e.Rules.Rejected != e.Cases {
		t.Fatalf("incomplete corpus or metering: %+v", e.Model)
	}
	value, err := json.MarshalIndent(e, "", "  ")
	must(t, err)
	must(t, os.WriteFile(report, append(value, '\n'), 0644))
	t.Logf("A13 original cross-repository changes: rule=%+v, model=%+v", e.Rules, e.Model)
}
