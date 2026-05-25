package shardmap

import randv2 "math/rand/v2"

// SampleEntry is one drawn live candidate: a read-only snapshot of a slot's key and
// value. The engine uses it to compute the candidate's cost and fingerprint for
// victim selection without a second map lookup (eviction design §6).
type SampleEntry[K comparable, V any] struct {
	Key K
	Val V
}

// Sample draw budgeting. Each outer attempt either lands a fresh distinct candidate
// or is wasted on an empty shard / a duplicate; the budget bounds total attempts so
// a map holding fewer than n distinct live entries still terminates.
const (
	sampleDrawFactor = 8
	sampleDrawFloor  = 16

	// sampleSlotTries bounds the per-shard random-slot reject-sampling before the
	// guaranteed forward-scan fallback. A small budget keeps the common (low load
	// factor) case O(1) while the fallback always finds a live slot in a denser table.
	sampleSlotTries = 8
)

// Sample draws up to n random LIVE entries spread across shards, taking each touched
// shard's RLock only — it never mutates the map. rand returns a uniform uint64
// (injectable for deterministic tests; nil selects an internal concurrency-safe
// source). It returns fewer than n entries only when the map holds fewer than n
// distinct live entries. Removal is NOT part of Sample: the caller picks a victim
// from the result and calls the existing Delete (backward-shift) under the shard
// Lock, one key at a time (eviction design §6).
func (m *Map[K, V]) Sample(n int, rand func() uint64) []SampleEntry[K, V] {
	if n <= 0 {
		return nil
	}
	if rand == nil {
		rand = randUint64
	}
	shardMask := uint64(len(m.shards) - 1) //nolint:gosec // New floors shards at 1; len-1 is non-negative
	out := make([]SampleEntry[K, V], 0, n)
	seen := make(map[K]struct{}, n)
	for budget := n*sampleDrawFactor + sampleDrawFloor; len(out) < n && budget > 0; budget-- {
		e, ok := sampleShard(&m.shards[rand()&shardMask], rand)
		if !ok {
			continue // empty shard: try again
		}
		if _, dup := seen[e.Key]; dup {
			continue // already drawn: keep the result distinct
		}
		seen[e.Key] = struct{}{}
		out = append(out, e)
	}
	return out
}

// randUint64 is the default uniform source: math/rand/v2's top-level generator is
// concurrency-safe and fast (per-P state, no global lock), so eviction sampling
// needs no shared-PRNG mutex (eviction design §6).
func randUint64() uint64 { return randv2.Uint64() }

// sampleShard returns one randomly chosen live entry from s under its read lock, or
// ok=false when the shard is empty. It only reads slots — never mutating the map —
// so it cannot perturb Robin Hood ordering.
func sampleShard[K comparable, V any](s *shard[K, V], rand func() uint64) (SampleEntry[K, V], bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := &s.t
	if t.count == 0 {
		return SampleEntry[K, V]{}, false
	}
	i := t.sampleSlot(rand)
	return SampleEntry[K, V]{Key: t.keys[i], Val: t.vals[i]}, true
}

// sampleSlot returns the index of a randomly chosen occupied slot. The caller MUST
// guarantee count > 0, so a slot always exists. Reject-sampling (a fresh random
// index, retried on an empty hit) gives a near-uniform draw over occupied slots
// without the positional bias a forward-from-random scan would introduce; when the
// reject budget is exhausted on a denser table it falls back to a guaranteed forward
// scan to the next occupied slot.
func (t *table[K, V]) sampleSlot(rand func() uint64) uint64 {
	for tries := 0; tries < sampleSlotTries; tries++ {
		if i := rand() & t.capm1; t.meta[i] != 0 {
			return i
		}
	}
	for start := rand() & t.capm1; ; start = (start + 1) & t.capm1 {
		if t.meta[start] != 0 {
			return start
		}
	}
}
