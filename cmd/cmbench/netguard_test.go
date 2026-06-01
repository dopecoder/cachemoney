package main

import (
	"os/exec"
	"strings"
	"testing"
)

// shippedPackages are the production packages that make up the cachemoney binary and its
// engine stack. NONE of them may import the benchmark tooling (internal/bench or
// cmd/cmbench): the benchmark is leaf, additive tooling that depends on the engine only by
// running the built binary over a socket (spec Req 8 s2).
var shippedPackages = []string{
	"github.com/dopecoder/cachemoney/cmd/cachemoney",
	"github.com/dopecoder/cachemoney/internal/cache",
	"github.com/dopecoder/cachemoney/internal/shardmap",
	"github.com/dopecoder/cachemoney/internal/resp",
	"github.com/dopecoder/cachemoney/internal/server",
	"github.com/dopecoder/cachemoney/internal/hash",
	"github.com/dopecoder/cachemoney/internal/tinylfu",
}

var benchPackages = []string{
	"github.com/dopecoder/cachemoney/internal/bench",
	"github.com/dopecoder/cachemoney/cmd/cmbench",
}

// TestShippedPackagesDoNotImportBench runs `go list -deps` over each shipped package and
// fails if a benchmark package appears in its transitive import set. Adding
// `import _ ".../internal/bench"` to any engine package MUST turn this red.
func TestShippedPackagesDoNotImportBench(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH; skipping import guard")
	}
	for _, pkg := range shippedPackages {
		deps := transitiveDeps(t, pkg)
		for _, bp := range benchPackages {
			if deps[bp] {
				t.Errorf("%s imports %s; the benchmark must be leaf tooling, imported by nothing shipped", pkg, bp)
			}
		}
	}
}

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
