package bench

import "testing"

func TestMedianThroughput(t *testing.T) {
	t.Parallel()

	runs := [][]ThroughputResult{
		{{Command: "GET", RPS: 100, P50: 0.2, P99: 0.5}, {Command: "SET", RPS: 90}},
		{{Command: "GET", RPS: 300, P50: 0.4, P99: 0.7}, {Command: "SET", RPS: 110}},
		{{Command: "GET", RPS: 200, P50: 0.3, P99: 0.6}, {Command: "SET", RPS: 100}},
	}
	got := MedianThroughput(runs)
	// Commands are emitted sorted; medians per command.
	if len(got) != 2 || got[0].Command != "GET" || got[1].Command != "SET" {
		t.Fatalf("MedianThroughput shape = %+v", got)
	}
	if got[0].RPS != 200 || got[0].P50 != 0.3 || got[0].P99 != 0.6 {
		t.Fatalf("GET medians = %+v, want rps 200 / p50 0.3 / p99 0.6", got[0])
	}
	if got[1].RPS != 100 {
		t.Fatalf("SET median rps = %v, want 100", got[1].RPS)
	}
}

func TestMedianThroughputEmpty(t *testing.T) {
	t.Parallel()
	if got := MedianThroughput(nil); len(got) != 0 {
		t.Fatalf("MedianThroughput(nil) = %v, want empty", got)
	}
}

func TestMedianHitRatio(t *testing.T) {
	t.Parallel()

	runs := []HitRatioResult{
		{HitRatio: 0.80, OpsPerSec: 1000, Lat: Latency{P999: 5}},
		{HitRatio: 0.90, OpsPerSec: 2000, Lat: Latency{P999: 7}},
		{HitRatio: 0.85, OpsPerSec: 1500, Lat: Latency{P999: 6}},
	}
	got := MedianHitRatio(runs)
	// Median ratio is 0.85 → returns that exact (co-measured) run, not a synthesized one.
	if got.HitRatio != 0.85 || got.OpsPerSec != 1500 || got.Lat.P999 != 6 {
		t.Fatalf("MedianHitRatio = %+v, want the 0.85 run", got)
	}
}

func TestMedianHitRatioEmpty(t *testing.T) {
	t.Parallel()
	if got := MedianHitRatio(nil); got != (HitRatioResult{}) {
		t.Fatalf("MedianHitRatio(nil) = %+v, want zero value", got)
	}
}
