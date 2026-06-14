package server

import (
	"bytes"
	"testing"
)

// TestParseCommand covers the incremental RESP parser that drives the gnet event-loop
// backend: complete frames, partial frames (need=true), pipelining, scratch reuse, and
// malformed input (protocol error).
func TestParseCommand(t *testing.T) {
	t.Parallel()

	var scratch [][]byte

	t.Run("complete GET", func(t *testing.T) {
		b := []byte("*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n")
		args, n, need, err := parseCommand(b, &scratch)
		if err != nil || need {
			t.Fatalf("err=%v need=%v, want a complete frame", err, need)
		}
		if n != len(b) {
			t.Fatalf("consumed %d, want %d", n, len(b))
		}
		if len(args) != 2 || string(args[0]) != "GET" || string(args[1]) != "foo" {
			t.Fatalf("args=%q, want [GET foo]", args)
		}
	})

	t.Run("complete SET", func(t *testing.T) {
		b := []byte("*3\r\n$3\r\nSET\r\n$1\r\na\r\n$1\r\nb\r\n")
		args, n, need, err := parseCommand(b, &scratch)
		if err != nil || need || n != len(b) || len(args) != 3 {
			t.Fatalf("SET parse: args=%q n=%d need=%v err=%v", args, n, need, err)
		}
	})

	t.Run("partial frames need more", func(t *testing.T) {
		for _, b := range [][]byte{
			{},
			[]byte("*2\r\n"),
			[]byte("*2\r\n$3\r\nGE"),
			[]byte("*2\r\n$3\r\nGET\r\n$3\r\nfo"),
			[]byte("*2\r\n$3\r\nGET\r\n$3\r\nfoo\r"),
		} {
			_, _, need, err := parseCommand(b, &scratch)
			if err != nil || !need {
				t.Fatalf("%q: need=%v err=%v, want need=true no error", b, need, err)
			}
		}
	})

	t.Run("pipelined", func(t *testing.T) {
		first := []byte("*1\r\n$4\r\nPING\r\n")
		second := []byte("*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")
		b := append(append([]byte(nil), first...), second...)
		args, n, _, err := parseCommand(b, &scratch)
		if err != nil || n != len(first) || string(args[0]) != "PING" {
			t.Fatalf("first: args=%q n=%d err=%v", args, n, err)
		}
		args2, n2, _, err := parseCommand(b[n:], &scratch)
		if err != nil || n2 != len(second) || string(args2[0]) != "GET" || string(args2[1]) != "k" {
			t.Fatalf("second: args=%q n=%d err=%v", args2, n2, err)
		}
	})

	t.Run("malformed is a protocol error", func(t *testing.T) {
		for _, b := range [][]byte{
			[]byte("$3\r\nfoo\r\n"),       // not an array
			[]byte("*x\r\n"),              // bad multibulk count
			[]byte("*1\r\n#3\r\nfoo\r\n"), // element header not '$'
			[]byte("*1\r\n$x\r\nfoo\r\n"), // bad bulk length
			[]byte("*1\r\n$3\r\nfooXX"),   // payload not CRLF-terminated
		} {
			_, _, _, err := parseCommand(b, &scratch)
			if err == nil {
				t.Fatalf("%q: want a protocol error, got nil", b)
			}
		}
	})
}

// TestNewConnWriterFlushesToSink asserts the event-loop path's writer (a resp.Writer over
// an arbitrary io.Writer sink, here a bytes.Buffer standing in for the socket) writes
// through correctly — the reuse of the transport-agnostic handlers depends on it.
func TestNewConnWriterFlushesToSink(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	s := New(nil, Config{})
	c := s.newConnWriter(&buf)
	if err := c.w.WriteSimpleString("OK"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.w.Flush()
	if got := buf.String(); got != "+OK\r\n" {
		t.Fatalf("sink got %q, want +OK\\r\\n", got)
	}
}
