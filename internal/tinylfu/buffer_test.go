package tinylfu

import (
	"sync"
	"testing"
)

// The access buffer is the C-INV-2 engine: a striped, bounded, lossy ring that the
// read path pushes into without ever blocking. push reports only a wake hint (true
// when a stripe crosses its high-water mark); acceptance is observed by draining.
// These tests pin the non-blocking drop-on-contention / drop-on-full contract.

func TestAccessBufferPushDrainRoundTrips(t *testing.T) {
	t.Parallel()

	b := newAccessBuffer(4, 8)
	want := []uint64{1, 2, 3, 5, 8}
	for _, fp := range want {
		b.push(fp) // few, spread across stripes → all accepted
	}

	got := map[uint64]int{}
	b.drainInto(nil, func(fp uint64) { got[fp]++ })
	for _, fp := range want {
		if got[fp] != 1 {
			t.Fatalf("drained count for %d = %d, want 1", fp, got[fp])
		}
	}
}

func TestAccessBufferDropsWhenFull(t *testing.T) {
	t.Parallel()

	// Stripe 0 holds fingerprints whose low bits are 0. Fill it to depth: the push
	// that fills it must report the wake hint, and the next same-stripe push drops.
	const stripes, depth = 4, 4
	b := newAccessBuffer(stripes, depth)
	for i := 0; i < depth; i++ {
		fp := uint64(i * stripes) // all map to stripe 0
		woke := b.push(fp)
		if i == depth-1 && !woke {
			t.Fatal("the filling push did not report the wake hint")
		}
		if i < depth-1 && woke {
			t.Fatalf("push %d reported a wake hint before the stripe was full", i)
		}
	}
	if b.push(uint64(depth * stripes)) {
		t.Fatal("push into a full stripe reported a wake hint, want silent drop")
	}

	drained := 0
	b.drainInto(nil, func(uint64) { drained++ })
	if drained != depth {
		t.Fatalf("drained %d, want %d (overflow must be dropped)", drained, depth)
	}
}

func TestAccessBufferDropsWhenContended(t *testing.T) {
	t.Parallel()

	b := newAccessBuffer(4, 8)
	// Hold stripe 0's lock so a push targeting stripe 0 TryLock-fails and drops.
	b.stripes[0].mu.Lock()
	if b.push(0) {
		b.stripes[0].mu.Unlock()
		t.Fatal("push into a contended stripe reported a wake hint")
	}
	b.stripes[0].mu.Unlock()

	// Nothing landed in stripe 0; a push to a different stripe is accepted.
	got := map[uint64]int{}
	b.push(1)
	b.drainInto(nil, func(fp uint64) { got[fp]++ })
	if got[0] != 0 {
		t.Fatal("a contended push was not dropped")
	}
	if got[1] != 1 {
		t.Fatal("an uncontended push was lost")
	}
}

func TestAccessBufferDrainEmptyIsNoop(t *testing.T) {
	t.Parallel()

	b := newAccessBuffer(4, 8)
	calls := 0
	b.drainInto(nil, func(uint64) { calls++ })
	if calls != 0 {
		t.Fatalf("drainInto over an empty buffer made %d calls, want 0", calls)
	}
}

func TestAccessBufferScratchIsReused(t *testing.T) {
	t.Parallel()

	b := newAccessBuffer(2, 8)
	b.push(0)
	b.push(1)
	out := b.drainInto(make([]uint64, 0, 8), func(uint64) {})
	if cap(out) == 0 {
		t.Fatal("drainInto did not return a usable scratch slice")
	}
}

func TestNewAccessBufferClampsDepthToOne(t *testing.T) {
	t.Parallel()

	// A non-positive depth is clamped to a usable minimum of one slot per stripe.
	b := newAccessBuffer(1, 0)
	if !b.push(0) {
		t.Fatal("first push into a depth-1 stripe did not fill it")
	}
	if b.push(0) {
		t.Fatal("second push into a full depth-1 stripe was not dropped")
	}
	drained := 0
	b.drainInto(nil, func(uint64) { drained++ })
	if drained != 1 {
		t.Fatalf("drained %d, want 1", drained)
	}
}

func TestAccessBufferConcurrentPushDrain(t *testing.T) {
	t.Parallel()

	b := newAccessBuffer(8, 64)
	var drainerWG, pushersWG sync.WaitGroup
	stop := make(chan struct{})

	drainerWG.Add(1)
	go func() {
		defer drainerWG.Done()
		var scratch []uint64
		for {
			select {
			case <-stop:
				b.drainInto(scratch, func(uint64) {})
				return
			default:
				scratch = b.drainInto(scratch, func(uint64) {})
			}
		}
	}()

	for w := 0; w < 8; w++ {
		pushersWG.Add(1)
		go func(seed uint64) {
			defer pushersWG.Done()
			for i := uint64(0); i < 100000; i++ {
				b.push(seed*100000 + i) // must never block or race
			}
		}(uint64(w))
	}

	pushersWG.Wait()
	close(stop)
	drainerWG.Wait()
}
