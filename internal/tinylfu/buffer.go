package tinylfu

import "sync"

// stripe is one lane of the access buffer: a bounded slice of buffered fingerprints
// guarded by its own mutex, so concurrent readers contend on different lanes.
type stripe struct {
	mu  sync.Mutex
	buf []uint64
}

// accessBuffer is the striped, bounded, lossy ring the read path pushes into. A
// fingerprint selects its stripe by low bits; push never blocks (TryLock, drop on
// contention or fullness). The drainer empties each stripe under its own lock by
// swapping the buffered slice into a reusable scratch (design §5.4).
type accessBuffer struct {
	stripes []stripe
	mask    uint64 // stripe count - 1; the count is a power of two
	depth   int    // per-stripe capacity (high-water == full)
}

// newAccessBuffer builds a buffer with the given stripe count (rounded up to a power
// of two) and per-stripe depth.
func newAccessBuffer(stripes, depth int) *accessBuffer {
	count := nextPow2(uint64(stripes)) //nolint:gosec // stripe count is a small positive int (GOMAXPROCS or a positive config)
	if depth < 1 {
		depth = 1
	}
	b := &accessBuffer{
		stripes: make([]stripe, count),
		mask:    count - 1,
		depth:   depth,
	}
	for i := range b.stripes {
		b.stripes[i].buf = make([]uint64, 0, depth)
	}
	return b
}

// push records fp into its stripe without ever blocking, and reports whether this
// push just filled the stripe (the wake hint). A contended stripe (TryLock fails) or
// a full stripe drops the sample and returns false, so the hot path stays lossy and
// lock-free under contention.
func (b *accessBuffer) push(fp uint64) bool {
	s := &b.stripes[fp&b.mask]
	if !s.mu.TryLock() {
		return false
	}
	if len(s.buf) >= b.depth {
		s.mu.Unlock()
		return false
	}
	s.buf = append(s.buf, fp)
	full := len(s.buf) >= b.depth
	s.mu.Unlock()
	return full
}

// drainInto folds every buffered fingerprint through fn, emptying each stripe under
// its own lock. The caller-owned scratch is reused across calls to avoid per-drain
// allocation; the (possibly grown) scratch is returned for the next call.
func (b *accessBuffer) drainInto(scratch []uint64, fn func(fp uint64)) []uint64 {
	for i := range b.stripes {
		s := &b.stripes[i]
		s.mu.Lock()
		if len(s.buf) == 0 {
			s.mu.Unlock()
			continue
		}
		scratch = append(scratch[:0], s.buf...)
		s.buf = s.buf[:0]
		s.mu.Unlock()
		for _, fp := range scratch {
			fn(fp)
		}
	}
	return scratch
}
