package tinylfu

// Doorkeeper sizing constants.
const (
	// doorkeeperBitsPerEntry sizes the bloom filter for a target false-positive rate
	// of ~0.05: bitsPerEntry ≈ -log2(p)/ln2 ≈ 6 (design §5.3).
	doorkeeperBitsPerEntry = 6

	// doorkeeperK is the number of bloom hash functions, derived from the same target
	// FPR (k ≈ -log2(p) ≈ 4). The k positions are produced by Kirsch–Mitzenmacher
	// double hashing from two base hashes, so only two integer mixes are computed.
	doorkeeperK = 4
)

// doorkeeper is the first-seen bloom filter that keeps one-hit-wonders out of the
// sketch: a fingerprint only reaches the sketch on its SECOND sighting. It is owned
// exclusively by the drainer goroutine, so it needs no synchronization, and it is
// cleared in lockstep with sketch aging so it never saturates (design §5.3).
type doorkeeper struct {
	bits []uint64
	mask uint64 // numBits-1; numBits is a power of two
	seed uint64
}

// newDoorkeeper builds a filter sized for entries fingerprints at the design FPR,
// floored at one 64-bit word so indexing is always well defined.
func newDoorkeeper(entries int, seed uint64) *doorkeeper {
	if entries < 0 {
		entries = 0
	}
	numBits := nextPow2(uint64(doorkeeperBitsPerEntry * entries)) //nolint:gosec // entries clamped to >=0; bit count is non-negative
	if numBits < 64 {
		numBits = 64
	}
	return &doorkeeper{
		bits: make([]uint64, numBits/64),
		mask: numBits - 1,
		seed: seed,
	}
}

// putOrHas records fp and reports whether it was ALREADY present. A first sighting
// returns false (and sets the bits); any later sighting returns true. The two base
// hashes feed Kirsch–Mitzenmacher double hashing to derive k bit positions.
func (d *doorkeeper) putOrHas(fp uint64) bool {
	h1 := fp ^ d.seed
	h2 := mix(fp) | 1 // odd step keeps the position sequence well distributed
	seen := true
	for i := 0; i < doorkeeperK; i++ {
		bit := (h1 + uint64(i)*h2) & d.mask //nolint:gosec // i is in [0,doorkeeperK); the product never overflows uint64
		word, b := bit>>6, uint64(1)<<(bit&63)
		if d.bits[word]&b == 0 {
			seen = false
			d.bits[word] |= b
		}
	}
	return seen
}

// reset clears the filter, dropping all first-seen memory. The drainer calls it at
// every aging point so the bloom never fills and starts returning "seen" for
// everything.
func (d *doorkeeper) reset() {
	for i := range d.bits {
		d.bits[i] = 0
	}
}
