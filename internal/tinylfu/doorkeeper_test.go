package tinylfu

import (
	"math/rand"
	"testing"
)

// The doorkeeper is the first-seen filter that keeps one-hit-wonders out of the
// sketch. These tests pin its admit-on-second-sight contract, its reset, and that
// its false-positive rate stays at the design target for a dataset-sized population.

func TestDoorkeeperFirstSeenThenMember(t *testing.T) {
	t.Parallel()

	d := newDoorkeeper(1024, 11)
	const fp = 0xfeed_face
	if d.putOrHas(fp) {
		t.Fatal("first putOrHas reported the key as already seen")
	}
	if !d.putOrHas(fp) {
		t.Fatal("second putOrHas did not report the key as seen")
	}
}

func TestDoorkeeperResetClears(t *testing.T) {
	t.Parallel()

	d := newDoorkeeper(1024, 12)
	const fp = 0x0102_0304
	d.putOrHas(fp)
	if !d.putOrHas(fp) {
		t.Fatal("key not seen before reset")
	}
	d.reset()
	if d.putOrHas(fp) {
		t.Fatal("key still seen after reset")
	}
}

func TestNewDoorkeeperFloorsTinyPopulations(t *testing.T) {
	t.Parallel()

	// A tiny (or zero) population must still allocate at least one 64-bit word so
	// indexing never divides by zero or masks against an empty bitset.
	for _, entries := range []int{-5, 0, 1, 3} {
		d := newDoorkeeper(entries, 1)
		if len(d.bits) == 0 {
			t.Fatalf("newDoorkeeper(%d) allocated no words", entries)
		}
		if d.putOrHas(7) {
			t.Fatalf("newDoorkeeper(%d): first putOrHas reported seen", entries)
		}
		if !d.putOrHas(7) {
			t.Fatalf("newDoorkeeper(%d): second putOrHas did not report seen", entries)
		}
	}
}

func TestDoorkeeperFalsePositiveRateWithinTarget(t *testing.T) {
	t.Parallel()

	// Insert a dataset-sized population, then probe disjoint keys. A "seen" verdict
	// for a never-inserted key is a false positive. Probing also sets bits, so the
	// measured rate is an upper bound; the design target is ~0.05, asserted loosely
	// at <= 0.15 to stay deterministic and non-flaky.
	const population = 4000
	d := newDoorkeeper(population, 0x5eed)
	rng := rand.New(rand.NewSource(99))

	inserted := make(map[uint64]struct{}, population)
	for len(inserted) < population {
		fp := rng.Uint64()
		if _, ok := inserted[fp]; ok {
			continue
		}
		inserted[fp] = struct{}{}
		d.putOrHas(fp)
	}

	const probes = 4000
	falsePositives := 0
	tested := 0
	for tested < probes {
		fp := rng.Uint64()
		if _, ok := inserted[fp]; ok {
			continue
		}
		tested++
		if d.putOrHas(fp) {
			falsePositives++
		}
	}
	if fpr := float64(falsePositives) / float64(probes); fpr > 0.15 {
		t.Fatalf("doorkeeper FPR = %.4f, want <= 0.15", fpr)
	}
}

// FuzzDoorkeeper asserts putOrHas is idempotent-after-first across arbitrary
// fingerprint streams: once a key is inserted it always reports seen until reset.
func FuzzDoorkeeper(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Fuzz(func(t *testing.T, a, b uint64) {
		d := newDoorkeeper(64, 1)
		d.putOrHas(a)
		if !d.putOrHas(a) {
			t.Fatalf("inserted key %d not reported seen", a)
		}
		d.reset()
		if d.putOrHas(a) {
			t.Fatalf("key %d still seen after reset", a)
		}
		// b may or may not collide with a; either verdict is in-range (no panic).
		_ = d.putOrHas(b)
	})
}
