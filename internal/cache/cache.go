package cache

import (
	"context"
	"time"

	"github.com/dopecoder/cachemoney/internal/hash"
	"github.com/dopecoder/cachemoney/internal/shardmap"
)

// Clock returns the current time. It is injectable (via WithClock) so TTL behavior
// can be exercised deterministically in tests without sleeping on wall time; the
// default is time.Now.
type Clock func() time.Time

// entry is the value the engine stores in its backing shardmap: a defensively
// copied byte slice plus an optional expiry. A zero expiresAt means the entry
// never expires. shardmap treats entry as an opaque V — all TTL and copy logic
// lives in this package (design §4.1, option-b).
type entry struct {
	value     []byte
	expiresAt time.Time
}

// Cache is the M0 in-memory Engine implementation. It stores entries in a sharded,
// concurrency-safe Robin Hood map keyed by string, hashing keys with the seeded
// internal/hash seam. *Cache implements Engine. Construct one with New; the zero
// value is not usable.
type Cache struct {
	m   *shardmap.Map[string, entry]
	now Clock
}

// config holds the resolved construction settings collected from the Option list.
// It is internal: callers only ever see the functional options below. shardOpts
// accumulates the options passed straight through to shardmap.New.
type config struct {
	clock     Clock
	shardOpts []shardmap.Option
}

// Option configures a Cache at construction time. Options are applied in order by
// New; later options override earlier ones for the same setting.
type Option func(*config)

// WithClock overrides the clock used for TTL calculations. It is primarily useful
// in tests; production code relies on the default (time.Now). A nil clock is
// ignored, leaving the default in place.
func WithClock(c Clock) Option {
	return func(cfg *config) {
		if c != nil {
			cfg.clock = c
		}
	}
}

// WithShards requests n backing shards. The value is passed straight through to
// shardmap, which floors it at 1 and rounds up to the next power of two so shard
// selection stays a single shift with no modulo.
func WithShards(n int) Option {
	return func(cfg *config) {
		cfg.shardOpts = append(cfg.shardOpts, shardmap.WithShards(n))
	}
}

// WithSeed pins the hashing seed, making key routing deterministic within the
// process. Tests inject a fixed seed for reproducibility; production omits it and
// the backing map draws a fresh process-random seed (HashDoS resistance). The seed
// is passed straight through to shardmap.
func WithSeed(seed hash.Seed) Option {
	return func(cfg *config) {
		cfg.shardOpts = append(cfg.shardOpts, shardmap.WithSeed(seed))
	}
}

// New returns a Cache ready for use. It instantiates the backing
// shardmap.Map[string, entry] with hash.String as the injected hasher and applies
// any options (WithClock, WithShards, WithSeed) — the shard-related options pass
// straight through to shardmap.New. The returned *Cache implements Engine.
func New(opts ...Option) *Cache {
	cfg := config{clock: time.Now}
	for _, opt := range opts {
		opt(&cfg)
	}

	return &Cache{
		m:   shardmap.New[string, entry](hash.String, cfg.shardOpts...),
		now: cfg.clock,
	}
}

// Get returns a defensive copy of the live value stored under key. ok is false
// when the key is absent or expired.
//
// The lookup takes the backing shard's read lock internally (via shardmap.Get) and
// returns the entry by value; expiry is then evaluated with the injected clock
// (lazy TTL — an expired entry reads as absent and is NOT removed on read). The
// defensive out-copy is taken AFTER shardmap has released its read lock: that is
// race-free because shardmap never mutates a stored value's backing array in place
// (copy-on-write invariant, design §9), and it keeps the lock hold time to the bare
// map probe.
func (c *Cache) Get(ctx context.Context, key string) (value []byte, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	e, found := c.m.Get(key)
	if !found || c.expired(e) {
		return nil, false, nil
	}
	return cloneBytes(e.value), true, nil
}

// Set stores a defensive copy of value under key with an optional TTL, overwriting
// any existing entry.
//
// The ctx.Err() entry check returns before any lock or mutation, so a cancelled Set
// stores nothing (design §8). The input is cloned BEFORE the shardmap insert so the
// map always holds a private copy — mutating the caller's slice afterward cannot
// reach storage. expiresAt is computed from the injected clock when ttl > 0; a
// ttl <= 0 yields a zero expiresAt, meaning the entry never expires.
//
// Accepted TOCTOU nuance (design §8): the entry check honors an already-cancelled
// context, but a context cancelled BETWEEN the check and the shardmap insert still
// completes the store. This is the standard "honor cancellation at entry" contract
// and is acceptable for these nanosecond-scale O(1) ops; longer M1/M3 operations can
// add deadline-aware checks at their own slow points.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	e := entry{value: cloneBytes(value)}
	if ttl > 0 {
		e.expiresAt = c.now().Add(ttl)
	}
	c.m.Set(key, e)
	return nil
}

// Del removes key, reporting whether it was present and live at deletion time.
//
// An expired entry is still physically removed but reported existed == false (it
// was present in storage but not live), matching the original store semantics. An
// absent key reports existed == false.
func (c *Cache) Del(ctx context.Context, key string) (existed bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	e, found := c.m.Delete(key)
	if !found {
		return false, nil
	}
	return !c.expired(e), nil
}

// TTL reports the remaining lifetime of a live key. ok is false for an absent or
// expired key; a live key with no expiry returns the Redis-style sentinel remaining
// of -1. TTL is read-only — like Get it takes the backing shard's read lock (via
// shardmap.Get) and never mutates stored state: an entry whose TTL has elapsed reads
// as absent and is NOT reclaimed on the read (lazy expiry, design §7).
//
// The single ctx.Err() entry check runs before the shard lock (uniform with the
// other four ops), so a cancelled TTL touches nothing. For a live key with an
// expiry, remaining is expiresAt.Sub(now()), guaranteed > 0 because the prior
// expired() check already excluded the at/after-expiry instant.
func (c *Cache) TTL(ctx context.Context, key string) (remaining time.Duration, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	e, found := c.m.Get(key)
	if !found || c.expired(e) {
		return 0, false, nil
	}
	if e.expiresAt.IsZero() {
		return -1, true, nil
	}
	return e.expiresAt.Sub(c.now()), true, nil
}

// Len returns the number of live (non-expired) entries.
//
// It scans the backing map with shardmap.Range, which holds each shard's read lock
// for the duration of that shard's walk and is per-shard-consistent rather than a
// single global snapshot (see the package doc's iteration / re-entrancy contract).
// The callback only reads entry expiry and never mutates, and it never calls back
// into this Cache (which would deadlock on the held read lock). Entries whose TTL
// has elapsed but whose memory has not been reclaimed are excluded (lazy TTL).
func (c *Cache) Len(ctx context.Context) (n int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count := 0
	c.m.Range(func(_ string, e entry) bool {
		if !c.expired(e) {
			count++
		}
		return true
	})
	return count, nil
}

// expired reports whether e has passed its expiry as of the current clock time. A
// zero expiresAt (set with ttl <= 0) never expires. This is byte-for-byte the rule
// the original store used, so every TTL boundary behavior carries over unchanged.
func (c *Cache) expired(e entry) bool {
	return !e.expiresAt.IsZero() && !c.now().Before(e.expiresAt)
}

// cloneBytes returns a copy of b. A nil input yields an empty, non-nil slice so
// stored values are always addressable and round-trip identically (a nil value set
// reads back as a length-0 non-nil slice). The engine clones on the way in (before
// shardmap.Set) and on the way out (after the shard read lock is released); the
// out-copy is sound outside the lock because shardmap never mutates a stored value's
// backing array in place (copy-on-write invariant, design §9).
func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
