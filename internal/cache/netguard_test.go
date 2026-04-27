package cache_test

import (
	"os/exec"
	"strings"
	"testing"
)

// netFreePackages are the engine and its supporting packages, all of which MUST
// stay free of any transitive dependency on net (spec "Engine and store packages
// do not import net"; design §3). The engine is remote-ready by interface shape,
// not by linking the network stack — keeping net out of the transitive set is
// what guarantees it stays embeddable and unit-testable.
var netFreePackages = []string{
	"github.com/dopecoder/cachemoney/internal/cache",
	"github.com/dopecoder/cachemoney/internal/shardmap",
	"github.com/dopecoder/cachemoney/internal/hash",
}

// TestNetFreeImportGuard runs `go list -deps` over each engine-stack package and
// fails loudly if "net" appears anywhere in its transitive import set.
//
// Negative control (the guard's teeth): this test only protects anything because
// "net" WOULD appear below if any listed package — or anything they import —
// pulled it in. Temporarily adding `import _ "net"` to internal/cache and
// re-running this test MUST turn it red; that is the intended failure mode.
//
// Robustness: if the go tool is not on PATH the test skips rather than flakes,
// but whenever the tool is available a net import is a hard failure.
func TestNetFreeImportGuard(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH; skipping net-free import guard")
	}

	for _, pkg := range netFreePackages {
		deps := transitiveDeps(t, pkg)
		if deps["net"] {
			t.Errorf("%s transitively imports net; the engine stack MUST be net-free", pkg)
		}
	}
}

// transitiveDeps returns the full set of import paths reachable from pkg
// (including pkg itself) as reported by `go list -deps`.
func transitiveDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()

	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}

	deps := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps[line] = true
		}
	}
	return deps
}
