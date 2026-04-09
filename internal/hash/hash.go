// Package hash is cachemoney's single hashing chokepoint: it produces 64-bit,
// seeded, HashDoS-resistant hashes and is the one place a faster hash could be
// swapped in (ADR-0008).
//
// Every cache operation hashes its key exactly once, at the shardmap boundary,
// and the resulting 64-bit value is split into disjoint bit ranges — high bits
// pick the shard, low bits pick the per-shard bucket (design §5). That split
// only works if the hash has good avalanche across all 64 bits, which seeded
// hash/maphash provides.
//
// # Seed semantics (HashDoS posture)
//
// The seed is process-random by default: NewSeed draws a fresh maphash.Seed at
// startup so an attacker cannot pre-compute colliding keys offline (the classic
// hash-flooding DoS). Consequently hashing is deterministic only WITHIN a single
// process for a fixed seed — the same key hashes identically as long as the same
// seed is reused, but two independently constructed seeds will (overwhelmingly
// likely) hash the same key to different values. Cross-process determinism is
// intentionally NOT a property; it is exactly the security trade-off we want.
// Tests that need reproducibility capture one NewSeed and inject it into both the
// system under test and its reference model.
//
// # The swap seam (ADR-0008)
//
// This file is the only place that names a concrete hash function. If a future
// benchmark proves maphash.String/maphash.Bytes is the bottleneck on the hot
// path, drop in cespare/xxhash or zeebo/xxh3 here — seeded to preserve DoS
// resistance — without touching shardmap or cache. Keep this surface tiny so the
// swap stays a one-file change.
package hash

import "hash/maphash"

// Seed is the seed type for the hashing seam — a type alias for maphash.Seed.
// Aliasing it here keeps the concrete stdlib seed type from leaking into callers'
// APIs (for example shardmap's WithSeed), so a future swap to a uint64-seeded hash
// such as xxhash/xxh3 stays close to a one-file change (ADR-0008).
type Seed = maphash.Seed

// NewSeed returns a fresh, process-random seed suitable for HashDoS-resistant
// hashing. Each shardmap.Map should hold its own seed (created once at
// construction) and reuse it for every operation so routing stays stable for the
// lifetime of that map.
func NewSeed() Seed {
	return maphash.MakeSeed()
}

// String returns the 64-bit seeded hash of s. It uses the Go 1.19+ one-shot
// maphash.String helper, which hashes without the buffering copy that the older
// maphash.Hash.Write path incurs — keeping the hot path allocation-free.
//
// For any seed, String(seed, s) equals Bytes(seed, []byte(s)).
func String(seed Seed, s string) uint64 {
	return maphash.String(seed, s)
}

// Bytes returns the 64-bit seeded hash of b. It is the []byte twin of String and
// uses the one-shot maphash.Bytes helper, so it allocates nothing on the hot
// path. A nil slice hashes the same as an empty one.
//
// For any seed, Bytes(seed, []byte(s)) equals String(seed, s).
func Bytes(seed Seed, b []byte) uint64 {
	return maphash.Bytes(seed, b)
}
