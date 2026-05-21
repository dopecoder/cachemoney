package tinylfu

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Drainer and default sizing constants.
const (
	// defaultRingDepth is the per-stripe buffer capacity when Config.RingDepth is 0.
	defaultRingDepth = 128

	// drainTick is the backstop drain interval. The drainer also wakes on demand when
	// a stripe fills, so this only bounds staleness under light traffic (design §5.5).
	drainTick = 20 * time.Millisecond

	// agingFactor scales the aging window: the sketch halves (and the doorkeeper
	// resets) every agingFactor*width increments (design §5.2).
	agingFactor = 1
)

// Config sizes a Policy from the engine's estimate of how many live entries fit.
type Config struct {
	// Counters is the expected live-entry count E. The sketch width is the next power
	// of two at or above it (floored at minWidth).
	Counters int
	// Stripes is the access-buffer stripe count; 0 selects next_pow2(GOMAXPROCS).
	Stripes int
	// RingDepth is the per-stripe ring capacity; 0 selects defaultRingDepth.
	RingDepth int
	// Seed personalizes the sketch and doorkeeper index derivation; fix it in tests
	// for reproducibility.
	Seed uint64
}

// Policy is the Ristretto-simplified W-TinyLFU facade: a Count-Min sketch and a
// doorkeeper bloom fed by striped lossy access buffers and a single async drainer.
// It is pure and net-free and speaks uint64 fingerprints, not keys. Record and
// Estimate are safe for concurrent use; Close exactly once (design §4.1, §10).
type Policy struct {
	buf    *accessBuffer
	active atomic.Bool

	// sk and dk are published behind atomic pointers so Resize can rebuild them
	// without locking the Record/Estimate paths. The drainer is their sole mutator.
	sk atomic.Pointer[sketch]
	dk atomic.Pointer[doorkeeper]

	seed  uint64
	incrs int // drainer-local increment counter for aging; touched only by the drainer

	wake      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// New builds a Policy and starts its drainer goroutine (parked until activated). The
// returned Policy must be Closed to stop and join the drainer.
func New(cfg Config) *Policy {
	stripes := cfg.Stripes
	if stripes <= 0 {
		stripes = runtime.GOMAXPROCS(0)
	}
	depthRing := cfg.RingDepth
	if depthRing <= 0 {
		depthRing = defaultRingDepth
	}
	p := &Policy{
		buf:  newAccessBuffer(stripes, depthRing),
		seed: cfg.Seed,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	p.sk.Store(newSketch(cfg.Counters, cfg.Seed))
	p.dk.Store(newDoorkeeper(cfg.Counters, cfg.Seed))

	p.wg.Add(1)
	go p.drain()
	return p
}

// SetActive toggles whether Record feeds the sketch. The engine enables it only under
// allkeys-lfu; otherwise Record is a cheap no-op with zero read-path overhead.
func (p *Policy) SetActive(on bool) {
	p.active.Store(on)
}

// Record is the read-path call: a NON-BLOCKING, LOSSY push of fp into a striped ring.
// A full or contended stripe drops the sample. It takes no sketch lock and never
// blocks; it signals the drainer only when a stripe crosses its high-water mark, so
// the wake channel stays off the hot path.
func (p *Policy) Record(fp uint64) {
	if !p.active.Load() {
		return
	}
	if p.buf.push(fp) {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// Estimate returns fp's approximate access frequency in [0, 15] from the sketch.
// It is called on the eviction path, races the drainer's writes safely via atomic
// word loads, and takes no lock.
func (p *Policy) Estimate(fp uint64) uint8 {
	return p.sk.Load().estimate(fp)
}

// Resize rebuilds the sketch and doorkeeper for a new expected-entry count, dropping
// accumulated frequency (it re-accumulates from the drainer within one aging window).
// The engine calls it when a live capacity change moves the sketch's power-of-two
// width bucket.
func (p *Policy) Resize(cfg Config) {
	p.sk.Store(newSketch(cfg.Counters, cfg.Seed))
	p.dk.Store(newDoorkeeper(cfg.Counters, cfg.Seed))
}

// Close signals the drainer to perform a final drain and exit, then joins it with no
// goroutine leak. It is idempotent.
func (p *Policy) Close() error {
	p.closeOnce.Do(func() {
		close(p.done)
		p.wg.Wait()
	})
	return nil
}

// drain is the sole-writer loop over the sketch and doorkeeper. It wakes on a stripe
// high-water signal or a ticker backstop, performs a final drain on Close, and exits.
func (p *Policy) drain() {
	defer p.wg.Done()
	ticker := time.NewTicker(drainTick)
	defer ticker.Stop()

	var scratch []uint64
	for {
		select {
		case <-p.done:
			p.drainOnce(&scratch)
			return
		case <-p.wake:
			p.drainOnce(&scratch)
		case <-ticker.C:
			p.drainOnce(&scratch)
		}
	}
}

// drainOnce folds all buffered fingerprints into the sketch through the doorkeeper:
// a first-seen fingerprint only enters the doorkeeper; a second sighting increments
// the sketch. Every agingN increments the sketch halves and the doorkeeper resets.
func (p *Policy) drainOnce(scratch *[]uint64) {
	sk := p.sk.Load()
	dk := p.dk.Load()
	agingN := agingFactor * sk.width()
	*scratch = p.buf.drainInto(*scratch, func(fp uint64) {
		if !dk.putOrHas(fp) {
			return // first sighting: filtered as a possible one-hit-wonder
		}
		sk.increment(fp)
		p.incrs++
		if p.incrs >= agingN {
			sk.halve()
			dk.reset()
			p.incrs = 0
		}
	})
}
