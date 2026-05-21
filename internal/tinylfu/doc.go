// Package tinylfu is cachemoney's pure, net-free admission-frequency policy: a
// Ristretto-simplified W-TinyLFU built from a 4-bit Count-Min sketch, a doorkeeper
// bloom filter, striped lossy access buffers, and a single asynchronous drainer
// (ADR-0011, which supersedes ADR-0006). It is the library the engine composes to
// turn an unbounded map into a bounded, frequency-aware cache.
//
// # Vocabulary boundary
//
// This package speaks 64-bit fingerprints, counters, and buffers — nothing else.
// It does not know the words "key", "value", "Redis", "maxmemory", or "noeviction",
// and it imports no intra-repo package and no net: it is a leaf, enforced by an
// import guard. The engine (internal/cache) owns hashing keys to fingerprints, byte
// accounting, and victim selection; tinylfu only answers "how frequently has this
// fingerprint been accessed?" via Estimate, and absorbs access samples via Record.
//
// # The two invariants this design serves
//
// C-INV-2 (the read path stays contention-free) is why Record is a NON-BLOCKING,
// LOSSY push into a striped ring buffer: a full or contended stripe simply drops the
// sample. No read ever takes the sketch lock, and the sketch has no lock — a single
// drainer goroutine is its SOLE mutator, so Estimate and the drainer never contend.
// C-INV-1 (writes always stick) lives in the engine, not here; tinylfu never decides
// what to store, only reports frequencies the engine uses to pick OTHER victims.
//
// # Concurrency model
//
//   - Record (any goroutine): TryLock a stripe, append-or-drop, best-effort wake.
//   - drainer (one goroutine): the only writer of the sketch and doorkeeper. It
//     swaps each stripe's buffer out under that stripe's lock, then folds the drained
//     fingerprints through the doorkeeper into the sketch off-lock, ageing every N
//     increments by halving all counters and clearing the doorkeeper.
//   - Estimate (any goroutine): atomic word loads of the sketch — lock-free, and
//     race-free against the drainer because every counter word is read/written
//     atomically and an in-flight increment reads either the pre- or post-value,
//     both valid 4-bit numbers for an approximate estimator.
//   - Close: signal the drainer's done channel and join it; idempotent, no leak.
//
// The sketch and doorkeeper are published behind atomic pointers so Resize can
// rebuild them on a live capacity change without locking the read or estimate paths.
package tinylfu

import "math/bits"

// nextPow2 returns the smallest power of two greater than or equal to n, with a
// floor of one. It is the sizing primitive for the sketch width, the doorkeeper bit
// count, and the buffer stripe count — all of which must be powers of two so an
// index can be derived with a single mask.
func nextPow2(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	return uint64(1) << bits.Len64(n-1)
}

// mix is a splitmix64 finalizer: a fast, allocation-free integer hash that gives a
// fingerprint good avalanche across all 64 bits. The sketch and doorkeeper use it to
// derive several independent indices from one fingerprint without a stateful hasher.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
