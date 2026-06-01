package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestServerStartArgsSetFairnessConfig asserts every server's start command carries the
// same maxmemory ceiling and (for the LFU-capable servers) the allkeys-lfu policy — the
// uniform fairness config (spec Req 4 s1) — in both local and Docker modes.
func TestServerStartArgsSetFairnessConfig(t *testing.T) {
	t.Parallel()

	wantMem := strconv.Itoa(maxmemoryBytes)
	lfuServers := map[string]bool{"cachemoney": true, "redis": true, "valkey": true}

	for _, s := range servers {
		for _, m := range []mode{modeLocal, modeDocker} {
			if m == modeDocker && s.image == "" {
				continue // cachemoney has no image
			}
			argv := strings.Join(s.startCmd(m, s), " ")
			if !strings.Contains(argv, wantMem) {
				t.Errorf("%s (%v) start args missing maxmemory %s: %s", s.name, m, wantMem, argv)
			}
			if !strings.Contains(argv, strconv.Itoa(s.port)) {
				t.Errorf("%s (%v) start args missing port %d: %s", s.name, m, s.port, argv)
			}
			if lfuServers[s.name] && !strings.Contains(argv, "allkeys-lfu") {
				t.Errorf("%s (%v) start args missing allkeys-lfu: %s", s.name, m, argv)
			}
			if m == modeDocker && !strings.Contains(argv, "--network host") {
				t.Errorf("%s docker start args missing --network host: %s", s.name, argv)
			}
		}
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("BENCH_TEST_IMAGE", "custom:9.9")
	if got := env("BENCH_TEST_IMAGE", "default"); got != "custom:9.9" {
		t.Errorf("env override = %q, want custom:9.9", got)
	}
	if got := env("BENCH_TEST_UNSET_XYZ", "default"); got != "default" {
		t.Errorf("env default = %q, want default", got)
	}
}
