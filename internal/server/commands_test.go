package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/cache"
)

func TestPing(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")

	mustWrite(t, client, encodeCmd("PING", "hello"))
	wantReply(t, client, "$5\r\nhello\r\n")
}

func TestQuitRepliesThenCloses(t *testing.T) {
	client, done := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n") // open before QUIT

	mustWrite(t, client, encodeCmd("QUIT"))
	wantReply(t, client, "+OK\r\n")
	expectClosed(t, client)
	waitDone(t, done)
}

func TestGetSetRoundTrip(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("SET", "k", "hello"))
	wantReply(t, client, "+OK\r\n")

	mustWrite(t, client, encodeCmd("GET", "k"))
	wantReply(t, client, "$5\r\nhello\r\n")
}

func TestGetMissingReturnsNullBulk(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("GET", "absent"))
	wantReply(t, client, "$-1\r\n")
}

func TestDelSingleKey(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("SET", "k", "v"))
	wantReply(t, client, "+OK\r\n")

	mustWrite(t, client, encodeCmd("DEL", "k"))
	wantReply(t, client, ":1\r\n")

	mustWrite(t, client, encodeCmd("GET", "k"))
	wantReply(t, client, "$-1\r\n") // removed

	mustWrite(t, client, encodeCmd("DEL", "gone"))
	wantReply(t, client, ":0\r\n")
}

func TestHandlerEngineErrorsKeepConnectionOpen(t *testing.T) {
	boom := errors.New("engine boom")
	sends := [][]byte{
		encodeCmd("GET", "k"),
		encodeCmd("SET", "k", "v"),
		encodeCmd("DEL", "k"),
	}
	for _, send := range sends {
		client, _ := runConn(t, New(errEngine{err: boom}, Config{}))
		mustWrite(t, client, send)
		line := readLine(t, client)
		if !strings.HasPrefix(line, "-ERR ") || !strings.HasSuffix(line, "\r\n") {
			t.Fatalf("reply = %q, want a -ERR line", line)
		}
		// the connection survives an engine error
		mustWrite(t, client, encodeCmd("PING"))
		wantReply(t, client, "+PONG\r\n")
	}
}
