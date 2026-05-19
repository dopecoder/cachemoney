package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/resp"
)

// newPipeConn wires a *conn to the server side of a net.Pipe and returns the
// client side. It does not start the serve loop.
func newPipeConn(t *testing.T, srv *Server) (*conn, net.Conn) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	_ = clientSide.SetDeadline(time.Now().Add(2 * time.Second))
	_ = serverSide.SetDeadline(time.Now().Add(2 * time.Second))
	c := &conn{
		srv:   srv,
		nc:    serverSide,
		r:     resp.NewReader(serverSide),
		w:     resp.NewWriter(serverSide),
		proto: 2,
		id:    1,
	}
	t.Cleanup(func() { _ = clientSide.Close() })
	return c, clientSide
}

// runConn wires a *conn over net.Pipe, runs serveConn in a goroutine, and returns
// the client side plus a channel closed when serveConn exits.
func runConn(t *testing.T, srv *Server) (client net.Conn, done chan struct{}) {
	t.Helper()
	c, client := newPipeConn(t, srv)
	done = make(chan struct{})
	go func() {
		srv.serveConn(c)
		close(done)
	}()
	return client, done
}

// waitDone fails the test if serveConn does not exit promptly.
func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveConn did not return")
	}
}

// errConn is a net.Conn whose Read always returns a fixed non-EOF transport error.
type errConn struct {
	net.Conn
	readErr error
}

func (e *errConn) Read([]byte) (int, error) { return 0, e.readErr }

// --- classify (pure, Req 7) ---------------------------------------------------

func TestClassify(t *testing.T) {
	pe := &resp.ProtocolError{Kind: resp.KindBadLength, Msg: "invalid length"}
	cases := []struct {
		name   string
		err    error
		want   readOutcome
		wantPE bool
	}{
		{"nil dispatches", nil, outcomeDispatch, false},
		{"clean eof", io.EOF, outcomeCloseSilent, false},
		{"wrapped eof", fmt.Errorf("ctx: %w", io.EOF), outcomeCloseSilent, false},
		{"unexpected eof", io.ErrUnexpectedEOF, outcomeCloseSilent, false},
		{"wrapped unexpected eof", fmt.Errorf("ctx: %w", io.ErrUnexpectedEOF), outcomeCloseSilent, false},
		{"protocol error", pe, outcomeProtocolError, true},
		{"wrapped protocol error", fmt.Errorf("ctx: %w", pe), outcomeProtocolError, true},
		{"transport error", errors.New("boom"), outcomeCloseSilent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, gotPE := classify(tc.err)
			if got != tc.want {
				t.Errorf("outcome = %d, want %d", got, tc.want)
			}
			if (gotPE != nil) != tc.wantPE {
				t.Errorf("protocolError = %v, want present=%v", gotPE, tc.wantPE)
			}
			if tc.wantPE && gotPE.Msg != "invalid length" {
				t.Errorf("pe.Msg = %q, want %q", gotPE.Msg, "invalid length")
			}
		})
	}
}

// --- close-policy matrix over net.Pipe (Req 7) --------------------------------

func TestProtocolErrorRepliesThenCloses(t *testing.T) {
	client, done := runConn(t, New(cache.New(), Config{}))

	// "*1\r\n$xy\r\n": a non-numeric bulk length -> *resp.ProtocolError(invalid length).
	mustWrite(t, client, []byte("*1\r\n$xy\r\n"))
	wantReply(t, client, "-ERR Protocol error: invalid length\r\n")
	expectClosed(t, client)
	waitDone(t, done)
}

func TestCleanEOFClosesSilently(t *testing.T) {
	client, done := runConn(t, New(cache.New(), Config{}))

	// Close at a command boundary -> the codec reports io.EOF -> silent close.
	_ = client.Close()
	waitDone(t, done)
}

func TestTruncatedFrameClosesSilently(t *testing.T) {
	client, done := runConn(t, New(cache.New(), Config{}))

	go func() {
		// header promises a 5-byte bulk; only 2 bytes follow before close.
		_, _ = client.Write([]byte("*1\r\n$5\r\nab"))
		_ = client.Close()
	}()
	waitDone(t, done)
}

func TestTransportErrorClosesSilently(t *testing.T) {
	srv := New(cache.New(), Config{})
	serverSide, clientSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	ec := &errConn{Conn: serverSide, readErr: errors.New("transport boom")}
	c := &conn{
		srv:   srv,
		nc:    ec,
		r:     resp.NewReader(ec),
		w:     resp.NewWriter(ec),
		proto: 2,
		id:    1,
	}
	done := make(chan struct{})
	go func() {
		srv.serveConn(c)
		close(done)
	}()
	waitDone(t, done)
}

func TestServeConnStopsWhenShuttingDown(t *testing.T) {
	srv := New(cache.New(), Config{})
	srv.inShutdown.Store(true)
	c, client := newPipeConn(t, srv)
	_ = client
	done := make(chan struct{})
	go func() {
		srv.serveConn(c)
		close(done)
	}()
	waitDone(t, done) // the fast-path returns before any read
}

// --- Increment 5: idle timeout + rejectConn polite close (Req 9) --------------

func TestIdleTimeoutClosesIdleConnection(t *testing.T) {
	srv := New(cache.New(), Config{IdleTimeout: 20 * time.Millisecond})
	_, done := runConn(t, srv)
	// The client sends nothing; the per-read idle deadline fires and the loop
	// classifies the deadline error as a silent close.
	waitDone(t, done)
}

func TestRejectConnWritesPoliteErrorThenCloses(t *testing.T) {
	srv := New(cache.New(), Config{})
	serverSide, clientSide := net.Pipe()
	defer func() { _ = clientSide.Close() }()
	_ = clientSide.SetDeadline(time.Now().Add(2 * time.Second))

	done := make(chan struct{})
	go func() {
		srv.rejectConn(serverSide)
		close(done)
	}()

	wantReply(t, clientSide, "-ERR max number of clients reached\r\n")
	expectClosed(t, clientSide)
	waitDone(t, done)
}
