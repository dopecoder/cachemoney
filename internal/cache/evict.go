package cache

import "github.com/dopecoder/cachemoney/internal/shardmap"

// Eviction sampling constants.
const (
	// sampleN is the candidate fan-out per eviction pass (the Redis maxmemory-samples
	// default). It is an internal knob, not a live CONFIG tunable (eviction design §7.2).
	sampleN = 5

	// maxEvictMisses bounds consecutive sampling passes that yield no removable victim
	// — e.g. when an oversized newcomer is the only remaining entry — so an eviction
	// loop always terminates (usage then stays over by exactly that one entry's cost).
	maxEvictMisses = 100
)

// evictUntilUnder frees room until live usage is at or under ceiling, choosing
// victims from the OTHER keys only: when hasExclude is set, exclude (the just-written
// key) is never evicted, which is what makes C-INV-1 structural. pol is read once by
// the caller and passed in, so a mid-pass CONFIG SET cannot mix strategies within a
// single pass. Each victim is removed with one single-key backward-shift Delete (one
// shard Lock for one key) and the sample is redrawn every iteration, so a heavy pass
// never holds a shard long enough to stall its readers (C-INV-2; eviction design §7.2).
func (c *Cache) evictUntilUnder(ceiling int64, pol EvictionPolicy, exclude string, hasExclude bool) {
	misses := 0
	for c.usage.Load() > ceiling {
		victim, ok := c.pickVictim(c.m.Sample(sampleN, nil), pol, exclude, hasExclude)
		if !ok {
			misses++
			if misses > maxEvictMisses {
				return // no removable victim remains: terminate (oversized-newcomer case)
			}
			continue
		}
		e, existed := c.m.Delete(victim)
		if existed {
			c.usage.Add(-costOf(victim, e.value))
			c.evictions.Add(1)
		}
	}
}

// pickVictim selects one victim from the sampled candidates under pol, skipping the
// excluded newcomer (only when hasExclude is set, so a legitimately stored
// empty-string key is not accidentally immune). Under allkeys-random it returns the
// first eligible candidate (the sample is already uniform); under allkeys-lfu it
// returns the candidate of lowest estimated frequency. ok is false only when every
// candidate is the excluded key (so the caller can re-sample or terminate).
func (c *Cache) pickVictim(cands []shardmap.SampleEntry[string, entry], pol EvictionPolicy, exclude string, hasExclude bool) (string, bool) {
	var best string
	var bestFreq uint8
	found := false
	for _, cand := range cands {
		if hasExclude && cand.Key == exclude {
			continue
		}
		if pol == PolicyAllKeysRandom {
			return cand.Key, true
		}
		if f := c.policy.Estimate(c.fingerprint(cand.Key)); !found || f < bestFreq {
			best, bestFreq, found = cand.Key, f, true
		}
	}
	return best, found
}
