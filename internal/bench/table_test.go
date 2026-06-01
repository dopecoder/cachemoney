package bench

import (
	"strings"
	"testing"
)

func sampleSuite() Suite {
	// Intentionally NOT in canonical order, to prove RenderTable sorts. GET throughput
	// carries redis-benchmark p50/p99 (raw); Hit carries memtier p99.9 (under load).
	return Suite{
		Results: []Result{
			{
				Server:     "valkey",
				Throughput: []ThroughputResult{{Command: "GET", RPS: 200000, P50: 0.21, P99: 0.55}, {Command: "SET", RPS: 190000}},
				Hit:        &HitRatioResult{HitRatio: 0.84, OpsPerSec: 90000, Lat: Latency{P999: 3.1}},
			},
			{
				Server:     "cachemoney",
				Throughput: []ThroughputResult{{Command: "GET", RPS: 185000, P50: 0.207, P99: 0.567}, {Command: "SET", RPS: 178000}},
				Hit:        &HitRatioResult{HitRatio: 0.88, OpsPerSec: 95000, Lat: Latency{P999: 3.78}},
			},
		},
		Skipped: []string{"pogocache", "redis"},
	}
}

func TestRenderTableIsDeterministic(t *testing.T) {
	t.Parallel()

	s := sampleSuite()
	a := RenderTable(s)
	b := RenderTable(s)
	if a != b {
		t.Fatalf("RenderTable is not deterministic:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

func TestRenderTableCanonicalOrderingAndContent(t *testing.T) {
	t.Parallel()

	out := RenderTable(sampleSuite())
	// Canonical order places cachemoney before valkey regardless of input order.
	ci := strings.Index(out, "cachemoney")
	vi := strings.Index(out, "valkey")
	if ci < 0 || vi < 0 || ci > vi {
		t.Fatalf("canonical ordering not honored (cachemoney @%d, valkey @%d):\n%s", ci, vi, out)
	}
	// Skipped servers surface as a footnote.
	if !strings.Contains(out, "pogocache") || !strings.Contains(strings.ToLower(out), "skip") {
		t.Fatalf("skipped-server footnote missing:\n%s", out)
	}
}

func TestRenderTableLatencySourcing(t *testing.T) {
	t.Parallel()

	// p50/p99 come from the redis-benchmark GET result (raw); p99.9 from memtier (Hit).
	out := RenderTable(sampleSuite())
	if !strings.Contains(out, "0.207") || !strings.Contains(out, "0.567") {
		t.Fatalf("table missing redis-benchmark GET p50/p99 (0.207/0.567):\n%s", out)
	}
	if !strings.Contains(out, "3.780") {
		t.Fatalf("table missing memtier p99.9 (3.780):\n%s", out)
	}
}

func TestMedian(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   []float64
		want float64
	}{
		{in: nil, want: 0},
		{in: []float64{5}, want: 5},
		{in: []float64{3, 1, 2}, want: 2},      // odd → middle of sorted
		{in: []float64{4, 1, 3, 2}, want: 2.5}, // even → mean of two middle
		{in: []float64{9, 9, 1, 1}, want: 5},   // even, unsorted input
	}
	for _, tc := range cases {
		if got := Median(tc.in); got != tc.want {
			t.Errorf("Median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
