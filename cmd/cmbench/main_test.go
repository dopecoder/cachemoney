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

// allAbsent is a lookup where nothing is available (the authoring-host reality).
func allAbsent() lookup {
	return lookup{
		hasBinary: func(string) bool { return false },
		hasDocker: func() bool { return false },
		hasImage:  func(string) bool { return false },
	}
}

func withBinaries(names ...string) lookup {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	lk := allAbsent()
	lk.hasBinary = func(n string) bool { return set[n] }
	return lk
}

func withDockerImages(images ...string) lookup {
	set := map[string]bool{}
	for _, i := range images {
		set[i] = true
	}
	lk := allAbsent()
	lk.hasDocker = func() bool { return true }
	lk.hasImage = func(i string) bool { return set[i] }
	return lk
}

func TestServerModeLocalDockerSkip(t *testing.T) {
	t.Parallel()

	redis := servers[1] // redis spec
	if got := allAbsent().serverMode(redis); got != modeSkip {
		t.Errorf("all-absent redis mode = %v, want skip", got)
	}
	if got := withBinaries("redis-server").serverMode(redis); got != modeLocal {
		t.Errorf("local redis mode = %v, want local", got)
	}
	if got := withDockerImages(redis.image).serverMode(redis); got != modeDocker {
		t.Errorf("docker redis mode = %v, want docker", got)
	}
	// cachemoney has no image: docker availability alone cannot run it.
	if got := withDockerImages("anything").serverMode(servers[0]); got != modeSkip {
		t.Errorf("cachemoney with only docker = %v, want skip (no image)", got)
	}
}

func TestToolMode(t *testing.T) {
	t.Parallel()

	if got := allAbsent().toolMode("redis-benchmark", "redis:7.4"); got != modeSkip {
		t.Errorf("absent tool mode = %v, want skip", got)
	}
	if got := withBinaries("memtier_benchmark").toolMode("memtier_benchmark", "img"); got != modeLocal {
		t.Errorf("local tool mode = %v, want local", got)
	}
	if got := withDockerImages("img").toolMode("memtier_benchmark", "img"); got != modeDocker {
		t.Errorf("docker tool mode = %v, want docker", got)
	}
}

func TestMakePlanAllAbsentSkipsEverything(t *testing.T) {
	t.Parallel()

	p := makePlan(servers, allAbsent())
	if len(p.run) != 0 {
		t.Fatalf("all-absent plan has %d runs, want 0", len(p.run))
	}
	if len(p.skipped) != len(servers) {
		t.Fatalf("skipped %d, want %d (all)", len(p.skipped), len(servers))
	}
}

func TestRunCompareSkipsWhenAbsentNeverMeasures(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	measure := func(planned) (bench.Result, error) {
		t.Fatal("measurer must not be called when everything is skipped")
		return bench.Result{}, nil
	}
	suite := runCompare(&out, servers, allAbsent(), measure)

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
	suite := runCompare(&out, servers, withBinaries("bin/cachemoney"), measure)
	if len(suite.Results) != 0 {
		t.Fatalf("got %d results, want 0 (measure failed)", len(suite.Results))
	}
	if !contains(suite.Skipped, "cachemoney") {
		t.Fatalf("cachemoney should be skipped after a measure error: %v", suite.Skipped)
	}
}

func TestRunCompareMeasureSuccess(t *testing.T) {
	t.Parallel()

	measure := func(p planned) (bench.Result, error) {
		return bench.Result{Server: p.spec.name, Hit: &bench.HitRatioResult{HitRatio: 0.9}}, nil
	}
	suite := runCompare(&bytes.Buffer{}, servers, withBinaries("bin/cachemoney"), measure)
	if len(suite.Results) != 1 || suite.Results[0].Server != "cachemoney" {
		t.Fatalf("expected one cachemoney result, got %+v", suite.Results)
	}
}

func TestExecMeasurerNoToolsErrors(t *testing.T) {
	t.Parallel()

	m := execMeasurer(toolset{redisBench: modeSkip, memtier: modeSkip})
	if _, err := m(planned{spec: servers[0], mode: modeLocal}); err == nil {
		t.Fatal("execMeasurer with no tools: want error, got nil")
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

func TestRealLookupAndToolsConstruct(t *testing.T) {
	t.Parallel()

	lk := realLookup()
	if !lk.hasBinary("go") {
		t.Error("realLookup should find the go binary on PATH")
	}
	_ = lk.hasDocker()
	_ = lk.hasImage("cachemoney/definitely-not-an-image:0") // exercises the inspect path
	tools := realTools(lk)
	_ = tools.redisImage
}

// TestDriverEndToEndSmoke runs a real measurement only when the tooling is present;
// otherwise it skips (the authoring host has neither the images nor the native tools).
func TestDriverEndToEndSmoke(t *testing.T) {
	lk := realLookup()
	tools := realTools(lk)
	if tools.redisBench == modeSkip && tools.memtier == modeSkip {
		t.Skip("no redis-benchmark/memtier_benchmark available; skipping end-to-end smoke")
	}
	suite := runCompare(&bytes.Buffer{}, servers, lk, execMeasurer(tools))
	_ = bench.RenderTable(suite) // must not panic
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
