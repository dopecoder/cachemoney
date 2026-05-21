package tinylfu

import "sync/atomic"

// Sketch sizing and counter constants.
const (
	// depth is the number of Count-Min rows. Four is the Ristretto/Caffeine standard:
	// the overestimate probability falls geometrically in the row count while four
	// hashes keep the per-access cost low (design §5.1).
	depth = 4

	// nibblesPerWord is how many 4-bit counters pack into one uint64 (64/4). Packing
	// maximizes cache density and lets the drainer mutate and Estimate read a whole
	// word atomically.
	nibblesPerWord = 16

	// nibbleMax is the saturation value of a 4-bit counter.
	nibbleMax = 15

	// minWidth floors the sketch at a single packed word (16 counters) so a tiny
	// maxmemory cannot degenerate the estimator into an empty bitset.
	minWidth = nibblesPerWord

	// halfNibbles masks out the bit that bleeds across nibble boundaries when an
	// entire 64-bit word is shifted right by one — turning a word-wide shift into a
	// per-nibble halving (each nibble's top bit cleared).
	halfNibbles = 0x7777_7777_7777_7777
)

// rowSeeds give each Count-Min row an independent index derivation from a single
// fingerprint, so the four counters a fingerprint touches are (almost surely)
// distinct. The values are arbitrary odd 64-bit constants with good bit spread.
var rowSeeds = [depth]uint64{
	0x9e3779b97f4a7c15,
	0xbf58476d1ce4e5b9,
	0x94d049bb133111eb,
	0xd6e8feb86659fd93,
}

// sketch is a 4-bit Count-Min frequency estimator with conservative update and
// halve-on-N aging. Each row is a []atomic.Uint64 of width/16 words; a fingerprint
// indexes one counter per row. The drainer is the SOLE mutator (increment, halve);
// Estimate reads concurrently via atomic word loads, so the structure needs no lock
// (design §5.1, §10).
type sketch struct {
	rows [depth][]atomic.Uint64
	mask uint64 // width-1; width is a power of two
	wd   int    // width: counters per row, a small power of two >= minWidth
	seed uint64
}

// newSketch builds a sketch whose width is the next power of two at or above width,
// floored at minWidth. seed personalizes the index derivation so two sketches
// disperse fingerprints differently.
func newSketch(width int, seed uint64) *sketch {
	if width < minWidth {
		width = minWidth
	}
	w := int(nextPow2(uint64(width))) //nolint:gosec // width is floored to minWidth>0; its next power of two fits int
	words := w / nibblesPerWord
	s := &sketch{mask: uint64(w - 1), wd: w, seed: seed} //nolint:gosec // w>=minWidth>0; w-1 is a non-negative count
	for i := range s.rows {
		s.rows[i] = make([]atomic.Uint64, words)
	}
	return s
}

// width reports the counter count per row (the aging window base; design §5.2).
func (s *sketch) width() int {
	return s.wd
}

// counterIndex maps a fingerprint to its counter index in the given row.
func (s *sketch) counterIndex(fp uint64, row int) uint64 {
	return mix(fp^s.seed^rowSeeds[row]) & s.mask
}

// read returns the 4-bit counter value for idx in the given row.
func (s *sketch) read(row int, idx uint64) uint8 {
	word := s.rows[row][idx>>4].Load()
	shift := (idx & (nibblesPerWord - 1)) * 4
	return uint8((word >> shift) & nibbleMax) //nolint:gosec // masked to nibbleMax (<=15); lossless narrowing
}

// raise increments the 4-bit counter at idx by one. The caller MUST guarantee the
// counter is below nibbleMax (it always equals the row minimum, which is < nibbleMax
// when raise is reached), so adding 1<<shift cannot carry into the neighbouring
// nibble. The drainer is the only caller, so the load/store needs no CAS.
func (s *sketch) raise(row int, idx uint64) {
	w := &s.rows[row][idx>>4]
	shift := (idx & (nibblesPerWord - 1)) * 4
	w.Store(w.Load() + (uint64(1) << shift))
}

// increment applies one conservative-update increment for fp: it bumps only the
// row counters that sit at the current minimum, keeping estimate a tight upper
// bound. When every counter is already saturated there is nothing to do.
func (s *sketch) increment(fp uint64) {
	var idx [depth]uint64
	var val [depth]uint8
	lo := uint8(nibbleMax)
	for i := 0; i < depth; i++ {
		idx[i] = s.counterIndex(fp, i)
		val[i] = s.read(i, idx[i])
		if val[i] < lo {
			lo = val[i]
		}
	}
	if lo >= nibbleMax {
		return
	}
	for i := 0; i < depth; i++ {
		if val[i] == lo {
			s.raise(i, idx[i])
		}
	}
}

// estimate returns fp's approximate access frequency: the minimum of its four
// counters, in [0, nibbleMax].
func (s *sketch) estimate(fp uint64) uint8 {
	lo := uint8(nibbleMax)
	for i := 0; i < depth; i++ {
		if v := s.read(i, s.counterIndex(fp, i)); v < lo {
			lo = v
		}
	}
	return lo
}

// halve ages the sketch by right-shifting every 4-bit counter, geometrically
// decaying stale frequency and preventing saturation. Shifting a whole word right
// by one and masking halfNibbles halves all 16 packed counters at once.
func (s *sketch) halve() {
	for i := range s.rows {
		row := s.rows[i]
		for j := range row {
			row[j].Store((row[j].Load() >> 1) & halfNibbles)
		}
	}
}
