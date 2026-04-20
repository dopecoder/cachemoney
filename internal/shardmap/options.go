package shardmap

import "hash/maphash"

// Option configures a Map at construction time. Options are applied in order by
// New; later options override earlier ones for the same setting.
type Option func(*config)

// config holds the resolved construction settings collected from the Option list.
// It is internal: callers only ever see the functional options below.
type config struct {
	numShards  int          // requested shard count; 0 => derive the default
	seed       maphash.Seed // routing seed; meaningful only when seedSet is true
	seedSet    bool         // whether WithSeed supplied an explicit seed
	initialCap int          // total capacity hint, split across shards; 0 => none
}

// WithShards requests n shards. The value is floored at 1 and then rounded up to
// the next power of two by New, so shard selection stays a single shift with no
// modulo (design §5). A non-positive n is treated as a single shard.
func WithShards(n int) Option {
	return func(c *config) {
		if n < 1 {
			n = 1
		}
		c.numShards = n
	}
}

// WithSeed pins the hashing seed, making routing deterministic within the process.
// Tests inject one seed into both a Map and its reference model so the two route
// identically; production omits it and New draws a fresh process-random seed
// (HashDoS resistance, design §5).
func WithSeed(seed maphash.Seed) Option {
	return func(c *config) {
		c.seed = seed
		c.seedSet = true
	}
}

// WithInitialCapacity hints the total number of entries the Map will hold so the
// per-shard tables can be pre-sized and avoid an immediate grow. The hint is split
// evenly (rounded up) across the shards; a non-positive hint is ignored.
func WithInitialCapacity(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.initialCap = n
		}
	}
}

// nextPow2 returns the smallest power of two that is >= n, with a floor of 1.
func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// log2PowerOf2 returns the base-2 logarithm of n, which the caller guarantees is a
// power of two >= 1 (log2PowerOf2(1) == 0). It is the small, allocation-free
// companion to nextPow2 used once at construction to derive the shard shift.
func log2PowerOf2(n int) uint {
	var k uint
	for (1 << k) < n {
		k++
	}
	return k
}
