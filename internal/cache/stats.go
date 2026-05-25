package cache

// Stats is an atomic snapshot of the engine's counters and byte accounting — an
// additive surface; the core Engine interface is unchanged (eviction design §8.1).
type Stats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
	Usage     int64
	MaxMemory int64
}

// Stats returns a snapshot of the live hit/miss/eviction counters and the current
// byte usage and ceiling. Each field is loaded atomically, so the snapshot is not a
// single consistent instant but every field is individually race-free.
func (c *Cache) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Usage:     c.usage.Load(),
		MaxMemory: c.maxmemory.Load(),
	}
}

// Close stops the policy drainer goroutine and joins it with no leak. It is
// idempotent. main calls it AFTER server.Shutdown has drained connections, so no
// Get/Set runs concurrently with Close (eviction design §5.5, §8.1).
func (c *Cache) Close() error {
	return c.policy.Close()
}
