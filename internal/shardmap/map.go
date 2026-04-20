package shardmap

import (
	"hash/maphash"
	"runtime"
	"sync"
)

// cacheLineSize is the common x86-64 / arm64 cache-line width. Padding each shard
// to a whole number of these keeps two adjacent shards' hot fields off the same
// line (false-sharing mitigation, design §4.3).
const cacheLineSize = 64

// unpaddedShardSize is sizeof(sync.RWMutex) + sizeof(table[K,V]) on a 64-bit
// platform: 24 + 120. table[K,V] is the same size for every K and V because the
// type parameters appear only inside 24-byte slice headers, so this constant holds
// for all instantiations. TestShard_CacheLinePadding asserts the real layout still
// matches; if it ever drifts (new arch / Go layout change) that test fails and the
// []*shard fallback below should be adopted instead.
const unpaddedShardSize = 144

// shardPad rounds the shard struct up to a whole number of cache lines. With the
// constants above it evaluates to 48 (144 + 48 = 192 = 3×64).
const shardPad = (cacheLineSize - unpaddedShardSize%cacheLineSize) % cacheLineSize

// shard couples one Robin Hood table with its own RWMutex. The mutex sits at the
// struct head because it is touched on every operation, and the trailing pad makes
// sizeof(shard) a multiple of the cache line so the next shard's mutex starts on a
// fresh line — preventing the false sharing that would otherwise silently throttle
// the sharding (design §4.3). Padding is necessary but not strictly sufficient: it
// assumes the []shard backing-array base is 64-byte aligned, which Go's allocator
// provides in practice (page-aligned span, 192-byte element) though it is not a
// language guarantee; TestShard_CacheLinePadding guards the size and the scaling
// benchmark (increment 9) is the empirical check.
//
// The hot-path Map methods take and release mu explicitly (no defer) to keep the
// critical section minimal; the wrapped table ops are panic-free, so the lock
// cannot leak short of a process-fatal OOM in grow/shrink.
//
// The shard array is a padded value slice ([]shard) rather than a pointer slice
// ([]*shard): it is pointer-free (less for the GC to scan) and contiguous (better
// locality). The documented fallback, if exact padding ever proves impractical, is
// []*shard, where each shard is independently heap-allocated and the allocator's
// size classes keep the shards apart.
type shard[K comparable, V any] struct {
	mu sync.RWMutex
	t  table[K, V]
	_  [shardPad]byte
}

// Map is a concurrency-safe, generic sharded hashmap. It routes each key to one of
// N power-of-two shards using the high bits of the key's seeded 64-bit hash, and
// each shard owns an independent Robin Hood open-addressing table guarded by its
// own RWMutex. Operations on keys in different shards proceed fully in parallel;
// within a shard, many readers run concurrently while writers are exclusive.
//
// The shard array is built once by New and is immutable thereafter, so routing to
// a shard is lock-free — only the chosen shard's table is guarded.
type Map[K comparable, V any] struct {
	shards     []shard[K, V]
	seed       maphash.Seed
	hasher     func(maphash.Seed, K) uint64
	shardShift uint // 64 - log2(len(shards)); shardIndex = hash >> shardShift
}

// New builds a Map. The hasher is injected — Go generics cannot derive a hash for
// an arbitrary K, so the engine supplies one (for string keys, hash.String). The
// shard count comes from WithShards (rounded up to a power of two) or defaults to
// next_pow2(GOMAXPROCS×4); the seed comes from WithSeed or is drawn fresh per Map.
// New panics on a nil hasher because no operation could route without one.
func New[K comparable, V any](hasher func(maphash.Seed, K) uint64, opts ...Option) *Map[K, V] {
	if hasher == nil {
		panic("shardmap: New requires a non-nil hasher")
	}

	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}

	numShards := cfg.numShards
	if numShards < 1 {
		numShards = defaultShardCount()
	}
	numShards = nextPow2(numShards)

	seed := cfg.seed
	if !cfg.seedSet {
		seed = maphash.MakeSeed()
	}

	// Split the total capacity hint across shards (rounded up) so no shard grows on
	// the first inserts when a hint is given.
	perShardCap := 0
	if cfg.initialCap > 0 {
		perShardCap = (cfg.initialCap + numShards - 1) / numShards
	}

	shards := make([]shard[K, V], numShards)
	for i := range shards {
		shards[i].t = newTable[K, V](perShardCap)
	}

	return &Map[K, V]{
		shards:     shards,
		seed:       seed,
		hasher:     hasher,
		shardShift: 64 - log2PowerOf2(numShards),
	}
}

// defaultShardCount derives the M0 default shard count: next_pow2(GOMAXPROCS×4),
// floor 1. More shards than cores keeps the odds that two concurrently-served keys
// serialize on the same shard lock low (design §5).
func defaultShardCount() int {
	return nextPow2(runtime.GOMAXPROCS(0) * 4)
}

// shardIndex returns the shard slot for a 64-bit hash: its top log2(numShards)
// bits, selected as hash >> shift where shift = 64 - log2(numShards). For a single
// shard shift is 64 and the result is always 0 (Go defines x>>64 == 0 for uint64),
// so the index can never fall outside the shard array. These high bits are disjoint
// from the low bits the per-shard table uses for its home bucket (design §5).
func shardIndex(h uint64, shift uint) uint64 {
	return h >> shift
}

// Get returns the value stored under key and whether it was present. It takes the
// shard's read lock only, so concurrent readers of the same shard run in parallel
// and the read path never mutates shared state.
func (m *Map[K, V]) Get(key K) (V, bool) {
	h := m.hasher(m.seed, key)
	s := &m.shards[shardIndex(h, m.shardShift)]
	s.mu.RLock()
	v, ok := s.t.lookup(h, key)
	s.mu.RUnlock()
	return v, ok
}

// Set stores value under key, overwriting any existing entry. It holds the shard's
// write lock across the table insert (which may grow and reallocate the slot
// arrays), so concurrent readers never observe a torn table.
func (m *Map[K, V]) Set(key K, value V) {
	h := m.hasher(m.seed, key)
	s := &m.shards[shardIndex(h, m.shardShift)]
	s.mu.Lock()
	s.t.insert(h, key, value)
	s.mu.Unlock()
}

// Delete removes the entry under key, returning the removed value and whether it
// was present. The shard write lock is held across the ENTIRE table.delete call
// because backward-shift deletion may shrink and reallocate every slot array; a
// reader without the lock would otherwise see torn slices.
func (m *Map[K, V]) Delete(key K) (V, bool) {
	h := m.hasher(m.seed, key)
	s := &m.shards[shardIndex(h, m.shardShift)]
	s.mu.Lock()
	v, ok := s.t.delete(h, key)
	s.mu.Unlock()
	return v, ok
}

// Len returns the total number of stored entries, summing each shard's count under
// that shard's read lock (count is a plain int and must never be read lock-free).
// The result is per-shard-consistent, not a single global snapshot: a concurrent
// writer on a not-yet-counted shard may be reflected while an already-counted shard
// is not. That is the deliberate M0 model — Len is not on the hot path.
func (m *Map[K, V]) Len() int {
	n := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.RLock()
		n += s.t.count
		s.mu.RUnlock()
	}
	return n
}

// Range calls fn for every stored entry until fn returns false, then stops. Each
// shard is walked under its own read lock, released before moving to the next
// shard, so Range is per-shard-consistent rather than a single global snapshot: an
// entry inserted into an unvisited shard during the walk may or may not be seen.
// fn MUST NOT call back into the same Map (it would deadlock on the held read lock).
func (m *Map[K, V]) Range(fn func(K, V) bool) {
	for i := range m.shards {
		if !rangeShard(&m.shards[i], fn) {
			return
		}
	}
}

// rangeShard walks one shard's occupied slots under its read lock, invoking fn for
// each. It returns false as soon as fn does, signaling Range to stop.
func rangeShard[K comparable, V any](s *shard[K, V], fn func(K, V) bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := &s.t
	for i := uint64(0); i <= t.capm1; i++ {
		if t.meta[i] == 0 {
			continue
		}
		if !fn(t.keys[i], t.vals[i]) {
			return false
		}
	}
	return true
}
