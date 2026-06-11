package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/bench"
)

// errMeasure is a stand-in measurement failure.
var errMeasure = errors.New("measure failed")

// injectLookup builds a lookup with a fixed Docker verdict and a set of "missing" images.
func injectLookup(docker bool, missing ...string) lookup {
	miss := map[string]bool{}
	for _, m := range missing {
		miss[m] = true
	}
	return lookup{
		hasDocker: func() bool { return docker },
		hasImage:  func(img string) bool { return !miss[img] },
	}
}

func TestMakePlanNoDockerSkipsEverything(t *testing.T) {
	t.Parallel()

	p := makePlan(servers, injectLookup(false))
	if len(p.run) != 0 {
		t.Fatalf("no-docker plan has %d runs, want 0", len(p.run))
	}
	if len(p.skipped) != len(servers) {
		t.Fatalf("skipped %d, want %d (all)", len(p.skipped), len(servers))
	}
}

func TestMakePlanAllAvailableRunsEverything(t *testing.T) {
	t.Parallel()

	p := makePlan(servers, injectLookup(true))
	if len(p.run) != len(servers) {
		t.Fatalf("all-available plan has %d runs, want %d", len(p.run), len(servers))
	}
	if len(p.skipped) != 0 {
		t.Fatalf("skipped %v, want none", p.skipped)
	}
}

func TestMakePlanMissingServerImageSkipsOnlyThatServer(t *testing.T) {
	t.Parallel()

	p := makePlan(servers, injectLookup(true, pogocacheImage))
	if !contains(p.skipped, "pogocache") {
		t.Fatalf("pogocache should be skipped when its image is missing: skipped=%v", p.skipped)
	}
	if len(p.run) != len(servers)-1 {
		t.Fatalf("expected %d runs, got %d", len(servers)-1, len(p.run))
	}
	for _, pl := range p.run {
		if pl.spec.name == "pogocache" {
			t.Fatal("pogocache must not be in the run set")
		}
	}
}

func TestMakePlanMissingToolImageSkipsEverything(t *testing.T) {
	t.Parallel()

	// memtier is a shared tool image every server needs → all are skipped.
	p := makePlan(servers, injectLookup(true, memtierImage))
	if len(p.run) != 0 {
		t.Fatalf("missing memtier image: %d runs, want 0", len(p.run))
	}
}

func TestRunCompareSkipsWhenAbsentNeverMeasures(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	measure := func(planned) (bench.Result, error) {
		t.Fatal("measurer must not be called when everything is skipped")
		return bench.Result{}, nil
	}
	suite := runCompare(&out, servers, injectLookup(false), measure)

	if len(suite.Results) != 0 || len(suite.Skipped) != len(servers) {
		t.Fatalf("suite = %d results / %d skipped, want 0 / %d", len(suite.Results), len(suite.Skipped), len(servers))
	}
	if !strings.Contains(out.String(), "skipping") {
		t.Fatalf("expected skip messages, got %q", out.String())
	}
}

func TestRunCompareMeasureErrorMovesToSkipped(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	measure := func(planned) (bench.Result, error) {
		return bench.Result{}, errMeasure
	}
	suite := runCompare(&out, servers, injectLookup(true), measure)
	if len(suite.Results) != 0 {
		t.Fatalf("got %d results, want 0 (measure failed)", len(suite.Results))
	}
	if len(suite.Skipped) != len(servers) {
		t.Fatalf("every server should be skipped after a measure error: %v", suite.Skipped)
	}
}

func TestRunCompareMeasureSuccess(t *testing.T) {
	t.Parallel()

	measure := func(p planned) (bench.Result, error) {
		return bench.Result{Server: p.spec.name, Hit: &bench.HitRatioResult{HitRatio: 0.9}}, nil
	}
	suite := runCompare(&bytes.Buffer{}, servers, injectLookup(true), measure)
	if len(suite.Results) != len(servers) {
		t.Fatalf("expected %d results, got %d", len(servers), len(suite.Results))
	}
	if suite.Results[0].Server != "cachemoney" {
		t.Fatalf("first result = %q, want cachemoney (canonical order)", suite.Results[0].Server)
	}
}

func TestSpliceDoc(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	body := "# Doc\n\n" + tableStart + "\nold\n" + tableEnd + "\n\ntail\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := spliceDoc(path, "| a | b |"); err != nil {
		t.Fatalf("spliceDoc: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "| a | b |") || !strings.Contains(string(got), "tail") {
		t.Fatalf("splice result missing table or tail:\n%s", got)
	}

	// No markers → error; missing file → error.
	noMarkers := filepath.Join(dir, "plain.md")
	_ = os.WriteFile(noMarkers, []byte("nothing here"), 0o600)
	if err := spliceDoc(noMarkers, "x"); err == nil {
		t.Fatal("spliceDoc without markers: want error")
	}
	if err := spliceDoc(filepath.Join(dir, "nope.md"), "x"); err == nil {
		t.Fatal("spliceDoc on a missing file: want error")
	}
}

func TestRealLookupConstructs(t *testing.T) {
	t.Parallel()

	lk := realLookup()
	// hasDocker runs `docker info` (cheap, no side effects). hasImage is deliberately NOT
	// called here — it pulls a missing image, a network side effect that does not belong
	// in `make test`. It is exercised by the opt-in smoke and by `make bench-compare`.
	_ = lk.hasDocker()
}

// TestDriverEndToEndSmoke runs a real measurement against live servers on the bench bridge.
// It is opt-in (BENCH_SMOKE=1) so `make test` / CI never pulls images or runs a multi-minute
// benchmark; the real entrypoint is `make bench-compare`.
func TestDriverEndToEndSmoke(t *testing.T) {
	if os.Getenv("BENCH_SMOKE") == "" {
		t.Skip("set BENCH_SMOKE=1 to run the end-to-end benchmark smoke (needs Docker + images)")
	}
	lk := realLookup()
	cleanupContainers(servers)
	if err := ensureNetwork(benchNetwork); err != nil {
		t.Skipf("cannot create docker network: %v", err)
	}
	defer func() {
		cleanupContainers(servers)
		removeNetwork(benchNetwork)
	}()
	suite := runCompare(&bytes.Buffer{}, servers, lk, execMeasurer(benchNetwork))
	_ = bench.RenderTable(suite) // must not panic
}

func TestFilterServers(t *testing.T) {
	t.Parallel()

	var warn bytes.Buffer
	if got := filterServers(servers, "", &warn); len(got) != len(servers) {
		t.Fatalf("empty BENCH_ONLY: got %d, want %d (all)", len(got), len(servers))
	}
	if got := filterServers(servers, "  ", &warn); len(got) != len(servers) {
		t.Fatalf("blank BENCH_ONLY: got %d, want %d (all)", len(got), len(servers))
	}

	got := filterServers(servers, "redis, cachemoney", &warn)
	if len(got) != 2 || got[0].name != "cachemoney" || got[1].name != "redis" {
		t.Fatalf("subset = %v, want [cachemoney redis] in canonical order", serverNames(got))
	}

	// Unknown name → empty set + a warning naming it (the footgun: an empty set must not
	// silently sail on; main refuses to splice the canonical doc for BENCH_ONLY runs).
	warn.Reset()
	if got := filterServers(servers, "redys", &warn); len(got) != 0 {
		t.Fatalf("unknown name: got %d, want 0", len(got))
	}
	if !strings.Contains(warn.String(), "redys") {
		t.Fatalf("expected a warning naming the unknown server, got %q", warn.String())
	}
}

func TestEnvInt(t *testing.T) {
	if got := envInt("CM_ENVINT_UNSET", 7); got != 7 {
		t.Errorf("unset → %d, want 7", got)
	}
	t.Setenv("CM_ENVINT_X", "42")
	if got := envInt("CM_ENVINT_X", 7); got != 42 {
		t.Errorf("valid → %d, want 42", got)
	}
	t.Setenv("CM_ENVINT_X", "0")
	if got := envInt("CM_ENVINT_X", 7); got != 7 {
		t.Errorf("zero → %d, want 7 (fallback)", got)
	}
	t.Setenv("CM_ENVINT_X", "abc")
	if got := envInt("CM_ENVINT_X", 7); got != 7 {
		t.Errorf("non-int → %d, want 7 (fallback)", got)
	}
}

// TestStartServerMissingLocalFile pins the fast-fail: a server whose localFile is absent
// errors before any docker invocation (runArgs must not even be consulted).
func TestStartServerMissingLocalFile(t *testing.T) {
	t.Parallel()

	spec := serverSpec{
		name:      "x",
		localFile: filepath.Join(t.TempDir(), "nope"),
		runArgs: func(string) []string {
			t.Fatal("runArgs must not run when localFile is missing")
			return nil
		},
	}
	if _, err := startServer(spec, "net"); err == nil {
		t.Fatal("want error when localFile is absent")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
