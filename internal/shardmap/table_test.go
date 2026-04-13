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
//   - the load factor stays at or below ~0.75 (count <= growAt).
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
