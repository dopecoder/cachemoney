package shardmap

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dopecoder/cachemoney/internal/hash"
)

// ===========================================================================
// Increment 9 — concurrency benchmark: sharded Robin Hood map vs stdlib
// map + sync.RWMutex (design §11; ADR-0005/0010; spec "Concurrency performance
// acceptance").
// ===========================================================================
//
// The benchmark lives at the shardmap layer on purpose: both contenders are plain
// generic [K]V containers, so the comparison isolates the DATA-STRUCTURE axis and
// carries none of the engine's TTL / defensive-copy / context overhead (design
// §11). The acceptance bar is DIRECTIONAL — the sharded map's ns/op must be lower
// than the stdlib-map+RWMutex baseline under parallel contention; there is no
// fixed-multiple gate (§9-Q2 / ADR-0010). Measured numbers and methodology are
// recorded honestly in BENCH.md.
//
// Run:
//
//	go test -bench=. -benchmem -run='^$' ./internal/shardmap
//	go test -bench=BenchmarkConcurrent -benchmem -run='^$' -cpu=1,2,4,8,16 ./internal/shardmap
//
// The -cpu sweep is the core-scaling / false-sharing acceptance check (§4.3): the
// sharded map's ns/op should hold roughly flat (throughput scales with cores) while
// the single-lock baseline degrades; if the sharded map instead degrades as cores
// rise, that flags the cache-line padding or shard count (see BenchmarkScaling).

// concurrentMap is the minimal Get/Set surface both contenders expose. The parallel
// benchmark loop drives the map through this interface so the two implementations
// run byte-identical workloads; the interface-dispatch cost (if any) is paid equally
// by both, so the directional comparison stays apples-to-apples.
type concurrentMap[K comparable, V any] interface {
	Get(key K) (V, bool)
	Set(key K, value V)
}

// rwMutexMap is the baseline: a stdlib map guarded by a single sync.RWMutex. It is
// the obvious, correct, non-sharded design the sharded Map must beat under parallel
// contention (ADR-0005). Get takes the read lock (readers parallelize but still
// contend on the RWMutex reader count); Set takes the exclusive write lock, so every
// writer serializes on the one lock — the bottleneck sharding removes.
type rwMutexMap[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

// newRWMutexMap builds the baseline pre-sized for capacity entries so neither
// contender pays map-grow cost during the timed loop (the sharded Map is likewise
// pre-sized via WithInitialCapacity).
func newRWMutexMap[K comparable, V any](capacity int) *rwMutexMap[K, V] {
	return &rwMutexMap[K, V]{m: make(map[K]V, capacity)}
}

// Get returns the value stored under key and whether it was present, under the read
// lock.
func (r *rwMutexMap[K, V]) Get(key K) (V, bool) {
	r.mu.RLock()
	v, ok := r.m[key]
	r.mu.RUnlock()
	return v, ok
}

// Set stores value under key under the exclusive write lock.
func (r *rwMutexMap[K, V]) Set(key K, value V) {
	r.mu.Lock()
	r.m[key] = value
	r.mu.Unlock()
}

// benchKeys returns n distinct string keys ("key:0" … "key:n-1"). String keys match
// the engine's real Map[string, entry] usage and route through the seeded hash.
func benchKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "key:" + strconv.Itoa(i)
	}
	return keys
}

// prepopulate fills m with every key (value = its index) before timing. It runs
// through the concurrentMap interface so both contenders are seeded identically.
func prepopulate(m concurrentMap[string, int], keys []string) {
	for i, k := range keys {
		m.Set(k, i)
	}
}

// benchSeq hands each parallel goroutine a distinct PRNG seed without any shared
// rand: one atomic increment per goroutine at startup, outside the hot loop.
var benchSeq int64

// runConcurrent is the shared parallel workload. Each goroutine owns a private
// *rand.Rand (no shared rand, no shared state beyond the map under test) and runs a
// read/write mix: with probability readPct% it Gets a random pre-populated key,
// otherwise it Sets that key to a fresh value (an overwrite — the keys already
// exist, so no grow happens and the loop stays allocation-free, isolating the access
// cost). Requires GOMAXPROCS > 1 to exercise real contention.
func runConcurrent(b *testing.B, m concurrentMap[string, int], keys []string, readPct int) {
	b.Helper()
	n := len(keys)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// Per-goroutine PRNG: a distinct deterministic seed, never shared.
		rng := rand.New(rand.NewSource(atomic.AddInt64(&benchSeq, 1)))
		for pb.Next() {
			k := keys[rng.Intn(n)]
			if rng.Intn(100) < readPct {
				m.Get(k)
			} else {
				m.Set(k, rng.Int())
			}
		}
	})
}

// benchMix names a read/write split for the sub-benchmark labels.
type benchMix struct {
	name    string
	readPct int
}

// benchMixes are the two TRIANGULATE workloads: the default read-heavy 90/10 and a
// write-heavy 50/50 that stresses the baseline's single write lock the hardest. Two
// mixes (plus two key counts below) avoid cherry-picking one favorable point.
var benchMixes = []benchMix{
	{"read90", 90},
	{"write50", 50},
}

// benchKeyCounts are the two cardinalities: 1<<16 (the design's reference N) and a
// smaller 1<<12 that concentrates traffic on fewer keys, raising lock/shard
// contention — the second key count required by the increment-9 TRIANGULATE gate.
var benchKeyCounts = []int{1 << 16, 1 << 12}

// benchImpls pairs each contender's label with a constructor producing it behind the
// concurrentMap interface, pre-sized to the given capacity.
var benchImpls = []struct {
	name string
	make func(capacity int) concurrentMap[string, int]
}{
	{"shardmap", func(c int) concurrentMap[string, int] {
		return New[string, int](hash.String, WithInitialCapacity(c))
	}},
	{"rwmutex", func(c int) concurrentMap[string, int] {
		return newRWMutexMap[string, int](c)
	}},
}

// BenchmarkConcurrent is the acceptance benchmark. It sweeps {shardmap, rwmutex} ×
// {read90, write50} × {65536, 4096 keys} under b.RunParallel, reporting ns/op per
// combination. Compare matching shardmap/* and rwmutex/* sub-benchmarks: under
// parallel contention (GOMAXPROCS > 1) the sharded map's ns/op must be lower
// (directional bar). Add -cpu=1,2,4,8,16 to read the core-scaling / false-sharing
// signal (§4.3).
func BenchmarkConcurrent(b *testing.B) {
	for _, impl := range benchImpls {
		for _, mix := range benchMixes {
			for _, kc := range benchKeyCounts {
				keys := benchKeys(kc)
				name := fmt.Sprintf("%s/%s/keys=%d", impl.name, mix.name, kc)
				b.Run(name, func(b *testing.B) {
					m := impl.make(kc)
					prepopulate(m, keys)
					runConcurrent(b, m, keys, mix.readPct)
				})
			}
		}
	}
}

// BenchmarkScaling is the explicit parallelism sweep that doubles as the
// false-sharing / cache-line-padding acceptance check (design §4.3). For each
// contender it raises b.SetParallelism (goroutines = p × GOMAXPROCS) over a fixed
// read-heavy workload. The sharded map's ns/op should stay roughly flat as
// parallelism climbs — evidence the per-shard padding keeps adjacent shards off the
// same cache line and the sharding scales. If shardmap/* instead worsens sharply
// while rwmutex/* is the one expected to degrade, that is the signal to revisit the
// padding or shard count (the documented []*shard fallback, map.go). This is a
// diagnostic: it reports numbers, it does not fail the build.
func BenchmarkScaling(b *testing.B) {
	const keyCount = 1 << 16
	keys := benchKeys(keyCount)

	for _, impl := range benchImpls {
		for _, p := range []int{1, 2, 4, 8} {
			name := fmt.Sprintf("%s/parallelism=%d", impl.name, p)
			b.Run(name, func(b *testing.B) {
				m := impl.make(keyCount)
				prepopulate(m, keys)
				b.SetParallelism(p)
				runConcurrent(b, m, keys, 90)
			})
		}
	}
}
