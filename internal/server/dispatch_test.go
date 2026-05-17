package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/cache"
)

func TestUnknownCommandKeepsConnectionOpen(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("FOO"))
	line := readLine(t, client)
	if !strings.HasPrefix(line, "-ERR unknown command 'FOO'") {
		t.Fatalf("reply = %q, want prefix %q", line, "-ERR unknown command 'FOO'")
	}
	if !strings.HasSuffix(line, "\r\n") {
		t.Fatalf("reply = %q, want it to end with CRLF", line)
	}

	// The same connection still serves a valid command.
	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")
}

func TestWrongArityKeepsConnectionOpen(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("GET")) // GET with no key
	wantReply(t, client, "-ERR wrong number of arguments for 'get'\r\n")

	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")
}

func TestEmptyArrayFrameIsIgnored(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, []byte("*0\r\n")) // zero-length array: ignored, stays open
	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")
}

func TestLowercaseCommandIsUpperCased(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("ping")) // ASCII lower-cased to PING
	wantReply(t, client, "+PONG\r\n")
}

func TestUnknownCommandSanitizesControlBytes(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// A command name carrying CR/LF must not inject a second reply frame.
	raw := "FO\r\nO"
	frame := []byte(fmt.Sprintf("*1\r\n$%d\r\n%s\r\n", len(raw), raw))
	mustWrite(t, client, frame)

	line := readLine(t, client) // reads up to the first '\n'
	if !strings.HasPrefix(line, "-ERR unknown command 'FO??O'") {
		t.Fatalf("reply = %q, want sanitized name FO??O on a single line", line)
	}
}

func TestUnknownCommandTruncatesEchoedName(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	long := strings.Repeat("A", 300)
	frame := []byte(fmt.Sprintf("*1\r\n$%d\r\n%s\r\n", len(long), long))
	mustWrite(t, client, frame)

	line := readLine(t, client)
	start := strings.IndexByte(line, '\'')
	end := strings.IndexByte(line[start+1:], '\'')
	if start < 0 || end < 0 {
		t.Fatalf("reply = %q, want a quoted command name", line)
	}
	echoed := line[start+1 : start+1+end]
	if len(echoed) > maxErrEchoLen {
		t.Fatalf("echoed name length = %d, want <= %d", len(echoed), maxErrEchoLen)
	}
}

func TestAsciiUpper(t *testing.T) {
	cases := map[string]string{
		"get":     "GET",
		"GET":     "GET",
		"PiNg":    "PING",
		"set123":  "SET123",
		"":        "",
		"a-b_c.d": "A-B_C.D",
	}
	for in, want := range cases {
		if got := asciiUpper([]byte(in)); got != want {
			t.Errorf("asciiUpper(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeText(t *testing.T) {
	if got := sanitizeText([]byte("ok-name")); got != "ok-name" {
		t.Errorf("sanitizeText(plain) = %q", got)
	}
	if got := sanitizeText([]byte("a\r\nb\x00c\x7f")); got != "a??b?c?" {
		t.Errorf("sanitizeText(control) = %q, want %q", got, "a??b?c?")
	}
	long := strings.Repeat("x", maxErrEchoLen+50)
	if got := sanitizeText([]byte(long)); len(got) != maxErrEchoLen {
		t.Errorf("sanitizeText truncation len = %d, want %d", len(got), maxErrEchoLen)
	}
}
