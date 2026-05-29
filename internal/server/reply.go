package server

import (
	"errors"
	"strings"
)

// errClientQuit is the sentinel a handler returns to ask the serve loop to close
// the connection after flushing the handler's reply. Only QUIT returns it.
var errClientQuit = errors.New("server: client quit")

// maxErrEchoLen bounds how much client-derived text is echoed back inside an -ERR
// reply, matching Redis's ~128-byte truncation of the offending command.
const maxErrEchoLen = 128

// sanitizeText makes client-derived bytes safe to embed in a single-line RESP error
// frame: it truncates to maxErrEchoLen and replaces control bytes (notably CR and
// LF) with '?', so a malicious command name cannot inject a second reply frame.
func sanitizeText(b []byte) string {
	if len(b) > maxErrEchoLen {
		b = b[:maxErrEchoLen]
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for _, ch := range b {
		if ch < 0x20 || ch == 0x7f {
			sb.WriteByte('?')
		} else {
			sb.WriteByte(ch)
		}
	}
	return sb.String()
}

// replyUnknownCommand writes the Redis-style unknown-command error. raw is the
// bytes the client sent for args[0] (case preserved) and is sanitized first. The
// connection stays open.
//
// resp.Writer.WriteError prepends '-' and appends CRLF, so the "ERR " prefix is
// part of the message text we pass.
func (c *conn) replyUnknownCommand(raw []byte) {
	_ = c.w.WriteError("ERR unknown command '" + sanitizeText(raw) + "', with args beginning with: ")
}

// replyWrongArity writes the Redis-style wrong-arity error. name is the canonical
// (upper-case) command name; Redis lower-cases it in the message. The connection
// stays open.
func (c *conn) replyWrongArity(name string) {
	_ = c.w.WriteError("ERR wrong number of arguments for '" + strings.ToLower(name) + "'")
}

// replyEngineErr reports an unexpected engine failure as a generic -ERR. The M0
// in-memory engine never fails when called with a background context, but the
// Engine contract allows an error, so it is surfaced (sanitized) rather than
// ignored. The connection stays open.
func (c *conn) replyEngineErr(err error) error {
	return c.w.WriteError("ERR " + sanitizeText([]byte(err.Error())))
}

// writeOOM emits the exact Redis out-of-memory reply for a write rejected under
// maxmemory-policy=noeviction at capacity. resp.Writer.WriteError prepends '-' and
// appends CRLF, so the framed reply is
// "-OOM command not allowed when used memory > 'maxmemory'\r\n". The connection
// stays open.
func (c *conn) writeOOM() error {
	return c.w.WriteError("OOM command not allowed when used memory > 'maxmemory'")
}
