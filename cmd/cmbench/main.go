// Command cmbench is the bench-vs-redis orchestration driver: it detects which of
// cachemoney / Redis / Valkey / pogocache (and the bench tools) are available, runs the
// available ones over RESP, parses the output with internal/bench, and renders a
// comparison table — skipping anything absent without ever failing the build.
//
// It is leaf tooling: the shipped cachemoney binary and the engine packages import
// neither it nor internal/bench (an import guard enforces this).
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/dopecoder/cachemoney/internal/bench"
)

// Markers in the results doc between which cmbench splices the rendered comparison table.
const (
	tableStart = "<!-- BENCH:TABLE:START -->"
	tableEnd   = "<!-- BENCH:TABLE:END -->"
)

func main() {
	lk := realLookup()
	suite := runCompare(os.Stderr, servers, lk, execMeasurer(realTools(lk)))
	table := bench.RenderTable(suite)
	fmt.Println(table)

	if doc := os.Getenv("BENCH_DOC"); doc != "" {
		if err := spliceDoc(doc, table); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not update %s: %v\n", doc, err)
		}
	}
	// Always exit 0: a missing optional bench tool must never fail `make`/CI.
}

// spliceDoc replaces the content between the table markers in the doc at path with table.
// It returns an error if the markers are absent (so the caller can warn).
func spliceDoc(path, table string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the operator-provided results-doc path
	if err != nil {
		return err
	}
	content := string(raw)
	si := strings.Index(content, tableStart)
	ei := strings.Index(content, tableEnd)
	if si < 0 || ei < 0 || ei < si {
		return fmt.Errorf("table markers not found in %s", path)
	}
	updated := content[:si+len(tableStart)] + "\n\n" + table + "\n" + content[ei:]
	return os.WriteFile(path, []byte(updated), 0o600)
}
