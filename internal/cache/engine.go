package cache

import (
	"context"
	"time"
)

// Engine is the embeddable, protocol-agnostic cache core: the single contract
// every consumer (the M0 RESP codec and TCP server, M1's Raft layer, M3's storage
// tier) depends on. Implementations store binary-safe []byte values under string
// keys with optional per-key TTL.
//
// Every operation takes a context.Context as its first argument and returns an
// error, making the contract remote-ready from the outset (ADR-0003). The M0
// in-memory implementation (*Cache) returns a nil error on success and returns
// ctx.Err() when called with an already-cancelled context; at M1/M3 the error
// gains real failure modes (not leader, no quorum, storage unreachable).
//
// Get and TTL are read-only; Set and Del mutate. The implementing package imports
// no net, directly or transitively.
type Engine interface {
	// Get returns a defensive copy of the live value stored under key. ok is
	// false when the key is absent or its TTL has elapsed (lazy expiry on read).
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)

	// Set stores a defensive copy of value under key, overwriting any existing
	// entry. A ttl greater than zero expires the entry ttl from now; a ttl of
	// zero or less never expires.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Del removes key, reporting whether it was present AND live at deletion time.
	Del(ctx context.Context, key string) (existed bool, err error)

	// TTL reports the remaining lifetime of a live key. ok is false for an absent
	// or expired key; a live key with no expiry returns the sentinel remaining of
	// -1 (Redis-style). TTL does not mutate stored state.
	TTL(ctx context.Context, key string) (remaining time.Duration, ok bool, err error)

	// Len returns the number of live (non-expired) entries. Entries whose TTL has
	// elapsed but whose memory has not yet been reclaimed are not counted.
	Len(ctx context.Context) (n int, err error)
}
