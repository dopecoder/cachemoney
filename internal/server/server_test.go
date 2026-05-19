package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
)

// --- shared test helpers (used across the server package's _test.go files) ----

// encodeCmd builds a RESP array-of-bulk-strings request frame from parts.
func encodeCmd(parts ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return []byte(b.String())
}

// expect reads exactly len(want) bytes from r and compares them. It returns an
// error (rather than failing a *testing.T) so it is safe to call from spawned
// goroutines.
func expect(r io.Reader, want string) error {
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if string(buf) != want {
		return fmt.Errorf("reply = %q, want %q", buf, want)
	}
	return nil
}

// wantReply is the *testing.T wrapper around expect for the test goroutine.
func wantReply(t *testing.T, r io.Reader, want string) {
	t.Helper()
	if err := expect(r, want); err != nil {
		t.Fatalf("%v", err)
	}
}

// mustWrite writes b to c, failing the test on error.
func mustWrite(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readLine reads bytes from r up to and including the next '\n'.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	var out []byte
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, one); err != nil {
			t.Fatalf("readLine: %v", err)
		}
		out = append(out, one[0])
		if one[0] == '\n' {
			return string(out)
		}
	}
}

// expectClosed asserts that r is closed / at EOF (the next read returns an error).
func expectClosed(t *testing.T, r io.Reader) {
	t.Helper()
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err == nil {
		t.Fatalf("expected closed connection, read %q", buf[:1])
	}
}

// newLocalListener binds a loopback TCP listener on an ephemeral port.
func newLocalListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// startServer runs srv.Serve(ln) in a goroutine over a fresh loopback listener and
// registers a cleanup that shuts the server down and asserts Serve returned
// ErrServerClosed. It returns the server and its dial address.
func startServer(t *testing.T, engine cache.Engine, cfg Config) (srv *Server, addr string) {
	t.Helper()
	ln := newLocalListener(t)
	srv = New(engine, cfg)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, ErrServerClosed) {
				t.Errorf("Serve returned %v, want ErrServerClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("Serve did not return after Shutdown")
		}
	})
	return srv, ln.Addr().String()
}

// dial opens a loopback TCP connection with a short safety deadline.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// blockingEngine wraps a real engine and runs gate() on every Get, letting a test
// deterministically hold a request "in flight".
type blockingEngine struct {
	cache.Engine
	gate func()
}

func (b *blockingEngine) Get(ctx context.Context, key string) (value []byte, ok bool, err error) {
	if b.gate != nil {
		b.gate()
	}
	return b.Engine.Get(ctx, key)
}

// errEngine returns err from every method, exercising the handlers' engine-error
// branches.
type errEngine struct{ err error }

func (e errEngine) Get(context.Context, string) (value []byte, ok bool, err error) {
	return nil, false, e.err
}

func (e errEngine) Set(context.Context, string, []byte, time.Duration) error {
	return e.err
}

func (e errEngine) Del(context.Context, string) (bool, error) {
	return false, e.err
}

func (e errEngine) TTL(context.Context, string) (time.Duration, bool, error) {
	return 0, false, e.err
}

func (e errEngine) Len(context.Context) (int, error) {
	return 0, e.err
}

// --- Requirement 1: TCP server reachable over the Redis wire (I2 integration) -

func TestServerRoundTripSetGet(t *testing.T) {
	_, addr := startServer(t, cache.New(), Config{})
	c := dial(t, addr)

	mustWrite(t, c, encodeCmd("SET", "k", "v"))
	wantReply(t, c, "+OK\r\n")
	mustWrite(t, c, encodeCmd("GET", "k"))
	wantReply(t, c, "$1\r\nv\r\n")
}

func TestServerConcurrentConnections(t *testing.T) {
	_, addr := startServer(t, cache.New(), Config{})

	errs := make(chan error, 2)
	run := func(key, val, wantGet string) {
		errs <- func() error {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Write(encodeCmd("SET", key, val)); err != nil {
				return err
			}
			if err := expect(c, "+OK\r\n"); err != nil {
				return err
			}
			if _, err := c.Write(encodeCmd("GET", key)); err != nil {
				return err
			}
			return expect(c, wantGet)
		}()
	}
	go run("a", "1", "$1\r\n1\r\n")
	go run("b", "2", "$1\r\n2\r\n")
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent connection %d: %v", i, err)
		}
	}
}

func TestListenAndServe(t *testing.T) {
	// Find a free port, then bind it via ListenAndServe.
	probe := newLocalListener(t)
	addr := probe.Addr().String()
	_ = probe.Close()

	srv := New(cache.New(), Config{Addr: addr})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		c, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c == nil {
		t.Fatalf("ListenAndServe never bound %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))

	mustWrite(t, c, encodeCmd("PING"))
	wantReply(t, c, "+PONG\r\n")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("ListenAndServe = %v, want ErrServerClosed", err)
	}
}

func TestListenAndServeBindError(t *testing.T) {
	busy := newLocalListener(t)
	defer func() { _ = busy.Close() }()

	srv := New(cache.New(), Config{Addr: busy.Addr().String()})
	if err := srv.ListenAndServe(); err == nil {
		t.Fatal("ListenAndServe on a busy address: want bind error, got nil")
	}
}

func TestServeReturnsAcceptError(t *testing.T) {
	ln := newLocalListener(t)
	srv := New(cache.New(), Config{})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	time.Sleep(20 * time.Millisecond)
	_ = ln.Close() // close directly, without Shutdown: inShutdown stays false

	err := <-errCh
	if err == nil || errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve = %v, want a raw accept error", err)
	}
}

// --- Requirement 8: graceful shutdown with bounded drain (I1 integration) -----

func TestShutdownInFlightCompletes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	base := cache.New()
	if err := base.Set(context.Background(), "k", []byte("hello"), 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	eng := &blockingEngine{Engine: base, gate: func() {
		once.Do(func() { close(entered) })
		<-release
	}}

	srv, addr := startServer(t, eng, Config{})
	c := dial(t, addr)

	mustWrite(t, c, encodeCmd("GET", "k"))
	<-entered // the request is now in flight inside the engine

	shutErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutErr <- srv.Shutdown(ctx)
	}()

	time.Sleep(20 * time.Millisecond) // let the drain begin while in flight
	close(release)                    // the in-flight request finishes during drain

	if err := <-shutErr; err != nil {
		t.Fatalf("Shutdown returned %v, want nil (clean drain)", err)
	}
	wantReply(t, c, "$5\r\nhello\r\n")
}

func TestShutdownNoNewConnectionAccepted(t *testing.T) {
	ln := newLocalListener(t)
	addr := ln.Addr().String()
	srv := New(cache.New(), Config{})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	// Prove the server serves before shutdown.
	c := dial(t, addr)
	mustWrite(t, c, encodeCmd("PING"))
	wantReply(t, c, "+PONG\r\n")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve = %v, want ErrServerClosed", err)
	}

	// A connection attempt after shutdown is not served.
	nc, derr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if derr != nil {
		return // refused: not served — the spec's "refused or closed" is satisfied
	}
	defer func() { _ = nc.Close() }()
	_ = nc.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = nc.Write(encodeCmd("PING"))
	buf := make([]byte, 1)
	if _, rerr := nc.Read(buf); rerr == nil {
		t.Fatal("a new connection was served after shutdown began")
	}
}

func TestShutdownForceClosesStraggler(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	eng := &blockingEngine{Engine: cache.New(), gate: func() {
		once.Do(func() { close(entered) })
		<-release
	}}

	srv, addr := startServer(t, eng, Config{})
	c := dial(t, addr)

	mustWrite(t, c, encodeCmd("GET", "k"))
	<-entered // in flight, and it will not finish on its own

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := srv.Shutdown(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Shutdown hung %v; it must return at the deadline", elapsed)
	}
	close(release) // let the straggler goroutine unwind so it does not leak
}

// --- Config defaults (I1) -----------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	got := Config{}.defaults()
	if got.Addr != ":6379" {
		t.Errorf("Addr = %q, want :6379", got.Addr)
	}
	if got.MaxConns != 10000 {
		t.Errorf("MaxConns = %d, want 10000", got.MaxConns)
	}
	if got.DrainTimeout != 5*time.Second {
		t.Errorf("DrainTimeout = %v, want 5s", got.DrainTimeout)
	}
	if got.Version == "" {
		t.Error("Version default is empty")
	}
	if got.Now == nil {
		t.Error("Now default is nil")
	}
	if got.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want 0 (disabled)", got.IdleTimeout)
	}

	custom := Config{
		Addr:         "127.0.0.1:7000",
		IdleTimeout:  time.Minute,
		MaxConns:     5,
		DrainTimeout: time.Second,
		Version:      "9.9.9",
		Now:          time.Now,
	}.defaults()
	if custom.Addr != "127.0.0.1:7000" || custom.MaxConns != 5 ||
		custom.DrainTimeout != time.Second || custom.Version != "9.9.9" ||
		custom.IdleTimeout != time.Minute {
		t.Errorf("defaults overrode explicit config: %+v", custom)
	}
}

// compile-time guard: the test fakes really satisfy the published engine contract.
var (
	_ cache.Engine = (*blockingEngine)(nil)
	_ cache.Engine = errEngine{}
)

// TestShutdownBeforeServeReturnsClosed guards the bind-vs-shutdown ordering: if
// Shutdown runs before Serve binds the listener, Serve MUST refuse to accept and
// return ErrServerClosed (spec Req 8: no new connection after shutdown begins).
// Without synchronizing the listener + the shutdown flag, Serve would bind and
// Accept() forever, leaving the server up after a "completed" Shutdown.
func TestShutdownBeforeServeReturnsClosed(t *testing.T) {
	t.Parallel()
	srv := New(cache.New(), Config{})
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Serve: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("Serve after Shutdown = %v, want ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown-before-Serve (listener left open)")
	}
}

// --- Requirement 9: resource guards (I5 integration) --------------------------

func TestIdleTimeoutRealSocketClosesPromptly(t *testing.T) {
	_, addr := startServer(t, cache.New(), Config{IdleTimeout: 40 * time.Millisecond})
	c := dial(t, addr)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 1)
	start := time.Now()
	_, err := c.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the server to close the idle connection")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("idle close took %v; want it driven by the ~40ms server timeout", elapsed)
	}
}

func TestMaxConnsRejectsBeyondCapAndFreesOnClose(t *testing.T) {
	_, addr := startServer(t, cache.New(), Config{MaxConns: 1})

	c1 := dial(t, addr)
	mustWrite(t, c1, encodeCmd("PING"))
	wantReply(t, c1, "+PONG\r\n") // c1 holds the only slot

	c2, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial c2: %v", err)
	}
	defer func() { _ = c2.Close() }()
	_ = c2.SetDeadline(time.Now().Add(2 * time.Second))
	wantReply(t, c2, "-ERR max number of clients reached\r\n")
	expectClosed(t, c2)

	// The existing connection keeps serving while the cap is full.
	mustWrite(t, c1, encodeCmd("PING"))
	wantReply(t, c1, "+PONG\r\n")

	// Closing c1 frees its slot; a fresh connection is then served.
	_ = c1.Close()
	served := false
	for i := 0; i < 100; i++ {
		c3, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = c3.SetDeadline(time.Now().Add(time.Second))
		if _, werr := c3.Write(encodeCmd("PING")); werr == nil && expect(c3, "+PONG\r\n") == nil {
			served = true
			_ = c3.Close()
			break
		}
		_ = c3.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if !served {
		t.Fatal("a slot did not free after the holding connection closed")
	}
}
