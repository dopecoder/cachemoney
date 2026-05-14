package resp

import (
	"bufio"
	"io"
	"math"
	"math/big"
	"strconv"
)

// scratchLen sizes the fixed per-Writer scratch buffer used to format integer and
// float scalars without intermediate allocation. The widest formatted scalar is a
// 64-bit int ("-9223372036854775808", 20 bytes) or a shortest-round-trip float
// ("-1.7976931348623157e+308", 24 bytes); 32 bytes leaves headroom.
const scratchLen = 32

// Writer encodes RESP replies into byte-exact RESP2 or RESP3 frames. It owns an
// internal bufio.Writer and carries a per-connection protocol version (2 or 3,
// default 2) selected with SetProto. Aggregate replies are written as a header
// followed by the caller's child writes, so the caller drives nesting with no
// intermediate value tree.
//
// The error model is sticky: the first failed write is recorded and short-circuits
// every later write; the recorded error is surfaced at Flush. A Writer is not safe
// for concurrent use; see the package doc for the single-goroutine contract.
type Writer struct {
	bw      *bufio.Writer
	err     error
	proto   int
	scratch [scratchLen]byte
}

// NewWriter wraps w with internal buffering. The protocol version defaults to
// RESP2; call SetProto(3) to switch a connection to RESP3.
func NewWriter(w io.Writer) *Writer {
	return &Writer{bw: bufio.NewWriter(w), proto: 2}
}

// SetProto sets the per-connection dialect. Values are clamped to {2, 3}: anything
// below 2 becomes RESP2 and anything above 3 becomes RESP3. Change #3 calls this
// once after parsing HELLO.
func (w *Writer) SetProto(proto int) {
	switch {
	case proto < 2:
		proto = 2
	case proto > 3:
		proto = 3
	}
	w.proto = proto
}

// Flush pushes the buffered bytes to the wrapped writer and returns the sticky
// error (the first failure seen on this Writer, if any). Change #3 controls flush
// timing to coalesce a pipelined batch of replies.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	w.err = w.bw.Flush()
	return w.err
}

// --- low-level sticky-error primitives --------------------------------------

func (w *Writer) writeString(s string) {
	if w.err != nil {
		return
	}
	_, w.err = w.bw.WriteString(s)
}

func (w *Writer) writeByte(b byte) {
	if w.err != nil {
		return
	}
	w.err = w.bw.WriteByte(b)
}

func (w *Writer) writeBytes(b []byte) {
	if w.err != nil {
		return
	}
	_, w.err = w.bw.Write(b)
}

// writeNumber formats n into the fixed scratch and writes it with no intermediate
// allocation.
func (w *Writer) writeNumber(n int64) {
	if w.err != nil {
		return
	}
	w.writeBytes(strconv.AppendInt(w.scratch[:0], n, 10))
}

// writeLenPrefixed emits "<prefix><len>\r\n<body>\r\n" — the bulk-string framing
// reused for the RESP2 downgrades of RESP3 scalars.
func (w *Writer) writeLenPrefixed(prefix byte, body []byte) {
	w.writeByte(prefix)
	w.writeNumber(int64(len(body)))
	w.writeString("\r\n")
	w.writeBytes(body)
	w.writeString("\r\n")
}

// --- RESP2 + shared reply writers -------------------------------------------

// WriteSimpleString writes a simple-string reply ("+<s>\r\n"). The caller must not
// pass a string containing CR or LF (the codec holds no semantics and does not
// sanitize); RESP forbids them in this frame.
func (w *Writer) WriteSimpleString(s string) error {
	w.writeByte('+')
	w.writeString(s)
	w.writeString("\r\n")
	return w.err
}

// WriteError writes an error reply ("-<msg>\r\n"). As with simple strings, msg must
// not contain CR or LF.
func (w *Writer) WriteError(msg string) error {
	w.writeByte('-')
	w.writeString(msg)
	w.writeString("\r\n")
	return w.err
}

// WriteInt writes an integer reply (":<n>\r\n").
func (w *Writer) WriteInt(n int64) error {
	w.writeByte(':')
	w.writeNumber(n)
	w.writeString("\r\n")
	return w.err
}

// WriteBulk writes a bulk-string reply ("$<len>\r\n<b>\r\n"). An empty slice encodes
// as "$0\r\n\r\n", which is distinct from WriteNull.
func (w *Writer) WriteBulk(b []byte) error {
	w.writeByte('$')
	w.writeNumber(int64(len(b)))
	w.writeString("\r\n")
	w.writeBytes(b)
	w.writeString("\r\n")
	return w.err
}

// WriteBulkString is the string-typed twin of WriteBulk.
func (w *Writer) WriteBulkString(s string) error {
	w.writeByte('$')
	w.writeNumber(int64(len(s)))
	w.writeString("\r\n")
	w.writeString(s)
	w.writeString("\r\n")
	return w.err
}

// WriteNull writes a null reply. The bytes are dialect-dependent: "$-1\r\n" under
// RESP2, "_\r\n" under RESP3.
func (w *Writer) WriteNull() error {
	if w.proto >= 3 {
		w.writeString("_\r\n")
	} else {
		w.writeString("$-1\r\n")
	}
	return w.err
}

// WriteArrayHeader writes an array header ("*<n>\r\n"). The caller then emits
// exactly n child replies; the writer is a flat emitter, not a validator.
func (w *Writer) WriteArrayHeader(n int) error {
	w.writeByte('*')
	w.writeNumber(int64(n))
	w.writeString("\r\n")
	return w.err
}

// --- RESP3 types + version-selected downgrades ------------------------------
//
// The deferred RESP3 aggregates and scalars — set (~), verbatim string (=), push
// (>) and attribute (|) — slot in here as additional Write* methods with no change
// to anything existing; that is the structured slot the proposal (C4) reserves.

// WriteMapHeader writes a map header. Under RESP3 it is "%<n>\r\n"; under RESP2 it
// downgrades to a flat array of 2n elements ("*<2n>\r\n"), so the same logical map
// is expressible under either dialect. The caller emits n key/value child pairs.
func (w *Writer) WriteMapHeader(n int) error {
	if w.proto >= 3 {
		w.writeByte('%')
		w.writeNumber(int64(n))
	} else {
		w.writeByte('*')
		w.writeNumber(int64(2 * n))
	}
	w.writeString("\r\n")
	return w.err
}

// WriteBool writes a boolean reply. Under RESP3 it is "#t\r\n"/"#f\r\n"; under RESP2
// it downgrades to the integer ":1\r\n"/":0\r\n".
func (w *Writer) WriteBool(b bool) error {
	switch {
	case w.proto >= 3 && b:
		w.writeString("#t\r\n")
	case w.proto >= 3:
		w.writeString("#f\r\n")
	case b:
		w.writeString(":1\r\n")
	default:
		w.writeString(":0\r\n")
	}
	return w.err
}

// WriteDouble writes a double reply. Under RESP3 it is ",<value>\r\n" with the
// shortest round-trippable formatting (and the special tokens ",inf", ",-inf",
// ",nan"); under RESP2 it downgrades to a bulk string carrying the same text.
func (w *Writer) WriteDouble(f float64) error {
	var buf [scratchLen]byte
	body := appendDouble(buf[:0], f)
	if w.proto >= 3 {
		w.writeByte(',')
		w.writeBytes(body)
		w.writeString("\r\n")
	} else {
		w.writeLenPrefixed('$', body)
	}
	return w.err
}

// WriteBigNumber writes an arbitrary-precision integer reply. Under RESP3 it is
// "(<digits>\r\n"; under RESP2 it downgrades to a bulk string carrying the same
// decimal text.
func (w *Writer) WriteBigNumber(x *big.Int) error {
	body := x.Append(w.scratch[:0], 10)
	if w.proto >= 3 {
		w.writeByte('(')
		w.writeBytes(body)
		w.writeString("\r\n")
	} else {
		// body aliases w.scratch, which writeLenPrefixed reuses for the length —
		// copy to an independent buffer first.
		buf := append([]byte(nil), body...)
		w.writeLenPrefixed('$', buf)
	}
	return w.err
}

// appendDouble formats f for a RESP3 double, handling the non-finite special
// tokens before delegating to strconv's shortest round-trip 'g' formatting.
func appendDouble(dst []byte, f float64) []byte {
	switch {
	case math.IsInf(f, 1):
		return append(dst, "inf"...)
	case math.IsInf(f, -1):
		return append(dst, "-inf"...)
	case math.IsNaN(f):
		return append(dst, "nan"...)
	default:
		return strconv.AppendFloat(dst, f, 'g', -1, 64)
	}
}
