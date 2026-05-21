package tinylfu_test

import (
	"os/exec"
	"strings"
	"testing"
)

// tinylfuPkg is the import path of the policy library under guard.
const tinylfuPkg = "github.com/dopecoder/cachemoney/internal/tinylfu"

// repoPrefix is the module's internal-package prefix; tinylfu is a leaf and MUST NOT
// import any sibling internal package (design §2, §3).
const repoPrefix = "github.com/dopecoder/cachemoney/internal/"

// TestNetFreeAndLeafImportGuard runs `go list -deps` over internal/tinylfu and fails
// if "net" — or any intra-repo package — appears in its transitive import set. The
// policy library is pure: it speaks fingerprints and counters, imports only stdlib,
// and depends on no other cachemoney package, which is what keeps it independently
// reviewable and a clean revert boundary (proposal §8).
//
// Negative control (the guard's teeth): adding `import _ "net"` or
// `import _ ".../internal/cache"` to any tinylfu file MUST turn this test red.
//
// Robustness: if the go tool is not on PATH the test skips rather than flakes.
func TestNetFreeAndLeafImportGuard(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool not on PATH; skipping import guard")
	}

	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", tinylfuPkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", tinylfuPkg, err, out)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep := strings.TrimSpace(line)
		if dep == "" {
			continue
		}
		if dep == "net" || strings.HasPrefix(dep, "net/") {
			t.Errorf("%s transitively imports %s; the policy library MUST be net-free", tinylfuPkg, dep)
		}
		if dep != tinylfuPkg && strings.HasPrefix(dep, repoPrefix) {
			t.Errorf("%s imports sibling package %s; tinylfu MUST be a leaf", tinylfuPkg, dep)
		}
	}
}
