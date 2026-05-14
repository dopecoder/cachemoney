package resp

// ErrKind is the machine-readable classification carried by a ProtocolError. It
// lets a caller branch on the specific framing fault (via errors.As) without
// parsing the human-readable message.
type ErrKind uint8

const (
	// KindExpectedArray means the top-level frame did not begin with '*'.
	// Inline/telnet-style input is reported with this kind (arrays only, ADR-0007).
	KindExpectedArray ErrKind = iota
	// KindExpectedBulk means a command element did not begin with '$'.
	KindExpectedBulk
	// KindBadLength means a length field was non-numeric, negative, or overflowed.
	KindBadLength
	// KindMissingCRLF means a header line or bulk payload was not terminated by CRLF.
	KindMissingCRLF
	// KindBulkTooLong means a declared bulk length exceeded the configured maximum.
	KindBulkTooLong
	// KindMultibulkTooLong means a declared array length exceeded the configured maximum.
	KindMultibulkTooLong
	// KindLineTooLong means a header line exceeded the bounded length (a DoS guard).
	KindLineTooLong
)

// kindMessages holds the Redis-style human text for each ErrKind. It is indexed by
// the kind value; out-of-range kinds fall back via message.
var kindMessages = [...]string{
	KindExpectedArray:    "expected '*' array header",
	KindExpectedBulk:     "expected '$' bulk-string header",
	KindBadLength:        "invalid length",
	KindMissingCRLF:      "expected CRLF terminator",
	KindBulkTooLong:      "bulk length exceeds configured limit",
	KindMultibulkTooLong: "multibulk count exceeds configured limit",
	KindLineTooLong:      "header line exceeds limit",
}

// message returns the forwardable, Redis-style reason for the kind. Change #3
// embeds it in an "-ERR Protocol error: <msg>" reply.
func (k ErrKind) message() string {
	if int(k) < len(kindMessages) {
		return kindMessages[k]
	}
	return "malformed frame"
}

// ProtocolError reports malformed or over-limit input on the decode path. It is
// the typed half of the three-way ReadCommand return contract: a *ProtocolError
// means the bytes were malformed (matchable with errors.As), distinct from a clean
// io.EOF, a truncating io.ErrUnexpectedEOF, and a verbatim transport error.
type ProtocolError struct {
	// Kind is the machine-readable fault classification.
	Kind ErrKind
	// Msg is the human-readable, Redis-style reason ("Protocol error: <msg>").
	Msg string
}

// Error implements the error interface, rendering Redis-style protocol-error text.
func (e *ProtocolError) Error() string {
	return "resp: Protocol error: " + e.Msg
}

// newProtocolError builds a ProtocolError whose message is the canonical text for
// the kind.
func newProtocolError(kind ErrKind) *ProtocolError {
	return &ProtocolError{Kind: kind, Msg: kind.message()}
}
