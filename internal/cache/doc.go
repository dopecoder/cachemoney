// Package cache is cachemoney's embeddable, protocol-agnostic engine boundary: the
// Engine interface and its in-memory implementation, *Cache. It is the structural
// spine the rest of the system attaches to — M0's RESP codec and TCP server, M1's
// Raft/sharding, and M3's durable storage all depend ONLY on Engine (ADR-0003).
//
// # Net-free, remote-ready by shape
//
// Every Engine operation takes a context.Context first and returns an error so the
// contract is remote-ready from the outset: in M0 the in-memory engine returns a
// nil error on success and honors cancellation, while M1/M3 give those errors real
// failure modes without a later signature retrofit. The package — and its
// supporting internal/shardmap and internal/hash packages — import no net, so the
// engine stays unit-testable and embeddable. An import-guard test enforces this.
//
// # Layering
//
// cache → shardmap → hash, no cycles and no upward dependencies. The engine owns
// TTL semantics, the injectable Clock, defensive byte copying, and the
// context/error contract; internal/shardmap is a pure generic container that
// stores the engine's entry value as an opaque V and knows nothing about expiry,
// context, or []byte; internal/hash is the single seeded-hashing chokepoint. The
// engine instantiates shardmap.Map[string, entry] with hash.String as its hasher.
//
// # Iteration / re-entrancy contract (carry-forward)
//
// Len (whose body is wired up in increment 7) reports the number of live entries
// by scanning the backing map with shardmap.Map.Range. Range holds each shard's
// read lock for the duration of that shard's walk and is per-shard-consistent, not
// a single global snapshot. The range callback therefore MUST NOT call back into
// the same Cache (it would deadlock on the held read lock); Len's scan only reads
// entry expiry and never mutates, preserving the read-only guarantee that lets
// concurrent readers proceed in parallel.
package cache
