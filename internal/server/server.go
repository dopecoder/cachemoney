package server

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/resp"
)

// ErrServerClosed is returned by Serve and ListenAndServe after Shutdown has been
// called. Like net/http.ErrServerClosed it signals a clean stop, not a failure.
var ErrServerClosed = errors.New("server: Server closed")

// Server is a goroutine-per-connection RESP server over a cache.Engine. A single
// Serve/ListenAndServe call runs the accept loop; Shutdown may be called
// concurrently from another goroutine (for example a signal handler).
type Server struct {
	engine cache.Engine
	cfg    Config

	ln net.Listener

	// sem is a counting semaphore bounding concurrent served connections to
	// cfg.MaxConns. The accept loop sends on it before serving a connection and
	// the serve goroutine receives from it when the connection ends.
	sem chan struct{}

	mu    sync.Mutex
	conns map[*conn]struct{}

	inShutdown atomic.Bool
	nextID     atomic.Uint64
}

// New builds a Server serving engine with cfg. engine is a required collaborator,
// so it is an explicit argument rather than a Config field. New does not bind a
// socket.
func New(engine cache.Engine, cfg Config) *Server {
	cfg = cfg.defaults()
	return &Server{
		engine: engine,
		cfg:    cfg,
		conns:  make(map[*conn]struct{}),
		sem:    make(chan struct{}, cfg.MaxConns),
	}
}

// ListenAndServe binds a TCP listener on cfg.Addr and serves until Shutdown or a
// fatal accept error. It returns ErrServerClosed after a clean Shutdown.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve runs the accept loop over an already-open listener, spawning one goroutine
// per accepted connection. It returns ErrServerClosed after Shutdown closes the
// listener (or if Shutdown already ran before Serve bound it), and the underlying
// accept error otherwise. Serve is single-use: one Serve/ListenAndServe call per
// Server.
func (s *Server) Serve(ln net.Listener) error {
	// Publish the listener and read the shutdown flag under the same lock that
	// Shutdown takes, so the bind-vs-shutdown ordering is total: a Shutdown that
	// already ran here closes ln and stops us from accepting (spec Req 8); a later
	// Shutdown sees s.ln and closes it.
	s.mu.Lock()
	if s.inShutdown.Load() {
		s.mu.Unlock()
		_ = ln.Close()
		return ErrServerClosed
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if s.inShutdown.Load() {
				return ErrServerClosed
			}
			return err
		}
		// Accept first, then try to acquire a slot. A full semaphore means the
		// max-connections cap is reached: refuse politely in a separate goroutine
		// so the accept loop is never wedged and existing connections keep serving.
		select {
		case s.sem <- struct{}{}:
			c := s.newConn(nc)
			s.addConn(c)
			go func() {
				defer func() { <-s.sem }() // free the slot when the connection ends
				s.serveConn(c)
			}()
		default:
			go s.rejectConn(nc)
		}
	}
}

// rejectConn politely refuses a connection accepted past the max-connections cap:
// it writes the Redis-style notice and closes the socket. It never acquired a
// semaphore slot, so it releases nothing. It runs in its own goroutine so the
// accept loop returns immediately to Accept the next connection.
func (s *Server) rejectConn(nc net.Conn) {
	w := resp.NewWriter(nc)
	_ = w.WriteError("ERR max number of clients reached")
	_ = w.Flush()
	_ = nc.Close()
}

// newConn builds a per-connection state object with its own codec reader/writer.
func (s *Server) newConn(nc net.Conn) *conn {
	return &conn{
		srv:   s,
		nc:    nc,
		r:     resp.NewReader(nc),
		w:     resp.NewWriter(nc),
		proto: 2,
		id:    s.nextID.Add(1),
	}
}

// addConn records c in the active-connection set.
func (s *Server) addConn(c *conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

// dropConn closes c's socket, removes it from the active set, and is called once,
// deferred, by serveConn.
func (s *Server) dropConn(c *conn) {
	_ = c.nc.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// connCount reports how many connections are still tracked.
func (s *Server) connCount() int {
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	return n
}

// closeIdleConns force-closes every connection currently blocked on a read
// (active == false). Closing the socket unblocks the read, which classifies as a
// silent close.
func (s *Server) closeIdleConns() {
	s.mu.Lock()
	for c := range s.conns {
		if !c.active.Load() {
			_ = c.nc.Close()
		}
	}
	s.mu.Unlock()
}

// closeAllConns force-closes every remaining connection, including in-flight
// stragglers, at the shutdown deadline.
func (s *Server) closeAllConns() {
	s.mu.Lock()
	for c := range s.conns {
		_ = c.nc.Close()
	}
	s.mu.Unlock()
}
