package shardmap

import (
	"hash/maphash"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"unsafe"

	"github.com/dopecoder/cachemoney/internal/hash"
)

// hashByFNV is a deterministic, seed-independent hasher for routing-controlled
// tests: it ignores the seed and returns fnv64(key), so a system-under-test and a
// reference model route identically without depending on maphash randomness.
func hashByFNV(_ maphash.Seed, key string) uint64 { return fnv64(key) }

// ===========================================================================
// Increment 4 — RED: routing helpers (high bits → shard, low bits → bucket)
// ===========================================================================

// TestRouting_HighLowSplitDisjoint pins the design §5 invariant: shardIndex reads
// only the HIGH bits of the 64-bit hash and the home bucket reads only the LOW
// bits, and the two ranges never overlap (spec "Hash split selects shard from high
// bits and bucket from low bits"). It exercises the pure helpers directly.
func TestRouting_HighLowSplitDisjoint(t *testing.T) {
	t.Parallel()

	const numShards = 16 // s = 4 high bits
	shardBits := log2PowerOf2(numShards)
	if shardBits != 4 {
		t.Fatalf("log2PowerOf2(%d) = %d, want 4", numShards, shardBits)
	}
	shift := uint(64) - shardBits // 60
	const capm1 = uint64(7)       // cap 8 => low 3 bits select the bucket

	highOnly := uint64(0xF) << shift // only the top 4 bits set
	lowOnly := uint64(0x7)           // only the low 3 bits set

	// shardIndex sees ONLY the high bits.
	if got := shardIndex(highOnly, shift); got != 0xF {
		t.Fatalf("shardIndex(highOnly) = %d, want 15", got)
	}
	if got := shardIndex(lowOnly, shift); got != 0 {
		t.Fatalf("shardIndex(lowOnly) = %d, want 0 (low bits invisible to shard select)", got)
	}

	// home (hash & capm1) sees ONLY the low bits.
	if got := lowOnly & capm1; got != 0x7 {
		t.Fatalf("home(lowOnly) = %d, want 7", got)
	}
	if got := highOnly & capm1; got != 0 {
		t.Fatalf("home(highOnly) = %d, want 0 (high bits invisible to bucket select)", got)
	}

	// Disjointness: shard bits (top s) and bucket bits (low capBits) cannot overlap
	// as long as s + capBits <= 64. Here 4 + 3 = 7.
	capBits := log2PowerOf2(int(capm1) + 1)
	if shardBits+capBits > 64 {
		t.Fatalf("shard bits %d + bucket bits %d overlap (> 64)", shardBits, capBits)
	}

	// Varying only the low bits must never change the shard selection.
	base := uint64(0xA) << shift
	for low := uint64(0); low < 8; low++ {
		if got := shardIndex(base|low, shift); got != 0xA {
			t.Fatalf("shardIndex changed by low bits: got %d, want 10", got)
		}
	}
}

// TestShardIndex_SingleShardAlwaysZero pins that a single-shard map (shift 64)
// routes every key to shard 0 — Go defines x>>64 == 0 for uint64, so no key can
// index out of a 1-element shard array.
func TestShardIndex_SingleShardAlwaysZero(t *testing.T) {
	t.Parallel()

	shift := uint(64) - log2PowerOf2(1) // 64
	for _, h := range []uint64{0, 1, 0xFFFF_FFFF_FFFF_FFFF, 0x8000_0000_0000_0000} {
		if got := shardIndex(h, shift); got != 0 {
			t.Fatalf("shardIndex(%#x, 64) = %d, want 0", h, got)
		}
	}
}

// ===========================================================================
// Increment 4 — RED: shard count derivation (WithShards / default)
// ===========================================================================

func TestWithShards_RoundsUpToPowerOfTwo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int
		want int
	}{
		{1, 1}, {2, 2}, {3, 4}, {5, 8}, {8, 8}, {9, 16}, {17, 32}, {64, 64},
	}
	for _, tc := range cases {
		m := New[string, int](hashByFNV, WithShards(tc.in))
		if got := len(m.shards); got != tc.want {
			t.Fatalf("WithShards(%d) => %d shards, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWithShards_FloorOne pins that non-positive shard counts floor to a single
// shard rather than zero (which would divide-by-zero / index an empty array).
func TestWithShards_FloorOne(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -64} {
		m := New[string, int](hashByFNV, WithShards(n))
		if got := len(m.shards); got != 1 {
			t.Fatalf("WithShards(%d) => %d shards, want 1 (floor)", n, got)
		}
	}
}

// TestDefaultShardCount pins design §5: the default count is next_pow2(GOMAXPROCS×4),
// floor 1, a power of two.
func TestDefaultShardCount(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV)
	got := len(m.shards)
	want := nextPow2(runtime.GOMAXPROCS(0) * 4)
	if got != want {
		t.Fatalf("default shard count = %d, want next_pow2(GOMAXPROCS×4) = %d", got, want)
	}
	if got < 1 || got&(got-1) != 0 {
		t.Fatalf("default shard count %d is not a power of two >= 1", got)
	}
}

// ===========================================================================
// Increment 4 — RED: Map round-trip across shards (Get/Set/Delete/Len/Range)
// ===========================================================================

func TestMap_RoundTripAcrossShards(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(16))
	const n = 500
	for i := 0; i < n; i++ {
		m.Set("key"+strconv.Itoa(i), i)
	}
	if got := m.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}
	for i := 0; i < n; i++ {
		if v, ok := m.Get("key" + strconv.Itoa(i)); !ok || v != i {
			t.Fatalf("Get(key%d) = (%d, %v), want (%d, true)", i, v, ok, i)
		}
	}
	if _, ok := m.Get("absent"); ok {
		t.Fatalf("Get(absent) = true, want false")
	}

	// Delete half, confirm presence reporting and Len drop.
	for i := 0; i < n; i += 2 {
		v, ok := m.Delete("key" + strconv.Itoa(i))
		if !ok || v != i {
			t.Fatalf("Delete(key%d) = (%d, %v), want (%d, true)", i, v, ok, i)
		}
	}
	if v, ok := m.Delete("key0"); ok || v != 0 {
		t.Fatalf("re-Delete(key0) = (%d, %v), want (0, false)", v, ok)
	}
	if got := m.Len(); got != n/2 {
		t.Fatalf("Len after deleting half = %d, want %d", got, n/2)
	}
	for i := 1; i < n; i += 2 {
		if v, ok := m.Get("key" + strconv.Itoa(i)); !ok || v != i {
			t.Fatalf("survivor Get(key%d) = (%d, %v), want (%d, true)", i, v, ok, i)
		}
	}
}

// TestMap_LenEqualsSumOfShardCounts pins design §2.2: Len is the Σ of per-shard
// live counts (read-locked), not a separately tracked total.
func TestMap_LenEqualsSumOfShardCounts(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(8))
	const n = 300
	for i := 0; i < n; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	sum := 0
	for i := range m.shards {
		sum += m.shards[i].t.count
	}
	if got := m.Len(); got != sum || sum != n {
		t.Fatalf("Len = %d, Σ shard counts = %d, want both %d", got, sum, n)
	}
}

func TestMap_RangeVisitsEveryEntry(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(16))
	want := map[string]int{}
	const n = 200
	for i := 0; i < n; i++ {
		k := "k" + strconv.Itoa(i)
		m.Set(k, i)
		want[k] = i
	}
	got := map[string]int{}
	m.Range(func(k string, v int) bool {
		got[k] = v
		return true
	})
	if len(got) != len(want) {
		t.Fatalf("Range visited %d entries, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("Range value for %q = %d, want %d", k, got[k], v)
		}
	}
}

func TestMap_RangeStopsOnFalse(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(4))
	for i := 0; i < 50; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	visited := 0
	m.Range(func(string, int) bool {
		visited++
		return false // stop immediately
	})
	if visited != 1 {
		t.Fatalf("Range visited %d entries after returning false, want 1", visited)
	}
}

func TestMap_EmptyRangeAndLen(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(8))
	if got := m.Len(); got != 0 {
		t.Fatalf("empty Len = %d, want 0", got)
	}
	calls := 0
	m.Range(func(string, int) bool {
		calls++
		return true
	})
	if calls != 0 {
		t.Fatalf("empty Range made %d callbacks, want 0", calls)
	}
}

// ===========================================================================
// Increment 4 — TRIANGULATE
// ===========================================================================

// TestMap_SingleVsManyShardEquivalence drives the same op sequence against a
// 1-shard and a 64-shard map (same hasher, same seed) and asserts identical
// observable behavior — sharding must not change semantics (design §6).
func TestMap_SingleVsManyShardEquivalence(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	one := New[string, int](hashByFNV, WithShards(1), WithSeed(seed))
	many := New[string, int](hashByFNV, WithShards(64), WithSeed(seed))

	keys := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		k := "key" + strconv.Itoa(i%150) // force overwrites and re-touches
		keys = append(keys, k)
		switch i % 4 {
		case 0, 1:
			one.Set(k, i)
			many.Set(k, i)
		case 2:
			o, ok1 := one.Get(k)
			mv, ok2 := many.Get(k)
			if ok1 != ok2 || o != mv {
				t.Fatalf("Get(%q): one=(%d,%v) many=(%d,%v)", k, o, ok1, mv, ok2)
			}
		default:
			_, ok1 := one.Delete(k)
			_, ok2 := many.Delete(k)
			if ok1 != ok2 {
				t.Fatalf("Delete(%q): one existed=%v many existed=%v", k, ok1, ok2)
			}
		}
		if one.Len() != many.Len() {
			t.Fatalf("op %d: Len one=%d many=%d", i, one.Len(), many.Len())
		}
	}
	// Final reconciliation over the full key space.
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		o, ok1 := one.Get(k)
		mv, ok2 := many.Get(k)
		if ok1 != ok2 || o != mv {
			t.Fatalf("final Get(%q): one=(%d,%v) many=(%d,%v)", k, o, ok1, mv, ok2)
		}
	}
}

// TestMap_RoutingStableUnderFixedSeed pins that, for a fixed seed, a key routes to
// the same shard every time (deterministic routing — design §5, the property the
// property suite relies on in increment 5).
func TestMap_RoutingStableUnderFixedSeed(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	m := New[string, int](hash.String, WithShards(64), WithSeed(seed))
	for i := 0; i < 64; i++ {
		k := "stable" + strconv.Itoa(i)
		h := hash.String(seed, k)
		want := h >> m.shardShift
		for r := 0; r < 16; r++ {
			if got := hash.String(seed, k) >> m.shardShift; got != want {
				t.Fatalf("routing for %q drifted: got %d want %d", k, got, want)
			}
		}
		if want >= uint64(len(m.shards)) {
			t.Fatalf("shard index %d out of range for %d shards", want, len(m.shards))
		}
	}
}

// TestMap_WithInitialCapacity pins that the capacity hint is split across shards so
// none grows immediately (design §2.2: hint split across shards).
func TestMap_WithInitialCapacity(t *testing.T) {
	t.Parallel()

	const shards = 8
	const hint = 800 // 100 per shard
	m := New[string, int](hashByFNV, WithShards(shards), WithInitialCapacity(hint))
	perShard := hint / shards
	for i := range m.shards {
		if m.shards[i].t.growAt < perShard {
			t.Fatalf("shard %d growAt = %d, want >= %d (capacity hint not split)",
				i, m.shards[i].t.growAt, perShard)
		}
	}
}

// TestShard_CacheLinePadding is the design §4.3 acceptance witness: a shard struct
// is padded to a whole number of 64-byte cache lines so two adjacent shards' hot
// mu fields never share a line (false-sharing mitigation). If this ever fails on a
// new arch/Go version, adopt the []*shard fallback documented in map.go.
func TestShard_CacheLinePadding(t *testing.T) {
	t.Parallel()

	sz := unsafe.Sizeof(shard[string, int]{})
	if sz%cacheLineSize != 0 {
		t.Fatalf("sizeof(shard) = %d, not a multiple of cache line %d", sz, cacheLineSize)
	}
	if sz < cacheLineSize {
		t.Fatalf("sizeof(shard) = %d, want >= %d", sz, cacheLineSize)
	}
}

// ===========================================================================
// Increment 4 — `-race` concurrency smoke (spec: Concurrent mixed operations are
// race-free; Reads do not mutate shared state)
// ===========================================================================

// TestMap_ConcurrentMixedOps hammers Get/Set/Delete/Len/Range from many goroutines
// across a shared key space. Run under `go test -race` it must report no data race;
// afterward every shard's table still satisfies the Robin Hood invariants.
func TestMap_ConcurrentMixedOps(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(16))
	const (
		workers    = 24
		iterations = 2000
		keySpace   = 256
	)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				k := "k" + strconv.Itoa((id*7+i)%keySpace)
				switch i % 5 {
				case 0, 1:
					m.Set(k, i)
				case 2:
					m.Get(k)
				case 3:
					if i%2 == 0 {
						m.Delete(k)
					} else {
						_ = m.Len()
					}
				default:
					m.Range(func(string, int) bool { return true })
				}
			}
		}(w)
	}
	wg.Wait()

	// No concurrent access remains: every shard's table must still be structurally
	// sound (no holes / no tombstones / DIB monotonic / count agrees).
	for i := range m.shards {
		checkInvariants(t, &m.shards[i].t)
	}
}

// TestMap_ConcurrentReadersOnly confirms read-only ops (Get/Len/Range) under heavy
// parallelism never race and never change stored state (spec "Reads do not mutate
// shared state").
func TestMap_ConcurrentReadersOnly(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(8))
	const n = 300
	for i := 0; i < n; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	before := snapshot(m)

	var wg sync.WaitGroup
	const readers = 16
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				m.Get("k" + strconv.Itoa(i%n))
				_ = m.Len()
				m.Range(func(string, int) bool { return true })
			}
		}()
	}
	wg.Wait()

	after := snapshot(m)
	if len(before) != len(after) {
		t.Fatalf("reader workload changed entry count: %d -> %d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("reader workload mutated %q: %d -> %d", k, v, after[k])
		}
	}
	if m.Len() != n {
		t.Fatalf("Len after readers = %d, want %d", m.Len(), n)
	}
}

// snapshot collects every live entry via Range into a plain map for before/after
// comparison.
func snapshot(m *Map[string, int]) map[string]int {
	out := map[string]int{}
	m.Range(func(k string, v int) bool {
		out[k] = v
		return true
	})
	return out
}

// TestMap_NewNilHasherPanics locks the documented contract that New requires a
// non-nil hasher: no operation could route a key without one.
func TestMap_NewNilHasherPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatalf("New(nil hasher) did not panic")
		}
	}()
	_ = New[string, int](nil)
}
