package server

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
)

// engineCtx is the context handlers pass to the engine. It is deliberately not
// cancelled by Shutdown: an in-flight request must complete and deliver its reply
// (the drain bound is enforced by closing the socket, not by aborting the engine).
func engineCtx() context.Context { return context.Background() }

// cmdPing answers PING. Argless PING replies "+PONG\r\n"; "PING <msg>" echoes msg
// as a bulk string.
func cmdPing(c *conn, args [][]byte) error {
	if len(args) == 2 {
		return c.w.WriteBulk(args[1])
	}
	return c.w.WriteSimpleString("PONG")
}

// cmdQuit replies "+OK\r\n" and asks the loop to close the connection.
func cmdQuit(c *conn, _ [][]byte) error {
	_ = c.w.WriteSimpleString("OK")
	return errClientQuit
}

// cmdGet replies with the live value as a bulk string, or the null bulk for a
// missing/expired key.
func cmdGet(c *conn, args [][]byte) error {
	v, ok, err := c.srv.engine.Get(engineCtx(), string(args[1]))
	if err != nil {
		return c.replyEngineErr(err)
	}
	if !ok {
		return c.w.WriteNull()
	}
	return c.w.WriteBulk(v)
}

// cmdSet stores value under key. Plain SET (no option) persists (ttl 0); an EX/PX/
// EXAT/PXAT option sets a relative TTL. A parse/guard failure replies -ERR, stores
// nothing, and keeps the connection open. Under maxmemory-policy=noeviction at
// capacity the engine returns cache.ErrOOM, which maps to the Redis -OOM reply.
func cmdSet(c *conn, args [][]byte) error {
	ttl, errMsg := c.parseSetExpiry(args[3:])
	if errMsg != "" {
		return c.w.WriteError(errMsg)
	}
	if err := c.srv.engine.Set(engineCtx(), string(args[1]), args[2], ttl); err != nil {
		if errors.Is(err, cache.ErrOOM) {
			return c.writeOOM()
		}
		return c.replyEngineErr(err)
	}
	return c.w.WriteSimpleString("OK")
}

// Redis-parity -ERR texts for the SET option parser.
const (
	errSyntax        = "ERR syntax error"
	errNotInteger    = "ERR value is not an integer or out of range"
	errInvalidExpire = "ERR invalid expire time in 'set' command"
)

// Magnitude bounds that keep the SET-expiry time.Duration math from overflowing
// int64. A value beyond these is rejected as an invalid expire time (matching
// redis-server, which guards before converting to a duration) rather than wrapping
// to a small positive TTL.
const (
	maxExpireSeconds = int64(math.MaxInt64) / int64(time.Second)
	maxExpireMillis  = int64(math.MaxInt64) / int64(time.Millisecond)
)

// parseExpireInt parses a canonical base-10 signed integer (no '+' sign, no
// leading zeros beyond a lone "0"), matching Redis's string2ll for SET expiry
// arguments. It rejects empty input, a lone '-', a '+'-prefix, leading zeros, and
// overflow.
func parseExpireInt(b []byte) (int64, bool) {
	s := string(b)
	neg := false
	if s != "" && s[0] == '-' {
		neg = true
		s = s[1:]
	}
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// parseSetExpiry maps the option tokens after "SET k v" to a relative ttl for
// engine.Set. It returns (0, "") for plain SET (persistent). On any fault it
// returns (0, msg) with the matching Redis -ERR text and stores nothing:
//
//   - no option           -> ttl 0 (persistent)
//   - missing/extra token  -> syntax error
//   - unrecognized option  -> syntax error (NX/XX/GET/KEEPTTL are out of scope)
//   - non-integer argument -> "value is not an integer or out of range"
//   - computed ttl <= 0     -> "invalid expire time" (the engine treats ttl<=0 as
//     persistent, so an EXAT/PXAT in the past or EX 0 must be rejected, not stored)
func (c *conn) parseSetExpiry(opts [][]byte) (ttl time.Duration, errMsg string) {
	if len(opts) == 0 {
		return 0, "" // plain SET: persistent
	}
	if len(opts) != 2 {
		return 0, errSyntax // an option requires exactly one argument
	}
	unit := asciiUpper(opts[0])
	if unit != "EX" && unit != "PX" && unit != "EXAT" && unit != "PXAT" {
		return 0, errSyntax
	}
	n, ok := parseExpireInt(opts[1])
	if !ok {
		return 0, errNotInteger
	}
	switch unit {
	case "EX":
		if n > maxExpireSeconds || n < -maxExpireSeconds {
			return 0, errInvalidExpire // would overflow time.Duration
		}
		ttl = time.Duration(n) * time.Second
	case "PX":
		if n > maxExpireMillis || n < -maxExpireMillis {
			return 0, errInvalidExpire
		}
		ttl = time.Duration(n) * time.Millisecond
	case "EXAT":
		if n > maxExpireSeconds || n < -maxExpireSeconds {
			return 0, errInvalidExpire
		}
		ttl = time.Unix(n, 0).Sub(c.srv.cfg.Now())
	default: // PXAT
		if n > maxExpireMillis || n < -maxExpireMillis {
			return 0, errInvalidExpire
		}
		ttl = time.UnixMilli(n).Sub(c.srv.cfg.Now())
	}
	if ttl <= 0 {
		return 0, errInvalidExpire
	}
	return ttl, ""
}

// cmdDel removes one or more keys, replying with the count actually removed. An
// expired or absent key reports existed == false and is not counted, so the reply
// is the Redis "keys actually removed" count.
func cmdDel(c *conn, args [][]byte) error {
	var removed int64
	for _, key := range args[1:] {
		existed, err := c.srv.engine.Del(engineCtx(), string(key))
		if err != nil {
			return c.replyEngineErr(err)
		}
		if existed {
			removed++
		}
	}
	return c.w.WriteInt(removed)
}

// cmdExists replies with the number of arguments that name a live key, counting
// duplicates per argument (EXISTS k k for one live k -> :2). Liveness is probed
// via engine.Get per the locked proposal (no new engine method).
func cmdExists(c *conn, args [][]byte) error {
	var present int64
	for _, key := range args[1:] {
		_, ok, err := c.srv.engine.Get(engineCtx(), string(key))
		if err != nil {
			return c.replyEngineErr(err)
		}
		if ok {
			present++
		}
	}
	return c.w.WriteInt(present)
}

// ttlUnit selects the TTL reply granularity: seconds (TTL) or milliseconds (PTTL).
type ttlUnit int

const (
	ttlSeconds ttlUnit = iota
	ttlMilliseconds
)

// cmdTTL answers TTL in seconds; cmdPTTL answers PTTL in milliseconds.
func cmdTTL(c *conn, args [][]byte) error  { return c.replyTTL(args[1], ttlSeconds) }
func cmdPTTL(c *conn, args [][]byte) error { return c.replyTTL(args[1], ttlMilliseconds) }

// replyTTL derives the Redis sentinels from engine.TTL's (remaining, ok): :-2 for
// an absent/expired key (ok == false), :-1 for a live key with no expiry (the
// engine's remaining == -1 sentinel), otherwise the remaining lifetime in the
// requested unit. Seconds round to nearest ((ms+500)/1000) for redis-server parity.
func (c *conn) replyTTL(key []byte, unit ttlUnit) error {
	remaining, ok, err := c.srv.engine.TTL(engineCtx(), string(key))
	if err != nil {
		return c.replyEngineErr(err)
	}
	switch {
	case !ok:
		return c.w.WriteInt(-2)
	case remaining < 0:
		return c.w.WriteInt(-1)
	case unit == ttlSeconds:
		return c.w.WriteInt((remaining.Milliseconds() + 500) / 1000)
	default:
		return c.w.WriteInt(remaining.Milliseconds())
	}
}
