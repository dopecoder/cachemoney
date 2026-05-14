package resp_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// ---- Requirement: Typed, distinguishable error surface ----------------------

func TestProtocolErrorKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		kind  resp.ErrKind
	}{
		{"invalid type byte", "*1\r\n:5\r\n", resp.KindExpectedBulk},
		{"non-numeric bulk length", "*1\r\n$xy\r\n", resp.KindBadLength},
		{"non-numeric multibulk length", "*xy\r\n", resp.KindBadLength},
		{"empty multibulk length", "*\r\n", resp.KindBadLength},
		{"overflowing bulk length", "*1\r\n$99999999999999999999\r\n", resp.KindBadLength},
		{"negative bulk length", "*1\r\n$-1\r\n", resp.KindBadLength},
		{"negative multibulk length", "*-1\r\n", resp.KindBadLength},
		{"lone minus length", "*1\r\n$-\r\n", resp.KindBadLength},
		{"negative-zero bulk length", "*1\r\n$-0\r\n", resp.KindBadLength},
		{"negative-zero multibulk length", "*-0\r\n", resp.KindBadLength},
		{"leading-zero bulk length", "*1\r\n$007\r\n1234567\r\n", resp.KindBadLength},
		{"leading-zero multibulk length", "*007\r\n", resp.KindBadLength},
		{"missing CRLF terminator", "*1\r\n$4\r\nPINGXX", resp.KindMissingCRLF},
		{"missing CRLF on header", "*1\n", resp.KindMissingCRLF},
		{"empty header", "\r\n", resp.KindExpectedArray},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := resp.NewReader(strings.NewReader(tc.input))
			_, err := r.ReadCommand()
			var pe *resp.ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v, want *resp.ProtocolError", err)
			}
			if pe.Kind != tc.kind {
				t.Fatalf("Kind = %v, want %v", pe.Kind, tc.kind)
			}
			if pe.Error() == "" {
				t.Fatalf("Error() is empty for kind %v", pe.Kind)
			}
			// A protocol error must be distinguishable from EOF and transport.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("protocol error %v should not satisfy errors.Is(io.EOF/ErrUnexpectedEOF)", err)
			}
		})
	}
}

func TestHeaderLineTooLong(t *testing.T) {
	t.Parallel()
	// Path 1: the line fits in the buffer but exceeds the header-length cap.
	long := "*" + strings.Repeat("0", 80) + "\r\n"
	r := resp.NewReader(strings.NewReader(long))
	_, err := r.ReadCommand()
	var pe *resp.ProtocolError
	if !errors.As(err, &pe) || pe.Kind != resp.KindLineTooLong {
		t.Fatalf("over-cap header: error = %v, want KindLineTooLong", err)
	}

	// Path 2: the line overflows a tiny buffer (bufio.ErrBufferFull).
	r2 := resp.NewReader(strings.NewReader("*0000000000000000000000\r\n"), resp.WithBufferSize(16))
	_, err = r2.ReadCommand()
	if !errors.As(err, &pe) || pe.Kind != resp.KindLineTooLong {
		t.Fatalf("buffer-overflow header: error = %v, want KindLineTooLong", err)
	}
}

func TestCleanEOFDistinguishable(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(strings.NewReader(""))
	_, err := r.ReadCommand()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want io.EOF", err)
	}
	var pe *resp.ProtocolError
	if errors.As(err, &pe) {
		t.Fatalf("clean EOF should not be a *resp.ProtocolError")
	}
}

func TestTruncatedMidFrameIsUnexpectedEOF(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"truncated header":      "*1",
		"truncated elem header": "*1\r\n$4",
		"truncated payload":     "*1\r\n$4\r\nPI",
		"missing trailing crlf": "*1\r\n$4\r\nPING",
		"partial trailing cr":   "*1\r\n$4\r\nPING\r",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := resp.NewReader(strings.NewReader(input))
			_, err := r.ReadCommand()
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("truncation %q must be distinguishable from a clean io.EOF", input)
			}
		})
	}
}

// Transport errors propagate verbatim — never wrapped in *ProtocolError — at both
// the first header read and a mid-frame read.
func TestTransportErrorPropagatedVerbatim(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom: transport failed")

	t.Run("first read", func(t *testing.T) {
		t.Parallel()
		r := resp.NewReader(&errReader{err: sentinel})
		_, err := r.ReadCommand()
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want sentinel", err)
		}
		var pe *resp.ProtocolError
		if errors.As(err, &pe) {
			t.Fatalf("transport error must not be a *resp.ProtocolError")
		}
	})

	t.Run("mid-frame read", func(t *testing.T) {
		t.Parallel()
		r := resp.NewReader(&errReader{prefix: []byte("*1\r\n"), err: sentinel})
		_, err := r.ReadCommand()
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want sentinel", err)
		}
	})

	t.Run("deadline exceeded distinguishable", func(t *testing.T) {
		t.Parallel()
		r := resp.NewReader(&errReader{err: context.DeadlineExceeded})
		_, err := r.ReadCommand()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		var pe *resp.ProtocolError
		if errors.As(err, &pe) {
			t.Fatalf("deadline error must not be a *resp.ProtocolError")
		}
	})
}
