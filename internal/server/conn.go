package server

import (
	"errors"
	"io"
	"net"
	"sync/atomic"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// conn is the per-connection state for one accepted socket. Exactly one goroutine
// touches a given conn (and thus its reader/writer) for the connection's whole
// life, satisfying the codec's single-goroutine contract.
type conn struct {
	srv    *Server
	nc     net.Conn
	r      *resp.Reader
	w      *resp.Writer
	proto  int
	id     uint64
	active atomic.Bool
}

// readOutcome classifies the result of a ReadCommand call.
type readOutcome int

const (
	// outcomeDispatch means a valid frame was read and should be dispatched.
	outcomeDispatch readOutcome = iota
	// outcomeCloseSilent means the connection should close with no reply
	// (clean EOF, truncation, idle timeout, or any transport error).
	outcomeCloseSilent
	// outcomeProtocolError means the bytes were malformed: reply
	// "-ERR Protocol error: <msg>" and then close.
	outcomeProtocolError
)

// classify maps the codec's four-way ReadCommand return onto a close-policy
// outcome by error identity (errors.Is/errors.As), never by string comparison, so
// a future codec that wraps these errors still classifies correctly.
func classify(err error) (readOutcome, *resp.ProtocolError) {
	switch {
	case err == nil:
		return outcomeDispatch, nil
	case errors.Is(err, io.EOF):
		return outcomeCloseSilent, nil
	case errors.Is(err, io.ErrUnexpectedEOF):
		return outcomeCloseSilent, nil
	default:
		var pe *resp.ProtocolError
		if errors.As(err, &pe) {
			return outcomeProtocolError, pe
		}
		return outcomeCloseSilent, nil
	}
}

// serveConn runs the per-connection read/dispatch/flush loop until the connection
// closes. It is the goroutine body launched per accepted socket.
func (s *Server) serveConn(c *conn) {
	defer s.dropConn(c)
	for {
		if s.inShutdown.Load() {
			// Stop serving once shutdown begins. Any reply for the previous
			// command was already flushed; a second command buffered in the same
			// pipelined read is intentionally dropped (graceful-shutdown behavior).
			return
		}
		c.active.Store(false) // blocked on read == idle (shutdown may close us)
		args, err := c.r.ReadCommand()
		c.active.Store(true) // a frame arrived: we are now in flight
		if err != nil {
			if outcome, pe := classify(err); outcome == outcomeProtocolError {
				// resp.Writer.WriteError prepends '-' and appends CRLF, so the
				// "ERR " prefix is part of the message we pass.
				_ = c.w.WriteError("ERR Protocol error: " + pe.Msg)
				_ = c.w.Flush()
			}
			return // every read error closes (silently, or after the -ERR reply)
		}
		if stop := s.dispatch(c, args); stop {
			_ = c.w.Flush() // deliver QUIT's +OK before closing
			return
		}
		if err := c.w.Flush(); err != nil {
			return // client gone mid-reply
		}
	}
}
