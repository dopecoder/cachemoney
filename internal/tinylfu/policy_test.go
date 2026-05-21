package tinylfu

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The Policy facade wires the buffer, drainer, sketch, and doorkeeper into the
// engine-facing contract: Record (lossy, only when active), Estimate, Resize, and a
// leak-free Close. These tests pin activation gating, eventual frequency learning
// through the async drainer, live resize, and clean shutdown.

func testConfig() Config {
	// A wide sketch keeps the aging window far away so a few increments accumulate
	// monotonically, making Estimate assertions deterministic.
	return Config{Counters: 4096, Stripes: 4, RingDepth: 128, Seed: 0x1234}
}

func TestPolicyInactiveRecordIsNoop(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	defer func() { _ = p.Close() }()

	// active defaults to false: Record must feed nothing to the sketch.
	const fp = 0xabcdef
	for i := 0; i < 50; i++ {
		p.Record(fp)
	}
	time.Sleep(50 * time.Millisecond) // give any (erroneous) drain a chance to run
	if got := p.Estimate(fp); got != 0 {
		t.Fatalf("inactive policy learned a frequency: estimate = %d, want 0", got)
	}
}

func TestPolicyLearnsFrequencyViaWake(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	defer func() { _ = p.Close() }()
	p.SetActive(true)

	// Flood one fingerprint past a stripe's high-water mark so Record signals wake.
	const fp = 0x55aa55aa
	for i := 0; i < 400; i++ {
		p.Record(fp)
	}
	eventually(t, 2*time.Second, func() bool { return p.Estimate(fp) > 0 })
}

func TestPolicyLearnsFrequencyViaTicker(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	defer func() { _ = p.Close() }()
	p.SetActive(true)

	// Few records (below high-water): no wake fires, so the ticker backstop must
	// still drain them. The doorkeeper admits to the sketch on the second sighting,
	// so several records are needed before the estimate moves.
	const fp = 0x99
	for i := 0; i < 6; i++ {
		p.Record(fp)
	}
	eventually(t, 2*time.Second, func() bool { return p.Estimate(fp) > 0 })
}

func TestPolicyResizeRebuildsFresh(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	defer func() { _ = p.Close() }()
	p.SetActive(true)

	const fp = 0x1357
	for i := 0; i < 400; i++ {
		p.Record(fp)
	}
	eventually(t, 2*time.Second, func() bool { return p.Estimate(fp) > 0 })

	cfg := testConfig()
	cfg.Counters = 1 << 16 // a different power-of-two bucket → rebuild
	p.Resize(cfg)
	if got := p.Estimate(fp); got != 0 {
		t.Fatalf("estimate survived a resize rebuild: %d, want 0", got)
	}
}

func TestPolicyDefaultsFromZeroConfig(t *testing.T) {
	t.Parallel()

	// A zero Config must yield a working policy: stripes default to GOMAXPROCS, ring
	// depth to its default, width to the floor.
	p := New(Config{})
	defer func() { _ = p.Close() }()
	p.SetActive(true)
	const fp = 0x2468
	for i := 0; i < 400; i++ {
		p.Record(fp)
	}
	eventually(t, 2*time.Second, func() bool { return p.Estimate(fp) > 0 })
}

func TestPolicyCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	if err := p.Close(); err != nil {
		t.Fatalf("first Close returned %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}
}

func TestPolicyRecordAfterCloseDoesNotPanic(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	p.SetActive(true)
	if err := p.Close(); err != nil {
		t.Fatalf("Close returned %v", err)
	}
	// A late read racing shutdown must not panic (stripes are slices, wake is a
	// non-blocking cap-1 send to an un-closed channel).
	for i := 0; i < 100; i++ {
		p.Record(uint64(i))
	}
}

func TestPolicyCloseJoinsDrainerNoLeak(t *testing.T) {
	// Not parallel: NumGoroutine is process-global.
	settleGoroutines()
	before := runtime.NumGoroutine()

	const policies = 16
	ps := make([]*Policy, policies)
	for i := range ps {
		ps[i] = New(testConfig())
		ps[i].SetActive(true)
	}
	for _, p := range ps {
		if err := p.Close(); err != nil {
			t.Fatalf("Close returned %v", err)
		}
	}

	eventually(t, 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= before
	})
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestPolicyConcurrentRecordEstimate(t *testing.T) {
	t.Parallel()

	p := New(testConfig())
	defer func() { _ = p.Close() }()
	p.SetActive(true)

	var wg sync.WaitGroup
	var reads atomic.Uint64

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(base uint64) {
			defer wg.Done()
			for i := uint64(0); i < 50000; i++ {
				fp := base ^ (i & 0x3ff)
				p.Record(fp)
				reads.Add(uint64(p.Estimate(fp)))
			}
		}(uint64(w) * 0x10000)
	}
	wg.Wait()
	_ = reads.Load()
}

// eventually polls cond until it returns true or the timeout elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// settleGoroutines gives background goroutines from earlier work a chance to exit so
// the leak baseline is stable.
func settleGoroutines() {
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPolicyConcurrentResize(t *testing.T) {
	t.Parallel()

	// Resize publishes fresh sketch/doorkeeper pointers from a non-drainer goroutine.
	// Driving Record + Estimate + Resize concurrently asserts (under -race) that the
	// atomic-pointer publication keeps the read/estimate paths safe across rebuilds.
	p := New(testConfig())
	defer func() { _ = p.Close() }()
	p.SetActive(true)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(base uint64) {
			defer wg.Done()
			for i := uint64(0); i < 200000; i++ {
				fp := base ^ (i & 0x3ff)
				p.Record(fp)
				_ = p.Estimate(fp)
			}
		}(uint64(w) * 0x1000)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		sizes := []int{1 << 12, 1 << 14, 1 << 16}
		for i := 0; i < 300; i++ {
			cfg := testConfig()
			cfg.Counters = sizes[i%len(sizes)]
			p.Resize(cfg)
		}
	}()

	wg.Wait()
}
