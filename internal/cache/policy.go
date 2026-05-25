package cache

import "errors"

// ErrOOM is returned by Set under maxmemory-policy=noeviction when the operation
// would push live usage above maxmemory. Nothing is stored when it is returned. The
// server maps errors.Is(err, ErrOOM) to the Redis -OOM reply; no policy vocabulary
// leaks into the codec (eviction design §8.2).
var ErrOOM = errors.New("cache: write rejected, used memory exceeds maxmemory (noeviction)")

// EvictionPolicy selects how the engine reclaims room under capacity pressure. The
// zero value is PolicyAllKeysLFU — the default when maxmemory-policy is unset.
type EvictionPolicy int32

const (
	// PolicyAllKeysLFU is the default: frequency-aware sampled cost-aware eviction
	// (the W-TinyLFU policy). It is the zero value so an unconfigured engine defaults
	// to it.
	PolicyAllKeysLFU EvictionPolicy = iota
	// PolicyAllKeysRandom evicts a uniformly random victim from each sample — the
	// benchmark baseline.
	PolicyAllKeysRandom
	// PolicyNoEviction rejects over-capacity writes with ErrOOM instead of evicting.
	PolicyNoEviction
)

// policyNames maps each supported policy to its canonical Redis name.
var policyNames = map[EvictionPolicy]string{
	PolicyAllKeysLFU:    "allkeys-lfu",
	PolicyAllKeysRandom: "allkeys-random",
	PolicyNoEviction:    "noeviction",
}

// String returns the canonical Redis name of the policy (for CONFIG GET).
func (p EvictionPolicy) String() string {
	if name, ok := policyNames[p]; ok {
		return name
	}
	return "unknown"
}

// ParsePolicy maps a Redis policy name to its EvictionPolicy. It is the single
// source of truth for the supported set: only noeviction, allkeys-lfu, and
// allkeys-random are accepted (volatile-* and anything else return ok=false). The
// server maps a false result to a -ERR reply; main maps it to a startup error
// (eviction design §8.2).
func ParsePolicy(name string) (EvictionPolicy, bool) {
	switch name {
	case "allkeys-lfu":
		return PolicyAllKeysLFU, true
	case "allkeys-random":
		return PolicyAllKeysRandom, true
	case "noeviction":
		return PolicyNoEviction, true
	default:
		return 0, false
	}
}

// Tunable is the optional interface the server type-asserts for live CONFIG of
// maxmemory and maxmemory-policy. *Cache satisfies it; the core Engine interface is
// deliberately NOT widened with these methods (eviction design §8.1).
type Tunable interface {
	SetMaxMemory(int64)
	MaxMemory() int64
	SetEvictionPolicy(EvictionPolicy)
	EvictionPolicy() EvictionPolicy
}
