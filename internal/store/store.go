// Package store provides an in-memory, concurrency-safe key-value store with
// per-key time-to-live (TTL). It is the core data structure that cachemoney's
// network server and sharding layer are built on.
//
// Values are treated as opaque, binary-safe byte slices. The store defensively
// copies values on the way in and out so callers can never mutate stored data
// through the slices they pass to Set or receive from Get.
//
// Expiration is lazy: an expired entry is reported as absent by Get and is
// excluded from Len, but its memory is not reclaimed until the key is
// overwritten or deleted. Active expiration and size-bounded eviction arrive
// with the LRU layer in a later milestone.
package store

import (
	"sync"
	"time"
)

// Clock returns the current time. It is injectable so TTL behavior can be
// tested deterministically.
type Clock func() time.Time

// entry is a stored value with an optional expiry. A zero expiresAt means the
// entry never expires.
type entry struct {
	value     []byte
	expiresAt time.Time
}

// Store is an in-memory, concurrency-safe key-value store with per-key TTL.
// The zero value is not usable; construct a Store with New.
type Store struct {
	mu    sync.RWMutex
	items map[string]entry
	now   Clock
}

// Option configures a Store.
type Option func(*Store)

// WithClock overrides the clock used for TTL calculations. It is primarily
// useful in tests; production code should rely on the default (time.Now).
func WithClock(clock Clock) Option {
	return func(s *Store) {
		if clock != nil {
			s.now = clock
		}
	}
}

// New returns an empty Store ready for use.
func New(opts ...Option) *Store {
	s := &Store{
		items: make(map[string]entry),
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Set stores value under key. If ttl is greater than zero the entry expires
// after ttl has elapsed; a ttl of zero or less never expires. The value is
// copied, so later mutations of the caller's slice do not affect the store.
func (s *Store) Set(key string, value []byte, ttl time.Duration) {
	e := entry{value: cloneBytes(value)}
	if ttl > 0 {
		e.expiresAt = s.now().Add(ttl)
	}

	s.mu.Lock()
	s.items[key] = e
	s.mu.Unlock()
}

// Get returns a copy of the value stored under key. The second result is false
// if the key is absent or has expired.
func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	e, ok := s.items[key]
	s.mu.RUnlock()

	if !ok || s.expired(e) {
		return nil, false
	}
	return cloneBytes(e.value), true
}

// Del removes key from the store. It reports whether the key was present and
// live at the time of deletion.
func (s *Store) Del(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.items[key]
	if !ok {
		return false
	}
	delete(s.items, key)
	return !s.expired(e)
}

// Len returns the number of live (non-expired) entries in the store.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := 0
	for _, e := range s.items {
		if !s.expired(e) {
			n++
		}
	}
	return n
}

// expired reports whether e has passed its expiry as of the current clock time.
func (s *Store) expired(e entry) bool {
	return !e.expiresAt.IsZero() && !s.now().Before(e.expiresAt)
}

// cloneBytes returns a copy of b. A nil input yields an empty, non-nil slice so
// stored values are always addressable and round-trip identically.
func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
