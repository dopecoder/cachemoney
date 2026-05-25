package cache

import (
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ===========================================================================
// Eviction PR B / I4 — engine byte accounting, Set-evicts, Get-logs, Stats, Close
// ===========================================================================
//
// These tests pin the engine-level eviction contract: bounded memory (Req 1), the
// two invariants C-INV-1 SET-always-stores (Req 2) and C-INV-2 contention-free reads
// (Req 3), TTL/value preservation (Req 4), exact byte accounting (Req 5), TinyLFU
// hot-key retention (Req 6 s1), drainer-lifecycle Close (Req 11 s1), and Stats (Req 12).

func bg() context.Context { return context.Background() }

// fakeClock is a manually advanced clock for deterministic TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// recomputeUsage independently sums the cost of every physically-present entry, the
// ground truth the running counter must equal.
func recomputeUsage(c *Cache) int64 {
	var sum int64
	c.m.Range(func(k string, e entry) bool {
		sum += costOf(k, e.value)
		return true
	})
	return sum
}

func mustSet(t *testing.T, c *Cache, key string, val []byte) {
	t.Helper()
	if err := c.Set(bg(), key, val, 0); err != nil {
		t.Fatalf("Set(%q) = %v", key, err)
	}
}

func mustGet(t *testing.T, c *Cache, key string) ([]byte, bool) {
	t.Helper()
	v, ok, err := c.Get(bg(), key)
	if err != nil {
		t.Fatalf("Get(%q) = %v", key, err)
	}
	return v, ok
}

func val(n int) []byte { return make([]byte, n) }

func TestEvictionBoundsSustainedWrites(t *testing.T) {
	t.Parallel()

	const maxBytes = 64 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4))
	defer func() { _ = c.Close() }()

	oneCost := costOf("key000000", val(100))
	for i := 0; i < 5000; i++ {
		mustSet(t, c, "key"+strconv.Itoa(i), val(100))
		if u := c.usage.Load(); u > maxBytes+oneCost {
			t.Fatalf("usage %d exceeded ceiling %d by more than one entry (%d) at write %d", u, maxBytes, oneCost, i)
		}
	}
	if u := c.usage.Load(); u > maxBytes+oneCost {
		t.Fatalf("final usage %d exceeds ceiling %d (+1 entry %d)", u, maxBytes, oneCost)
	}
	if c.Stats().Evictions == 0 {
		t.Fatal("no evictions occurred under sustained over-capacity writes")
	}
}

func TestLoweringMaxMemoryEvictsDown(t *testing.T) {
	t.Parallel()

	c := New(WithMaxMemory(1<<20), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4))
	defer func() { _ = c.Close() }()

	for i := 0; i < 2000; i++ {
		mustSet(t, c, "k"+strconv.Itoa(i), val(100))
	}
	used := c.usage.Load()
	newMax := used / 2
	c.SetMaxMemory(newMax)

	oneCost := costOf("k000000", val(100))
	if u := c.usage.Load(); u > newMax+oneCost {
		t.Fatalf("after lowering maxmemory usage %d exceeds new ceiling %d (+1 entry)", u, newMax)
	}
}

func TestMaxMemoryZeroNeverEvicts(t *testing.T) {
	t.Parallel()

	c := New(WithShards(4)) // maxmemory unset (0) → unbounded
	defer func() { _ = c.Close() }()

	for i := 0; i < 3000; i++ {
		mustSet(t, c, "k"+strconv.Itoa(i), val(50))
	}
	if ev := c.Stats().Evictions; ev != 0 {
		t.Fatalf("unbounded engine evicted %d entries, want 0", ev)
	}
	if n, _ := c.Len(bg()); n != 3000 {
		t.Fatalf("Len = %d, want 3000 (no eviction)", n)
	}
	if got, want := c.usage.Load(), recomputeUsage(c); got != want {
		t.Fatalf("usage %d != recomputed %d", got, want)
	}
}

func TestSetAlwaysSticksAtCapacity_LFU(t *testing.T) {
	t.Parallel()
	assertSetSticksAtCapacity(t, PolicyAllKeysLFU)
}

func TestSetAlwaysSticksAtCapacity_Random(t *testing.T) {
	t.Parallel()
	assertSetSticksAtCapacity(t, PolicyAllKeysRandom)
}

func assertSetSticksAtCapacity(t *testing.T, pol EvictionPolicy) {
	t.Helper()
	const maxBytes = 32 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(pol), WithShards(4))
	defer func() { _ = c.Close() }()

	// Fill to capacity.
	for i := 0; i < 4000; i++ {
		mustSet(t, c, "fill"+strconv.Itoa(i), val(100))
	}
	// The just-written key must always be readable immediately after its SET.
	mustSet(t, c, "newcomer", val(100))
	v, ok := mustGet(t, c, "newcomer")
	if !ok {
		t.Fatal("just-written key was evicted by its own SET (C-INV-1 violated)")
	}
	if len(v) != 100 {
		t.Fatalf("newcomer value len = %d, want 100", len(v))
	}
}

func TestSetSurvivesOwnSetAcrossChurn(t *testing.T) {
	t.Parallel()

	const maxBytes = 16 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4))
	defer func() { _ = c.Close() }()

	for i := 0; i < 8000; i++ {
		k := "churn" + strconv.Itoa(i)
		mustSet(t, c, k, val(80))
		v, ok := mustGet(t, c, k)
		if !ok || len(v) != 80 {
			t.Fatalf("write %d: just-written key %q lost its own SET (C-INV-1)", i, k)
		}
	}
}

func TestGetDoesNoEvictionAndDoesNotMoveCounter(t *testing.T) {
	t.Parallel()

	const maxBytes = 32 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4))
	defer func() { _ = c.Close() }()
	for i := 0; i < 4000; i++ {
		mustSet(t, c, "k"+strconv.Itoa(i), val(100))
	}

	usageBefore := c.usage.Load()
	evBefore := c.Stats().Evictions
	for r := 0; r < 5; r++ {
		for i := 0; i < 4000; i++ {
			_, _ = mustGet(t, c, "k"+strconv.Itoa(i))
		}
	}
	if got := c.usage.Load(); got != usageBefore {
		t.Fatalf("GET moved the byte counter: %d -> %d", usageBefore, got)
	}
	if got := c.Stats().Evictions; got != evBefore {
		t.Fatalf("GET caused eviction: %d -> %d", evBefore, got)
	}
}

func TestGetUnderConcurrentEvictionRaceFree(t *testing.T) {
	t.Parallel()

	const maxBytes = 64 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(8))
	defer func() { _ = c.Close() }()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers drive eviction.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.Set(bg(), "w"+strconv.Itoa(base)+"-"+strconv.Itoa(i), val(100), 0)
			}
		}(w)
	}
	// Readers hammer GET concurrently.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200000; i++ {
				_, _, _ = c.Get(bg(), "w0-"+strconv.Itoa(i&1023))
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestUnEvictedKeyKeepsValueAndTTL(t *testing.T) {
	t.Parallel()

	fc := newFakeClock()
	const maxBytes = 32 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4), WithClock(fc.now))
	defer func() { _ = c.Close() }()

	// Keep a key with a TTL; make it hot so LFU never evicts it.
	if err := c.Set(bg(), "keep", []byte("treasure"), 30*time.Second); err != nil {
		t.Fatalf("Set keep: %v", err)
	}
	for i := 0; i < 3000; i++ {
		_, _, _ = c.Get(bg(), "keep")
	}
	time.Sleep(80 * time.Millisecond) // let the drainer fold keep's frequency

	for i := 0; i < 4000; i++ {
		mustSet(t, c, "cold"+strconv.Itoa(i), val(100))
	}

	v, ok := mustGet(t, c, "keep")
	if !ok || string(v) != "treasure" {
		t.Fatalf("kept key value = %q (ok=%v), want \"treasure\"", v, ok)
	}
	rem, ok, err := c.TTL(bg(), "keep")
	if err != nil || !ok || rem != 30*time.Second {
		t.Fatalf("kept key TTL = %v (ok=%v, err=%v), want 30s unchanged", rem, ok, err)
	}
}

func TestByteCounterEqualsRecomputedTruth(t *testing.T) {
	t.Parallel()

	fc := newFakeClock()
	const maxBytes = 48 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4), WithClock(fc.now))
	defer func() { _ = c.Close() }()

	check := func(stepDesc string) {
		t.Helper()
		if got, want := c.usage.Load(), recomputeUsage(c); got != want {
			t.Fatalf("%s: usage %d != recomputed %d", stepDesc, got, want)
		}
	}

	for i := 0; i < 1500; i++ {
		mustSet(t, c, "k"+strconv.Itoa(i), val(60))
		check("insert " + strconv.Itoa(i))
	}
	// Overwrites, growing and shrinking.
	mustSet(t, c, "k1", val(500))
	check("overwrite grow")
	mustSet(t, c, "k1", val(10))
	check("overwrite shrink")
	// Deletes.
	for i := 0; i < 300; i++ {
		if _, err := c.Del(bg(), "k"+strconv.Itoa(i)); err != nil {
			t.Fatalf("Del: %v", err)
		}
		check("del " + strconv.Itoa(i))
	}
	// TTL set, expire via clock, then physical reclaim via Del.
	if err := c.Set(bg(), "tk", val(40), 5*time.Second); err != nil {
		t.Fatalf("Set tk: %v", err)
	}
	check("ttl set")
	fc.advance(10 * time.Second) // tk now expired but still physically present (lazy)
	check("ttl expired (lazy, counter unchanged)")
	if _, err := c.Del(bg(), "tk"); err != nil {
		t.Fatalf("Del tk: %v", err)
	}
	check("ttl reclaimed via del")
}

func TestOverwriteDeltaBothDirections(t *testing.T) {
	t.Parallel()

	c := New(WithShards(2))
	defer func() { _ = c.Close() }()

	mustSet(t, c, "k", val(100))
	base := c.usage.Load()
	mustSet(t, c, "k", val(300)) // grow by 200
	if got := c.usage.Load(); got != base+200 {
		t.Fatalf("after grow usage = %d, want %d", got, base+200)
	}
	mustSet(t, c, "k", val(50)) // shrink by 250 from the 300-value
	if got := c.usage.Load(); got != base-50 {
		t.Fatalf("after shrink usage = %d, want %d", got, base-50)
	}
}

func TestHotKeyRetainedColdEvicted(t *testing.T) {
	t.Parallel()

	const maxBytes = 48 * 1024
	c := New(WithMaxMemory(maxBytes), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(4))
	defer func() { _ = c.Close() }()

	mustSet(t, c, "hot", val(100))
	for i := 0; i < 5000; i++ {
		_, _, _ = c.Get(bg(), "hot")
	}
	time.Sleep(100 * time.Millisecond) // let the drainer fold the hot key's frequency

	for i := 0; i < 6000; i++ {
		mustSet(t, c, "cold"+strconv.Itoa(i), val(100))
		if i%97 == 0 {
			_, _, _ = c.Get(bg(), "hot") // keep it hot
		}
	}
	if _, ok := mustGet(t, c, "hot"); !ok {
		t.Fatal("hot key was evicted under allkeys-lfu (should be retained)")
	}
	if n, _ := c.Len(bg()); n == 6001 {
		t.Fatal("no cold keys were evicted")
	}
}

func TestCloseJoinsDrainerNoLeak(t *testing.T) {
	settleGoroutines()
	before := runtime.NumGoroutine()

	for i := 0; i < 16; i++ {
		c := New(WithMaxMemory(8<<10), WithEvictionPolicy(PolicyAllKeysLFU))
		for j := 0; j < 100; j++ {
			_ = c.Set(bg(), "k"+strconv.Itoa(j), val(20), 0)
			_, _, _ = c.Get(bg(), "k"+strconv.Itoa(j))
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutine leak: before %d, after %d", before, after)
	}
}

func TestStatsEqualGroundTruth(t *testing.T) {
	t.Parallel()

	c := New(WithShards(2)) // unbounded: isolate hit/miss counting from eviction
	defer func() { _ = c.Close() }()

	for i := 0; i < 100; i++ {
		mustSet(t, c, "k"+strconv.Itoa(i), val(10))
	}
	wantHits, wantMisses := 0, 0
	for i := 0; i < 100; i++ {
		if _, ok := mustGet(t, c, "k"+strconv.Itoa(i)); ok {
			wantHits++
		}
	}
	for i := 100; i < 150; i++ {
		if _, ok := mustGet(t, c, "missing"+strconv.Itoa(i)); !ok {
			wantMisses++
		}
	}
	s := c.Stats()
	if int(s.Hits) != wantHits {
		t.Fatalf("Stats.Hits = %d, want %d", s.Hits, wantHits)
	}
	if int(s.Misses) != wantMisses {
		t.Fatalf("Stats.Misses = %d, want %d", s.Misses, wantMisses)
	}
}

func TestStatsRaceFreeUnderConcurrentTraffic(t *testing.T) {
	t.Parallel()

	c := New(WithMaxMemory(32<<10), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(8))
	defer func() { _ = c.Close() }()

	var wg sync.WaitGroup
	var last atomic.Uint64
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.Set(bg(), "k"+strconv.Itoa(i), val(80), 0)
			_, _, _ = c.Get(bg(), "k"+strconv.Itoa(i&255))
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100000; i++ {
			s := c.Stats()
			if s.Hits < last.Load() {
				t.Errorf("hits went backwards")
				return
			}
			last.Store(s.Hits)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestByteCounterExactUnderConcurrentSameKeyWrites(t *testing.T) {
	t.Parallel()

	// Many goroutines hammer a SHARED key space (heavy same-key contention) plus Dels.
	// Because the overwrite delta comes from the atomic Swap (not a racy Get-then-Set),
	// the running counter must still equal the recomputed live-set truth at quiescence.
	c := New(WithShards(4)) // unbounded: isolate accounting from eviction
	defer func() { _ = c.Close() }()

	const keySpace = 16
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 20000; i++ {
				k := "shared" + strconv.Itoa((seed*7+i)%keySpace)
				if i%5 == 0 {
					_, _ = c.Del(bg(), k)
				} else {
					_ = c.Set(bg(), k, val(1+(i%64)), 0)
				}
			}
		}(w)
	}
	wg.Wait()

	if got, want := c.usage.Load(), recomputeUsage(c); got != want {
		t.Fatalf("byte counter drifted under concurrent same-key writes: usage %d != recomputed %d", got, want)
	}
}

// settleGoroutines lets background goroutines exit so the leak baseline is stable.
func settleGoroutines() {
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}
