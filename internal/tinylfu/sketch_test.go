package tinylfu

import (
	"math/rand"
	"testing"
)

// The sketch is the property-tested heart of the policy: a 4-bit packed Count-Min
// estimator with conservative update and halve-on-N aging. These tests pin the
// contract the drainer relies on — estimate is a monotonic, +1-bounded, saturating
// upper bound, and aging geometrically decays every counter.

func TestNewSketchNormalizesWidth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int
		want int
	}{
		{in: 1, want: minWidth},  // below the floor → floored
		{in: minWidth, want: 16}, // exact power of two preserved
		{in: 100, want: 128},     // rounded up to the next power of two
		{in: 4096, want: 4096},   // already a power of two
		{in: 0, want: minWidth},  // zero → floored
		{in: -7, want: minWidth}, // negative → floored
	}
	for _, tc := range cases {
		if got := newSketch(tc.in, 1).width(); got != tc.want {
			t.Errorf("newSketch(%d).width() = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSketchZeroEstimateForUnseen(t *testing.T) {
	t.Parallel()

	s := newSketch(1024, 42)
	if got := s.estimate(0xdeadbeef); got != 0 {
		t.Fatalf("estimate of unseen fp = %d, want 0", got)
	}
}

func TestSketchSingleKeyCountsExactly(t *testing.T) {
	t.Parallel()

	// In a wide sketch a single key's four counters are (almost surely) collision
	// free, so its estimate equals the increment count until it saturates at 15.
	s := newSketch(1<<14, 7)
	const fp = 0x1234_5678_9abc_def0
	for i := 1; i <= 30; i++ {
		s.increment(fp)
		want := uint8(i)
		if want > nibbleMax {
			want = nibbleMax
		}
		if got := s.estimate(fp); got != want {
			t.Fatalf("after %d increments estimate = %d, want %d", i, got, want)
		}
	}
}

func TestSketchSaturatesAtFifteen(t *testing.T) {
	t.Parallel()

	s := newSketch(1024, 1)
	const fp = 99
	for i := 0; i < 100; i++ {
		s.increment(fp)
	}
	if got := s.estimate(fp); got != nibbleMax {
		t.Fatalf("saturated estimate = %d, want %d", got, nibbleMax)
	}
}

func TestSketchEstimateIsMonotonicAndStepBounded(t *testing.T) {
	t.Parallel()

	// Property: a single increment(fp) never lowers estimate(fp) and raises it by at
	// most one. This is the invariant conservative update guarantees and the drainer
	// depends on for a stable frequency signal.
	s := newSketch(512, 0xabc)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		fp := rng.Uint64()
		before := s.estimate(fp)
		s.increment(fp)
		after := s.estimate(fp)
		if after < before {
			t.Fatalf("estimate decreased on increment: before=%d after=%d", before, after)
		}
		if after > before+1 {
			t.Fatalf("estimate jumped by more than one: before=%d after=%d", before, after)
		}
		if after > nibbleMax {
			t.Fatalf("estimate exceeded nibble max: %d", after)
		}
	}
}

func TestSketchHalveDecaysEveryCounter(t *testing.T) {
	t.Parallel()

	s := newSketch(1<<13, 5)
	fps := []uint64{1, 2, 3, 5, 8, 13, 21}
	// Drive each fp to a distinct level, then halve and assert each estimate is the
	// floor of half its prior value (collision-free in a wide sketch).
	levels := make(map[uint64]uint8, len(fps))
	for _, fp := range fps {
		n := int(fp % 16)
		for i := 0; i < n; i++ {
			s.increment(fp)
		}
		levels[fp] = s.estimate(fp)
	}
	s.halve()
	for _, fp := range fps {
		if got, want := s.estimate(fp), levels[fp]>>1; got != want {
			t.Fatalf("after halve estimate(%d) = %d, want %d (was %d)", fp, got, want, levels[fp])
		}
	}
}

func TestSketchHalveOnSaturatedKey(t *testing.T) {
	t.Parallel()

	s := newSketch(1024, 9)
	const fp = 77
	for i := 0; i < 40; i++ {
		s.increment(fp)
	}
	s.halve()
	if got := s.estimate(fp); got != nibbleMax>>1 {
		t.Fatalf("halved-from-saturated estimate = %d, want %d", got, nibbleMax>>1)
	}
}

func TestSketchConservativeUpdateLeavesNonMinimumUntouched(t *testing.T) {
	t.Parallel()

	// The distinguishing property of conservative update vs plain Count-Min: a single
	// increment(fp) bumps ONLY the row counters sitting at the current minimum, never
	// the ones already above it. Rows are independent backing slices, so we can pre-load
	// one of fp's counters via raise() and confirm increment leaves it untouched.
	s := newSketch(1024, 123)
	const fp = 0xC0FFEE
	i0 := s.counterIndex(fp, 0)
	for k := 0; k < 5; k++ {
		s.raise(0, i0) // row 0's counter → 5; rows 1..3 stay at 0 (the minimum)
	}
	if got := s.read(0, i0); got != 5 {
		t.Fatalf("setup: row0 counter = %d, want 5", got)
	}

	s.increment(fp)

	if got := s.read(0, i0); got != 5 {
		t.Fatalf("conservative update bumped a non-minimum counter: row0 = %d, want 5", got)
	}
	for r := 1; r < depth; r++ {
		ir := s.counterIndex(fp, r)
		if got := s.read(r, ir); got != 1 {
			t.Fatalf("minimum counter row%d = %d, want 1", r, got)
		}
	}
	if got := s.estimate(fp); got != 1 {
		t.Fatalf("estimate = %d, want 1 (the new row minimum)", got)
	}
}

// FuzzSketch throws arbitrary fingerprint streams at the sketch and asserts the
// estimator never panics and never reports a value outside [0, 15].
func FuzzSketch(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		s := newSketch(256, 3)
		var fp uint64
		for i, b := range data {
			fp = fp<<8 | uint64(b)
			if i%8 == 7 {
				s.increment(fp)
				if got := s.estimate(fp); got > nibbleMax {
					t.Fatalf("estimate out of range: %d", got)
				}
			}
		}
		if len(data) > 64 {
			s.halve()
		}
		// A final sweep must still produce in-range estimates.
		if got := s.estimate(fp); got > nibbleMax {
			t.Fatalf("post-halve estimate out of range: %d", got)
		}
	})
}
