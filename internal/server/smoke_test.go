package server

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
)

// findRedisCLI returns the path to a redis-cli-compatible client, or "" when none
// is on PATH. valkey-cli is wire-compatible and accepted as a fallback.
func findRedisCLI() string {
	for _, name := range []string{"redis-cli", "valkey-cli"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// TestRedisCLISmoke is the real-world validator of the handshake (§9) and the
// COMMAND/CONFIG stub depth (§9.2): it drives an in-process server with a stock
// redis-cli session (PING / SET / GET / HELLO 3 / CONFIG GET) and asserts a clean
// exchange. It is skipped when no client binary is installed, mirroring the
// codec's tool-absent skip.
func TestRedisCLISmoke(t *testing.T) {
	cli := findRedisCLI()
	if cli == "" {
		t.Skip("redis-cli/valkey-cli not on PATH; skipping smoke test")
	}

	_, addr := startServer(t, cache.New(), Config{})
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr %q: %v", addr, err)
	}

	script := strings.Join([]string{
		"PING",
		"SET smoke value",
		"GET smoke",
		"HELLO 3",
		"CONFIG GET maxmemory",
	}, "\n") + "\n"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "-h", host, "-p", port)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("redis-cli session failed: %v\n%s", err, out)
	}

	got := string(out)
	if !strings.Contains(strings.ToUpper(got), "PONG") {
		t.Fatalf("redis-cli output missing PONG:\n%s", got)
	}
	if !strings.Contains(got, "value") {
		t.Fatalf("redis-cli output missing the GET value:\n%s", got)
	}
}
