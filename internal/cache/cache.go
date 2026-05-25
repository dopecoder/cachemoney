package cache

import (
	"context"
	randv2 "math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/dopecoder/cachemoney/internal/hash"
	"github.com/dopecoder/cachemoney/internal/shardmap"
	"github.com/dopecoder/cachemoney/internal/tinylfu"
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
// internal/hash seam. *Cache implements Engine, and additively satisfies Tunable
// (live maxmemory / maxmemory-policy) and exposes Stats / Close. Construct one with
// New; the zero value is not usable.
type Cache struct {
	m   *shardmap.Map[string, entry]
	now Clock

	// policy is the W-TinyLFU frequency oracle. Get records access fingerprints into
	// it (lossy); eviction queries Estimate for victim selection. fpSeed seeds the
	// key→fingerprint hash; policySeed seeds the sketch (stable across Resize).
	policy     *tinylfu.Policy
	fpSeed     hash.Seed
	policySeed uint64

	// usage is the running data-byte counter (Σ cost over live entries); maxmemory is
	// the ceiling (0 = unbounded); evictPol holds the EvictionPolicy enum. All three
	// are read on the write path and tuned live, so they are lock-free atomics.
	usage     atomic.Int64
	maxmemory atomic.Int64
	evictPol  atomic.Int32

	// policyCounters records the last sketch sizing so a no-op SetMaxMemory skips the
	// rebuild.
	policyCounters atomic.Int64

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// config holds the resolved construction settings collected from the Option list.
// It is internal: callers only ever see the functional options below. shardOpts
// accumulates the options passed straight through to shardmap.New.
type config struct {
	clock     Clock
	shardOpts []shardmap.Option
	maxmemory int64
	policy    EvictionPolicy
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

// WithMaxMemory sets the byte ceiling. Zero (the default) means unbounded: no
// eviction and no read-path policy cost beyond a parked drainer goroutine
// (eviction design §1). The value can be changed live with SetMaxMemory.
func WithMaxMemory(bytes int64) Option {
	return func(cfg *config) { cfg.maxmemory = bytes }
}

// WithEvictionPolicy selects the eviction policy (default PolicyAllKeysLFU). It can
// be changed live with SetEvictionPolicy.
func WithEvictionPolicy(p EvictionPolicy) Option {
	return func(cfg *config) { cfg.policy = p }
}

// New returns a Cache ready for use. It instantiates the backing
// shardmap.Map[string, entry] with hash.String as the injected hasher, builds the
// W-TinyLFU policy sized from maxmemory, and applies any options. The returned
// *Cache implements Engine and Tunable. The policy drainer goroutine starts here and
// is stopped by Close.
func New(opts ...Option) *Cache {
	cfg := config{clock: time.Now, policy: PolicyAllKeysLFU}
	for _, opt := range opts {
		opt(&cfg)
	}

	c := &Cache{
		m:          shardmap.New[string, entry](hash.String, cfg.shardOpts...),
		now:        cfg.clock,
		fpSeed:     hash.NewSeed(),
		policySeed: randv2.Uint64(),
	}
	c.maxmemory.Store(cfg.maxmemory)
	c.evictPol.Store(int32(cfg.policy))

	pcfg := c.policyConfig()
	c.policyCounters.Store(int64(pcfg.Counters))
	c.policy = tinylfu.New(pcfg)
	c.refreshActive()
	return c
}

// Policy sketch sizing.
const (
	// assumedAvgEntryCost seeds the expected-entry estimate (≈ maxmemory / cost) used
	// only to size the sketch at construction, before any entries exist.
	assumedAvgEntryCost = 256
	// policyCountersMin / policyCountersMax floor and cap the sketch so a tiny or huge
	// ceiling cannot degenerate or bloat the frequency estimator.
	policyCountersMin = 1024
	policyCountersMax = 1 << 22
)

// policyConfig sizes the frequency sketch from the current maxmemory: roughly one
// counter per expected live entry, clamped. When maxmemory is unset the floor is used
// (the policy is inactive anyway). The sketch seed is stable across rebuilds.
func (c *Cache) policyConfig() tinylfu.Config {
	counters := int64(policyCountersMin)
	if mm := c.maxmemory.Load(); mm > 0 {
		counters = mm / assumedAvgEntryCost
		if counters < policyCountersMin {
			counters = policyCountersMin
		}
		if counters > policyCountersMax {
			counters = policyCountersMax
		}
	}
	// counters is clamped to [policyCountersMin, policyCountersMax] before narrowing,
	// so the conversion is lossless on every platform.
	return tinylfu.Config{Counters: int(counters), Seed: c.policySeed} //nolint:gosec // clamped to <= policyCountersMax; fits int
}

// fingerprint maps a key to the uint64 the policy speaks. It is a second cheap,
// lock-free, contention-free hash (the shard hash is internal to shardmap); computing
// it costs CPU but takes no lock, so it does not threaten C-INV-2 (eviction design §3).
func (c *Cache) fingerprint(key string) uint64 { return hash.String(c.fpSeed, key) }

// refreshActive enables the frequency drainer only when it can matter — under
// allkeys-lfu with a positive ceiling. Otherwise Record is a no-op, so an unbounded
// or random/noeviction engine pays no read-path policy cost (eviction design §1, §7.4).
func (c *Cache) refreshActive() {
	c.policy.SetActive(EvictionPolicy(c.evictPol.Load()) == PolicyAllKeysLFU && c.maxmemory.Load() > 0)
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
		c.misses.Add(1)
		return nil, false, nil
	}
	c.hits.Add(1)
	c.policy.Record(c.fingerprint(key)) // non-blocking, lossy; a no-op unless lfu+bounded
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

	newCost := costOf(key, e.value)
	ceiling := c.maxmemory.Load()
	pol := EvictionPolicy(c.evictPol.Load())

	// noeviction guard: reject only when the operation would GROW usage past the
	// ceiling. A shrinking or equal overwrite (delta <= 0) is always allowed, even at
	// capacity, because it cannot increase memory (eviction design §7.1, C5). The
	// incumbent cost is read lock-free here purely for the reject DECISION; the
	// authoritative delta for accounting comes from the atomic Swap below.
	if ceiling > 0 && pol == PolicyNoEviction {
		var oldCost int64
		if old, existed := c.m.Get(key); existed {
			oldCost = costOf(key, old.value)
		}
		if delta := newCost - oldCost; delta > 0 && c.usage.Load()+delta > ceiling {
			return ErrOOM
		}
	}

	// Store and account atomically: Swap returns the value this write actually replaced
	// under one shard Lock, so the overwrite delta is exact even when concurrent writers
	// race on the same key — no Get-then-Set accounting drift. The newcomer is stored
	// and accounted BEFORE any eviction (C-INV-1).
	old, existed := c.m.Swap(key, e)
	var oldCost int64
	if existed {
		oldCost = costOf(key, old.value)
	}
	c.usage.Add(newCost - oldCost)

	// Then free room from the OTHER keys — the newcomer is excluded, so it is never
	// its own victim (C-INV-1). noeviction already guarded above and never evicts.
	if ceiling > 0 && pol != PolicyNoEviction && c.usage.Load() > ceiling {
		c.evictUntilUnder(ceiling, pol, key, true)
	}
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
	// Physical removal: subtract the cost whether or not the entry was already expired
	// (lazy-TTL reclaim happens here on the next touch).
	c.usage.Add(-costOf(key, e.value))
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

// SetMaxMemory updates the byte ceiling live. A sizing change rebuilds the frequency
// sketch (history re-accumulates within an aging window). Lowering the ceiling below
// current usage under an eviction policy triggers an immediate eviction pass down to
// the new ceiling — the "lowering maxmemory live evicts down" contract (eviction
// design §7.4). It is part of the Tunable interface the server drives from CONFIG SET.
func (c *Cache) SetMaxMemory(bytes int64) {
	c.maxmemory.Store(bytes)
	if pcfg := c.policyConfig(); int64(pcfg.Counters) != c.policyCounters.Swap(int64(pcfg.Counters)) {
		c.policy.Resize(pcfg)
	}
	c.refreshActive()
	if bytes > 0 {
		if pol := EvictionPolicy(c.evictPol.Load()); pol != PolicyNoEviction && c.usage.Load() > bytes {
			c.evictUntilUnder(bytes, pol, "", false)
		}
	}
}

// MaxMemory returns the current byte ceiling (0 = unbounded).
func (c *Cache) MaxMemory() int64 { return c.maxmemory.Load() }

// SetEvictionPolicy switches the policy live; the change affects subsequent eviction
// passes only (an in-flight pass read its policy once). It also toggles the frequency
// drainer, which is active only under allkeys-lfu.
func (c *Cache) SetEvictionPolicy(p EvictionPolicy) {
	c.evictPol.Store(int32(p))
	c.refreshActive()
}

// EvictionPolicy returns the current policy.
func (c *Cache) EvictionPolicy() EvictionPolicy {
	return EvictionPolicy(c.evictPol.Load())
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
