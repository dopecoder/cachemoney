package shardmap

import (
	"bytes"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"unsafe"
)

// ===========================================================================
// Increment 5 — property / fuzz suite: structural invariant assertions
// ===========================================================================
//
// This file holds the white-box invariant hook the property/fuzz suite drives
// (checkMapInvariants) plus the dedicated invariant tests that do not fit the
// model-equivalence shape: grow/shrink entry preservation, the copy-on-write
// (stored-bytes-never-mutated-in-place) lock, and the V-independent shard-size
// constant the cache-line padding depends on (design §4.3, §9, §10).

// checkMapInvariants asserts the Robin Hood structural invariants on every shard of
// m. It is the sharded-Map-level white-box hook the property/fuzz suite uses to
// inspect per-shard tables (increment 5). It reuses the increment-3
// checkInvariants helper per shard — DIB monotonicity along every chain, no holes /
// no tombstones after deletes, the lookup early-exit reachability walk,
// occupied-slots == count, and the per-shard load-factor bound (count <= growAt,
// i.e. <= ~0.75) — and additionally reconciles Len with the sum of per-shard counts
// so a routing or count-drift bug cannot hide behind a shard boundary.
func checkMapInvariants[K comparable, V any](t *testing.T, m *Map[K, V]) {
	t.Helper()
	sum := 0
	for i := range m.shards {
		checkInvariants(t, &m.shards[i].t)
		sum += m.shards[i].t.count
	}
	if got := m.Len(); got != sum {
		t.Fatalf("Len %d != Σ per-shard counts %d", got, sum)
	}
}

// TestMap_GrowShrinkPreserveAllEntries runs a grow-then-mass-delete-then-shrink
// cycle across shards and asserts every live entry survives each resize, the load
// factor stays bounded throughout (checkMapInvariants enforces count <= growAt per
// shard), and no deleted key lingers — the sharded-scope form of "grow/shrink
// preserve all live entries" and "load factor stays bounded across grow and shrink"
// (design §10).
func TestMap_GrowShrinkPreserveAllEntries(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(8))
	model := make(map[string]int)

	// Grow phase: enough inserts to drive several grows in every shard.
	const n = 4000
	for i := 0; i < n; i++ {
		k := "k" + strconv.Itoa(i)
		m.Set(k, i)
		model[k] = i
	}
	checkMapInvariants(t, m)
	for k, want := range model {
		if v, ok := m.Get(k); !ok || v != want {
			t.Fatalf("after grow Get(%q) = (%d, %v), want (%d, true)", k, v, ok, want)
		}
	}

	// Mass-delete 90% to drive shrink across shards; keep every 10th key.
	for i := 0; i < n; i++ {
		if i%10 == 0 {
			continue
		}
		k := "k" + strconv.Itoa(i)
		if _, ok := m.Delete(k); !ok {
			t.Fatalf("Delete(%q) = false, want true", k)
		}
		delete(model, k)
	}
	checkMapInvariants(t, m)
	if got := m.Len(); got != len(model) {
		t.Fatalf("Len after mass-delete = %d, want %d", got, len(model))
	}
	for k, want := range model {
		if v, ok := m.Get(k); !ok || v != want {
			t.Fatalf("after shrink Get(%q) = (%d, %v), want (%d, true)", k, v, ok, want)
		}
	}
	for i := 0; i < n; i++ {
		if i%10 == 0 {
			continue
		}
		k := "k" + strconv.Itoa(i)
		if _, ok := m.Get(k); ok {
			t.Fatalf("stale Get(%q) = true, want false after delete", k)
		}
	}
}

// TestMap_CopyOnWriteStoredBytesNeverMutated locks the load-bearing design §9
// invariant the engine's clone-outside-the-lock depends on.
//
// shardmap is generic over V and stores whatever it is given; defensively copying
// values is the ENGINE's responsibility, not shardmap's (shardmap never clones an
// input or output). The complementary half — the one tested here — is that
// shardmap's own operations (overwrite, grow, shrink, backward-shift) only ever
// MOVE whole V values (slice headers) between slots and NEVER write into a stored
// value's backing byte array in place. That in-place immutability is precisely what
// makes it sound for the engine to clone a Get result after releasing the shard
// lock. (This is structurally guaranteed because shardmap has no []byte-specific
// code; the test is the regression lock that keeps it that way.)
//
// We record every []byte ever stored — the exact backing slice plus an independent
// deep-copy snapshot of its bytes at store time — then drive heavy churn that
// triggers grow, shrink, overwrite, and backward-shift, and assert no recorded
// backing array's bytes ever changed.
func TestMap_CopyOnWriteStoredBytesNeverMutated(t *testing.T) {
	t.Parallel()

	m := New[string, []byte](hashByFNV, WithShards(4))
	rng := rand.New(rand.NewSource(0xC0DEC0FFEE))

	type rec struct {
		backing []byte // the exact slice shardmap stores (shares its backing array)
		want    []byte // independent deep copy of those bytes at store time
	}
	var stored []rec
	live := make(map[string][]byte) // key -> expected current bytes

	const (
		ops      = 6000
		keySpace = 300
	)
	for n := 0; n < ops; n++ {
		k := "k" + strconv.Itoa(rng.Intn(keySpace))
		switch rng.Intn(3) {
		case 0, 1: // set/overwrite with a fresh, distinct, non-empty value
			v := make([]byte, 1+rng.Intn(48))
			for j := range v {
				v[j] = byte(rng.Intn(256))
			}
			m.Set(k, v)
			stored = append(stored, rec{backing: v, want: append([]byte(nil), v...)})
			live[k] = append([]byte(nil), v...)
		default: // delete (drives backward-shift + shrink)
			m.Delete(k)
			delete(live, k)
		}
	}

	// 1) shardmap never byte-mutated, in place, ANY value it ever stored — including
	//    values whose keys were later overwritten or deleted.
	for i, r := range stored {
		if !bytes.Equal(r.backing, r.want) {
			t.Fatalf("stored value #%d mutated in place by shardmap: got %x, want %x",
				i, r.backing, r.want)
		}
	}
	// 2) every currently-live key reads back exactly the bytes last stored.
	for k, want := range live {
		got, ok := m.Get(k)
		if !ok {
			t.Fatalf("live key %q missing after churn", k)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Get(%q) = %x, want %x", k, got, want)
		}
	}
	checkMapInvariants(t, m)
}

// TestShard_SizeConstantAcrossValueTypes carries forward increment 4's single-type
// TestShard_CacheLinePadding to MULTIPLE V (and K) types. The shardPad arithmetic in
// options.go hinges on unpaddedShardSize == 144 being identical for every
// instantiation: the type parameters appear only inside 24-byte slice headers in
// table[K,V], so sizeof(table) — and therefore sizeof(shard) — is V- and
// K-independent. This locks that invariant so a future field that embedded V or K
// by value (silently breaking the constant and the false-sharing mitigation) fails
// loudly here.
func TestShard_SizeConstantAcrossValueTypes(t *testing.T) {
	t.Parallel()

	// table[K,V] size across a scalar, two array sizes, a pointer, and a string V,
	// plus a different K — must all be identical.
	tableSizes := []uintptr{
		unsafe.Sizeof(table[string, int]{}),
		unsafe.Sizeof(table[string, [16]byte]{}),
		unsafe.Sizeof(table[string, [64]byte]{}),
		unsafe.Sizeof(table[string, *int]{}),
		unsafe.Sizeof(table[string, string]{}),
		unsafe.Sizeof(table[int, [16]byte]{}),
	}
	for i, sz := range tableSizes {
		if sz != tableSizes[0] {
			t.Fatalf("table size #%d = %d, want %d (sizeof(table) must be constant across K/V)",
				i, sz, tableSizes[0])
		}
	}

	// The unpadded shard size constant must equal the real RWMutex + table layout.
	if got := unsafe.Sizeof(sync.RWMutex{}) + tableSizes[0]; got != unpaddedShardSize {
		t.Fatalf("RWMutex+table = %d, but unpaddedShardSize const = %d (constant is stale)",
			got, unpaddedShardSize)
	}

	// And the padded shard is a whole number of cache lines for every instantiation.
	shardSizes := []uintptr{
		unsafe.Sizeof(shard[string, int]{}),
		unsafe.Sizeof(shard[string, [16]byte]{}),
		unsafe.Sizeof(shard[string, *int]{}),
		unsafe.Sizeof(shard[int, [64]byte]{}),
	}
	for i, sz := range shardSizes {
		if sz != shardSizes[0] {
			t.Fatalf("shard size #%d = %d, want %d (sizeof(shard) must be constant across K/V)",
				i, sz, shardSizes[0])
		}
		if sz%cacheLineSize != 0 {
			t.Fatalf("shard size #%d = %d, not a multiple of cache line %d", i, sz, cacheLineSize)
		}
	}
}
