// Package bench is cachemoney's pure, tested benchmark result core: it parses captured
// `redis-benchmark` and `memtier_benchmark` output into structured results and renders a
// deterministic markdown comparison table. It is the strict-TDD heart of the
// bench-vs-redis harness (the orchestration that actually runs the tools lives in
// cmd/cmbench).
//
// # Purity & the additive boundary
//
// This package consumes bytes (captured tool output) and produces values and strings.
// It imports no os/exec, no net, and no cachemoney engine package, so it is trivially
// unit-testable from committed fixtures (testdata/) at ~100% coverage. The shipped
// cmd/cachemoney binary and every internal engine package import NEITHER this package
// NOR cmd/cmbench — the benchmark is leaf tooling (an import guard enforces it).
//
// # Metric sourcing
//
// Each percentile is attributed to the tool that measures it, never fabricated:
//
//   - requests/sec for raw GET/SET → redis-benchmark (the canonical Redis throughput
//     number; --csv gives at least "test","rps", and p50/p99 when the build emits them);
//   - hit ratio + ops/sec + p50/p99/p99.9 under eviction → memtier_benchmark (its JSON
//     reports Hits/sec, Misses/sec, and the latency percentiles, including p99.9 which
//     redis-benchmark does not emit).
package bench

import "fmt"

// ParseError is the typed error the parsers return for malformed, empty, or unexpected
// tool output. It names the tool and what was wrong so the driver can report which
// capture failed; the parsers never panic.
type ParseError struct {
	Tool string
	Msg  string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("bench: %s: %s", e.Tool, e.Msg)
}
