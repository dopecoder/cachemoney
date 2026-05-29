package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/cache"
)

// Eviction PR C / I5 — live CONFIG for maxmemory + maxmemory-policy and the -OOM
// reply. These are byte-exact net.Pipe tests over the dispatch surface, covering spec
// Req 7 (-OOM), Req 8 (live config), and Req 9 (policy values).

const oomReply = "-OOM command not allowed when used memory > 'maxmemory'\r\n"

// configPair builds the byte-exact CONFIG GET reply frame [key, value].
func configPair(key, value string) string {
	return fmt.Sprintf("*2\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(key), key, len(value), value)
}

// bigValue is a wire value sized to dominate any small ceiling so a single SET of it
// is guaranteed over capacity.
func bigValue(n int) string { return strings.Repeat("x", n) }

func TestConfigGetSetMaxMemoryLive(t *testing.T) {
	c := cache.New()
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory", "1048576"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory"))
	wantReply(t, client, configPair("maxmemory", "1048576"))

	if c.MaxMemory() != 1048576 {
		t.Fatalf("engine MaxMemory() = %d, want 1048576", c.MaxMemory())
	}
}

func TestConfigInvalidMaxMemoryRejected(t *testing.T) {
	c := cache.New()
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory", "100mb"))
	line := readLine(t, client)
	if !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("CONFIG SET maxmemory 100mb = %q, want -ERR", line)
	}
	// The connection stays open and the value is unchanged.
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory"))
	wantReply(t, client, configPair("maxmemory", "0"))
}

func TestConfigDefaultPolicyIsLFU(t *testing.T) {
	c := cache.New()
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory-policy"))
	wantReply(t, client, configPair("maxmemory-policy", "allkeys-lfu"))
}

func TestConfigEachSupportedPolicySelectable(t *testing.T) {
	c := cache.New()
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	for _, name := range []string{"noeviction", "allkeys-lfu", "allkeys-random"} {
		mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory-policy", name))
		wantReply(t, client, "+OK\r\n")
		mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory-policy"))
		wantReply(t, client, configPair("maxmemory-policy", name))
	}
}

func TestConfigUnsupportedPolicyRejected(t *testing.T) {
	c := cache.New()
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory-policy", "volatile-lru"))
	line := readLine(t, client)
	if !strings.HasPrefix(line, "-ERR ") || !strings.HasSuffix(line, "\r\n") {
		t.Fatalf("rejected policy reply = %q, want a -ERR …\\r\\n line", line)
	}
	// Active policy unchanged and the connection stays open.
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory-policy"))
	wantReply(t, client, configPair("maxmemory-policy", "allkeys-lfu"))
	mustWrite(t, client, encodeCmd("PING"))
	wantReply(t, client, "+PONG\r\n")
}

func TestConfigGetReflectsSeededEngine(t *testing.T) {
	// The "startup flags seed the values" contract (Req 8 s4) is observable as: an
	// engine constructed with options surfaces those values via CONFIG GET.
	c := cache.New(cache.WithMaxMemory(1048576), cache.WithEvictionPolicy(cache.PolicyAllKeysLFU))
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory"))
	wantReply(t, client, configPair("maxmemory", "1048576"))
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory-policy"))
	wantReply(t, client, configPair("maxmemory-policy", "allkeys-lfu"))
}

func TestSetNoEvictionOverCapacityReturnsOOM(t *testing.T) {
	c := cache.New(cache.WithMaxMemory(4096), cache.WithEvictionPolicy(cache.PolicyNoEviction), cache.WithShards(2))
	defer func() { _ = c.Close() }()

	// Fill to capacity via the engine API (under noeviction, Set returns ErrOOM once
	// full).
	filled := false
	for i := 0; i < 1_000_000; i++ {
		if err := c.Set(context.Background(), "fill"+strconv.Itoa(i), make([]byte, 100), 0); errors.Is(err, cache.ErrOOM) {
			filled = true
			break
		}
	}
	if !filled {
		t.Fatal("engine never reached capacity under noeviction")
	}

	client, _ := runConn(t, New(c, Config{}))

	// A new over-capacity write returns the exact -OOM frame and stores nothing.
	mustWrite(t, client, encodeCmd("SET", "newkey", bigValue(200)))
	wantReply(t, client, oomReply)
	mustWrite(t, client, encodeCmd("GET", "newkey"))
	wantReply(t, client, "$-1\r\n")

	// A shrinking overwrite of an existing key at capacity is allowed.
	mustWrite(t, client, encodeCmd("SET", "fill0", "x"))
	wantReply(t, client, "+OK\r\n")
}

func TestConfigLowerMaxMemoryEvictsLive(t *testing.T) {
	c := cache.New(cache.WithMaxMemory(1<<20), cache.WithEvictionPolicy(cache.PolicyAllKeysLFU), cache.WithShards(4))
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	for i := 0; i < 500; i++ {
		mustWrite(t, client, encodeCmd("SET", "k"+strconv.Itoa(i), bigValue(100)))
		wantReply(t, client, "+OK\r\n")
	}
	used := c.Stats().Usage
	if used == 0 {
		t.Fatal("no usage accrued")
	}
	newMax := used / 2
	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory", strconv.FormatInt(newMax, 10)))
	wantReply(t, client, "+OK\r\n")

	if got := c.Stats().Usage; got > newMax+512 {
		t.Fatalf("after lowering maxmemory live, usage %d exceeds new ceiling %d (+slack)", got, newMax)
	}
}

func TestConfigSwitchToNoEvictionThenOOM(t *testing.T) {
	c := cache.New(cache.WithMaxMemory(4096), cache.WithEvictionPolicy(cache.PolicyAllKeysLFU), cache.WithShards(2))
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	// Drive the engine to capacity under lfu (it evicts to stay bounded).
	for i := 0; i < 400; i++ {
		mustWrite(t, client, encodeCmd("SET", "k"+strconv.Itoa(i), bigValue(100)))
		wantReply(t, client, "+OK\r\n")
	}
	// Switch to noeviction live, then a clearly-over-capacity write returns -OOM.
	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory-policy", "noeviction"))
	wantReply(t, client, "+OK\r\n")
	mustWrite(t, client, encodeCmd("SET", "huge", bigValue(8192)))
	wantReply(t, client, oomReply)
}

func TestConfigSwitchPolicyStillBounds(t *testing.T) {
	c := cache.New(cache.WithMaxMemory(8192), cache.WithEvictionPolicy(cache.PolicyAllKeysLFU), cache.WithShards(2))
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory-policy", "allkeys-random"))
	wantReply(t, client, "+OK\r\n")
	for i := 0; i < 1000; i++ {
		mustWrite(t, client, encodeCmd("SET", "k"+strconv.Itoa(i), bigValue(100)))
		wantReply(t, client, "+OK\r\n")
	}
	if got := c.Stats().Usage; got > 8192+512 {
		t.Fatalf("allkeys-random did not bound memory: usage %d > ceiling 8192 (+slack)", got)
	}
}

func TestConfigArityAndUnknownParams(t *testing.T) {
	c := cache.New()
	defer func() { _ = c.Close() }()
	client, _ := runConn(t, New(c, Config{}))

	// GET with no parameter and SET with no value are arity errors.
	mustWrite(t, client, encodeCmd("CONFIG", "GET"))
	if line := readLine(t, client); !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("CONFIG GET (no param) = %q, want -ERR", line)
	}
	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory"))
	if line := readLine(t, client); !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("CONFIG SET maxmemory (no value) = %q, want -ERR", line)
	}
	// An unknown GET parameter is an empty array; an unknown SET parameter is a no-op +OK.
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "save"))
	wantReply(t, client, "*0\r\n")
	mustWrite(t, client, encodeCmd("CONFIG", "SET", "appendonly", "no"))
	wantReply(t, client, "+OK\r\n")
}

func TestConfigDegradesWhenEngineNotTunable(t *testing.T) {
	// An engine that does not satisfy cache.Tunable: GET reports defaults; SET of a
	// managed key replies -ERR (the defined degraded fallback).
	client, _ := runConn(t, New(errEngine{err: errors.New("boom")}, Config{}))

	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory"))
	wantReply(t, client, configPair("maxmemory", "0"))
	mustWrite(t, client, encodeCmd("CONFIG", "GET", "maxmemory-policy"))
	wantReply(t, client, configPair("maxmemory-policy", "allkeys-lfu"))

	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory", "1024"))
	if line := readLine(t, client); !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("CONFIG SET maxmemory (non-Tunable) = %q, want -ERR", line)
	}
	mustWrite(t, client, encodeCmd("CONFIG", "SET", "maxmemory-policy", "allkeys-lfu"))
	if line := readLine(t, client); !strings.HasPrefix(line, "-ERR ") {
		t.Fatalf("CONFIG SET maxmemory-policy (non-Tunable) = %q, want -ERR", line)
	}
}
