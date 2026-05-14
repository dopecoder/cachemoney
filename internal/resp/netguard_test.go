package resp_test

import (
	"os/exec"
	"strings"
	"testing"
)

const respPkg = "github.com/dopecoder/cachemoney/internal/resp"

// enginePackages are the engine stack the codec MUST NOT depend on: importing any
// of them (directly or transitively) would couple the untrusted-input frontier to
// command semantics and break extractability as a standalone resp library.
var enginePackages = []string{
	"github.com/dopecoder/cachemoney/internal/cache",
	"github.com/dopecoder/cachemoney/internal/shardmap",
	"github.com/dopecoder/cachemoney/internal/hash",
}

// transitiveDeps returns the full set of import paths reachable from pkg
// (including pkg itself) as reported by `go list -deps`.
//
// Robustness: if the go tool is not on PATH the caller skips rather than flakes,
// but whenever the tool is available a forbidden import is a hard failure.
func transitiveDeps(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH; skipping import guard")
	}
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

// isStdlib reports whether an import path is a Go standard-library package. Stdlib
// paths have no dot in their first path segment (e.g. "bufio", "internal/poll");
// third-party paths begin with a domain ("github.com/...").
func isStdlib(pkg string) bool {
	first := pkg
	if i := strings.IndexByte(pkg, '/'); i >= 0 {
		first = pkg[:i]
	}
	return !strings.Contains(first, ".")
}

// TestCodecDoesNotImportNet is the guard's primary tooth: "net" WOULD appear below
// if the codec — or anything it imports — pulled it in. Adding `import _ "net"` to
// any production file MUST turn this red.
func TestCodecDoesNotImportNet(t *testing.T) {
	deps := transitiveDeps(t, respPkg)
	if deps["net"] {
		t.Error("internal/resp transitively imports net; the codec MUST be net-free")
	}
}

func TestCodecDoesNotImportEngine(t *testing.T) {
	deps := transitiveDeps(t, respPkg)
	for _, pkg := range enginePackages {
		if deps[pkg] {
			t.Errorf("internal/resp transitively imports %s; the codec MUST be engine-free", pkg)
		}
	}
}

func TestCodecDependsOnlyOnStdlib(t *testing.T) {
	deps := transitiveDeps(t, respPkg)
	for dep := range deps {
		if dep == respPkg {
			continue
		}
		if !isStdlib(dep) {
			t.Errorf("internal/resp depends on non-stdlib package %s; it MUST be self-contained", dep)
		}
	}
}
