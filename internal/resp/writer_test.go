package resp_test

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// failWriter fails every Write once armed, surfacing the sticky-error contract
// through Flush. bufio buffers small writes, so the error appears at Flush time.
type failWriter struct {
	err error
}

func (f *failWriter) Write(_ []byte) (int, error) {
	return 0, f.err
}

// ew is an error-eliding wrapper used by the golden tables: each method drives the
// underlying sticky Writer and discards the per-call error, which the harness
// re-checks once at Flush (the §7.1 sticky-error model).
type ew struct{ w *resp.Writer }

func (e ew) setProto(p int)    { e.w.SetProto(p) }
func (e ew) simple(s string)   { _ = e.w.WriteSimpleString(s) }
func (e ew) fail(s string)     { _ = e.w.WriteError(s) }
func (e ew) integer(n int64)   { _ = e.w.WriteInt(n) }
func (e ew) bulk(b []byte)     { _ = e.w.WriteBulk(b) }
func (e ew) bulkStr(s string)  { _ = e.w.WriteBulkString(s) }
func (e ew) null()             { _ = e.w.WriteNull() }
func (e ew) array(n int)       { _ = e.w.WriteArrayHeader(n) }
func (e ew) mapHdr(n int)      { _ = e.w.WriteMapHeader(n) }
func (e ew) boolean(b bool)    { _ = e.w.WriteBool(b) }
func (e ew) double(f float64)  { _ = e.w.WriteDouble(f) }
func (e ew) bignum(x *big.Int) { _ = e.w.WriteBigNumber(x) }

// encode runs fn against a fresh Writer over a buffer and returns the flushed
// bytes. proto 0 leaves the default RESP2; any other value calls SetProto.
func encode(t *testing.T, proto int, fn func(e ew)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := resp.NewWriter(&buf)
	if proto != 0 {
		w.SetProto(proto)
	}
	fn(ew{w})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	return buf.Bytes()
}

// ---- Requirement: RESP2 reply encoding (byte-exact) -------------------------

func TestRESP2GoldenTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		emit func(e ew)
	}{
		{"simple string", "+OK\r\n", func(e ew) { e.simple("OK") }},
		{"error", "-ERR bad request\r\n", func(e ew) { e.fail("ERR bad request") }},
		{"integer", ":1000\r\n", func(e ew) { e.integer(1000) }},
		{"bulk", "$5\r\nhello\r\n", func(e ew) { e.bulk([]byte("hello")) }},
		{"bulk string", "$5\r\nhello\r\n", func(e ew) { e.bulkStr("hello") }},
		{"empty bulk", "$0\r\n\r\n", func(e ew) { e.bulk([]byte{}) }},
		{"null", "$-1\r\n", func(e ew) { e.null() }},
		{
			"nested array",
			"*2\r\n*2\r\n:1\r\n:2\r\n$1\r\nx\r\n",
			func(e ew) {
				e.array(2)
				e.array(2)
				e.integer(1)
				e.integer(2)
				e.bulk([]byte("x"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encode(t, 2, tc.emit)
			if !bytes.Equal([]byte(tc.want), got) {
				t.Fatalf("bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRESP2EmptyVsNullBulkDistinct(t *testing.T) {
	t.Parallel()
	empty := encode(t, 2, func(e ew) { e.bulk([]byte{}) })
	null := encode(t, 2, func(e ew) { e.null() })
	if bytes.Equal(empty, null) {
		t.Fatalf("empty bulk and null must differ; both = %q", empty)
	}
	if !bytes.Equal(empty, []byte("$0\r\n\r\n")) {
		t.Fatalf("empty bulk = %q, want %q", empty, "$0\r\n\r\n")
	}
	if !bytes.Equal(null, []byte("$-1\r\n")) {
		t.Fatalf("null = %q, want %q", null, "$-1\r\n")
	}
}

// Triangulation: edge scalars and structural depth.
func TestRESP2Triangulation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
		emit func(e ew)
	}{
		{"empty simple string", "+\r\n", func(e ew) { e.simple("") }},
		{"zero int", ":0\r\n", func(e ew) { e.integer(0) }},
		{"negative int", ":-42\r\n", func(e ew) { e.integer(-42) }},
		{"min int64", ":-9223372036854775808\r\n", func(e ew) { e.integer(math.MinInt64) }},
		{"max int64", ":9223372036854775807\r\n", func(e ew) { e.integer(math.MaxInt64) }},
		{
			"deeply nested",
			"*1\r\n*1\r\n*1\r\n:7\r\n",
			func(e ew) {
				e.array(1)
				e.array(1)
				e.array(1)
				e.integer(7)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encode(t, 2, tc.emit)
			if !bytes.Equal([]byte(tc.want), got) {
				t.Fatalf("bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

// The sticky-error contract: the first failed write short-circuits the rest and
// the error is surfaced at Flush; subsequent writes return the same error.
func TestWriterStickyErrorContract(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom: writer failed")
	w := resp.NewWriter(&failWriter{err: sentinel})

	if err := w.WriteSimpleString("OK"); err != nil {
		t.Fatalf("buffered write should not surface the error yet: %v", err)
	}
	if err := w.Flush(); !errors.Is(err, sentinel) {
		t.Fatalf("Flush() error = %v, want sentinel", err)
	}
	// After the sticky error, further writes are no-ops returning the same error.
	if err := w.WriteInt(1); !errors.Is(err, sentinel) {
		t.Fatalf("post-error WriteInt error = %v, want sentinel", err)
	}
	if err := w.WriteBulk([]byte("x")); !errors.Is(err, sentinel) {
		t.Fatalf("post-error WriteBulk error = %v, want sentinel", err)
	}
	if err := w.WriteBulkString("x"); !errors.Is(err, sentinel) {
		t.Fatalf("post-error WriteBulkString error = %v, want sentinel", err)
	}
	if err := w.Flush(); !errors.Is(err, sentinel) {
		t.Fatalf("post-error Flush error = %v, want sentinel", err)
	}
}
