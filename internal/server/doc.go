// Package server is cachemoney's TCP/RESP adapter: the one package that wires a
// network socket to the cache engine. It accepts connections, runs a
// per-connection "read command -> dispatch -> write reply -> flush" loop over the
// resp codec, owns command semantics and the -ERR/close policy, and manages the
// connection lifecycle (graceful shutdown, and — in later increments — idle
// timeouts and a max-connections cap).
//
// # The boundary
//
// This package imports net by design and must remain the only one that does. It
// composes the engine exclusively through the published cache.Engine interface
// (Get/Set/Del/TTL/Len); it never touches engine internals, and it adds no engine
// capability. That boundary is enforced by netguard_test.go.
//
// # Concurrency model
//
// One goroutine per accepted connection (ADR-0004). Each connection owns its own
// resp.Reader and resp.Writer for its whole life, so the codec's single-goroutine
// contract holds by construction and no per-connection state is shared. The only
// shared state is the engine (concurrency-safe on its own), the active-connection
// set (mutex-guarded), and a couple of atomics (the connection-id counter and the
// shutdown flag).
//
// # Close policy
//
// Read faults are mapped from the codec's four-way ReadCommand contract by error
// identity (errors.Is/errors.As), never by string match: a clean io.EOF or a
// truncated/transport error closes silently; a *resp.ProtocolError is answered with
// "-ERR Protocol error: <msg>" and then closes. Dispatch-time application errors
// (unknown command, wrong arity) reply "-ERR ..." and keep the connection open.
// Client-derived text echoed into an error reply is sanitized (control bytes
// replaced, length-capped) so it cannot inject a second reply frame.
package server
