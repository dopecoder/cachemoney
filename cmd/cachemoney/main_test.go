package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/server"
)

// respCmd encodes a RESP array command for the raw-socket integration tests.
func respCmd(parts ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, p := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(p), p)
	}
	return []byte(b.String())
}

// emptyEnv is a lookupEnv that reports every key as unset.
func emptyEnv(string) (string, bool) { return "", false }

// mapEnv builds a lookupEnv backed by m.
func mapEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// --- parseConfig (pure unit, Req 11) ------------------------------------------

func TestParseConfigDefaults(t *testing.T) {
	cfg, _, showVersion, err := parseConfig(nil, emptyEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if showVersion {
		t.Error("showVersion = true, want false")
	}
	if cfg.Addr != ":6379" {
		t.Errorf("Addr = %q, want :6379", cfg.Addr)
	}
	if cfg.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want 0", cfg.IdleTimeout)
	}
	if cfg.MaxConns != 10000 {
		t.Errorf("MaxConns = %d, want 10000", cfg.MaxConns)
	}
	if cfg.DrainTimeout != 5*time.Second {
		t.Errorf("DrainTimeout = %v, want 5s", cfg.DrainTimeout)
	}
}

func TestParseConfigFlagsOverride(t *testing.T) {
	args := []string{
		"-addr=:7000",
		"-idle-timeout=90s",
		"-max-conns=5",
		"-drain-timeout=2s",
	}
	cfg, _, _, err := parseConfig(args, emptyEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Addr != ":7000" || cfg.IdleTimeout != 90*time.Second ||
		cfg.MaxConns != 5 || cfg.DrainTimeout != 2*time.Second {
		t.Errorf("flags not applied: %+v", cfg)
	}
}

func TestParseConfigVersionFlag(t *testing.T) {
	_, _, showVersion, err := parseConfig([]string{"-version"}, emptyEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if !showVersion {
		t.Error("showVersion = false, want true")
	}
}

func TestParseConfigEnvFallback(t *testing.T) {
	env := mapEnv(map[string]string{
		"CACHEMONEY_ADDR":          "1.2.3.4:9999",
		"CACHEMONEY_IDLE_TIMEOUT":  "30s",
		"CACHEMONEY_MAX_CONNS":     "42",
		"CACHEMONEY_DRAIN_TIMEOUT": "7s",
	})
	cfg, _, _, err := parseConfig(nil, env)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Addr != "1.2.3.4:9999" || cfg.IdleTimeout != 30*time.Second ||
		cfg.MaxConns != 42 || cfg.DrainTimeout != 7*time.Second {
		t.Errorf("env not applied: %+v", cfg)
	}
}

func TestParseConfigInvalidEnvFallsBackToDefault(t *testing.T) {
	env := mapEnv(map[string]string{
		"CACHEMONEY_MAX_CONNS":     "notanumber",
		"CACHEMONEY_IDLE_TIMEOUT":  "notaduration",
		"CACHEMONEY_DRAIN_TIMEOUT": "alsobad",
	})
	cfg, _, _, err := parseConfig(nil, env)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.MaxConns != 10000 || cfg.IdleTimeout != 0 || cfg.DrainTimeout != 5*time.Second {
		t.Errorf("invalid env did not fall back to defaults: %+v", cfg)
	}
}

func TestParseConfigFlagBeatsEnv(t *testing.T) {
	env := mapEnv(map[string]string{"CACHEMONEY_ADDR": "1.2.3.4:9999"})
	cfg, _, _, err := parseConfig([]string{"-addr=:7000"}, env)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Addr != ":7000" {
		t.Errorf("Addr = %q, want :7000 (flag must beat env)", cfg.Addr)
	}
}

func TestParseConfigParseError(t *testing.T) {
	if _, _, _, err := parseConfig([]string{"-nonexistent-flag"}, emptyEnv); err == nil {
		t.Fatal("parseConfig with an unknown flag: want error, got nil")
	}
}

// --- run lifecycle (integration, Req 11) --------------------------------------

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	for i := 0; i < 200; i++ {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never came up on %s", addr)
	return nil
}

func TestRunServesThenShutsDownOnCancel(t *testing.T) {
	addr := freeAddr(t)
	srv := server.New(cache.New(), server.Config{Addr: addr})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, srv, time.Second) }()

	c := waitDial(t, addr)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		t.Fatalf("write PING: %v", err)
	}
	buf := make([]byte, len("+PONG\r\n"))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "+PONG\r\n" {
		t.Fatalf("PING reply = %q, err=%v", buf, err)
	}

	cancel() // SIGINT/SIGTERM equivalent
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run = %v, want nil after a clean drain", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after context cancel")
	}
}

func TestRunReturnsBindError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()

	srv := server.New(cache.New(), server.Config{Addr: busy.Addr().String()})
	if err := run(context.Background(), srv, time.Second); err == nil {
		t.Fatal("run on a busy address: want bind error, got nil")
	}
}

func TestRunReturnsNilWhenServerClosedExternally(t *testing.T) {
	addr := freeAddr(t)
	srv := server.New(cache.New(), server.Config{Addr: addr})
	errCh := make(chan error, 1)
	go func() { errCh <- run(context.Background(), srv, time.Second) }()

	c := waitDial(t, addr)
	_ = c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("external Shutdown: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run = %v, want nil when ListenAndServe reports ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after an external shutdown")
	}
}

// --- eviction flags + Close ordering (PR C / I5) ------------------------------

func TestParseConfigEvictionFlags(t *testing.T) {
	cfg, opts, _, err := parseConfig(
		[]string{"-maxmemory=1048576", "-maxmemory-policy=allkeys-random"}, emptyEnv,
	)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	_ = cfg
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.MaxMemory() != 1048576 {
		t.Errorf("seeded MaxMemory() = %d, want 1048576", c.MaxMemory())
	}
	if c.EvictionPolicy() != cache.PolicyAllKeysRandom {
		t.Errorf("seeded EvictionPolicy() = %v, want allkeys-random", c.EvictionPolicy())
	}
}

func TestParseConfigDefaultEvictionFlags(t *testing.T) {
	_, opts, _, err := parseConfig(nil, emptyEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.MaxMemory() != 0 {
		t.Errorf("default MaxMemory() = %d, want 0 (unbounded)", c.MaxMemory())
	}
	if c.EvictionPolicy() != cache.PolicyAllKeysLFU {
		t.Errorf("default EvictionPolicy() = %v, want allkeys-lfu", c.EvictionPolicy())
	}
}

func TestParseConfigInvalidPolicyErrors(t *testing.T) {
	if _, _, _, err := parseConfig([]string{"-maxmemory-policy=volatile-lru"}, emptyEnv); err == nil {
		t.Fatal("parseConfig with an unsupported -maxmemory-policy: want error, got nil")
	}
}

func TestParseConfigPolicyFlagIsCaseInsensitive(t *testing.T) {
	_, opts, _, err := parseConfig([]string{"-maxmemory-policy=AllKeys-Random"}, emptyEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.EvictionPolicy() != cache.PolicyAllKeysRandom {
		t.Errorf("mixed-case policy flag = %v, want allkeys-random", c.EvictionPolicy())
	}
}

func TestParseConfigInvalidPolicyEnvFallsBack(t *testing.T) {
	// A malformed env policy falls back to the default (allkeys-lfu) rather than
	// failing startup, matching the other env helpers.
	_, opts, _, err := parseConfig(nil, mapEnv(map[string]string{"CACHEMONEY_MAXMEMORY_POLICY": "garbage"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.EvictionPolicy() != cache.PolicyAllKeysLFU {
		t.Errorf("malformed env policy did not fall back: %v, want allkeys-lfu", c.EvictionPolicy())
	}
}

func TestParseConfigPolicyEnv(t *testing.T) {
	_, opts, _, err := parseConfig(nil, mapEnv(map[string]string{"CACHEMONEY_MAXMEMORY_POLICY": "noeviction"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.EvictionPolicy() != cache.PolicyNoEviction {
		t.Errorf("env policy = %v, want noeviction", c.EvictionPolicy())
	}
}

func TestParseConfigMaxMemoryEnv(t *testing.T) {
	_, opts, _, err := parseConfig(nil, mapEnv(map[string]string{"CACHEMONEY_MAXMEMORY": "2048"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.MaxMemory() != 2048 {
		t.Errorf("MaxMemory() from env = %d, want 2048", c.MaxMemory())
	}
}

func TestParseConfigInvalidMaxMemoryEnvFallsBack(t *testing.T) {
	_, opts, _, err := parseConfig(nil, mapEnv(map[string]string{"CACHEMONEY_MAXMEMORY": "notanumber"}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	c := cache.New(opts...)
	defer func() { _ = c.Close() }()
	if c.MaxMemory() != 0 {
		t.Errorf("invalid CACHEMONEY_MAXMEMORY did not fall back: MaxMemory() = %d, want 0", c.MaxMemory())
	}
}

// TestRunThenCloseUnderEvictionNoPanic replicates main's lifecycle ordering
// (server.Shutdown via run, THEN engine.Close) under live eviction + drainer traffic
// and asserts no panic and clean returns (spec Req 11 s2).
func TestRunThenCloseUnderEvictionNoPanic(t *testing.T) {
	addr := freeAddr(t)
	engine := cache.New(
		cache.WithMaxMemory(8192), cache.WithEvictionPolicy(cache.PolicyAllKeysLFU), cache.WithShards(4),
	)
	srv := server.New(engine, server.Config{Addr: addr})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, srv, time.Second) }()

	conn := waitDial(t, addr)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// SET (drives eviction past the 8 KB ceiling) + an immediate GET (drives the
	// frequency drainer via Record); the just-written key is always readable (C-INV-1).
	for i := 0; i < 400; i++ {
		k := "k" + strconv.Itoa(i)
		if _, err := conn.Write(respCmd("SET", k, "v")); err != nil {
			t.Fatalf("write SET: %v", err)
		}
		if err := readExact(conn, "+OK\r\n"); err != nil {
			t.Fatalf("SET reply: %v", err)
		}
		if _, err := conn.Write(respCmd("GET", k)); err != nil {
			t.Fatalf("write GET: %v", err)
		}
		if err := readExact(conn, "$1\r\nv\r\n"); err != nil {
			t.Fatalf("GET reply: %v", err)
		}
	}
	_ = conn.Close()

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run = %v, want nil after a clean drain", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after context cancel")
	}

	// Close the engine AFTER the server has drained — must not panic and returns nil.
	if err := engine.Close(); err != nil {
		t.Fatalf("engine.Close after shutdown = %v", err)
	}
}

// readExact reads len(want) bytes from r and checks they equal want.
func readExact(r io.Reader, want string) error {
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if string(buf) != want {
		return fmt.Errorf("reply = %q, want %q", buf, want)
	}
	return nil
}
