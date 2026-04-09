package hash_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/hash"
)

// representativeKeys covers the inputs the hash seam must handle: empty, ascii,
// sequential (clustering bait), long, and binary keys containing 0x00 / 0xff.
func representativeKeys() []string {
	return []string{
		"",
		"a",
		"key",
		"user:1000",
		"user:1001",
		"user:1002",
		strings.Repeat("long-key-", 512), // long key (>4KiB)
		string([]byte{0x00}),
		string([]byte{0xff}),
		string([]byte{0x00, 0xff, 0x10, 0x00}),
		string([]byte{0xff, 0xfe, 0xfd, 0x00, 0x01, 0x02}),
	}
}

// TestString_DeterministicUnderFixedSeed pins the spec scenario "Seeded hashing
// is per-process random yet deterministic under a fixed seed": with one captured
// seed, String(seed, k) must be identical across repeated calls in this process.
func TestString_DeterministicUnderFixedSeed(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	for _, k := range representativeKeys() {
		want := hash.String(seed, k)
		for i := 0; i < 8; i++ {
			if got := hash.String(seed, k); got != want {
				t.Fatalf("String(seed, %q) not deterministic: call %d = %#x, want %#x", k, i, got, want)
			}
		}
	}
}

// TestBytes_DeterministicUnderFixedSeed mirrors the determinism guarantee for the
// []byte entry point.
func TestBytes_DeterministicUnderFixedSeed(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	for _, k := range representativeKeys() {
		b := []byte(k)
		want := hash.Bytes(seed, b)
		for i := 0; i < 8; i++ {
			if got := hash.Bytes(seed, b); got != want {
				t.Fatalf("Bytes(seed, %q) not deterministic: call %d = %#x, want %#x", k, i, got, want)
			}
		}
	}
}

// TestStringBytesParity asserts String(seed, s) == Bytes(seed, []byte(s)) for
// every representative key, including empty and binary inputs. This is what lets
// the engine hash a string key once and the shardmap route identically.
func TestStringBytesParity(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	for _, k := range representativeKeys() {
		s := hash.String(seed, k)
		b := hash.Bytes(seed, []byte(k))
		if s != b {
			t.Errorf("parity mismatch for %q: String=%#x Bytes=%#x", k, s, b)
		}
	}
}

// TestDistributionSpread is a cheap avalanche/uniformity smoke (not a statistical
// suite): sequential keys must NOT cluster. With one fixed seed, hashing a small
// set of distinct keys must yield distinct 64-bit values with good spread in BOTH
// the high bits (shard selection) and the low bits (bucket selection) — the two
// disjoint ranges the routing layer relies on (design §5).
func TestDistributionSpread(t *testing.T) {
	t.Parallel()

	const n = 256
	seed := hash.NewSeed()

	full := make(map[uint64]struct{}, n)
	high := make(map[uint16]struct{}, n)
	low := make(map[uint16]struct{}, n)

	for i := 0; i < n; i++ {
		// Sequential keys are the classic clustering bait for a weak hash.
		k := "user:" + strconv.Itoa(1000+i)
		h := hash.String(seed, k)
		full[h] = struct{}{}
		high[uint16(h>>48)] = struct{}{}
		low[uint16(h)] = struct{}{}
	}

	// All 64-bit outputs distinct: collisions among 256 distinct keys would
	// indicate a catastrophically bad hash (birthday bound makes ~0 expected).
	if len(full) != n {
		t.Errorf("expected %d distinct 64-bit hashes, got %d (collisions present)", n, len(full))
	}
	// Good spread in each disjoint 16-bit projection. 256 keys into 65536 buckets
	// expect ~0.5 collisions; a threshold of 240 tolerates noise yet still catches
	// a hash with no avalanche (which would collapse the projection).
	if len(high) < 240 {
		t.Errorf("poor high-bit spread: only %d distinct top-16-bit values of %d", len(high), n)
	}
	if len(low) < 240 {
		t.Errorf("poor low-bit spread: only %d distinct bottom-16-bit values of %d", len(low), n)
	}
}

// TestParity_EdgeKeys triangulates the parity + determinism guarantees against the
// tricky inputs: empty, long, NUL-only, 0xff-only, and mixed binary keys. For each,
// String must equal Bytes (parity) and repeat calls must agree (determinism).
func TestParity_EdgeKeys(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	cases := []struct {
		name string
		key  []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"single-nul", []byte{0x00}},
		{"single-ff", []byte{0xff}},
		{"nul-embedded", []byte{0x00, 0xff, 0x10, 0x00}},
		{"high-bytes", []byte{0xff, 0xfe, 0xfd, 0xfc}},
		{"long-ascii", []byte(strings.Repeat("long-key-", 1024))},
		{"long-nul", make([]byte, 8192)}, // 8 KiB of 0x00
		{"long-ff", bytesRepeat(0xff, 8192)},
		{"mixed-binary", []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff, 0x00}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := hash.String(seed, string(tc.key))
			b := hash.Bytes(seed, tc.key)
			if s != b {
				t.Fatalf("parity mismatch: String=%#x Bytes=%#x", s, b)
			}
			if again := hash.Bytes(seed, tc.key); again != b {
				t.Fatalf("non-deterministic: %#x then %#x", b, again)
			}
		})
	}
}

// TestNewSeed_FreshnessAcrossSeeds documents the HashDoS posture: two independent
// NewSeed() values must hash the same key to different outputs (within-process
// observable; NOT a claim of cross-process determinism — design §2.1 nuance).
func TestNewSeed_FreshnessAcrossSeeds(t *testing.T) {
	t.Parallel()

	const key = "user:1000"
	seedA := hash.NewSeed()
	seedB := hash.NewSeed()

	if hash.String(seedA, key) == hash.String(seedB, key) {
		t.Errorf("two fresh seeds produced the same hash for %q; seed is not randomizing (HashDoS exposure)", key)
	}
}

// TestNewSeed_IsUsableSeed sanity-checks that the returned seed type plugs into
// the same maphash API the seam is built on.
func TestNewSeed_IsUsableSeed(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	got := hash.String(seed, "probe")
	if got != hash.String(seed, "probe") {
		t.Fatalf("seed not stable: %#x", got)
	}
}

// TestBytes_NilEqualsEmpty pins the documented invariant that a nil slice hashes
// identically to an empty one (maphash reads len bytes only). The engine relies on
// this when cloneBytes maps nil -> empty non-nil (increment 7), so route + value
// handling stay consistent for nil inputs.
func TestBytes_NilEqualsEmpty(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	if hash.Bytes(seed, nil) != hash.Bytes(seed, []byte{}) {
		t.Fatalf("nil and empty slice hashed differently")
	}
	if hash.Bytes(seed, nil) != hash.String(seed, "") {
		t.Fatalf("nil bytes hash != empty string hash")
	}
}

// TestNoAllocations locks the ADR-0008 allocation-free hot-path guarantee: the
// one-shot maphash helpers must not allocate. A regression (e.g. swapping in a
// buffered hasher) would fail here loudly instead of silently taxing every op.
func TestNoAllocations(t *testing.T) {
	seed := hash.NewSeed()
	const key = "user:1000"
	b := []byte(key)

	if n := testing.AllocsPerRun(1000, func() { _ = hash.String(seed, key) }); n != 0 {
		t.Errorf("String allocates %v per call, want 0", n)
	}
	if n := testing.AllocsPerRun(1000, func() { _ = hash.Bytes(seed, b) }); n != 0 {
		t.Errorf("Bytes allocates %v per call, want 0", n)
	}
}

// bytesRepeat returns a slice of n bytes all equal to v.
func bytesRepeat(v byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}
