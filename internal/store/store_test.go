package store_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dopecoder/cachemoney/internal/store"
)

// fakeClock is a deterministic clock for exercising TTL behavior without
// sleeping in tests.
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

func TestStore_GetMissingKey(t *testing.T) {
	s := store.New()

	got, ok := s.Get("absent")
	if ok {
		t.Fatalf("Get(absent) ok = true, want false")
	}
	if got != nil {
		t.Errorf("Get(absent) value = %q, want nil", got)
	}
}

func TestStore_SetThenGet(t *testing.T) {
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
			s := store.New()
			s.Set(tc.key, tc.value, 0)

			got, ok := s.Get(tc.key)
			if !ok {
				t.Fatalf("Get(%q) ok = false, want true", tc.key)
			}
			if diff := cmp.Diff(tc.value, got); diff != "" {
				t.Errorf("Get(%q) mismatch (-want +got):\n%s", tc.key, diff)
			}
		})
	}
}

func TestStore_SetOverwrites(t *testing.T) {
	s := store.New()
	s.Set("k", []byte("first"), 0)
	s.Set("k", []byte("second"), 0)

	got, ok := s.Get("k")
	if !ok {
		t.Fatalf("Get(k) ok = false, want true")
	}
	if diff := cmp.Diff([]byte("second"), got); diff != "" {
		t.Errorf("Get(k) mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_Del(t *testing.T) {
	s := store.New()
	s.Set("k", []byte("v"), 0)

	if existed := s.Del("k"); !existed {
		t.Errorf("Del(k) = false, want true (key was present)")
	}
	if _, ok := s.Get("k"); ok {
		t.Errorf("Get(k) ok = true after Del, want false")
	}
	if existed := s.Del("k"); existed {
		t.Errorf("Del(k) second call = true, want false (already gone)")
	}
}

func TestStore_TTLExpiry(t *testing.T) {
	clk := newFakeClock()
	s := store.New(store.WithClock(clk.now))
	s.Set("k", []byte("v"), 100*time.Millisecond)

	// Just before expiry the key is live.
	clk.advance(99 * time.Millisecond)
	if _, ok := s.Get("k"); !ok {
		t.Fatalf("Get(k) ok = false before TTL elapsed, want true")
	}

	// At/after expiry the key is gone.
	clk.advance(1 * time.Millisecond)
	if _, ok := s.Get("k"); ok {
		t.Errorf("Get(k) ok = true after TTL elapsed, want false")
	}
}

func TestStore_ZeroTTLNeverExpires(t *testing.T) {
	clk := newFakeClock()
	s := store.New(store.WithClock(clk.now))
	s.Set("k", []byte("v"), 0)

	clk.advance(1000 * time.Hour)
	if _, ok := s.Get("k"); !ok {
		t.Errorf("Get(k) ok = false for non-expiring key, want true")
	}
}

func TestStore_Len(t *testing.T) {
	clk := newFakeClock()
	s := store.New(store.WithClock(clk.now))

	if got := s.Len(); got != 0 {
		t.Fatalf("Len() = %d on empty store, want 0", got)
	}

	s.Set("a", []byte("1"), 0)
	s.Set("b", []byte("2"), 50*time.Millisecond)
	if got := s.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}

	// Expired entries do not count toward Len.
	clk.advance(51 * time.Millisecond)
	if got := s.Len(); got != 1 {
		t.Errorf("Len() = %d after one key expired, want 1", got)
	}
}

// TestStore_ValueIsolation verifies the store defensively copies values so that
// callers cannot mutate stored data through the slices they pass in or get out.
func TestStore_ValueIsolation(t *testing.T) {
	s := store.New()

	in := []byte("abc")
	s.Set("k", in, 0)
	in[0] = 'X' // mutate caller's slice after Set

	got1, ok := s.Get("k")
	if !ok {
		t.Fatalf("Get(k) ok = false, want true")
	}
	if diff := cmp.Diff([]byte("abc"), got1); diff != "" {
		t.Errorf("store was corrupted by caller mutating input (-want +got):\n%s", diff)
	}

	got1[0] = 'Y' // mutate returned slice
	got2, _ := s.Get("k")
	if diff := cmp.Diff([]byte("abc"), got2); diff != "" {
		t.Errorf("store was corrupted by caller mutating output (-want +got):\n%s", diff)
	}
}

// TestStore_ConcurrentAccess is a race-detector smoke test: many goroutines
// hammering Set/Get/Del/Len must not race. Run via `go test -race`.
func TestStore_ConcurrentAccess(t *testing.T) {
	s := store.New()
	const workers = 16
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			key := "k"
			val := []byte{byte(id)}
			for i := 0; i < iterations; i++ {
				s.Set(key, val, time.Duration(i)*time.Millisecond)
				s.Get(key)
				_ = s.Len()
				if i%8 == 0 {
					s.Del(key)
				}
			}
		}(w)
	}
	wg.Wait()
}
