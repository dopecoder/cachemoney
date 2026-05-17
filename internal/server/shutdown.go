package server

import (
	"context"
	"time"
)

// shutdownPollInterval is how often the bounded-drain loop re-checks for
// connections that have become idle and can be closed. It is small so a clean
// drain returns promptly once the last in-flight request finishes.
const shutdownPollInterval = 2 * time.Millisecond

// Shutdown stops accepting new connections, closes connections that are idle
// (blocked on a read), and waits for in-flight connections to finish bounded by
// ctx. Any straggler still in flight past the deadline is force-closed so Shutdown
// cannot hang. It returns nil on a clean drain, or ctx.Err() if the deadline
// elapses with work still in flight.
//
// The engine context is deliberately not cancelled by Shutdown: an in-flight
// request must complete and deliver its reply (the drain bound is enforced by
// closing the socket at the deadline, not by aborting the engine call).
func (s *Server) Shutdown(ctx context.Context) error {
	// Set the shutdown flag and read the listener under the same lock Serve uses to
	// publish it, so neither the flag nor s.ln is accessed concurrently unsynchronized.
	s.mu.Lock()
	s.inShutdown.Store(true)
	ln := s.ln
	s.mu.Unlock()

	var lnErr error
	if ln != nil {
		lnErr = ln.Close()
	}

	ticker := time.NewTicker(shutdownPollInterval)
	defer ticker.Stop()
	for {
		s.closeIdleConns()
		if s.connCount() == 0 {
			return lnErr
		}
		select {
		case <-ctx.Done():
			s.closeAllConns()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
