package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/server"
)

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
	cfg, showVersion, err := parseConfig(nil, emptyEnv)
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
	cfg, _, err := parseConfig(args, emptyEnv)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Addr != ":7000" || cfg.IdleTimeout != 90*time.Second ||
		cfg.MaxConns != 5 || cfg.DrainTimeout != 2*time.Second {
		t.Errorf("flags not applied: %+v", cfg)
	}
}

func TestParseConfigVersionFlag(t *testing.T) {
	_, showVersion, err := parseConfig([]string{"-version"}, emptyEnv)
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
	cfg, _, err := parseConfig(nil, env)
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
	cfg, _, err := parseConfig(nil, env)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.MaxConns != 10000 || cfg.IdleTimeout != 0 || cfg.DrainTimeout != 5*time.Second {
		t.Errorf("invalid env did not fall back to defaults: %+v", cfg)
	}
}

func TestParseConfigFlagBeatsEnv(t *testing.T) {
	env := mapEnv(map[string]string{"CACHEMONEY_ADDR": "1.2.3.4:9999"})
	cfg, _, err := parseConfig([]string{"-addr=:7000"}, env)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Addr != ":7000" {
		t.Errorf("Addr = %q, want :7000 (flag must beat env)", cfg.Addr)
	}
}

func TestParseConfigParseError(t *testing.T) {
	if _, _, err := parseConfig([]string{"-nonexistent-flag"}, emptyEnv); err == nil {
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
