package shardmap

import (
	"math/rand"
	"strconv"
	"testing"
)

// ===========================================================================
// Eviction PR B / I3 — Sample primitive (read-only candidate draw)
// ===========================================================================
//
// Sample is the store's first extension since shipping: a read-only draw of random
// live candidates for the engine's cost-aware eviction. These tests pin that it
// never mutates the map, returns live distinct keys, degrades cleanly on small/empty
// maps, and that removing a sampled victim through the existing backward-shift
// Delete preserves every Robin Hood invariant (spec Req 10).

func TestSampleNonPositiveReturnsNil(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(4))
	m.Set("a", 1)
	if got := m.Sample(0, nil); got != nil {
		t.Fatalf("Sample(0) = %v, want nil", got)
	}
	if got := m.Sample(-3, nil); got != nil {
		t.Fatalf("Sample(-3) = %v, want nil", got)
	}
}

func TestSampleEmptyMapReturnsNothing(t *testing.T) {
	t.Parallel()

	// One shard makes the draw deterministic: every attempt hits the (empty) shard,
	// exercising the empty-shard skip and the budget-exhaustion exit.
	m := New[string, int](hashByFNV, WithShards(1))
	if got := m.Sample(5, nil); len(got) != 0 {
		t.Fatalf("Sample on empty map = %v, want empty", got)
	}
}

func TestSampleFewerThanNWhenMapSmaller(t *testing.T) {
	t.Parallel()

	// A single-entry single-shard map: the first draw returns the entry, every
	// subsequent draw is a duplicate (exercising the dedup skip) until the budget
	// is exhausted — so Sample returns exactly the one distinct live entry.
	m := New[string, int](hashByFNV, WithShards(1))
	m.Set("only", 7)
	got := m.Sample(5, nil)
	if len(got) != 1 {
		t.Fatalf("Sample(5) on a 1-entry map = %d entries, want 1", len(got))
	}
	if got[0].Key != "only" || got[0].Val != 7 {
		t.Fatalf("Sample returned %+v, want {only 7}", got[0])
	}
}

func TestSampleReturnsDistinctLiveEntries(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(4))
	model := make(map[string]int)
	for i := 0; i < 1000; i++ {
		k := "k" + strconv.Itoa(i)
		m.Set(k, i)
		model[k] = i
	}

	const n = 5
	got := m.Sample(n, nil)
	if len(got) != n {
		t.Fatalf("Sample(%d) returned %d entries, want %d", n, len(got), n)
	}
	seen := make(map[string]struct{}, n)
	for _, e := range got {
		if _, dup := seen[e.Key]; dup {
			t.Fatalf("Sample returned a duplicate key %q", e.Key)
		}
		seen[e.Key] = struct{}{}
		want, ok := model[e.Key]
		if !ok || want != e.Val {
			t.Fatalf("Sample returned non-live/mismatched entry %+v", e)
		}
		if v, live := m.Get(e.Key); !live || v != e.Val {
			t.Fatalf("sampled key %q not live in the map", e.Key)
		}
	}
}

func TestSampleDoesNotMutate(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(4))
	for i := 0; i < 500; i++ {
		m.Set("k"+strconv.Itoa(i), i)
	}
	before := snapshot(m)
	checkMapInvariants(t, m)

	for i := 0; i < 50; i++ {
		_ = m.Sample(8, nil)
	}

	after := snapshot(m)
	if len(before) != len(after) {
		t.Fatalf("Sample changed Len: before %d, after %d", len(before), len(after))
	}
	for k, v := range before {
		if av, ok := after[k]; !ok || av != v {
			t.Fatalf("Sample mutated key %q: before %d, after %v/%v", k, v, av, ok)
		}
	}
	checkMapInvariants(t, m)
}

// TestSampleSlotForwardScanFallback drives the table-level sampleSlot directly with
// a scripted source whose first sampleSlotTries draws all land on empty slots,
// forcing the guaranteed forward-scan fallback.
func TestSampleSlotForwardScanFallback(t *testing.T) {
	t.Parallel()

	tbl := newTable[string, int](0) // minCap = 8, capm1 = 7
	tbl.insert(0, "home0", 1)       // hash 0 → home slot 0
	occupied := uint64(0)
	for i := uint64(0); i <= tbl.capm1; i++ {
		if tbl.meta[i] != 0 {
			occupied = i
			break
		}
	}
	// Find an empty slot to aim the misses + the forward-scan start at.
	var empty uint64
	for i := uint64(0); i <= tbl.capm1; i++ {
		if tbl.meta[i] == 0 {
			empty = i
			break
		}
	}
	// sampleSlotTries misses, then a forward-scan start on an empty slot that walks
	// to the single occupied slot.
	script := make([]uint64, 0, sampleSlotTries+1)
	for i := 0; i < sampleSlotTries; i++ {
		script = append(script, empty)
	}
	script = append(script, (occupied-1)&tbl.capm1) // start just before the occupied slot
	if got := tbl.sampleSlot(scriptedRand(script...)); tbl.meta[got] == 0 {
		t.Fatalf("sampleSlot returned empty slot %d", got)
	}
}

// TestSampleEvictPreservesInvariants is the spec Req 10 property: interleave
// inserts, deletes, and sample-then-evict, asserting every Robin Hood invariant
// holds after each step and that removal goes through the backward-shift Delete.
func TestSampleEvictPreservesInvariants(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(8))
	model := make(map[string]int)
	rng := rand.New(rand.NewSource(0xE71C7))

	for step := 0; step < 4000; step++ {
		switch rng.Intn(3) {
		case 0, 1: // grow the working set
			k := "k" + strconv.Itoa(rng.Intn(2000))
			m.Set(k, step)
			model[k] = step
		default: // sample then evict a victim via the existing backward-shift Delete
			cands := m.Sample(5, randFromRng(rng))
			for _, e := range cands {
				if _, ok := model[e.Key]; !ok {
					t.Fatalf("Sample returned a non-live key %q", e.Key)
				}
			}
			if len(cands) > 0 {
				victim := cands[0].Key
				if _, ok := m.Delete(victim); !ok {
					t.Fatalf("victim %q was not deletable", victim)
				}
				delete(model, victim)
				checkMapInvariants(t, m) // invariants hold after every sample/evict step
			}
		}
	}
	checkMapInvariants(t, m)
	if got := m.Len(); got != len(model) {
		t.Fatalf("Len %d != model %d", got, len(model))
	}
}

// TestSwapReturnsPriorValue pins the atomic overwrite-delta primitive: Swap stores
// the new value and returns the value it replaced (or the zero value + existed=false
// on a first insert), all under one write lock so the engine can account exactly.
func TestSwapReturnsPriorValue(t *testing.T) {
	t.Parallel()

	m := New[string, int](hashByFNV, WithShards(2))
	if old, existed := m.Swap("k", 1); existed || old != 0 {
		t.Fatalf("first Swap = (%d, %v), want (0, false)", old, existed)
	}
	if old, existed := m.Swap("k", 2); !existed || old != 1 {
		t.Fatalf("second Swap = (%d, %v), want (1, true)", old, existed)
	}
	if v, ok := m.Get("k"); !ok || v != 2 {
		t.Fatalf("Get after Swap = (%d, %v), want (2, true)", v, ok)
	}
}

// scriptedRand returns the given values in order, cycling at the end.
func scriptedRand(vals ...uint64) func() uint64 {
	i := 0
	return func() uint64 {
		v := vals[i%len(vals)]
		i++
		return v
	}
}

// randFromRng adapts a *rand.Rand into the uint64 source Sample expects.
func randFromRng(rng *rand.Rand) func() uint64 {
	return func() uint64 { return rng.Uint64() }
}
