package server

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
)

// fakeClock is a goroutine-safe, manually-advanced clock. The TTL/expiry scenarios
// are deterministic only with one clock shared by BOTH the engine
// (cache.WithClock) and the server's EXAT/PXAT conversion (Config.Now). The mutex
// keeps -race happy: serveConn reads the clock on another goroutine while the test
// advances it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// ttlServer builds a server whose engine and EXAT/PXAT conversion share clock.
func ttlServer(clock *fakeClock) *Server {
	return New(cache.New(cache.WithClock(clock.Now)), Config{Now: clock.Now})
}

// ttlBase is an arbitrary fixed instant with zero sub-second component so EXAT/PXAT
// arithmetic lands on exact seconds/milliseconds.
var ttlBase = time.Unix(1_700_000_000, 0)

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
		encodeCmd("EXISTS", "k"),
		encodeCmd("TTL", "k"),
		encodeCmd("PTTL", "k"),
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

// --- Increment 3: SET expiry options (Req 3) ----------------------------------

func TestSetExpiryOptionsSetObservableTTL(t *testing.T) {
	clock := newFakeClock(ttlBase)
	client, _ := runConn(t, ttlServer(clock))

	// EX seconds.
	mustWrite(t, client, encodeCmd("SET", "ex", "v", "EX", "30"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("TTL", "ex"))
	wantReply(t, client, ":30\r\n")
	mustWrite(t, client, encodeCmd("PTTL", "ex"))
	wantReply(t, client, ":30000\r\n")

	// PX milliseconds.
	mustWrite(t, client, encodeCmd("SET", "px", "v", "PX", "30000"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("PTTL", "px"))
	wantReply(t, client, ":30000\r\n")
	mustWrite(t, client, encodeCmd("TTL", "px"))
	wantReply(t, client, ":30\r\n")

	// EXAT absolute seconds.
	exat := strconv.FormatInt(ttlBase.Unix()+30, 10)
	mustWrite(t, client, encodeCmd("SET", "exat", "v", "EXAT", exat))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("TTL", "exat"))
	wantReply(t, client, ":30\r\n")

	// PXAT absolute milliseconds.
	pxat := strconv.FormatInt(ttlBase.UnixMilli()+30000, 10)
	mustWrite(t, client, encodeCmd("SET", "pxat", "v", "PXAT", pxat))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("PTTL", "pxat"))
	wantReply(t, client, ":30000\r\n")
}

func TestGetExpiredKeyReturnsNull(t *testing.T) {
	clock := newFakeClock(ttlBase)
	client, _ := runConn(t, ttlServer(clock))

	mustWrite(t, client, encodeCmd("SET", "k", "v", "EX", "30"))
	wantReply(t, client, "+OK\r\n")

	clock.Advance(31 * time.Second) // past the expiry instant

	mustWrite(t, client, encodeCmd("GET", "k"))
	wantReply(t, client, "$-1\r\n")
	// TTL of an expired key is the missing sentinel.
	mustWrite(t, client, encodeCmd("TTL", "k"))
	wantReply(t, client, ":-2\r\n")
}

func TestPlainSetPersistsTTLNegativeOne(t *testing.T) {
	clock := newFakeClock(ttlBase)
	client, _ := runConn(t, ttlServer(clock))

	mustWrite(t, client, encodeCmd("SET", "k", "v"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("TTL", "k"))
	wantReply(t, client, ":-1\r\n")
	mustWrite(t, client, encodeCmd("PTTL", "k"))
	wantReply(t, client, ":-1\r\n")
}

func TestSetExpiryGuards(t *testing.T) {
	clock := newFakeClock(ttlBase)
	client, _ := runConn(t, ttlServer(clock))

	cases := []struct {
		name string
		cmd  []byte
		want string
	}{
		{"non-integer", encodeCmd("SET", "k", "v", "EX", "notanumber"), "-ERR value is not an integer or out of range\r\n"},
		{"ex overflow", encodeCmd("SET", "k", "v", "EX", "18446744074"), "-ERR invalid expire time in 'set' command\r\n"},
		{"px overflow", encodeCmd("SET", "k", "v", "PX", "9999999999999"), "-ERR invalid expire time in 'set' command\r\n"},
		{"exat overflow", encodeCmd("SET", "k", "v", "EXAT", "99999999999"), "-ERR invalid expire time in 'set' command\r\n"},
		{"pxat overflow", encodeCmd("SET", "k", "v", "PXAT", "9999999999999"), "-ERR invalid expire time in 'set' command\r\n"},
		{"ex negative", encodeCmd("SET", "k", "v", "EX", "-5"), "-ERR invalid expire time in 'set' command\r\n"},
		{"ex lone minus", encodeCmd("SET", "k", "v", "EX", "-"), "-ERR value is not an integer or out of range\r\n"},
		{"ex overflow string", encodeCmd("SET", "k", "v", "EX", "99999999999999999999999"), "-ERR value is not an integer or out of range\r\n"},
		{"ex plus sign", encodeCmd("SET", "k", "v", "EX", "+5"), "-ERR value is not an integer or out of range\r\n"},
		{"ex leading zero", encodeCmd("SET", "k", "v", "EX", "007"), "-ERR value is not an integer or out of range\r\n"},
		{"ex zero", encodeCmd("SET", "k", "v", "EX", "0"), "-ERR invalid expire time in 'set' command\r\n"},
		{"exat in past", encodeCmd("SET", "k", "v", "EXAT", strconv.FormatInt(ttlBase.Unix()-1, 10)), "-ERR invalid expire time in 'set' command\r\n"},
		{"unrecognized NX", encodeCmd("SET", "k", "v", "NX"), "-ERR syntax error\r\n"},
		{"unrecognized KEEPTTL", encodeCmd("SET", "k", "v", "KEEPTTL"), "-ERR syntax error\r\n"},
		{"unrecognized opt with arg", encodeCmd("SET", "k", "v", "GET", "5"), "-ERR syntax error\r\n"},
		{"missing arg", encodeCmd("SET", "k", "v", "EX"), "-ERR syntax error\r\n"},
	}
	for _, tc := range cases {
		mustWrite(t, client, tc.cmd)
		wantReply(t, client, tc.want)
	}
	// Nothing was stored and the connection survives.
	mustWrite(t, client, encodeCmd("GET", "k"))
	wantReply(t, client, "$-1\r\n")
}

// --- Increment 3: variadic DEL / EXISTS (Req 4) -------------------------------

func TestDelVariadicCountsRemoved(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("SET", "a", "1"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("SET", "b", "2"))
	wantReply(t, client, "+OK\r\n")

	// a,b live, c absent -> 2 removed.
	mustWrite(t, client, encodeCmd("DEL", "a", "b", "c"))
	wantReply(t, client, ":2\r\n")
}

func TestExistsVariadicCountsPresentWithDuplicates(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	mustWrite(t, client, encodeCmd("SET", "k", "v"))
	wantReply(t, client, "+OK\r\n")

	// duplicates are counted per argument.
	mustWrite(t, client, encodeCmd("EXISTS", "k", "k"))
	wantReply(t, client, ":2\r\n")

	mustWrite(t, client, encodeCmd("EXISTS", "x", "y"))
	wantReply(t, client, ":0\r\n")
}

// --- Increment 3: TTL/PTTL sentinel derivation (Req 5) ------------------------

func TestTTLSentinels(t *testing.T) {
	client, _ := runConn(t, New(cache.New(), Config{}))

	// Missing key -> :-2 for both units.
	mustWrite(t, client, encodeCmd("TTL", "gone"))
	wantReply(t, client, ":-2\r\n")
	mustWrite(t, client, encodeCmd("PTTL", "gone"))
	wantReply(t, client, ":-2\r\n")
}

// TestTTLSecondsRounding pins the round-to-nearest boundary ((ms+500)/1000,
// matching Redis) that whole-second TTL tests do not exercise.
func TestTTLSecondsRounding(t *testing.T) {
	clock := newFakeClock(ttlBase)
	client, _ := runConn(t, ttlServer(clock))
	for _, tc := range []struct{ key, px, ttl string }{
		{"a", "1500", ":2\r\n"}, // 1500ms rounds to 2s
		{"b", "1499", ":1\r\n"}, // 1499ms rounds to 1s
		{"c", "500", ":1\r\n"},  // 500ms rounds to 1s
	} {
		mustWrite(t, client, encodeCmd("SET", tc.key, "v", "PX", tc.px))
		wantReply(t, client, "+OK\r\n")
		mustWrite(t, client, encodeCmd("TTL", tc.key))
		wantReply(t, client, tc.ttl)
	}
}
