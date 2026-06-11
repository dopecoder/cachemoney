package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestServerRunArgsSetFairnessConfig asserts every server's bridge run command carries the
// same maxmemory ceiling and (for the LFU-capable servers) the allkeys-lfu policy — the
// uniform fairness config (spec Req 4 s1) — and is wired onto the named bench bridge.
func TestServerRunArgsSetFairnessConfig(t *testing.T) {
	t.Parallel()

	const net = "testnet"
	wantMem := strconv.Itoa(maxmemoryBytes)
	wantPort := strconv.Itoa(serverPort)
	lfuServers := map[string]bool{"cachemoney": true, "redis": true, "valkey": true}

	for _, s := range servers {
		argv := strings.Join(s.runArgs(net), " ")
		if !strings.Contains(argv, wantMem) {
			t.Errorf("%s run args missing maxmemory %s: %s", s.name, wantMem, argv)
		}
		if !strings.Contains(argv, wantPort) {
			t.Errorf("%s run args missing port %s: %s", s.name, wantPort, argv)
		}
		if !strings.Contains(argv, "--network "+net) {
			t.Errorf("%s run args missing --network %s: %s", s.name, net, argv)
		}
		if !strings.Contains(argv, "--name "+containerName(s.name)) {
			t.Errorf("%s run args missing --name %s: %s", s.name, containerName(s.name), argv)
		}
		if lfuServers[s.name] && !strings.Contains(argv, "allkeys-lfu") {
			t.Errorf("%s run args missing allkeys-lfu: %s", s.name, argv)
		}
	}
}

// TestServerImagesIncludeToolImages asserts each server's availability check covers its
// own image plus the shared redis (tool/probe) and memtier images.
func TestServerImagesIncludeToolImages(t *testing.T) {
	t.Parallel()

	for _, s := range servers {
		imgs := strings.Join(s.images(), " ")
		for _, want := range []string{s.image, redisImage, memtierImage} {
			if !strings.Contains(imgs, want) {
				t.Errorf("%s images() missing %q: %v", s.name, want, s.images())
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
