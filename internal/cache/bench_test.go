package cache

import (
	"container/list"
	"context"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

// ===========================================================================
// Eviction PR C / I6 — Zipfian hit-ratio benchmark (the policy's proof artifact)
// ===========================================================================
//
// A self-contained, reproducible synthetic cache-aside experiment (no external Redis,
// no captured trace): replay the SAME fixed-seed Zipfian access trace under
// allkeys-lfu, allkeys-random, and a naive container/list LRU at the same capacity,
// and compare hit ratios. Under skew, frequency-aware eviction retains the heavy
// hitters, so allkeys-lfu must beat both baselines (spec Req 6 s2, design §9).

const (
	zipfKeySpace = 10000  // distinct keys
	zipfTraceLen = 300000 // total accesses
	zipfWarmup   = 50000  // unmeasured fill/age phase
	zipfSkew     = 1.3    // Zipf exponent (Go's NewZipf requires s > 1)
	zipfValueLen = 32     // fixed value size isolates the policy from cost effects
	zipfCapacity = 100    // entries the cache/LRU may hold (~1% of the key space)
	zipfKeyWidth = 5      // zero-padded key width (keeps per-entry cost uniform)
)

// zipfTrace builds a deterministic Zipfian access trace of fixed-width keys.
func zipfTrace(seed int64) []string {
	r := rand.New(rand.NewSource(seed))
	z := rand.NewZipf(r, zipfSkew, 1, zipfKeySpace-1)
	trace := make([]string, zipfTraceLen)
	for i := range trace {
		trace[i] = padKey(z.Uint64())
	}
	return trace
}

// padKey renders k as a fixed-width zero-padded decimal so every key has the same
// length (and therefore the same accounted cost).
func padKey(k uint64) string {
	s := strconv.FormatUint(k, 10)
	for len(s) < zipfKeyWidth {
		s = "0" + s
	}
	return s
}

// zipfMaxMemory sizes the byte ceiling to hold ~zipfCapacity uniform entries, so the
// engine and the count-based LRU baseline compare at the same effective capacity.
func zipfMaxMemory() int64 {
	return int64(zipfCapacity) * costOf(padKey(0), make([]byte, zipfValueLen))
}

// engineHitRatio replays the trace cache-aside through the engine under policy: a hit
// counts; a miss loads the key (the SET that drives eviction). Hits are counted over
// the measured phase only.
func engineHitRatio(trace []string, policy EvictionPolicy) float64 {
	c := New(WithMaxMemory(zipfMaxMemory()), WithEvictionPolicy(policy), WithShards(4))
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	val := make([]byte, zipfValueLen)

	hits, total := 0, 0
	for i, k := range trace {
		measured := i >= zipfWarmup
		if _, ok, _ := c.Get(ctx, k); ok {
			if measured {
				hits++
			}
		} else {
			_ = c.Set(ctx, k, val, 0)
		}
		if measured {
			total++
		}
		if i == zipfWarmup {
			time.Sleep(100 * time.Millisecond) // let the async drainer fold the warmup
		}
	}
	return float64(hits) / float64(total)
}

// lruHitRatio replays the same trace through an in-file naive LRU (container/list +
// map) at the same entry capacity — the recency baseline.
func lruHitRatio(trace []string) float64 {
	ll := list.New()
	index := make(map[string]*list.Element, zipfCapacity)

	hits, total := 0, 0
	for i, k := range trace {
		measured := i >= zipfWarmup
		if el, ok := index[k]; ok {
			ll.MoveToFront(el)
			if measured {
				hits++
			}
		} else {
			index[k] = ll.PushFront(k)
			if ll.Len() > zipfCapacity {
				tail := ll.Back()
				ll.Remove(tail)
				delete(index, tail.Value.(string))
			}
		}
		if measured {
			total++
		}
	}
	return float64(hits) / float64(total)
}

// TestZipfianHitRatioLFUBeatsBaselines is the proof: on a skewed trace, allkeys-lfu's
// hit ratio is strictly higher than both allkeys-random and naive-LRU at the same
// capacity (spec Req 6 s2). The three ratios are published via t.Log (run with -v).
func TestZipfianHitRatioLFUBeatsBaselines(t *testing.T) {
	trace := zipfTrace(0xCACE)
	lfu := engineHitRatio(trace, PolicyAllKeysLFU)
	random := engineHitRatio(trace, PolicyAllKeysRandom)
	lru := lruHitRatio(trace)

	t.Logf("Zipfian hit ratios (s=%.2f, K=%d, cap=%d entries, measured N=%d): "+
		"allkeys-lfu=%.4f  allkeys-random=%.4f  naive-LRU=%.4f",
		zipfSkew, zipfKeySpace, zipfCapacity, zipfTraceLen-zipfWarmup, lfu, random, lru)

	if lfu <= random {
		t.Errorf("allkeys-lfu hit ratio %.4f did not beat allkeys-random %.4f", lfu, random)
	}
	if lfu <= lru {
		t.Errorf("allkeys-lfu hit ratio %.4f did not beat naive-LRU %.4f", lfu, lru)
	}
}

// BenchmarkZipfianHitRatio publishes the three hit ratios as benchmark metrics
// (run with: go test -run='^$' -bench=ZipfianHitRatio ./internal/cache).
func BenchmarkZipfianHitRatio(b *testing.B) {
	trace := zipfTrace(0xCACE)
	for i := 0; i < b.N; i++ {
		lfu := engineHitRatio(trace, PolicyAllKeysLFU)
		random := engineHitRatio(trace, PolicyAllKeysRandom)
		lru := lruHitRatio(trace)
		b.ReportMetric(lfu, "lfu-hit-ratio")
		b.ReportMetric(random, "random-hit-ratio")
		b.ReportMetric(lru, "lru-hit-ratio")
	}
}
