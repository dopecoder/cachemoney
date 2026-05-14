// Package resp is cachemoney's pure RESP2/RESP3 framing codec: a streaming
// decoder that turns a socket's byte stream into command tokens, and a reply
// encoder that turns reply values into byte-exact RESP2 or RESP3 frames. It is the
// untrusted-input frontier of the wire protocol (ADR-0007) and sits one layer
// outside the engine boundary (ADR-0003).
//
// # Net-free, engine-free, self-contained
//
// The package imports neither net nor the engine stack (internal/cache,
// internal/shardmap, internal/hash); it depends only on the Go standard library
// (bufio, bytes, io, strconv, math, math/big). An import-guard test enforces this,
// so the codec is unit-testable without a socket, fuzzable as a standalone parser,
// and cleanly extractable as the resp library. It holds no command semantics: it
// produces and consumes tokens and values only, and never calls the engine.
//
// # The decode/encode seam
//
// Reader.ReadCommand decodes one RESP array of bulk strings into an argument
// vector ([][]byte); the request framing is identical in RESP2 and RESP3, so the
// reader carries no version flag. Writer encodes replies; only replies differ
// between dialects, so the per-connection protocol version lives solely on the
// Writer and is selected with SetProto. The two halves share only this package and
// the limit/error definitions.
//
// # Copy-out ownership
//
// The [][]byte returned by ReadCommand, and every []byte element within it, are
// owned by the caller and remain byte-for-byte stable for their entire lifetime,
// regardless of any number of subsequent ReadCommand calls. Each argument is read
// into a freshly allocated slice that never aliases the internal bufio buffer, so
// pipelined commands cannot corrupt one another.
//
// # Per-connection single-goroutine usage
//
// A Reader and a Writer carry no internal locking and are NOT safe for concurrent
// use by multiple goroutines. The intended model (matching a goroutine-per-
// connection server) is one connection owning one Reader for its read loop and one
// Writer for its reply path; two connections use two independent pairs and never
// share state.
package resp
