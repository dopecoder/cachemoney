package bench

import "sort"

// Latency holds tail-latency percentiles in milliseconds. P999 (p99.9) is sourced from
// memtier; redis-benchmark does not emit it.
type Latency struct {
	P50, P99, P999 float64
}

// ThroughputResult is one redis-benchmark command result: requests/sec, plus p50/p99
// when the build's --csv emitted them (0 otherwise — the table sources latency from
// memtier).
type ThroughputResult struct {
	Command  string
	RPS      float64
	P50, P99 float64
}

// HitRatioResult is one memtier run result at a fixed maxmemory: the hit ratio derived
// from the GET hit/miss rates, the aggregate ops/sec, and the latency percentiles.
type HitRatioResult struct {
	HitsPerSec, MissesPerSec float64
	HitRatio                 float64
	OpsPerSec                float64
	Lat                      Latency
}

// Result is one server's combined throughput and hit-ratio measurements.
type Result struct {
	Server     string
	Throughput []ThroughputResult
	Hit        *HitRatioResult
}

// Suite is the whole comparison: a result per measured server plus the names of the
// servers that were skipped (no tooling available).
type Suite struct {
	Results []Result
	Skipped []string
}

// Median returns the median of xs (mean of the two middle values for an even count). An
// empty slice yields 0. The input is not mutated.
func Median(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	c := make([]float64, n)
	copy(c, xs)
	sort.Float64s(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}
