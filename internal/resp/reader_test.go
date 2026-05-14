package resp_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// ---- shared test helpers (used by reader/limits/errors/fuzz tests) ----------

// segReader returns bytes from a list of segments, one segment at a time: a
// single Read never spans two segments, modelling "the first read delivers X, a
// later read delivers Y". It is the split-resumption and pipelining oracle.
type segReader struct {
	segs [][]byte
}

func (s *segReader) Read(p []byte) (int, error) {
	for len(s.segs) > 0 && len(s.segs[0]) == 0 {
		s.segs = s.segs[1:]
	}
	if len(s.segs) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.segs[0])
	s.segs[0] = s.segs[0][n:]
	return n, nil
}

// chunkedReader returns at most chunk bytes per Read, then io.EOF. With chunk==1
// it exercises maximal resumption (one byte per underlying Read).
type chunkedReader struct {
	data  []byte
	pos   int
	chunk int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.chunk
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

// errReader yields prefix verbatim, then returns err (a transport failure) once
// the prefix is drained. With an empty prefix it fails on the first Read.
type errReader struct {
	prefix []byte
	pos    int
	err    error
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.pos < len(e.prefix) {
		n := copy(p, e.prefix[e.pos:])
		e.pos += n
		return n, nil
	}
	return 0, e.err
}

// deepCopyArgs clones an argument vector so a later decode cannot mutate the
// retained oracle (the copy-out no-corruption check).
func deepCopyArgs(args [][]byte) [][]byte {
	out := make([][]byte, len(args))
	for i, a := range args {
		out[i] = append([]byte(nil), a...)
	}
	return out
}

func toArgs(ss ...string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}

// ---- Requirement: Streaming command decode ----------------------------------

func TestReadCommandGoldenFraming(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  [][]byte
	}{
		{"single arg", "*1\r\n$4\r\nPING\r\n", toArgs("PING")},
		{
			"multi arg",
			"*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n$2\r\n30\r\n",
			toArgs("SET", "k", "v", "EX", "30"),
		},
		{"zero-length bulk", "*1\r\n$0\r\n\r\n", [][]byte{{}}},
		{"empty array", "*0\r\n", [][]byte{}},
		{"binary payload", "*1\r\n$3\r\n\x00\xff\x0a\r\n", [][]byte{{0x00, 0xff, 0x0a}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := resp.NewReader(strings.NewReader(tc.input))
			got, err := r.ReadCommand()
			if err != nil {
				t.Fatalf("ReadCommand() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("decoded args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReadCommandZeroLengthBulkIsEmptyToken(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(strings.NewReader("*1\r\n$0\r\n\r\n"))
	got, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("ReadCommand() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(args) = %d, want 1", len(got))
	}
	if len(got[0]) != 0 {
		t.Fatalf("len(args[0]) = %d, want 0", len(got[0]))
	}
}

func TestReadCommandLargeArgumentCount(t *testing.T) {
	t.Parallel()
	const n = 1000
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", n)
	for i := 0; i < n; i++ {
		b.WriteString("$1\r\nx\r\n")
	}
	r := resp.NewReader(&b)
	got, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("ReadCommand() error = %v", err)
	}
	if len(got) != n {
		t.Fatalf("len(args) = %d, want %d", len(got), n)
	}
}

func TestReadCommandNonArrayRejected(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(strings.NewReader("PING\r\n"))
	got, err := r.ReadCommand()
	if got != nil {
		t.Fatalf("args = %v, want nil", got)
	}
	var pe *resp.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want *resp.ProtocolError", err)
	}
	if pe.Kind != resp.KindExpectedArray {
		t.Fatalf("Kind = %v, want KindExpectedArray", pe.Kind)
	}
}

// Split-at-every-offset: feeding any frame in two halves at every interior
// boundary must yield the same command (subsumes mid-length and mid-payload).
func TestReadCommandSplitAtEveryOffset(t *testing.T) {
	t.Parallel()
	frames := []struct {
		input string
		want  [][]byte
	}{
		{"*1\r\n$4\r\nPING\r\n", toArgs("PING")},
		{"*2\r\n$4\r\nECHO\r\n$2\r\nhi\r\n", toArgs("ECHO", "hi")},
		{"*1\r\n$0\r\n\r\n", [][]byte{{}}},
	}
	for _, f := range frames {
		frame := []byte(f.input)
		for k := 1; k < len(frame); k++ {
			r := resp.NewReader(&segReader{segs: [][]byte{
				append([]byte(nil), frame[:k]...),
				append([]byte(nil), frame[k:]...),
			}})
			got, err := r.ReadCommand()
			if err != nil {
				t.Fatalf("split %q at %d: error = %v", f.input, k, err)
			}
			if diff := cmp.Diff(f.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Fatalf("split %q at %d mismatch (-want +got):\n%s", f.input, k, diff)
			}
		}
	}
}

func TestReadCommandSplitOneBytePerRead(t *testing.T) {
	t.Parallel()
	frame := "*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n$2\r\n30\r\n"
	r := resp.NewReader(&chunkedReader{data: []byte(frame), chunk: 1})
	got, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("ReadCommand() error = %v", err)
	}
	if diff := cmp.Diff(toArgs("SET", "k", "v", "EX", "30"), got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

// ---- Requirement: Pipelining ------------------------------------------------

func TestPipelineTwoBackToBack(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(strings.NewReader("*1\r\n$4\r\nPING\r\n*2\r\n$4\r\nECHO\r\n$2\r\nhi\r\n"))
	first, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("first ReadCommand() error = %v", err)
	}
	if diff := cmp.Diff(toArgs("PING"), first); diff != "" {
		t.Fatalf("first mismatch (-want +got):\n%s", diff)
	}
	second, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("second ReadCommand() error = %v", err)
	}
	if diff := cmp.Diff(toArgs("ECHO", "hi"), second); diff != "" {
		t.Fatalf("second mismatch (-want +got):\n%s", diff)
	}
}

func TestPipelineNCommandsNoneLost(t *testing.T) {
	t.Parallel()
	const n = 100
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.WriteString("*1\r\n$4\r\nPING\r\n")
	}
	r := resp.NewReader(&b)
	for i := 0; i < n; i++ {
		got, err := r.ReadCommand()
		if err != nil {
			t.Fatalf("command %d: error = %v", i, err)
		}
		if diff := cmp.Diff(toArgs("PING"), got); diff != "" {
			t.Fatalf("command %d mismatch (-want +got):\n%s", i, diff)
		}
	}
	if _, err := r.ReadCommand(); !errors.Is(err, io.EOF) {
		t.Fatalf("after %d commands, error = %v, want io.EOF", n, err)
	}
}

func TestPipelineTrailingPartialResumes(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(&segReader{segs: [][]byte{
		[]byte("*1\r\n$4\r\nPING\r\n*2\r\n$4\r\nECHO\r\n$2\r\nh"),
		[]byte("i\r\n"),
	}})
	first, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("first ReadCommand() error = %v", err)
	}
	if diff := cmp.Diff(toArgs("PING"), first); diff != "" {
		t.Fatalf("first mismatch (-want +got):\n%s", diff)
	}
	second, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("second ReadCommand() error = %v", err)
	}
	if diff := cmp.Diff(toArgs("ECHO", "hi"), second); diff != "" {
		t.Fatalf("second mismatch (-want +got):\n%s", diff)
	}
}

// Copy-out no-corruption: a retained command's tokens stay byte-for-byte equal
// across two subsequent reads (the §6 copy-out contract).
func TestPipelineCopyOutNoCorruption(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(strings.NewReader(
		"*1\r\n$4\r\nPING\r\n*2\r\n$4\r\nECHO\r\n$2\r\nhi\r\n*1\r\n$3\r\nGET\r\n",
	))
	first, err := r.ReadCommand()
	if err != nil {
		t.Fatalf("first ReadCommand() error = %v", err)
	}
	oracle := deepCopyArgs(first)

	if _, err := r.ReadCommand(); err != nil {
		t.Fatalf("second ReadCommand() error = %v", err)
	}
	if _, err := r.ReadCommand(); err != nil {
		t.Fatalf("third ReadCommand() error = %v", err)
	}
	if diff := cmp.Diff(oracle, first); diff != "" {
		t.Fatalf("retained tokens corrupted after later reads (-oracle +retained):\n%s", diff)
	}
}

func TestReadCommandDrainedBufferReturnsEOF(t *testing.T) {
	t.Parallel()
	r := resp.NewReader(strings.NewReader("*1\r\n$4\r\nPING\r\n"))
	if _, err := r.ReadCommand(); err != nil {
		t.Fatalf("ReadCommand() error = %v", err)
	}
	if _, err := r.ReadCommand(); !errors.Is(err, io.EOF) {
		t.Fatalf("drained buffer error = %v, want io.EOF", err)
	}
}
