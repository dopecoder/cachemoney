package cache

// entryOverhead is the fixed per-entry byte cost the accounting model adds on top of
// the key and value lengths. It approximates the Go memory an entry occupies beyond
// its raw bytes: the string header (16) + []byte header (24) + time.Time expiresAt
// (24) + map slot/meta/Robin-Hood bookkeeping. Like Redis's used_memory, the model
// is an honest approximation chosen for bounded-and-predictable memory rather than
// byte-exact RSS (eviction design §7.3; recorded in ADR-0011).
const entryOverhead = 64

// costOf returns the accounted byte cost of one entry: len(key) + len(value) + the
// fixed overhead. The overwrite delta (newCost - oldCost) reduces to the value-length
// difference because the key and overhead cancel, so the running counter never drifts.
func costOf(key string, value []byte) int64 {
	return int64(len(key) + len(value) + entryOverhead)
}
