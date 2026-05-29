package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/cache"
)

// helloReply builds the byte-exact HELLO server map the handler emits, in the
// dialect selected by proto (RESP3 "%7" map vs RESP2 "*14" flat array). It mirrors
// writeHelloMap field-for-field so the test is a true golden oracle.
func helloReply(proto int, version string, id uint64) string {
	var b strings.Builder
	if proto >= 3 {
		b.WriteString("%7\r\n")
	} else {
		b.WriteString("*14\r\n")
	}
	bulk := func(s string) { fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(s), s) }
	bulk("server")
	bulk("cachemoney")
	bulk("version")
	bulk(version)
	bulk("proto")
	fmt.Fprintf(&b, ":%d\r\n", proto)
	bulk("id")
	fmt.Fprintf(&b, ":%d\r\n", id)
	bulk("mode")
	bulk("standalone")
	bulk("role")
	bulk("master")
	bulk("modules")
	b.WriteString("*0\r\n")
	return b.String()
}

// --- Increment 4: HELLO protocol negotiation + dialect flip (Req 6) -----------

func TestHelloNegotiatesProtocolAndFlipsDialect(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// HELLO 3 -> RESP3 map; a following logical null serializes as "_\r\n".
	mustWrite(t, client, encodeCmd("HELLO", "3"))
	wantReply(t, client, helloReply(3, "0.1.0", 1))
	mustWrite(t, client, encodeCmd("GET", "nope"))
	wantReply(t, client, "_\r\n")

	// HELLO 2 -> RESP2 map; the same logical null is now "$-1\r\n" (flip back).
	mustWrite(t, client, encodeCmd("HELLO", "2"))
	wantReply(t, client, helloReply(2, "0.1.0", 1))
	mustWrite(t, client, encodeCmd("GET", "nope"))
	wantReply(t, client, "$-1\r\n")
}

func TestHelloBareKeepsCurrentDialect(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// No version arg: reply at the connection default (RESP2), no dialect change.
	mustWrite(t, client, encodeCmd("HELLO"))
	wantReply(t, client, helloReply(2, "0.1.0", 1))
	mustWrite(t, client, encodeCmd("GET", "nope"))
	wantReply(t, client, "$-1\r\n")
}

func TestHelloUnsupportedVersionIsNoproto(t *testing.T) {
	for _, ver := range []string{"4", "1", "abc"} {
		t.Run(ver, func(t *testing.T) {
			client, _ := runConn(t, New(cache.New(), Config{}))
			mustWrite(t, client, encodeCmd("HELLO", ver))
			wantReply(t, client, "-NOPROTO unsupported protocol version\r\n")
			// No dialect change; the connection stays open at RESP2.
			mustWrite(t, client, encodeCmd("GET", "nope"))
			wantReply(t, client, "$-1\r\n")
		})
	}
}

func TestHelloAuthIsRejected(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("HELLO", "3", "AUTH", "user", "pass"))
	wantReply(t, client, "-ERR Client sent AUTH, but no password is set\r\n")
	// Rejected before any dialect flip: still RESP2.
	mustWrite(t, client, encodeCmd("GET", "nope"))
	wantReply(t, client, "$-1\r\n")
}

func TestHelloSetnameAcceptedAndIgnored(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// SETNAME tail is consumed; the normal RESP3 handshake follows.
	mustWrite(t, client, encodeCmd("HELLO", "3", "SETNAME", "conn-a"))
	wantReply(t, client, helloReply(3, "0.1.0", 1))
	mustWrite(t, client, encodeCmd("GET", "nope"))
	wantReply(t, client, "_\r\n")
}

func TestHelloSyntaxErrors(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// SETNAME without a name -> syntax error.
	mustWrite(t, client, encodeCmd("HELLO", "3", "SETNAME"))
	wantReply(t, client, "-ERR syntax error\r\n")
	// Unrecognized tail token -> syntax error.
	mustWrite(t, client, encodeCmd("HELLO", "3", "BOGUS"))
	wantReply(t, client, "-ERR syntax error\r\n")
	// Connection survives; still RESP2 (no flip happened).
	mustWrite(t, client, encodeCmd("GET", "nope"))
	wantReply(t, client, "$-1\r\n")
}

// --- Increment 4: COMMAND / CONFIG / SELECT (Req 6) ---------------------------

func TestCommandStub(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("COMMAND"))
	wantReply(t, client, "*0\r\n")

	mustWrite(t, client, encodeCmd("COMMAND", "DOCS"))
	wantReply(t, client, "*0\r\n")

	mustWrite(t, client, encodeCmd("COMMAND", "COUNT"))
	wantReply(t, client, fmt.Sprintf(":%d\r\n", len(commandTable)))

	// An unknown sub-command is a non-fatal -ERR.
	mustWrite(t, client, encodeCmd("COMMAND", "BOGUS"))
	line := readLine(t, client)
	if !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("COMMAND BOGUS = %q, want a -ERR line", line)
	}
	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")
}

func TestConfigStub(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// CONFIG GET maxmemory is now live: a default engine reports 0 (unbounded) as a
	// two-element key/value array.
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory"))
	wantReply(t, client, "*2\r\n$9\r\nmaxmemory\r\n$1\r\n0\r\n")

	// CONFIG SET maxmemory takes a decimal byte count and replies +OK.
	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory", "1048576"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory"))
	wantReply(t, client, "*2\r\n$9\r\nmaxmemory\r\n$7\r\n1048576\r\n")

	// An unknown sub-command stays a non-fatal -ERR.
	mustWrite(t, client, encodeCmd("CONFIG", "BOGUS"))
	line := readLine(t, client)
	if !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("CONFIG BOGUS = %q, want a -ERR line", line)
	}
}

func TestSelect(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("SELECT", "0"))
	wantReply(t, client, "+OK\r\n")

	mustWrite(t, client, encodeCmd("SELECT", "1"))
	wantReply(t, client, "-ERR DB index is out of range\r\n")

	mustWrite(t, client, encodeCmd("SELECT", "abc"))
	wantReply(t, client, "-ERR value is not an integer or out of range\r\n")

	// All errors keep the connection open.
	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")
}
