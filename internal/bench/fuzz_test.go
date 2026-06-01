package bench

import (
	"math"
	"testing"
)

// The parsers must never panic on arbitrary tool output — only return a typed error
// or a valid result. These fuzzers drive arbitrary bytes through both.

func FuzzParseRedisBench(f *testing.F) {
	f.Add([]byte("\"GET\",\"123.45\"\r\n"))
	f.Add([]byte("\"test\",\"rps\",\"p50_latency_ms\"\r\n\"GET\",\"1\",\"0.1\"\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		results, err := ParseRedisBench(data)
		if err != nil {
			return // a typed error is fine
		}
		for _, r := range results {
			if r.RPS < 0 || math.IsNaN(r.RPS) || math.IsInf(r.RPS, 0) {
				t.Fatalf("non-finite/negative rps parsed from %q: %+v", data, r)
			}
		}
	})
}

func FuzzParseMemtier(f *testing.F) {
	f.Add([]byte(`{"ALL STATS":{"Totals":{"Hits/sec":9,"Misses/sec":1}}}`))
	f.Add([]byte("{}"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := ParseMemtier(data)
		if err != nil {
			return
		}
		if got.HitRatio < 0 || got.HitRatio > 1 {
			t.Fatalf("hit ratio out of [0,1] from %q: %v", data, got.HitRatio)
		}
	})
}
