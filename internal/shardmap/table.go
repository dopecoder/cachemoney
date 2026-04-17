// Package shardmap implements the concurrency-safe, generic sharded hashmap that
// backs cachemoney's cache engine (ADR-0005). Each shard owns one
// open-addressing Robin Hood table; the engine routes a key to a shard with the
// high bits of its seeded 64-bit hash and to a bucket with the low bits
// (design §5).
//
// This file holds the per-shard table — a single-threaded, lock-free Robin Hood
// open-addressing table. Locking, sharding, and routing are layered on top by the
// Map wrapper (added in a later increment); the table itself never takes a lock
// and stores whatever V it is given, knowing nothing about TTL, context, or byte
// copying (those live in internal/cache).
//
// The table is deliberately hash-agnostic: callers pass the precomputed 64-bit
// hash to insert and lookup. That keeps the table generic over any
// K comparable (Go generics cannot derive a hash for an arbitrary comparable
// type) and lets tests supply synthetic hashes to force collisions and exercise
// the Robin Hood swap and probe-chain invariants deterministically.
package shardmap

// Metadata layout (design §4.2). Each slot has a uint16 of metadata held in a
// parallel array so the hot probe loop scans tiny, contiguous bytes and only
// touches the wide key/value arrays on a fragment hit:
//
//	meta == 0            -> empty slot (clean sentinel)
//	meta != 0            -> (dib+1) << fragBits | fragment
//
// Storing dib+1 (not dib) in the high bits keeps a home slot (DIB 0) non-zero so
// 0 unambiguously means "empty". fragBits low bits hold a hash fragment for fast
// rejection before the full key compare. fragBits = 8 gives an 8-bit fragment and
// supports DIB up to 254 — comfortable, since Robin Hood at the 0.75 load factor
// keeps the maximum DIB to Θ(log n) with a small constant.
const (
	fragBits = 8
	fragMask = uint16(1)<<fragBits - 1

	// maxDib is the largest distance-to-initial-bucket the metadata can store:
	// dib+1 must fit in the (16-fragBits) high bits, so dib <= 2^(16-fragBits)-2.
	// A probe that would exceed it forces a grow (the safety valve in set). With
	// the seeded engine hash this never fires (max DIB stays Theta(log n)); it
	// guards the deliberately hash-agnostic table against degenerate hashes that
	// share their low bits, turning silent metadata overflow into a clean grow.
	maxDib = uint16(1)<<(16-fragBits) - 2

	// minCap is the smallest table capacity (a power of two). The capacity is
	// always a power of two so home selection is a bitmask, not a modulo.
	minCap = 8

	// loadNum/loadDen express the ~0.75 grow threshold as growAt = cap*3/4.
	loadNum = 3
	loadDen = 4

	// shrinkNum/shrinkDen express the ~0.25 shrink threshold as shrinkBelow = cap/4.
	// The wide hysteresis band (grow at 3/4, shrink at 1/4) keeps a delete that drops
	// the load just below 1/4 from landing above 3/4 after halving, so capacity never
	// oscillates when operations hover around a single threshold (design §10).
	shrinkNum = 1
	shrinkDen = 4
)

// table is one open-addressing Robin Hood hashmap held as a structure-of-arrays
// (design §4.2). It is single-threaded and lock-free; the Map wrapper guards it
// with a per-shard lock. The zero value is not usable — build one with newTable.
type table[K comparable, V any] struct {
	meta   []uint16 // per-slot metadata: 0 = empty, else (dib+1)<<fragBits | fragment
	keys   []K
	vals   []V
	hashes []uint64 // cached full hash so grow/shrink/shift never rehash a key
	count  int      // number of occupied slots
	capm1  uint64   // capacity-1 bitmask (capacity is a power of two)
	growAt int      // grow when count reaches this (= capacity * 3/4)
}

// fragOf extracts the hash fragment stored in a slot's metadata low bits. The
// mask bounds the result to fragBits bits, so the narrowing to uint16 cannot
// lose information.
func fragOf(hash uint64) uint16 {
	return uint16(hash & uint64(fragMask)) //nolint:gosec // masked to fragBits (<=0xff); lossless narrowing
}

// dibOf returns the distance-to-initial-bucket recorded in an occupied slot's
// metadata. It must only be called on occupied slots (meta != 0).
func dibOf(m uint16) uint16 { return (m >> fragBits) - 1 }

// packMeta builds the metadata word for an occupied slot from its DIB and hash
// fragment.
func packMeta(dib, frag uint16) uint16 { return ((dib + 1) << fragBits) | frag }

// home returns the ideal (home) bucket index for hash: the low bits of the hash
// masked to the table capacity.
func (t *table[K, V]) home(hash uint64) uint64 { return hash & t.capm1 }

// newTable returns an empty table sized to hold at least capacityHint live
// entries without an immediate grow. The capacity is rounded up to a power of two
// no smaller than minCap.
func newTable[K comparable, V any](capacityHint int) table[K, V] {
	c := minCap
	for c*loadNum/loadDen < capacityHint {
		c <<= 1
	}
	var t table[K, V]
	t.alloc(c)
	return t
}

// alloc (re)allocates the slot arrays for the given power-of-two capacity and
// resets the derived fields. It is the single allocation path shared by
// construction and resize.
func (t *table[K, V]) alloc(capacity int) {
	t.meta = make([]uint16, capacity)
	t.keys = make([]K, capacity)
	t.vals = make([]V, capacity)
	t.hashes = make([]uint64, capacity)
	// len is non-negative, so deriving the mask from it is a lossless conversion.
	t.capm1 = uint64(len(t.meta)) - 1
	t.growAt = capacity * loadNum / loadDen
	t.count = 0
}

// insert stores value under key (identified by its precomputed hash), overwriting
// any existing entry for the same key. It grows the table first when the live
// count has reached the grow threshold, so the load factor stays at or below
// ~0.75.
func (t *table[K, V]) insert(hash uint64, key K, value V) {
	if t.count >= t.growAt {
		t.grow()
	}
	t.set(hash, key, value)
}

// set places (hash, key, value) into the table using the Robin Hood rule,
// overwriting in place if the key is already present. It assumes there is room
// (insert guarantees a prior grow), so it never resizes and is therefore safe to
// call from grow's reinsert loop.
func (t *table[K, V]) set(hash uint64, key K, value V) {
	curHash := hash
	curKey := key
	curVal := value
	curFrag := fragOf(hash)
	var curDib uint16
	swapped := false

	i := t.home(hash)
	for {
		if curDib > maxDib {
			// Probe chain would overflow the DIB field: grow and reinsert the entry
			// we are currently carrying (it is held in cur*, not in the table, so the
			// resize reinserts every other entry and this set re-homes the carried
			// one in the larger, shorter-chained table). Unreachable via the seeded
			// Map; prevents silent overflow corruption for degenerate hashes.
			t.grow()
			t.set(curHash, curKey, curVal)
			return
		}
		m := t.meta[i]
		if m == 0 {
			t.meta[i] = packMeta(curDib, curFrag)
			t.keys[i] = curKey
			t.vals[i] = curVal
			t.hashes[i] = curHash
			t.count++
			return
		}
		// Overwrite only applies to the original key (before any swap); a robbed
		// entry is already resident and unique, so it can never match here.
		if !swapped && m&fragMask == curFrag && t.keys[i] == curKey {
			t.vals[i] = curVal
			return
		}
		// Robin Hood: if we have probed farther than the resident, take this slot
		// and carry the resident onward ("rob from the rich").
		if rDib := dibOf(m); curDib > rDib {
			t.meta[i], curDib = packMeta(curDib, curFrag), rDib
			t.keys[i], curKey = curKey, t.keys[i]
			t.vals[i], curVal = curVal, t.vals[i]
			t.hashes[i], curHash = curHash, t.hashes[i]
			curFrag = fragOf(curHash)
			swapped = true
		}
		i = (i + 1) & t.capm1
		curDib++
	}
}

// lookup returns the value stored under key (identified by its precomputed hash)
// and whether it was found. It probes from the home slot and stops on a key
// match, an empty slot, or a resident whose DIB is smaller than the probe DIB —
// the Robin Hood early-exit invariant: had the key been present, it would have
// robbed that slot on insert.
func (t *table[K, V]) lookup(hash uint64, key K) (V, bool) {
	frag := fragOf(hash)
	i := t.home(hash)
	var dib uint16
	for {
		m := t.meta[i]
		if m == 0 {
			var zero V
			return zero, false
		}
		if dib > dibOf(m) {
			var zero V
			return zero, false
		}
		if m&fragMask == frag && t.keys[i] == key {
			return t.vals[i], true
		}
		i = (i + 1) & t.capm1
		dib++
	}
}

// grow doubles the table capacity and reinserts every live entry. Reinsertion
// reuses the cached hashes, so no key is ever rehashed.
func (t *table[K, V]) grow() {
	t.resize(len(t.meta) * 2)
}

// resize reallocates the table to newCap (a power of two) and reinserts all live
// entries from the old arrays using their cached hashes. It is the shared
// reinsert path for grow and shrink.
func (t *table[K, V]) resize(newCap int) {
	src := *t
	t.alloc(newCap)
	for i := range src.meta {
		if src.meta[i] != 0 {
			t.set(src.hashes[i], src.keys[i], src.vals[i])
		}
	}
}

// delete removes the entry stored under key (identified by its precomputed hash),
// returning the removed value and whether the key was present. It locates the key
// with the same probe rule as lookup (stop on match, empty, or the Robin Hood
// early-exit), then repairs the chain by backward-shift deletion: each following
// displaced entry is moved back one slot with its DIB decremented, stopping at an
// empty slot or an entry already in its home slot (DIB 0, which must not move).
// This leaves no tombstones and keeps probe chains contiguous and DIB-monotonic —
// exactly the state the table would hold had the key never been inserted
// (robin-hood deep dive §6). A delete that drops the load below ~0.25 shrinks.
func (t *table[K, V]) delete(hash uint64, key K) (V, bool) {
	frag := fragOf(hash)
	i := t.home(hash)
	var dib uint16
	for {
		m := t.meta[i]
		if m == 0 || dib > dibOf(m) {
			// Empty slot or early-exit: had the key been present it would have robbed
			// this slot on insert, so it cannot be here.
			var zero V
			return zero, false
		}
		if m&fragMask == frag && t.keys[i] == key {
			break // found at slot i
		}
		i = (i + 1) & t.capm1
		dib++
	}

	removed := t.vals[i]

	// Backward-shift: walk forward pulling each following displaced entry back one
	// slot and decrementing its DIB, until an empty slot or a home (DIB 0) entry.
	j := (i + 1) & t.capm1
	for t.meta[j] != 0 && dibOf(t.meta[j]) > 0 {
		t.meta[i] = packMeta(dibOf(t.meta[j])-1, t.meta[j]&fragMask)
		t.keys[i] = t.keys[j]
		t.vals[i] = t.vals[j]
		t.hashes[i] = t.hashes[j]
		i = j
		j = (j + 1) & t.capm1
	}

	// Vacate the tail slot and drop the K/V references so the GC can reclaim them:
	// the slot arrays outlive individual entries, so a stale copy would leak
	// (design §9 copy-on-write keeps stored bytes immutable, never reused in place).
	var zeroK K
	var zeroV V
	t.meta[i] = 0
	t.keys[i] = zeroK
	t.vals[i] = zeroV
	t.hashes[i] = 0
	t.count--

	t.shrink()
	return removed, true
}

// shrink halves the table when a delete has dropped the live count below ~0.25 of
// capacity and the capacity is above minCap, reinserting survivors through the
// same cached-hash resize path as grow. The 1/4 shrink threshold sits well below
// the 3/4 grow threshold so capacity cannot thrash (design §10).
func (t *table[K, V]) shrink() {
	curCap := len(t.meta)
	if curCap <= minCap {
		return
	}
	if t.count >= curCap*shrinkNum/shrinkDen {
		return
	}
	// curCap is a power of two greater than minCap, so curCap >= 2*minCap and
	// newCap = curCap/2 >= minCap — the floor is guaranteed, no clamp needed.
	t.resize(curCap / 2)
}
