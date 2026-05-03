package cache_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/hash"
)

// fakeClock is a deterministic clock for exercising TTL behavior without sleeping
// on wall time. It is the engine's port of the original store_test.go clock and is
// safe for concurrent use so it can drive the -race concurrency smoke. The engine
// reads time only through the injected cache.Clock, so advancing this clock is the
// sole way TTL elapses in tests (spec "Expiry is driven by the injected clock").
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---------------------------------------------------------------------------
// Increment 7a — Get / Set core semantics + binary-safe defensive copy.
// (Ports internal/store/store_test.go behaviors, adapted to ctx + error.)
// ---------------------------------------------------------------------------

// TestCache_GetMissingKey — spec "Get of an absent key reports not found".
func TestCache_GetMissingKey(t *testing.T) {
	c := cache.New()

	got, ok, err := c.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("Get(absent) err = %v, want nil", err)
	}
	if ok {
		t.Fatalf("Get(absent) ok = true, want false")
	}
	if got != nil {
		t.Errorf("Get(absent) value = %v, want nil", got)
	}
}

// TestCache_SetThenGet — spec "Set then Get returns the stored value" and
// "Binary values round-trip exactly".
func TestCache_SetThenGet(t *testing.T) {
	tests := map[string]struct {
		key   string
		value []byte
	}{
		"ascii":       {key: "k", value: []byte("hello")},
		"empty value": {key: "empty", value: []byte("")},
		"binary safe": {key: "bin", value: []byte{0x00, 0xff, 0x10, 0x00}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := cache.New()
			ctx := context.Background()

			if err := c.Set(ctx, tc.key, tc.value, 0); err != nil {
				t.Fatalf("Set(%q) err = %v, want nil", tc.key, err)
			}

			got, ok, err := c.Get(ctx, tc.key)
			if err != nil {
				t.Fatalf("Get(%q) err = %v, want nil", tc.key, err)
			}
			if !ok {
				t.Fatalf("Get(%q) ok = false, want true", tc.key)
			}
			if diff := cmp.Diff(tc.value, got); diff != "" {
				t.Errorf("Get(%q) mismatch (-want +got):\n%s", tc.key, diff)
			}
		})
	}
}

// TestCache_SetOverwrites — spec "Set overwrites an existing entry".
func TestCache_SetOverwrites(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("first"), 0); err != nil {
		t.Fatalf("Set first err = %v", err)
	}
	if err := c.Set(ctx, "k", []byte("second"), 0); err != nil {
		t.Fatalf("Set second err = %v", err)
	}

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get ok = %v, err = %v, want true/nil", ok, err)
	}
	if diff := cmp.Diff([]byte("second"), got); diff != "" {
		t.Errorf("Get after overwrite mismatch (-want +got):\n%s", diff)
	}
}

// TestCache_ValueIsolation — spec "Mutating the caller's input slice after Set
// does not corrupt storage" and "Mutating the slice returned by Get does not
// corrupt storage". Exercises the in-copy (before shardmap.Set) and the out-copy
// (after the shard RLock is released).
func TestCache_ValueIsolation(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	in := []byte("abc")
	if err := c.Set(ctx, "k", in, 0); err != nil {
		t.Fatalf("Set err = %v", err)
	}
	in[0] = 'X' // mutate the caller's input AFTER Set returns

	got1, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get ok = %v, err = %v", ok, err)
	}
	if diff := cmp.Diff([]byte("abc"), got1); diff != "" {
		t.Errorf("storage corrupted by mutating input (-want +got):\n%s", diff)
	}

	got1[0] = 'Y' // mutate the slice returned by Get
	got2, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get (2) ok = %v, err = %v", ok, err)
	}
	if diff := cmp.Diff([]byte("abc"), got2); diff != "" {
		t.Errorf("storage corrupted by mutating output (-want +got):\n%s", diff)
	}
}

// TestCache_NilValueRoundTrip — spec "nil value round-trips as empty non-nil
// slice".
func TestCache_NilValueRoundTrip(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	if err := c.Set(ctx, "k", nil, 0); err != nil {
		t.Fatalf("Set(nil) err = %v", err)
	}

	got, ok, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get err = %v", err)
	}
	if !ok {
		t.Fatalf("Get ok = false, want true")
	}
	if got == nil {
		t.Errorf("Get returned a nil slice, want an empty non-nil slice")
	}
	if len(got) != 0 {
		t.Errorf("Get len = %d, want 0", len(got))
	}
}

// TestCache_LargeValueRoundTrip (TRIANGULATE) — a 1 MiB value round-trips exactly.
func TestCache_LargeValueRoundTrip(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	large := make([]byte, 1<<20)
	for i := range large {
		large[i] = byte(i * 7)
	}

	if err := c.Set(ctx, "big", large, 0); err != nil {
		t.Fatalf("Set(big) err = %v", err)
	}
	got, ok, err := c.Get(ctx, "big")
	if err != nil || !ok {
		t.Fatalf("Get(big) ok = %v, err = %v", ok, err)
	}
	if !bytes.Equal(large, got) {
		t.Errorf("large value mismatch: want len %d, got len %d", len(large), len(got))
	}
}

// TestCache_RepeatedOverwrite (TRIANGULATE) — many overwrites keep the last value
// and never inflate the live count beyond one.
func TestCache_RepeatedOverwrite(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	var last []byte
	for i := 0; i < 100; i++ {
		last = []byte("v" + strconv.Itoa(i))
		if err := c.Set(ctx, "k", last, 0); err != nil {
			t.Fatalf("Set #%d err = %v", i, err)
		}
	}

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get ok = %v, err = %v", ok, err)
	}
	if diff := cmp.Diff(last, got); diff != "" {
		t.Errorf("final value mismatch (-want +got):\n%s", diff)
	}
	if n, err := c.Len(ctx); err != nil || n != 1 {
		t.Errorf("Len after repeated overwrite = %d (err %v), want 1", n, err)
	}
}

// TestCache_CloneIsolationUnderConcurrentReader (TRIANGULATE) — a second goroutine
// hammering Get and mutating its returned copy must not corrupt storage or the
// copies handed to the main reader. Run under -race this also proves the post-lock
// out-copy is race-free (design §9 copy-on-write invariant).
func TestCache_CloneIsolationUnderConcurrentReader(t *testing.T) {
	c := cache.New()
	ctx := context.Background()
	want := []byte("immutable-value")
	if err := c.Set(ctx, "k", want, 0); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			got, ok, err := c.Get(ctx, "k")
			if err != nil || !ok {
				continue
			}
			for j := range got { // scribble over the returned copy
				got[j] = 'X'
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		got, ok, err := c.Get(ctx, "k")
		if err != nil || !ok {
			t.Fatalf("Get ok = %v, err = %v", ok, err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("concurrent reader corrupted a returned copy: got %q, want %q", got, want)
		}
	}
	wg.Wait()

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || !bytes.Equal(want, got) {
		t.Errorf("storage corrupted after concurrent reads: got %q ok=%v err=%v", got, ok, err)
	}
}

// ---------------------------------------------------------------------------
// Increment 7b — Del + lazy TTL + Len + engine -race concurrency smoke.
// ---------------------------------------------------------------------------

// TestCache_Del — spec "Del of a present live key removes it and reports existed"
// and "Del of an absent key reports not existed".
func TestCache_Del(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	existed, err := c.Del(ctx, "k")
	if err != nil {
		t.Fatalf("Del err = %v", err)
	}
	if !existed {
		t.Errorf("Del(present live) existed = false, want true")
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Errorf("Get after Del ok = true, want false")
	}

	existed, err = c.Del(ctx, "k")
	if err != nil {
		t.Fatalf("Del (2) err = %v", err)
	}
	if existed {
		t.Errorf("Del(absent) existed = true, want false")
	}
}

// TestCache_DelExpired — spec "Del of an expired key reports not existed". The key
// is physically present in storage but not live, so Del removes it yet reports
// existed == false, and a subsequent Get reads absent.
func TestCache_DelExpired(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set err = %v", err)
	}
	clk.advance(100 * time.Millisecond) // expire it

	existed, err := c.Del(ctx, "k")
	if err != nil {
		t.Fatalf("Del err = %v", err)
	}
	if existed {
		t.Errorf("Del(expired) existed = true, want false")
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Errorf("Get after Del(expired) ok = true, want false")
	}
}

// TestCache_TTLExpiry — spec "Key is live up to the instant before expiry" and
// "Key is gone at and after expiry". Driven entirely by the fake clock, no sleeps.
func TestCache_TTLExpiry(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	clk.advance(99 * time.Millisecond) // just before expiry
	if _, ok, err := c.Get(ctx, "k"); err != nil || !ok {
		t.Fatalf("Get at +99ms ok = %v, err = %v, want true/nil", ok, err)
	}

	clk.advance(1 * time.Millisecond) // exactly at the expiry instant (+100ms)
	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Errorf("Get at +100ms ok = %v, err = %v, want false/nil", ok, err)
	}
}

// TestCache_ZeroTTLNeverExpires — spec "Zero or negative TTL never expires" (zero).
func TestCache_ZeroTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	clk.advance(1000 * time.Hour)
	if _, ok, err := c.Get(ctx, "k"); err != nil || !ok {
		t.Errorf("Get(non-expiring) ok = %v, err = %v, want true/nil", ok, err)
	}
}

// TestCache_NegativeTTLNeverExpires (TRIANGULATE) — spec "Zero or negative TTL
// never expires" (negative); a ttl <= 0 yields a zero expiresAt.
func TestCache_NegativeTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), -5*time.Second); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	clk.advance(1000 * time.Hour)
	if _, ok, err := c.Get(ctx, "k"); err != nil || !ok {
		t.Errorf("Get(negative ttl) ok = %v, err = %v, want true/nil", ok, err)
	}
}

// TestCache_Len — spec "Empty engine has length zero", "Live entries are counted",
// and "Expired entries are excluded from Len".
func TestCache_Len(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if n, err := c.Len(ctx); err != nil || n != 0 {
		t.Fatalf("Len(empty) = %d (err %v), want 0", n, err)
	}

	if err := c.Set(ctx, "a", []byte("1"), 0); err != nil {
		t.Fatalf("Set a err = %v", err)
	}
	if err := c.Set(ctx, "b", []byte("2"), 50*time.Millisecond); err != nil {
		t.Fatalf("Set b err = %v", err)
	}
	if n, err := c.Len(ctx); err != nil || n != 2 {
		t.Fatalf("Len(two live) = %d (err %v), want 2", n, err)
	}

	clk.advance(51 * time.Millisecond) // expire "b"
	if n, err := c.Len(ctx); err != nil || n != 1 {
		t.Errorf("Len(after one expired) = %d (err %v), want 1", n, err)
	}
}

// TestCache_LenMixedLiveExpired (TRIANGULATE) — Len counts only the live subset
// when live and expired entries are interleaved across shards.
func TestCache_LenMixedLiveExpired(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	for _, k := range []string{"x", "y", "z"} { // never expire
		if err := c.Set(ctx, k, []byte(k), 0); err != nil {
			t.Fatalf("Set %q err = %v", k, err)
		}
	}
	for _, k := range []string{"p", "q"} { // expire at +10ms
		if err := c.Set(ctx, k, []byte(k), 10*time.Millisecond); err != nil {
			t.Fatalf("Set %q err = %v", k, err)
		}
	}

	if n, err := c.Len(ctx); err != nil || n != 5 {
		t.Fatalf("Len(5 live) = %d (err %v), want 5", n, err)
	}

	clk.advance(10 * time.Millisecond) // expire p, q
	if n, err := c.Len(ctx); err != nil || n != 3 {
		t.Errorf("Len(mixed) = %d (err %v), want 3", n, err)
	}
}

// TestCache_DeleteThenLen (TRIANGULATE) — deleting a key drops the live count.
func TestCache_DeleteThenLen(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c"} {
		if err := c.Set(ctx, k, []byte(k), 0); err != nil {
			t.Fatalf("Set %q err = %v", k, err)
		}
	}
	if _, err := c.Del(ctx, "b"); err != nil {
		t.Fatalf("Del err = %v", err)
	}
	if n, err := c.Len(ctx); err != nil || n != 2 {
		t.Errorf("Len after delete = %d (err %v), want 2", n, err)
	}
}

// TestCache_OverwriteResetsTTL (TRIANGULATE) — overwriting a key with a fresh TTL
// rebases the expiry on the current clock, so the entry survives past the original
// deadline and expires on the new one.
func TestCache_OverwriteResetsTTL(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("first"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set first err = %v", err)
	}
	clk.advance(80 * time.Millisecond)

	if err := c.Set(ctx, "k", []byte("second"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set second err = %v", err)
	}

	clk.advance(80 * time.Millisecond) // +160ms from first set, +80ms from overwrite
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get after overwrite+80ms ok = %v, err = %v, want true (TTL reset)", ok, err)
	}
	if diff := cmp.Diff([]byte("second"), got); diff != "" {
		t.Errorf("value after overwrite mismatch (-want +got):\n%s", diff)
	}

	clk.advance(20 * time.Millisecond) // +100ms from overwrite → expired
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Errorf("Get after overwrite TTL elapsed ok = true, want false")
	}
}

// TestCache_ConcurrentAccess — spec "Concurrent mixed operations are race-free".
// Ported from store_test.go's TestStore_ConcurrentAccess (16 goroutines hammering
// mixed Set/Get/Len/Del). Run under `go test -race`.
func TestCache_ConcurrentAccess(t *testing.T) {
	c := cache.New()
	ctx := context.Background()
	const workers = 16
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			hot := "hot"                  // shared key → hammers one shard hard
			own := "k" + strconv.Itoa(id) // per-worker key → spreads across shards
			val := []byte{byte(id)}
			for i := 0; i < iterations; i++ {
				_ = c.Set(ctx, hot, val, time.Duration(i)*time.Millisecond)
				_ = c.Set(ctx, own, val, 0)
				_, _, _ = c.Get(ctx, hot)
				_, _, _ = c.Get(ctx, own)
				_, _ = c.Len(ctx)
				if i%8 == 0 {
					_, _ = c.Del(ctx, hot)
					_, _ = c.Del(ctx, own)
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestCache_ConcurrentReadsDoNotMutate — spec "Reads do not mutate shared state".
// Concurrent Get/Len against a fixed entry, with each reader scribbling over its
// own returned copy, must leave the stored value untouched.
func TestCache_ConcurrentReadsDoNotMutate(t *testing.T) {
	c := cache.New()
	ctx := context.Background()
	want := []byte("stable")
	if err := c.Set(ctx, "k", want, 0); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				got, ok, err := c.Get(ctx, "k")
				if err == nil && ok {
					for j := range got {
						got[j] ^= 0xff // mutate the returned copy only
					}
				}
				_, _ = c.Len(ctx)
			}
		}()
	}
	wg.Wait()

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get after concurrent reads ok = %v, err = %v", ok, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("concurrent reads mutated stored state (-want +got):\n%s", diff)
	}
}

// ---------------------------------------------------------------------------
// REFACTOR — randomized model-equivalence to a self-contained reference model
// (a plain Go map with the same lazy-TTL rule). This is the engine's regression
// oracle; it deliberately does NOT import internal/store so increment 10 can
// delete that package without breaking this suite. It also asserts the read path
// performs zero writes by reconciling Len after every operation.
// ---------------------------------------------------------------------------

type refEntry struct {
	value     []byte
	expiresAt time.Time
}

type refModel struct {
	items map[string]refEntry
	now   func() time.Time
}

func newRefModel(now func() time.Time) *refModel {
	return &refModel{items: make(map[string]refEntry), now: now}
}

func (m *refModel) expired(e refEntry) bool {
	return !e.expiresAt.IsZero() && !m.now().Before(e.expiresAt)
}

func (m *refModel) set(k string, v []byte, ttl time.Duration) {
	e := refEntry{value: append([]byte(nil), v...)}
	if ttl > 0 {
		e.expiresAt = m.now().Add(ttl)
	}
	m.items[k] = e
}

func (m *refModel) get(k string) ([]byte, bool) {
	e, ok := m.items[k]
	if !ok || m.expired(e) {
		return nil, false
	}
	return e.value, true
}

func (m *refModel) del(k string) bool {
	e, ok := m.items[k]
	if !ok {
		return false
	}
	delete(m.items, k)
	return !m.expired(e)
}

func (m *refModel) length() int {
	n := 0
	for _, e := range m.items {
		if !m.expired(e) {
			n++
		}
	}
	return n
}

func TestCache_ModelEquivalence(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now), cache.WithShards(8), cache.WithSeed(hash.NewSeed()))
	ref := newRefModel(clk.now)
	ctx := context.Background()

	rng := rand.New(rand.NewSource(0xCACE))
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	for i := 0; i < 5000; i++ {
		k := keys[rng.Intn(len(keys))]
		switch rng.Intn(5) {
		case 0, 1: // Set (weighted)
			v := make([]byte, 1+rng.Intn(8))
			for j := range v {
				v[j] = byte(rng.Intn(256))
			}
			var ttl time.Duration
			if rng.Intn(2) == 0 {
				ttl = time.Duration(1+rng.Intn(100)) * time.Millisecond
			}
			if err := c.Set(ctx, k, v, ttl); err != nil {
				t.Fatalf("op %d Set err = %v", i, err)
			}
			ref.set(k, v, ttl)
		case 2: // Del
			gotExisted, err := c.Del(ctx, k)
			if err != nil {
				t.Fatalf("op %d Del err = %v", i, err)
			}
			if wantExisted := ref.del(k); gotExisted != wantExisted {
				t.Fatalf("op %d Del(%q) existed = %v, want %v", i, k, gotExisted, wantExisted)
			}
		default: // advance the clock so TTLs elapse mid-run
			clk.advance(time.Duration(rng.Intn(25)) * time.Millisecond)
		}

		// Cross-check Get on a random key + Len after every op.
		ck := keys[rng.Intn(len(keys))]
		gotV, gotOK, err := c.Get(ctx, ck)
		if err != nil {
			t.Fatalf("op %d Get err = %v", i, err)
		}
		wantV, wantOK := ref.get(ck)
		if gotOK != wantOK {
			t.Fatalf("op %d Get(%q) ok = %v, want %v", i, ck, gotOK, wantOK)
		}
		if gotOK && !bytes.Equal(wantV, gotV) {
			t.Fatalf("op %d Get(%q) = %q, want %q", i, ck, gotV, wantV)
		}
		if gotN, err := c.Len(ctx); err != nil || gotN != ref.length() {
			t.Fatalf("op %d Len = %d (err %v), want %d", i, gotN, err, ref.length())
		}
	}
}

// ---------------------------------------------------------------------------
// Increment 8 — TTL(key) read op (Redis-style) + full cancellation matrix.
// Satisfies spec *TTL read operation* (4 scenarios), *Context cancellation is
// honored* (the "stores nothing" + live-ctx halves), and completes *Successful
// in-memory ops return nil error* (design §7 TTL semantics, §8 ctx/TOCTOU).
// ---------------------------------------------------------------------------

// TestCache_TTLLiveKeyWithExpiry — spec "Live key with an expiry reports positive
// remaining". Set ttl=100ms, advance the fake clock +40ms; TTL reports ok and a
// positive remaining in (0, 100ms] — deterministically ~60ms under the fake clock.
func TestCache_TTLLiveKeyWithExpiry(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	const ttl = 100 * time.Millisecond
	if err := c.Set(ctx, "k", []byte("v"), ttl); err != nil {
		t.Fatalf("Set err = %v", err)
	}
	clk.advance(40 * time.Millisecond)

	remaining, ok, err := c.TTL(ctx, "k")
	if err != nil {
		t.Fatalf("TTL err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("TTL ok = false, want true")
	}
	if remaining <= 0 || remaining > ttl {
		t.Errorf("TTL remaining = %v, want 0 < remaining <= %v", remaining, ttl)
	}
	if want := 60 * time.Millisecond; remaining != want {
		t.Errorf("TTL remaining = %v, want exactly %v (deterministic fake clock)", remaining, want)
	}
}

// TestCache_TTLLiveKeyNoExpiry — spec "Live key with no expiry returns the -1
// sentinel". A key set with ttl=0 reports remaining == -1, ok == true.
func TestCache_TTLLiveKeyNoExpiry(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	remaining, ok, err := c.TTL(ctx, "k")
	if err != nil {
		t.Fatalf("TTL err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("TTL ok = false, want true")
	}
	if remaining != -1 {
		t.Errorf("TTL remaining = %v, want -1 (Redis-style no-expiry sentinel)", remaining)
	}
}

// TestCache_TTLAbsentKey — spec "Absent key reports not found".
func TestCache_TTLAbsentKey(t *testing.T) {
	c := cache.New()

	remaining, ok, err := c.TTL(context.Background(), "absent")
	if err != nil {
		t.Fatalf("TTL err = %v, want nil", err)
	}
	if ok {
		t.Errorf("TTL(absent) ok = true, want false")
	}
	if remaining != 0 {
		t.Errorf("TTL(absent) remaining = %v, want 0", remaining)
	}
}

// TestCache_TTLExpiredKey — spec "Expired key reports not found". A key advanced
// past its expiry reports ok == false (lazy expiry; the TTL read never reclaims).
func TestCache_TTLExpiredKey(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set err = %v", err)
	}
	clk.advance(150 * time.Millisecond) // well past expiry

	remaining, ok, err := c.TTL(ctx, "k")
	if err != nil {
		t.Fatalf("TTL err = %v, want nil", err)
	}
	if ok {
		t.Errorf("TTL(expired) ok = true, want false")
	}
	if remaining != 0 {
		t.Errorf("TTL(expired) remaining = %v, want 0", remaining)
	}
}

// TestCache_TTLDoesNotMutate — TTL is read-only: Len, the stored value, and the
// key's liveness are unchanged after repeated TTL calls (lazy expiry never reclaims
// on a read, matching Get).
func TestCache_TTLDoesNotMutate(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set k err = %v", err)
	}
	if err := c.Set(ctx, "other", []byte("x"), 0); err != nil {
		t.Fatalf("Set other err = %v", err)
	}

	lenBefore, err := c.Len(ctx)
	if err != nil {
		t.Fatalf("Len before err = %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, ok, err := c.TTL(ctx, "k"); err != nil || !ok {
			t.Fatalf("TTL #%d ok = %v, err = %v, want true/nil", i, ok, err)
		}
	}

	lenAfter, err := c.Len(ctx)
	if err != nil {
		t.Fatalf("Len after err = %v", err)
	}
	if lenAfter != lenBefore {
		t.Errorf("Len mutated by TTL: before %d, after %d", lenBefore, lenAfter)
	}

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get after TTL ok = %v, err = %v, want true/nil", ok, err)
	}
	if diff := cmp.Diff([]byte("v"), got); diff != "" {
		t.Errorf("value mutated by TTL (-want +got):\n%s", diff)
	}
}

// TestCache_CancelledSetStoresNothing — spec "Cancelled context on a mutation
// returns ctx.Err() without applying" (reviewer carry-forward). A Set on an
// already-cancelled context returns ctx.Err() AND must store nothing — a later
// live-ctx Get reads absent and Len stays zero.
func TestCache_CancelledSetStoresNothing(t *testing.T) {
	c := cache.New()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Set(cancelled, "k", []byte("v"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Set(cancelled) err = %v, want context.Canceled", err)
	}

	// A later live-ctx Get proves the cancelled Set never stored the key.
	got, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get(live) err = %v, want nil", err)
	}
	if ok {
		t.Errorf("Get after cancelled Set ok = true (value %q), want false — cancelled Set must store nothing", got)
	}
	if n, err := c.Len(context.Background()); err != nil || n != 0 {
		t.Errorf("Len after cancelled Set = %d (err %v), want 0", n, err)
	}
}

// TestCache_TTLBoundary (TRIANGULATE) — TTL exactly at the expiry boundary: live
// with remaining == 1ms at +99ms, gone (ok == false) at +100ms, mirroring the Get
// boundary in TestCache_TTLExpiry.
func TestCache_TTLBoundary(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set err = %v", err)
	}

	clk.advance(99 * time.Millisecond) // just before expiry
	remaining, ok, err := c.TTL(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("TTL at +99ms ok = %v, err = %v, want true/nil", ok, err)
	}
	if want := 1 * time.Millisecond; remaining != want {
		t.Errorf("TTL at +99ms remaining = %v, want %v", remaining, want)
	}

	clk.advance(1 * time.Millisecond) // exactly at the expiry instant (+100ms)
	if remaining, ok, err := c.TTL(ctx, "k"); err != nil || ok || remaining != 0 {
		t.Errorf("TTL at +100ms = (%v, %v, %v), want (0, false, nil)", remaining, ok, err)
	}
}

// TestCache_TTLAfterOverwriteResetsRemaining (TRIANGULATE) — re-Setting a key with
// a fresh TTL rebases the expiry on the current clock, so a subsequent TTL reflects
// the NEW lifetime; re-Setting with ttl=0 clears the expiry to the -1 sentinel.
func TestCache_TTLAfterOverwriteResetsRemaining(t *testing.T) {
	clk := newFakeClock()
	c := cache.New(cache.WithClock(clk.now))
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("first"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set first err = %v", err)
	}
	clk.advance(40 * time.Millisecond)
	if remaining, ok, err := c.TTL(ctx, "k"); err != nil || !ok || remaining != 60*time.Millisecond {
		t.Fatalf("TTL before overwrite = (%v, %v, %v), want (60ms, true, nil)", remaining, ok, err)
	}

	// Overwrite with a fresh 100ms TTL rebased on the current clock.
	if err := c.Set(ctx, "k", []byte("second"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set second err = %v", err)
	}
	if remaining, ok, err := c.TTL(ctx, "k"); err != nil || !ok || remaining != 100*time.Millisecond {
		t.Errorf("TTL after overwrite = (%v, %v, %v), want (100ms, true, nil) — TTL not reset", remaining, ok, err)
	}

	// Re-Set with ttl=0 clears the expiry → -1 sentinel.
	if err := c.Set(ctx, "k", []byte("third"), 0); err != nil {
		t.Fatalf("Set third err = %v", err)
	}
	if remaining, ok, err := c.TTL(ctx, "k"); err != nil || !ok || remaining != -1 {
		t.Errorf("TTL after re-Set ttl=0 = (%v, %v, %v), want (-1, true, nil)", remaining, ok, err)
	}
}
