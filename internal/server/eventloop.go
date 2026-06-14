package server

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/panjf2000/gnet/v2"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// This file is a SPIKE: an alternative networking backend that runs the server on a
// gnet epoll event loop instead of goroutine-per-connection, to measure the tail-latency
// win. It reuses the engine, dispatch, command handlers, and resp.Writer unchanged; the
// only event-loop-specific piece is the incremental RESP parser below (the blocking
// resp.Reader cannot drive a non-blocking event loop). Selected via CM_NET=gnet.

const evMaxHeaderLine = 64 // matches resp's internal header-line bound

// newConnWriter builds a conn whose writer targets w (the connection's outbound), so the
// existing transport-agnostic handlers and dispatch run unchanged with no intermediate
// response buffer.
func (s *Server) newConnWriter(w io.Writer) *conn {
	return &conn{
		srv:   s,
		w:     resp.NewWriter(w),
		proto: 2,
		id:    s.nextID.Add(1),
	}
}

// gnetWriter adapts a gnet.Conn to io.Writer: resp.Writer's bufio flushes straight into
// gnet's outbound buffer (one copy, no bytes.Buffer hop).
type gnetWriter struct{ gc gnet.Conn }

func (g gnetWriter) Write(p []byte) (int, error) { return g.gc.Write(p) }

// ListenAndServeGnet serves on a gnet event loop. Spike-grade: it omits the std server's
// graceful drain and idle timeout. CM_GNET_LOOPS overrides the event-loop count;
// CM_GNET_ETRIGGER=1 enables edge-triggered I/O. TCP_NODELAY is on by gnet default.
func (s *Server) ListenAndServeGnet() error {
	h := &evHandler{srv: s}
	opts := []gnet.Option{
		gnet.WithMulticore(true),
		gnet.WithReuseAddr(true),
		// SO_REUSEPORT: each event loop gets its own listener socket and the kernel
		// distributes connections across them by 4-tuple hash — removes the single-acceptor
		// bottleneck and scales with cores (research: "linear throughput scaling").
		gnet.WithReusePort(os.Getenv("CM_GNET_NOREUSE") != "1"),
		// Pin each event loop to an OS thread for cache locality / no thread migration.
		gnet.WithLockOSThread(os.Getenv("CM_GNET_NOLOCK") != "1"),
		// Sized for the 4 KiB hit-ratio workload: fewer read/write syscalls per request.
		gnet.WithReadBufferCap(64 * 1024),
		gnet.WithWriteBufferCap(64 * 1024),
		gnet.WithSocketRecvBuffer(256 * 1024),
		gnet.WithSocketSendBuffer(256 * 1024),
	}
	if v := os.Getenv("CM_GNET_LOOPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts = append(opts, gnet.WithNumEventLoop(n))
		}
	}
	if os.Getenv("CM_GNET_ETRIGGER") == "1" {
		opts = append(opts, gnet.WithEdgeTriggeredIO(true))
	}
	return gnet.Run(h, "tcp://"+s.cfg.Addr, opts...)
}

type evHandler struct {
	gnet.BuiltinEventEngine
	srv *Server
}

// evConn is the per-connection state gnet carries in Conn.Context(). gnet binds a
// connection to a single event-loop goroutine for its lifetime, so this satisfies the
// codec's single-goroutine-per-conn contract. scratch is the reusable args slice.
type evConn struct {
	c       *conn
	scratch [][]byte
}

func (h *evHandler) OnOpen(gc gnet.Conn) ([]byte, gnet.Action) {
	gc.SetContext(&evConn{c: h.srv.newConnWriter(gnetWriter{gc})})
	return nil, gnet.None
}

// OnTraffic parses every complete command available, dispatches each (handlers write to
// ec.c.w, whose bufio flushes straight into gnet's outbound), then consumes the parsed
// bytes. A partial trailing frame stays in gnet's inbound buffer for the next call.
func (h *evHandler) OnTraffic(gc gnet.Conn) gnet.Action {
	ec, ok := gc.Context().(*evConn)
	if !ok {
		return gnet.Close
	}
	data, _ := gc.Peek(-1)
	consumed, stop := 0, false
	for {
		args, n, need, perr := parseCommand(data[consumed:], &ec.scratch)
		if perr != nil {
			_ = ec.c.w.WriteError("ERR Protocol error: " + perr.Error())
			stop = true
			break
		}
		if need {
			break
		}
		consumed += n
		// args alias the peek buffer; dispatch (and engine value clones) run synchronously
		// before Discard below, so the aliases stay valid.
		if h.srv.dispatch(ec.c, args) {
			stop = true
			break
		}
	}
	if consumed > 0 {
		_, _ = gc.Discard(consumed)
	}
	_ = ec.c.w.Flush()
	if stop {
		return gnet.Close
	}
	return gnet.None
}

// parseCommand parses one RESP array-of-bulk-strings command from b, appending the
// arguments (aliasing b) into *scratch (reused across calls to avoid per-command allocs).
// It returns the args, bytes consumed, need=true when b holds only a partial frame, or a
// protocol error for malformed input.
func parseCommand(b []byte, scratch *[][]byte) (args [][]byte, consumed int, need bool, perr error) {
	if len(b) == 0 {
		return nil, 0, true, nil
	}
	if b[0] != '*' {
		return nil, 0, false, fmt.Errorf("expected '*', got %q", b[0])
	}
	nl := indexCRLF(b)
	if nl < 0 {
		if len(b) > evMaxHeaderLine {
			return nil, 0, false, fmt.Errorf("multibulk header too long")
		}
		return nil, 0, true, nil
	}
	count, err := strconv.Atoi(string(b[1:nl]))
	if err != nil || count < 0 || count > resp.DefaultMaxMultibulkLen {
		return nil, 0, false, fmt.Errorf("invalid multibulk count")
	}
	i := nl + 2
	out := (*scratch)[:0]
	for e := 0; e < count; e++ {
		if i >= len(b) {
			return nil, 0, true, nil
		}
		if b[i] != '$' {
			return nil, 0, false, fmt.Errorf("expected '$', got %q", b[i])
		}
		rel := indexCRLF(b[i:])
		if rel < 0 {
			if len(b)-i > evMaxHeaderLine {
				return nil, 0, false, fmt.Errorf("bulk header too long")
			}
			return nil, 0, true, nil
		}
		blen, err := strconv.Atoi(string(b[i+1 : i+rel]))
		if err != nil || blen < 0 || blen > resp.DefaultMaxBulkLen {
			return nil, 0, false, fmt.Errorf("invalid bulk length")
		}
		i += rel + 2
		if i+blen+2 > len(b) {
			return nil, 0, true, nil
		}
		if b[i+blen] != '\r' || b[i+blen+1] != '\n' {
			return nil, 0, false, fmt.Errorf("bulk not CRLF-terminated")
		}
		out = append(out, b[i:i+blen])
		i += blen + 2
	}
	*scratch = out
	return out, i, false, nil
}

func indexCRLF(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\r' && b[i+1] == '\n' {
			return i
		}
	}
	return -1
}
