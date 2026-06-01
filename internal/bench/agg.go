package bench

import (
	"math"
	"sort"
)

// MedianThroughput reduces several redis-benchmark repeats to one result per command,
// taking the median of rps, p50, and p99 across the repeats. Commands are emitted in a
// stable (sorted) order so the result is deterministic. An empty input yields nil.
func MedianThroughput(runs [][]ThroughputResult) []ThroughputResult {
	type acc struct{ rps, p50, p99 []float64 }
	byCmd := map[string]*acc{}
	var order []string
	for _, run := range runs {
		for _, tr := range run {
			a, ok := byCmd[tr.Command]
			if !ok {
				a = &acc{}
				byCmd[tr.Command] = a
				order = append(order, tr.Command)
			}
			a.rps = append(a.rps, tr.RPS)
			a.p50 = append(a.p50, tr.P50)
			a.p99 = append(a.p99, tr.P99)
		}
	}
	sort.Strings(order)
	out := make([]ThroughputResult, 0, len(order))
	for _, cmd := range order {
		a := byCmd[cmd]
		out = append(out, ThroughputResult{Command: cmd, RPS: Median(a.rps), P50: Median(a.p50), P99: Median(a.p99)})
	}
	return out
}

// MedianHitRatio reduces several memtier repeats to the single repeat whose hit ratio is
// closest to the median ratio — preserving a coherent (real, co-measured) latency set
// rather than synthesizing one. An empty input yields the zero value.
func MedianHitRatio(runs []HitRatioResult) HitRatioResult {
	if len(runs) == 0 {
		return HitRatioResult{}
	}
	ratios := make([]float64, len(runs))
	for i, r := range runs {
		ratios[i] = r.HitRatio
	}
	med := Median(ratios)
	best := runs[0]
	for _, r := range runs {
		if math.Abs(r.HitRatio-med) < math.Abs(best.HitRatio-med) {
			best = r
		}
	}
	return best
}
