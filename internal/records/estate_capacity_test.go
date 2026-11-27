package records

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEstateCapacity(t *testing.T) {
	dsn, report := os.Getenv("UPGRADE_TEST_ESTATE_DSN"), os.Getenv("UPGRADE_TEST_ESTATE_CAPACITY")
	if dsn == "" || report == "" {
		t.Skip("set UPGRADE_TEST_ESTATE_DSN and UPGRADE_TEST_ESTATE_CAPACITY")
	}
	ctx := t.Context()
	must(t, Migrate(dsn, 0))
	store, err := Open(ctx, dsn)
	must(t, err)
	t.Cleanup(func() { _ = store.Close() })
	grant, err := store.Provision(ctx, "estate-capacity", "repo-load", "operator", "maintainer", time.Hour)
	must(t, err)
	identity, err := store.Identify(ctx, grant.Token)
	must(t, err)
	var seq int
	key := func() string { seq++; return fmt.Sprintf("load-request-%d", seq) }
	const sharedPackage = "example.test/capacity/provider"
	var request EstateImpactInput
	symbols := 0
	setup := time.Now()
	for index := range 50 {
		name := fmt.Sprintf("checkout-%02d", index)
		module := "example.test/capacity/" + name
		if index == 0 {
			module = sharedPackage
		}
		repo, err := store.RegisterEstateRepository(ctx, identity, key(), EstateRegistration{Package: module, Origin: "https://example.test/" + name, Version: "v1"})
		must(t, err)
		repo, err = store.SetEstatePolicy(ctx, identity, key(), repo.ID, EstatePolicyInput{Expected: repo.Revision, EstatePolicy: EstatePolicy{Index: []string{"go.mod", "module"}}})
		must(t, err)
		var goSource strings.Builder
		goSource.WriteString("package modules\n")
		if index > 0 {
			fmt.Fprintf(&goSource, "import shared %q\n", sharedPackage+"/module")
		}
		for n := range 999 {
			fmt.Fprintf(&goSource, "func Field%d(v int) int { return v + %d }\n", n, index+n)
		}
		prefix := goSource.String()
		if index == 0 {
			goSource.WriteString("func Shared(v int) int { return v }\n")
		} else {
			goSource.WriteString("func SharedUse(v int, target interface{ Load() }) int { target.Load(); return shared.Shared(v) }\n")
		}
		files := map[string]string{"go.mod": "module " + module + "\n\ngo 1.26.0\n", "module/symbols.go": goSource.String()}
		base := captureEstateFixture(t, "v1-"+name, repo.Policy.Index, files)
		repo, err = store.PinEstateSnapshot(ctx, identity, key(), repo.ID, EstateSnapshotInput{Expected: repo.Revision, Snapshot: base, Version: "v1"})
		must(t, err)
		current, err := store.EstateCatalog(ctx, identity, repo.ID, "")
		must(t, err)
		symbols += len(current.Symbols)
		if index == 0 {
			files["module/symbols.go"] = prefix + "func SharedNext(v int, region string) int { return v + len(region) }\n"
			next := captureEstateFixture(t, "v2-"+name, repo.Policy.Index, files)
			_, err = store.PinEstateSnapshot(ctx, identity, key(), repo.ID, EstateSnapshotInput{Expected: repo.Revision, Snapshot: next, Version: "v2", Stage: true})
			must(t, err)
			request.Producer, request.Previous, request.Current = repo.ID, base.Digest, next.Digest
		} else {
			request.Consumers = append(request.Consumers, EstatePin{Repository: repo.ID, Snapshot: base.Digest})
		}
	}
	setupMS := time.Since(setup).Milliseconds()
	var server string
	must(t, store.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&server))
	start := time.Now()
	result, err := store.EstateImpact(ctx, identity, request)
	includeMS := time.Since(start).Milliseconds()
	must(t, err)
	if len(result.Changes) != 1 || result.Changes[0].Previous.Target != "go:"+sharedPackage+"/module.Shared" || symbols != 50_000 ||
		len(result.Changes[0].ConsumerEdges) != 49 || result.Unresolved != 49 || includeMS > 60_000 {
		t.Fatalf("A14 symbols=%d affected=%+v duration=%d ms", symbols, result, includeMS)
	}
	data, err := json.MarshalIndent(map[string]any{"repositories": 50, "symbols": symbols, "affected_calls": 49,
		"unresolved": result.Unresolved, "indexing_ms": setupMS, "analysis_ms": includeMS, "go": runtime.Version(), "system": runtime.GOOS,
		"logical_processors": runtime.NumCPU(), "mysql": server, "approval": "isolated synthetic snapshots", "producer": request.Producer,
		"previous_snapshot": request.Previous, "current_snapshot": request.Current, "affected_target": result.Changes[0].Previous.Target}, "", "  ")
	must(t, err)
	must(t, os.WriteFile(report, append(data, '\n'), 0644))
	t.Logf("A14 50 repositories, %d MySQL-indexed symbols, 49 cross-repo calls, %d unresolved, impact %d ms", symbols, result.Unresolved, includeMS)
}
