package shardmap

import (
	"math/rand"
	"strconv"
	"testing"
)

// fnv64 is a tiny deterministic 64-bit hash used to supply "natural" hashes for
// string keys in tests that do not care about forcing collisions. Tests that DO
// need collisions pass synthetic uint64 hashes directly to insert/lookup, which
// is exactly why the table takes the hash as a caller-supplied parameter.
func fnv64(s string) uint64 {
	const (
		offset = uint64(1469598103934665603)
		prime  = uint64(1099511628211)
	)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// checkInvariants asserts the Robin Hood structural invariants on tbl:
//   - every occupied slot's stored DIB equals its actual displacement from home;
//   - probe chains are contiguous (an entry with DIB>0 has an occupied predecessor
//     — no holes);
//   - DIB monotonicity: an entry sits at most one slot deeper than its immediate
//     predecessor;
//   - the number of occupied slots equals t.count;
//   - the load factor stays at or below ~0.75 (count <= growAt);
//   - no tombstones / no holes: every occupied entry is reachable from its home by a
//     contiguous probe walk on which the Robin Hood early-exit never trips (the
//     post-delete contract that backward-shift must preserve — increment 3).
//
// It is reused by later increments (delete/shrink + property suite).
func checkInvariants[K comparable, V any](t *testing.T, tbl *table[K, V]) {
	t.Helper()
	occupied := 0
	for i := uint64(0); i <= tbl.capm1; i++ {
		m := tbl.meta[i]
		if m == 0 {
			continue
		}
		occupied++
		dib := dibOf(m)
		// No tombstones by construction: meta is only ever 0 (empty) or an occupied
		// encoding whose DIB fits the field; a sentinel/tombstone DIB would overflow it.
		if dib > maxDib {
			t.Fatalf("slot %d: dib %d exceeds maxDib %d (corrupt/tombstone meta)", i, dib, maxDib)
		}
		home := tbl.hashes[i] & tbl.capm1
		wantDib := (i - home) & tbl.capm1
		if uint64(dib) != wantDib {
			t.Fatalf("slot %d: stored dib %d != displacement %d (home %d)", i, dib, wantDib, home)
		}
		if dib > 0 {
			prev := (i - 1) & tbl.capm1
			if tbl.meta[prev] == 0 {
				t.Fatalf("slot %d has dib %d but predecessor %d is empty (hole)", i, dib, prev)
			}
			if pd := dibOf(tbl.meta[prev]); dib > pd+1 {
				t.Fatalf("slot %d dib %d jumps >1 over predecessor dib %d (non-monotonic)", i, dib, pd)
			}
		}
	}
	if occupied != tbl.count {
		t.Fatalf("occupied slots %d != count %d", occupied, tbl.count)
	}
	if tbl.count > tbl.growAt {
		t.Fatalf("count %d exceeds growAt %d (load factor over ~0.75)", tbl.count, tbl.growAt)
	}

	// Independent early-exit / no-hole pass: for every occupied slot, replay the
	// lookup probe from its home and confirm each intermediate slot is occupied with
	// DIB >= the probe distance. If a delete left a hole or a too-shallow resident on
	// the chain, a real lookup for this key would early-exit before reaching it — so
	// this walk is the direct structural witness that the key is still findable.
	for i := uint64(0); i <= tbl.capm1; i++ {
		if tbl.meta[i] == 0 {
			continue
		}
		home := tbl.hashes[i] & tbl.capm1
		for pd := uint64(0); (home+pd)&tbl.capm1 != i; pd++ {
			s := (home + pd) & tbl.capm1
			if tbl.meta[s] == 0 {
				t.Fatalf("slot %d (home %d) unreachable: hole at probe slot %d", i, home, s)
			}
			if uint64(dibOf(tbl.meta[s])) < pd {
				t.Fatalf("slot %d (home %d) unreachable: early-exit at slot %d (dib %d < probe %d)",
					i, home, s, dibOf(tbl.meta[s]), pd)
			}
		}
	}
}

// assertTablesEqual asserts two tables are structurally identical slot-for-slot
// (meta/keys/vals/hashes) and share the same derived scalars. Used by the
// delete-restores-as-if-never-inserted test, where backward-shift must reproduce
// the exact layout a fresh insertion of the surviving keys would yield.
func assertTablesEqual[K comparable, V comparable](t *testing.T, got, want *table[K, V]) {
	t.Helper()
	if got.count != want.count {
		t.Fatalf("count = %d, want %d", got.count, want.count)
	}
	if got.capm1 != want.capm1 {
		t.Fatalf("capm1 = %d, want %d", got.capm1, want.capm1)
	}
	if got.growAt != want.growAt {
		t.Fatalf("growAt = %d, want %d", got.growAt, want.growAt)
	}
	for i := uint64(0); i <= got.capm1; i++ {
		if got.meta[i] != want.meta[i] {
			t.Fatalf("slot %d meta = %d, want %d", i, got.meta[i], want.meta[i])
		}
		if got.meta[i] == 0 {
			continue // empty slot: key/val/hash content is irrelevant
		}
		if got.keys[i] != want.keys[i] {
			t.Fatalf("slot %d key = %v, want %v", i, got.keys[i], want.keys[i])
		}
		if got.vals[i] != want.vals[i] {
			t.Fatalf("slot %d val = %v, want %v", i, got.vals[i], want.vals[i])
		}
		if got.hashes[i] != want.hashes[i] {
			t.Fatalf("slot %d hash = %d, want %d", i, got.hashes[i], want.hashes[i])
		}
	}
}

func TestTable_InsertThenLookup(t *testing.T) {
	tbl := newTable[string, int](0)

	tbl.insert(fnv64("alpha"), "alpha", 42)

	if v, ok := tbl.lookup(fnv64("alpha"), "alpha"); !ok || v != 42 {
		t.Fatalf("lookup(alpha) = (%d, %v), want (42, true)", v, ok)
	}
	if v, ok := tbl.lookup(fnv64("absent"), "absent"); ok || v != 0 {
		t.Fatalf("lookup(absent) = (%d, %v), want (0, false)", v, ok)
	}
	checkInvariants(t, &tbl)
}

func TestTable_OverwriteKeepsCount(t *testing.T) {
	tbl := newTable[string, int](0)

	tbl.insert(fnv64("k"), "k", 1)
	before := tbl.count
	tbl.insert(fnv64("k"), "k", 2)

	if tbl.count != before {
		t.Fatalf("count after overwrite = %d, want %d", tbl.count, before)
	}
	if v, ok := tbl.lookup(fnv64("k"), "k"); !ok || v != 2 {
		t.Fatalf("lookup(k) after overwrite = (%d, %v), want (2, true)", v, ok)
	}
	checkInvariants(t, &tbl)
}

func TestTable_GrowDoublesCapacity(t *testing.T) {
	tbl := newTable[string, int](0) // cap 8, growAt 6
	startCap := int(tbl.capm1) + 1

	// Insert 7 keys: the 7th insert sees count == growAt (6) and grows first.
	const n = 7
	for i := 0; i < n; i++ {
		k := "k" + strconv.Itoa(i)
		tbl.insert(fnv64(k), k, i)
	}

	gotCap := int(tbl.capm1) + 1
	if gotCap != startCap*2 {
		t.Fatalf("capacity = %d, want %d (doubled from %d)", gotCap, startCap*2, startCap)
	}
	if tbl.count != n {
		t.Fatalf("count = %d, want %d", tbl.count, n)
	}
	for i := 0; i < n; i++ {
		k := "k" + strconv.Itoa(i)
		if v, ok := tbl.lookup(fnv64(k), k); !ok || v != i {
			t.Fatalf("lookup(%q) = (%d, %v), want (%d, true)", k, v, ok, i)
		}
	}
	if lf := float64(tbl.count) / float64(gotCap); lf > 0.75 {
		t.Fatalf("load factor %.3f > 0.75", lf)
	}
	checkInvariants(t, &tbl)
}

func TestTable_RobinHoodSwapMonotonicDIB(t *testing.T) {
	tbl := newTable[string, int](0) // cap 8, capm1 7

	// Synthetic hashes craft deliberate homes (hash & 7) so inserts must perform
	// Robin Hood "rob from the rich" swaps. f (home 2) robs a (home 3), which in
	// turn robs d (home 4) — two swaps in one insert.
	type kv struct {
		h uint64
		k string
	}
	seq := []kv{
		{3, "a"},  // home 3
		{11, "b"}, // home 3
		{19, "c"}, // home 3
		{4, "d"},  // home 4
		{2, "e"},  // home 2
		{10, "f"}, // home 2 -> triggers the swap cascade
	}
	for i, in := range seq {
		tbl.insert(in.h, in.k, i)
	}

	for i, in := range seq {
		if v, ok := tbl.lookup(in.h, in.k); !ok || v != i {
			t.Fatalf("lookup(%q) = (%d, %v), want (%d, true)", in.k, v, ok, i)
		}
	}
	// An absent key whose home is in the chain must early-exit, not run forever.
	if _, ok := tbl.lookup(27, "ghost"); ok {
		t.Fatalf("lookup(ghost) = true, want false")
	}
	checkInvariants(t, &tbl)

	// No grow should have happened (6 inserts == growAt at cap 8).
	if got := int(tbl.capm1) + 1; got != 8 {
		t.Fatalf("capacity = %d, want 8 (no grow expected)", got)
	}
}

// --- TRIANGULATE -----------------------------------------------------------

// TestTable_ZeroValueIsPresent proves presence is decided by the slot metadata,
// not by the stored value: a key whose value is the zero value of V is still
// reported found, distinct from an absent key.
func TestTable_ZeroValueIsPresent(t *testing.T) {
	tbl := newTable[string, int](0)
	tbl.insert(fnv64("zero"), "zero", 0)

	if v, ok := tbl.lookup(fnv64("zero"), "zero"); !ok || v != 0 {
		t.Fatalf("lookup(zero) = (%d, %v), want (0, true)", v, ok)
	}
	if _, ok := tbl.lookup(fnv64("missing"), "missing"); ok {
		t.Fatalf("lookup(missing) = true, want false")
	}
	checkInvariants(t, &tbl)
}

// TestTable_BinaryKeys round-trips keys that contain NUL and 0xff bytes — keys
// are opaque comparable strings, not text.
func TestTable_BinaryKeys(t *testing.T) {
	tbl := newTable[string, string](0)
	keys := []string{
		"\x00",
		"\xff",
		"\x00\xff\x10\x00",
		"a\x00b",
		"\xff\xfe\xfd",
		"",
	}
	for i, k := range keys {
		tbl.insert(fnv64(k), k, k+strconv.Itoa(i))
	}
	for i, k := range keys {
		if v, ok := tbl.lookup(fnv64(k), k); !ok || v != k+strconv.Itoa(i) {
			t.Fatalf("lookup(%q) = (%q, %v), want (%q, true)", k, v, ok, k+strconv.Itoa(i))
		}
	}
	checkInvariants(t, &tbl)
}

// TestTable_ManyCollisionBurst inserts a burst of keys that all share the same
// home slot at the starting capacity, forcing repeated Robin Hood swaps, while
// their differing higher bits let them redistribute as the table grows.
func TestTable_ManyCollisionBurst(t *testing.T) {
	tbl := newTable[string, int](0)
	const n = 60
	for i := 0; i < n; i++ {
		k := "c" + strconv.Itoa(i)
		// hash & 7 == 0 at cap 8 -> all home slot 0 initially; higher bits differ.
		tbl.insert(uint64(i)*8, k, i)
	}
	if tbl.count != n {
		t.Fatalf("count = %d, want %d", tbl.count, n)
	}
	for i := 0; i < n; i++ {
		k := "c" + strconv.Itoa(i)
		if v, ok := tbl.lookup(uint64(i)*8, k); !ok || v != i {
			t.Fatalf("lookup(%q) = (%d, %v), want (%d, true)", k, v, ok, i)
		}
	}
	checkInvariants(t, &tbl)
}

// TestTable_RepeatedGrows drives two grows so capacity walks 8 -> 16 -> 32, and
// confirms every prior key survives each resize.
func TestTable_RepeatedGrows(t *testing.T) {
	tbl := newTable[string, int](0) // cap 8
	const n = 13                    // 7th insert grows to 16, 13th grows to 32
	for i := 0; i < n; i++ {
		k := "g" + strconv.Itoa(i)
		tbl.insert(fnv64(k), k, i)
	}
	if got := int(tbl.capm1) + 1; got != 32 {
		t.Fatalf("capacity = %d, want 32 (8 -> 16 -> 32)", got)
	}
	if tbl.count != n {
		t.Fatalf("count = %d, want %d", tbl.count, n)
	}
	for i := 0; i < n; i++ {
		k := "g" + strconv.Itoa(i)
		if v, ok := tbl.lookup(fnv64(k), k); !ok || v != i {
			t.Fatalf("lookup(%q) = (%d, %v), want (%d, true)", k, v, ok, i)
		}
	}
	checkInvariants(t, &tbl)
}

// TestTable_RandomizedAgainstMapOracle cross-checks a randomized
// insert/overwrite/lookup stream against a reference Go map within a single
// table (single-shard scope; delete arrives in increment 3).
func TestTable_RandomizedAgainstMapOracle(t *testing.T) {
	const (
		ops      = 20000
		keySpace = 400 // small enough to force overwrites and collisions
	)
	rng := rand.New(rand.NewSource(0xC0FFEE))
	tbl := newTable[string, int](0)
	model := make(map[string]int)

	for n := 0; n < ops; n++ {
		key := "key" + strconv.Itoa(rng.Intn(keySpace))
		switch rng.Intn(3) {
		case 0, 1: // bias toward writes so the table fills and grows
			val := rng.Int()
			tbl.insert(fnv64(key), key, val)
			model[key] = val
		default: // lookup
			wantV, wantOK := model[key]
			gotV, gotOK := tbl.lookup(fnv64(key), key)
			if gotOK != wantOK || (wantOK && gotV != wantV) {
				t.Fatalf("op %d lookup(%q) = (%d, %v), want (%d, %v)", n, key, gotV, gotOK, wantV, wantOK)
			}
		}
		if n%500 == 0 {
			checkInvariants(t, &tbl)
		}
	}

	// Final full reconciliation: every model key found with the right value, and
	// the live count agrees.
	if tbl.count != len(model) {
		t.Fatalf("count = %d, want %d", tbl.count, len(model))
	}
	for k, want := range model {
		if got, ok := tbl.lookup(fnv64(k), k); !ok || got != want {
			t.Fatalf("final lookup(%q) = (%d, %v), want (%d, true)", k, got, ok, want)
		}
	}
	for i := 0; i < keySpace; i++ {
		k := "absent" + strconv.Itoa(i)
		if _, ok := tbl.lookup(fnv64(k), k); ok {
			t.Fatalf("lookup(%q) = true, want false", k)
		}
	}
	checkInvariants(t, &tbl)
}

// TestTable_NewTableCapacityHint exercises the capacity-hint path of newTable
// (used by the Map's WithInitialCapacity in a later increment): the table must be
// pre-sized so growAt >= hint, avoiding an immediate grow, with a power-of-two cap.
func TestTable_NewTableCapacityHint(t *testing.T) {
	t.Parallel()

	const hint = 100
	tbl := newTable[int, int](hint)
	if tbl.growAt < hint {
		t.Fatalf("growAt = %d, want >= %d (capacity hint not honored)", tbl.growAt, hint)
	}
	c := int(tbl.capm1) + 1
	if c&(c-1) != 0 {
		t.Fatalf("capacity %d is not a power of two", c)
	}
}

// TestTable_FragmentCollisionDistinctKeys pins that two different keys sharing the
// same home AND the same 8-bit fragment (here, the same full hash) are both stored
// and retrievable: the fast fragment reject must fall through to a full key
// compare and never false-match one key as the other.
func TestTable_FragmentCollisionDistinctKeys(t *testing.T) {
	t.Parallel()

	tbl := newTable[string, int](0)
	const h = uint64(0x1234_5600) // identical hash => identical home and fragment
	tbl.insert(h, "alpha", 1)
	tbl.insert(h, "beta", 2)
	if tbl.count != 2 {
		t.Fatalf("count = %d, want 2 (fragment collision merged distinct keys)", tbl.count)
	}
	if v, ok := tbl.lookup(h, "alpha"); !ok || v != 1 {
		t.Fatalf("lookup(alpha) = (%d, %v), want (1, true)", v, ok)
	}
	if v, ok := tbl.lookup(h, "beta"); !ok || v != 2 {
		t.Fatalf("lookup(beta) = (%d, %v), want (2, true)", v, ok)
	}
	checkInvariants(t, &tbl)
}

// field can hold (>254) using synthetic hashes that share their low bits at the
// load-factor-determined capacity, then verifies the table grows (the design's
// safety valve) instead of silently corrupting. Reachable only with degenerate
// hashes (never via the seeded Map); without the valve the 255th-deep slot's meta
// overflows to 0 and is misread as empty -> silent key loss + count drift.
func TestTable_DIBOverflowValve(t *testing.T) {
	t.Parallel()

	const n = 300
	tbl := newTable[int, int](0)
	// hash_i = i<<9: the low 9 bits are 0, so every key homes to bucket 0 until the
	// table grows past capacity 512, which would push the single chain past DIB
	// 254. The valve must grow (where bit 9 splits the keys) and recover.
	for i := 0; i < n; i++ {
		tbl.insert(uint64(i)<<9, i, i)
	}
	if tbl.count != n {
		t.Fatalf("count = %d, want %d (keys lost to DIB overflow)", tbl.count, n)
	}
	for i := 0; i < n; i++ {
		if v, ok := tbl.lookup(uint64(i)<<9, i); !ok || v != i {
			t.Fatalf("lookup(%d) = (%d, %v), want (%d, true) -- LOST KEY", i, v, ok, i)
		}
	}
	checkInvariants(t, &tbl)
}

// ===========================================================================
// Increment 3 — backward-shift delete + shrink
// ===========================================================================

// TestTable_DeleteBackwardShiftSpansBuckets deletes a key whose probe chain spans
// several home buckets and asserts the backward-shift leaves no holes, no
// tombstones, preserves DIB monotonicity, and keeps every surviving key findable
// (spec "Backward-shift deletion leaves no holes or tombstones").
func TestTable_DeleteBackwardShiftSpansBuckets(t *testing.T) {
	tbl := newTable[string, int](0) // cap 8, capm1 7

	// Synthetic homes build one contiguous run across buckets 2..6:
	//   e@2(dib0) a@3(dib0) b@4(dib1) c@5(dib2) d@6(dib2)
	type kv struct {
		h uint64
		k string
	}
	seq := []kv{
		{3, "a"},  // home 3 -> slot 3, dib 0
		{11, "b"}, // home 3 -> slot 4, dib 1
		{19, "c"}, // home 3 -> slot 5, dib 2
		{4, "d"},  // home 4 -> slot 6, dib 2
		{2, "e"},  // home 2 -> slot 2, dib 0
	}
	for i, in := range seq {
		tbl.insert(in.h, in.k, i)
	}
	checkInvariants(t, &tbl)

	// Delete c (mid-chain, home 3, currently slot 5). d must shift back one slot
	// with its DIB decremented; no tombstone, no hole.
	got, ok := tbl.delete(19, "c")
	if !ok || got != 2 {
		t.Fatalf("delete(c) = (%d, %v), want (2, true)", got, ok)
	}
	if tbl.count != 4 {
		t.Fatalf("count after delete = %d, want 4", tbl.count)
	}
	if _, ok := tbl.lookup(19, "c"); ok {
		t.Fatalf("lookup(c) after delete = true, want false")
	}
	for _, in := range []kv{{3, "a"}, {11, "b"}, {4, "d"}, {2, "e"}} {
		if _, ok := tbl.lookup(in.h, in.k); !ok {
			t.Fatalf("survivor lookup(%q) = false, want true", in.k)
		}
	}
	checkInvariants(t, &tbl) // no holes / no tombstones / DIB monotonic
}

// TestTable_DeleteRestoresAsIfNeverInserted pins the core backward-shift guarantee
// (robin-hood deep dive §6): deleting a key leaves the table structurally identical
// to one built without that key. Same-home keys preserve their relative chain order
// under both fresh insertion and deletion shift, so the comparison is exact.
func TestTable_DeleteRestoresAsIfNeverInserted(t *testing.T) {
	// Four keys all homing to bucket 0 at cap 8: slots 0,1,2,3 with dib 0,1,2,3.
	full := newTable[string, int](0)
	keys := []struct {
		h uint64
		k string
	}{{0, "k0"}, {8, "k1"}, {16, "k2"}, {24, "k3"}}
	for i, in := range keys {
		full.insert(in.h, in.k, i)
	}

	// Reference: the same keys minus k1, inserted in their original relative order.
	ref := newTable[string, int](0)
	ref.insert(0, "k0", 0)
	ref.insert(16, "k2", 2)
	ref.insert(24, "k3", 3)

	if _, ok := full.delete(8, "k1"); !ok {
		t.Fatalf("delete(k1) = false, want true")
	}
	assertTablesEqual(t, &full, &ref)
	checkInvariants(t, &full)
}

// TestTable_DeleteThenReinsertRoundTrips checks observable equivalence after a
// delete followed by a re-insert of the same key: every key is present with the
// right value, the count is restored, and the invariants hold (the post-reinsert
// layout may differ from the original, which is expected for order-dependent
// same-home chains — only observable behavior is asserted here).
func TestTable_DeleteThenReinsertRoundTrips(t *testing.T) {
	tbl := newTable[string, int](0)
	keys := []struct {
		h uint64
		k string
	}{{0, "k0"}, {8, "k1"}, {16, "k2"}, {24, "k3"}}
	for i, in := range keys {
		tbl.insert(in.h, in.k, i)
	}

	if _, ok := tbl.delete(8, "k1"); !ok {
		t.Fatalf("delete(k1) = false, want true")
	}
	tbl.insert(8, "k1", 1) // re-insert with the original value

	if tbl.count != 4 {
		t.Fatalf("count after delete+reinsert = %d, want 4", tbl.count)
	}
	for i, in := range keys {
		if v, ok := tbl.lookup(in.h, in.k); !ok || v != i {
			t.Fatalf("lookup(%q) = (%d, %v), want (%d, true)", in.k, v, ok, i)
		}
	}
	checkInvariants(t, &tbl)
}

// TestTable_ShrinkTriggerHalvesCap drives the table up to a larger capacity, then
// deletes enough entries to drop the load below ~0.25, and asserts the capacity
// halves, all survivors persist, and the load factor stays bounded (the shrink
// half of spec "Load factor stays bounded across grow and shrink").
func TestTable_ShrinkTriggerHalvesCap(t *testing.T) {
	tbl := newTable[string, int](0) // cap 8
	// 25 inserts walk cap 8->16->32->64 (grow at count 6/12/24). count 25, cap 64.
	const n = 25
	for i := 0; i < n; i++ {
		k := "s" + strconv.Itoa(i)
		tbl.insert(fnv64(k), k, i)
	}
	if got := int(tbl.capm1) + 1; got != 64 {
		t.Fatalf("capacity after %d inserts = %d, want 64", n, got)
	}

	// Delete down to count 15: load 15/64 < 0.25 trips a single halving to cap 32.
	for i := n - 1; i >= 15; i-- {
		k := "s" + strconv.Itoa(i)
		if _, ok := tbl.delete(fnv64(k), k); !ok {
			t.Fatalf("delete(%q) = false, want true", k)
		}
	}
	if got := int(tbl.capm1) + 1; got != 32 {
		t.Fatalf("capacity after shrink = %d, want 32 (halved from 64)", got)
	}
	if tbl.count != 15 {
		t.Fatalf("count after deletes = %d, want 15", tbl.count)
	}
	for i := 0; i < 15; i++ {
		k := "s" + strconv.Itoa(i)
		if v, ok := tbl.lookup(fnv64(k), k); !ok || v != i {
			t.Fatalf("survivor lookup(%q) = (%d, %v), want (%d, true)", k, v, ok, i)
		}
	}
	if lf := float64(tbl.count) / float64(int(tbl.capm1)+1); lf > 0.75 {
		t.Fatalf("load factor %.3f > 0.75 after shrink", lf)
	}
	checkInvariants(t, &tbl)
}

// TestTable_ShrinkHysteresisNoOscillation proves the wide grow(0.75)/shrink(0.25)
// band keeps capacity stable when operations hover around a single threshold: a
// fresh grow must not immediately shrink, a fresh shrink must not immediately
// grow, and a long alternating insert/delete run must not bounce the capacity.
func TestTable_ShrinkHysteresisNoOscillation(t *testing.T) {
	tbl := newTable[string, int](0) // cap 8
	// 13 inserts walk cap 8->16->32 (grow at count 6/12). count 13, cap 32.
	for i := 0; i < 13; i++ {
		k := "h" + strconv.Itoa(i)
		tbl.insert(fnv64(k), k, i)
	}
	if got := int(tbl.capm1) + 1; got != 32 {
		t.Fatalf("capacity = %d, want 32", got)
	}

	// A single delete right after a grow must NOT shrink (load 12/32 = 0.375 > 0.25).
	if _, ok := tbl.delete(fnv64("h12"), "h12"); !ok {
		t.Fatalf("delete(h12) = false, want true")
	}
	if got := int(tbl.capm1) + 1; got != 32 {
		t.Fatalf("capacity after one post-grow delete = %d, want 32 (no shrink)", got)
	}

	// Alternate insert/delete of a transient key many times: count stays at 12/13,
	// far from both the grow (24) and shrink (8) thresholds — cap must never move.
	for i := 0; i < 200; i++ {
		tbl.insert(fnv64("transient"), "transient", i)
		if _, ok := tbl.delete(fnv64("transient"), "transient"); !ok {
			t.Fatalf("delete(transient) iter %d = false, want true", i)
		}
		if got := int(tbl.capm1) + 1; got != 32 {
			t.Fatalf("capacity oscillated to %d at iter %d, want stable 32", got, i)
		}
	}
	checkInvariants(t, &tbl)
}

// --- TRIANGULATE -----------------------------------------------------------

// TestTable_DeleteChainPositions deletes the head, middle, and tail of a single
// same-home probe chain (each from a fresh table) and asserts every survivor is
// still found and the invariants hold — backward-shift must repair the chain
// regardless of which position is removed.
func TestTable_DeleteChainPositions(t *testing.T) {
	t.Parallel()

	// Homes all bucket 0 at cap 8: slots 0,1,2,3 with dib 0,1,2,3.
	keys := []struct {
		h uint64
		k string
	}{{0, "k0"}, {8, "k1"}, {16, "k2"}, {24, "k3"}}

	cases := []struct {
		name    string
		delH    uint64
		delK    string
		survive []string
	}{
		{"head", 0, "k0", []string{"k1", "k2", "k3"}},
		{"mid", 16, "k2", []string{"k0", "k1", "k3"}},
		{"tail", 24, "k3", []string{"k0", "k1", "k2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tbl := newTable[string, int](0)
			for i, in := range keys {
				tbl.insert(in.h, in.k, i)
			}
			if _, ok := tbl.delete(tc.delH, tc.delK); !ok {
				t.Fatalf("delete(%q) = false, want true", tc.delK)
			}
			if _, ok := tbl.lookup(tc.delH, tc.delK); ok {
				t.Fatalf("lookup(%q) after delete = true, want false", tc.delK)
			}
			if tbl.count != len(tc.survive) {
				t.Fatalf("count = %d, want %d", tbl.count, len(tc.survive))
			}
			for _, k := range tc.survive {
				var h uint64
				for _, in := range keys {
					if in.k == k {
						h = in.h
					}
				}
				if _, ok := tbl.lookup(h, k); !ok {
					t.Fatalf("survivor lookup(%q) = false, want true", k)
				}
			}
			checkInvariants(t, &tbl)
		})
	}
}

// TestTable_DeleteLastAndAbsent covers the degenerate ends: deleting the only
// remaining key empties the table (count 0, reads absent, cap floored at minCap),
// and deleting an absent key reports false and changes nothing.
func TestTable_DeleteLastAndAbsent(t *testing.T) {
	t.Parallel()

	tbl := newTable[string, int](0)
	tbl.insert(fnv64("only"), "only", 7)

	// Absent key: no change, ok == false, zero value.
	if v, ok := tbl.delete(fnv64("ghost"), "ghost"); ok || v != 0 {
		t.Fatalf("delete(ghost) = (%d, %v), want (0, false)", v, ok)
	}
	if tbl.count != 1 {
		t.Fatalf("count after absent delete = %d, want 1", tbl.count)
	}

	// Last remaining key: table goes empty, cap stays at minCap (cannot shrink below).
	if v, ok := tbl.delete(fnv64("only"), "only"); !ok || v != 7 {
		t.Fatalf("delete(only) = (%d, %v), want (7, true)", v, ok)
	}
	if tbl.count != 0 {
		t.Fatalf("count after deleting last key = %d, want 0", tbl.count)
	}
	if _, ok := tbl.lookup(fnv64("only"), "only"); ok {
		t.Fatalf("lookup(only) after delete = true, want false")
	}
	if got := int(tbl.capm1) + 1; got != minCap {
		t.Fatalf("capacity = %d, want minCap %d (no shrink below floor)", got, minCap)
	}
	// Deleting from the now-empty table is a clean miss.
	if _, ok := tbl.delete(fnv64("only"), "only"); ok {
		t.Fatalf("second delete(only) = true, want false")
	}
	checkInvariants(t, &tbl)
}

// TestTable_MassDeleteThenRegrow inserts a large set, deletes every entry (driving
// repeated shrinks down to minCap), then re-inserts a fresh set — proving the
// shrink path and a subsequent regrow both preserve all entries with no leftover
// state from the emptied table.
func TestTable_MassDeleteThenRegrow(t *testing.T) {
	t.Parallel()

	tbl := newTable[string, int](0)
	const n = 100
	for i := 0; i < n; i++ {
		k := "m" + strconv.Itoa(i)
		tbl.insert(fnv64(k), k, i)
	}
	checkInvariants(t, &tbl)

	for i := 0; i < n; i++ {
		k := "m" + strconv.Itoa(i)
		if _, ok := tbl.delete(fnv64(k), k); !ok {
			t.Fatalf("delete(%q) = false, want true", k)
		}
	}
	if tbl.count != 0 {
		t.Fatalf("count after mass delete = %d, want 0", tbl.count)
	}
	if got := int(tbl.capm1) + 1; got != minCap {
		t.Fatalf("capacity after mass delete = %d, want minCap %d", got, minCap)
	}
	checkInvariants(t, &tbl)

	// Regrow from empty: a fresh set must round-trip and grow cleanly.
	for i := 0; i < n; i++ {
		k := "r" + strconv.Itoa(i)
		tbl.insert(fnv64(k), k, i*2)
	}
	if tbl.count != n {
		t.Fatalf("count after regrow = %d, want %d", tbl.count, n)
	}
	for i := 0; i < n; i++ {
		k := "r" + strconv.Itoa(i)
		if v, ok := tbl.lookup(fnv64(k), k); !ok || v != i*2 {
			t.Fatalf("lookup(%q) = (%d, %v), want (%d, true)", k, v, ok, i*2)
		}
	}
	// No stale survivors from the deleted generation.
	for i := 0; i < n; i++ {
		k := "m" + strconv.Itoa(i)
		if _, ok := tbl.lookup(fnv64(k), k); ok {
			t.Fatalf("stale lookup(%q) = true, want false", k)
		}
	}
	checkInvariants(t, &tbl)
}

// TestTable_RandomizedDeletesAgainstMapOracle is the increment-3 model-equivalence
// gate: a long randomized Set/Get/Delete stream is mirrored against a reference Go
// map and the structural invariants are re-checked throughout. This is the
// reviewer-mandated guard that the backward-shift stop condition keeps probe chains
// contiguous and the early-exit invariant intact (no silent key loss / no
// duplicate-insert regression).
func TestTable_RandomizedDeletesAgainstMapOracle(t *testing.T) {
	const (
		ops      = 40000
		keySpace = 256 // small enough to force collisions, overwrites, and re-inserts
	)
	rng := rand.New(rand.NewSource(0xD15EA5E))
	tbl := newTable[string, int](0)
	model := make(map[string]int)

	for n := 0; n < ops; n++ {
		key := "key" + strconv.Itoa(rng.Intn(keySpace))
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // bias toward writes so the table fills and grows
			val := rng.Int()
			tbl.insert(fnv64(key), key, val)
			model[key] = val
		case 5, 6, 7: // delete: compare presence reporting against the model
			_, want := model[key]
			_, got := tbl.delete(fnv64(key), key)
			if got != want {
				t.Fatalf("op %d delete(%q) existed = %v, want %v", n, key, got, want)
			}
			delete(model, key)
		default: // lookup
			wantV, wantOK := model[key]
			gotV, gotOK := tbl.lookup(fnv64(key), key)
			if gotOK != wantOK || (wantOK && gotV != wantV) {
				t.Fatalf("op %d lookup(%q) = (%d, %v), want (%d, %v)", n, key, gotV, gotOK, wantV, wantOK)
			}
		}
		if n%250 == 0 {
			checkInvariants(t, &tbl)
		}
	}

	if tbl.count != len(model) {
		t.Fatalf("final count = %d, want %d", tbl.count, len(model))
	}
	for k, want := range model {
		if got, ok := tbl.lookup(fnv64(k), k); !ok || got != want {
			t.Fatalf("final lookup(%q) = (%d, %v), want (%d, true)", k, got, ok, want)
		}
	}
	checkInvariants(t, &tbl)
}
