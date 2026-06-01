package bench

import "encoding/json"

// ParseMemtier parses a memtier_benchmark --json-out-file document into a hit-ratio
// result: the GET hit ratio derived from the Hits/sec and Misses/sec rates, the
// aggregate ops/sec, and the p50/p99/p99.9 latency percentiles. Malformed JSON, a
// document with no GET hits/misses, or negative rates yield a *ParseError; it never
// panics.
func ParseMemtier(data []byte) (HitRatioResult, error) {
	var doc struct {
		AllStats struct {
			Totals struct {
				OpsPerSec    float64            `json:"Ops/sec"`
				HitsPerSec   float64            `json:"Hits/sec"`
				MissesPerSec float64            `json:"Misses/sec"`
				Percentiles  map[string]float64 `json:"Percentile Latencies"`
			} `json:"Totals"`
		} `json:"ALL STATS"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return HitRatioResult{}, &ParseError{Tool: "memtier", Msg: "invalid JSON: " + err.Error()}
	}

	t := doc.AllStats.Totals
	if t.HitsPerSec < 0 || t.MissesPerSec < 0 {
		return HitRatioResult{}, &ParseError{Tool: "memtier", Msg: "negative hit/miss rate"}
	}
	total := t.HitsPerSec + t.MissesPerSec
	if total <= 0 {
		return HitRatioResult{}, &ParseError{Tool: "memtier", Msg: "no GET hits/misses in output"}
	}
	return HitRatioResult{
		HitsPerSec:   t.HitsPerSec,
		MissesPerSec: t.MissesPerSec,
		HitRatio:     t.HitsPerSec / total,
		OpsPerSec:    t.OpsPerSec,
		Lat: Latency{
			P50:  t.Percentiles["p50.00"],
			P99:  t.Percentiles["p99.00"],
			P999: t.Percentiles["p99.90"],
		},
	}, nil
}
