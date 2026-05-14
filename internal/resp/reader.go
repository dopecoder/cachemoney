package resp

import (
	"bufio"
	"io"
)

// Type-byte prefixes for the request grammar (arrays of bulk strings only).
const (
	typeArray = '*'
	typeBulk  = '$'
)

// maxInt is the largest value of the platform int, used to reject length fields
// that would overflow during accumulation.
const maxInt = int(^uint(0) >> 1)

// Reader decodes RESP commands from a streaming byte source. It owns an internal
// bufio.Reader, so partial-read resumption and pipelining fall out for free: the
// buffer is the only resumption state. A Reader is not safe for concurrent use;
// see the package doc for the per-connection single-goroutine contract.
type Reader struct {
	br     *bufio.Reader
	limits Limits
}

// NewReader wraps r in a codec-owned buffered reader. Limits and buffer size are
// applied via options; omitted options resolve to the Redis-compatible defaults.
func NewReader(r io.Reader, opts ...Option) *Reader {
	cfg := resolveConfig(opts)
	return &Reader{
		br:     bufio.NewReaderSize(r, cfg.bufferSize),
		limits: cfg.limits,
	}
}

// ReadCommand reads exactly one RESP array of bulk strings and returns its
// arguments as caller-owned, byte-for-byte slices (the copy-out contract). The
// return value distinguishes four conditions:
//
//   - clean EOF at a command boundary -> (nil, io.EOF)
//   - stream ends mid-frame           -> (nil, io.ErrUnexpectedEOF)
//   - malformed or over-limit bytes   -> (nil, *ProtocolError)
//   - underlying transport error      -> (nil, that error, unwrapped)
//
// A zero-length array yields a non-nil, length-0 argument vector.
func (r *Reader) ReadCommand() ([][]byte, error) {
	header, err := r.readHeaderLine(true)
	if err != nil {
		return nil, err
	}
	if len(header) == 0 || header[0] != typeArray {
		return nil, newProtocolError(KindExpectedArray)
	}
	n, ok := parseInt(header[1:])
	if !ok {
		return nil, newProtocolError(KindBadLength)
	}
	if n > r.limits.MaxMultibulkLen {
		// Reject before make([][]byte, n): no element vector sized to n.
		return nil, newProtocolError(KindMultibulkTooLong)
	}
	args := make([][]byte, n)
	for i := 0; i < n; i++ {
		arg, argErr := r.readBulkArg()
		if argErr != nil {
			return nil, argErr
		}
		args[i] = arg
	}
	return args, nil
}

// readBulkArg reads one "$<len>\r\n<payload>\r\n" element, copying the payload
// into a freshly allocated, caller-owned slice.
func (r *Reader) readBulkArg() ([]byte, error) {
	header, err := r.readHeaderLine(false)
	if err != nil {
		return nil, err
	}
	if len(header) == 0 || header[0] != typeBulk {
		return nil, newProtocolError(KindExpectedBulk)
	}
	n, ok := parseInt(header[1:])
	if !ok {
		return nil, newProtocolError(KindBadLength)
	}
	if n > r.limits.MaxBulkLen {
		// Reject before make([]byte, n): no buffer sized to the declared length.
		return nil, newProtocolError(KindBulkTooLong)
	}
	buf := make([]byte, n) // the copy-out: io.ReadFull drains bytes out of bufio.
	if _, err := io.ReadFull(r.br, buf); err != nil {
		return nil, normalizeEOF(err)
	}
	var crlf [2]byte
	if _, err := io.ReadFull(r.br, crlf[:]); err != nil {
		return nil, normalizeEOF(err)
	}
	if crlf[0] != '\r' || crlf[1] != '\n' {
		return nil, newProtocolError(KindMissingCRLF)
	}
	return buf, nil
}

// readHeaderLine reads one CRLF-terminated header line and returns its content
// without the CRLF. The atBoundary flag distinguishes the first line of a fresh
// command (a clean io.EOF with no buffered bytes is a command boundary) from a
// mid-frame line (any EOF is truncation). Transport errors propagate verbatim.
func (r *Reader) readHeaderLine(atBoundary bool) ([]byte, error) {
	line, err := r.br.ReadSlice('\n')
	if err != nil {
		switch err {
		case bufio.ErrBufferFull:
			return nil, newProtocolError(KindLineTooLong)
		case io.EOF:
			if atBoundary && len(line) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
	return trimCRLF(line)
}

// trimCRLF validates the trailing CRLF and the header-line length cap, returning
// the content with the CRLF stripped. ReadSlice guarantees the final byte is '\n'.
func trimCRLF(line []byte) ([]byte, error) {
	if len(line) > maxHeaderLine {
		return nil, newProtocolError(KindLineTooLong)
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, newProtocolError(KindMissingCRLF)
	}
	return line[:len(line)-2], nil
}

// parseInt parses a non-negative, canonical base-10 integer from a RESP length
// field. A request array count or bulk length is never negative, so a sign is
// rejected outright, and leading zeros ("007", "-0", "00") are rejected as
// non-canonical for strict Redis parity (only a lone "0" starts with '0'). It also
// rejects empty input, stray non-digits, and values that would overflow int, and
// allocates nothing (it reads directly from the bufio-owned header slice).
func parseInt(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	// Canonical leading byte: '1'-'9', or a lone '0'. This rejects signs ('-'/'+'),
	// other non-digits, and any multi-digit run with a leading zero.
	if b[0] < '1' || b[0] > '9' {
		if len(b) != 1 || b[0] != '0' {
			return 0, false
		}
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int(c - '0')
		if n > (maxInt-d)/10 {
			return 0, false // overflow
		}
		n = n*10 + d
	}
	return n, true
}

// normalizeEOF maps a bare io.EOF observed mid-frame to io.ErrUnexpectedEOF and
// leaves every other error (including a transport error or io.ReadFull's own
// io.ErrUnexpectedEOF) untouched, so truncation is distinguishable from a clean
// boundary EOF and from a malformed frame.
func normalizeEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
