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
// Increment 6 is the engine skeleton: this method wires the single ctx.Err() entry
// check and returns zero values. The real lookup — lazy-TTL expiry and the
// defensive out-copy taken after the shard read lock is released — is wired in
// increment 7.
func (c *Cache) Get(ctx context.Context, key string) (value []byte, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

// Set stores a defensive copy of value under key with an optional TTL, overwriting
// any existing entry.
//
// Increment 6 skeleton: the ctx.Err() entry check returns before any lock or
// mutation, so a cancelled Set stores nothing. The defensive in-copy, expiresAt
// computation, and shardmap insert are wired in increment 7.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Del removes key, reporting whether it was present and live at deletion time.
//
// Increment 6 skeleton: the ctx.Err() entry check only. The shardmap delete and
// liveness result are wired in increment 7.
func (c *Cache) Del(ctx context.Context, key string) (existed bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// TTL reports the remaining lifetime of a live key (-1 for a live key with no
// expiry), with ok false for an absent or expired key.
//
// Increment 6 skeleton: the ctx.Err() entry check only. The remaining-lifetime
// computation is wired in increment 8.
func (c *Cache) TTL(ctx context.Context, key string) (remaining time.Duration, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

// Len returns the number of live (non-expired) entries.
//
// Increment 6 skeleton: the ctx.Err() entry check only. The Range-based live count
// (see the package doc's iteration / re-entrancy contract) is wired in increment 7.
func (c *Cache) Len(ctx context.Context) (n int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}
