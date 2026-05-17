package server

import (
	"os/exec"
	"strings"
	"testing"
)

const serverPkg = "github.com/dopecoder/cachemoney/internal/server"

// engineStack is the engine's net-free core. The whole point of internal/server is
// to be the ONE package that imports net; the engine stack must stay net-free so it
// remains embeddable (spec "No engine change"; design §3).
var engineStack = []string{
	"github.com/dopecoder/cachemoney/internal/cache",
	"github.com/dopecoder/cachemoney/internal/shardmap",
	"github.com/dopecoder/cachemoney/internal/hash",
}

// engineInternals are the engine's private packages. The dispatcher must reach the
// engine ONLY through the published cache.Engine interface (and cache.New in the
// binary), never these internals — so internal/server must not import them directly.
var engineInternals = []string{
	"github.com/dopecoder/cachemoney/internal/shardmap",
	"github.com/dopecoder/cachemoney/internal/hash",
}

func goList(t *testing.T, format, pkg string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH; skipping import guard")
	}
	out, err := exec.Command("go", "list", "-f", format, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list %s: %v\n%s", pkg, err, out)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

func toSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}

// TestEngineStackStaysNetFree re-runs the engine's proven net-free guard: importing
// net anywhere in the engine stack's transitive set must fail. This protects the
// "net lives only in internal/server" boundary from the server's side.
func TestEngineStackStaysNetFree(t *testing.T) {
	for _, pkg := range engineStack {
		deps := toSet(goList(t, "{{range .Deps}}{{.}}\n{{end}}", pkg))
		if deps["net"] {
			t.Errorf("%s transitively imports net; the engine stack MUST stay net-free", pkg)
		}
	}
}

// TestServerComposesOnlyPublishedEngine asserts internal/server imports the engine's
// published package (internal/cache) but NOT its internals (shardmap/hash). The check
// is on DIRECT imports: internal/cache transitively pulls in shardmap/hash, so only a
// direct import would mean the dispatcher reached past the cache.Engine interface.
func TestServerComposesOnlyPublishedEngine(t *testing.T) {
	imports := toSet(goList(t, "{{range .Imports}}{{.}}\n{{end}}", serverPkg))
	if !imports["github.com/dopecoder/cachemoney/internal/cache"] {
		t.Error("internal/server does not import internal/cache; it must compose the published engine")
	}
	for _, internal := range engineInternals {
		if imports[internal] {
			t.Errorf("internal/server imports %s directly; it MUST reach the engine only via cache.Engine", internal)
		}
	}
}

// TestServerIsTheNetImporter is the positive control: internal/server is expected to
// pull in net (that is its job). If this ever stops being true the adapter is not
// actually wired to the network.
func TestServerIsTheNetImporter(t *testing.T) {
	deps := toSet(goList(t, "{{range .Deps}}{{.}}\n{{end}}", serverPkg))
	if !deps["net"] {
		t.Error("internal/server does not import net; the TCP adapter must")
	}
}
