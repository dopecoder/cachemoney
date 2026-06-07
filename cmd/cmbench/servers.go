package main

import (
	"fmt"
	"os"
)

// Benchmark parameters (design §5). Fixed so the comparison is apples-to-apples and the
// run is reproducible; the results doc records them.
const (
	maxmemoryBytes = 64 * 1024 * 1024 // 64 MiB ceiling, set identically on every server
	valueSize      = 64               // bytes
	redisRequests  = 200000           // redis-benchmark -n
	redisClients   = 50               // redis-benchmark -c
	memtierKeyMax  = 1000000          // key space >> cache → eviction is exercised
	memtierOps     = 200000           // memtier -n per client
	memtierClients = 50               // memtier -c
	memtierThreads = 4                // memtier -t
	repeats        = 3                // R repeats; the median is reported
)

// serverSpec declares how to start and reach one RESP server. cachemoney runs only as the
// local built binary; Redis/Valkey/pogocache prefer a local binary and fall back to a
// pinned Docker image (run --network host so it binds localhost directly).
type serverSpec struct {
	name     string
	port     int
	localBin string                                 // local binary (preferred when on PATH / present)
	image    string                                 // pinned Docker image (fallback)
	startCmd func(mode mode, s serverSpec) []string // argv to start the server in the chosen mode
}

// env returns the environment override for key (set by sourcing bench/versions.env), or def.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// redisServerArgs are the redis-server/valkey-server flags shared by the local and Docker
// start paths: bind the port and set the same maxmemory + closest policy (allkeys-lfu).
func redisServerArgs(port int) []string {
	return []string{
		"redis-server", "--port", fmt.Sprintf("%d", port),
		"--maxmemory", fmt.Sprintf("%d", maxmemoryBytes), "--maxmemory-policy", "allkeys-lfu",
		"--save", "", "--appendonly", "no",
	}
}

// servers is the four-way matrix in canonical order.
var servers = []serverSpec{
	{
		name: "cachemoney", port: 6390, localBin: "bin/cachemoney",
		startCmd: func(_ mode, s serverSpec) []string {
			return []string{
				s.localBin,
				fmt.Sprintf("-addr=:%d", s.port),
				fmt.Sprintf("-maxmemory=%d", maxmemoryBytes),
				"-maxmemory-policy=allkeys-lfu",
			}
		},
	},
	{
		name: "redis", port: 6391, localBin: "redis-server", image: env("REDIS_IMAGE", "redis:7.4"),
		startCmd: func(m mode, s serverSpec) []string { return startRESP(m, s, redisServerArgs(s.port)) },
	},
	{
		name: "valkey", port: 6392, localBin: "valkey-server", image: env("VALKEY_IMAGE", "valkey/valkey:8.1"),
		startCmd: func(m mode, s serverSpec) []string {
			args := redisServerArgs(s.port)
			args[0] = "valkey-server" // the valkey image's CMD is valkey-server
			return startRESP(m, s, args)
		},
	},
	{
		name: "pogocache", port: 6393, localBin: "pogocache", image: env("POGOCACHE_IMAGE", "pogocache/pogocache:1.3.1"),
		// pogocache uses -p/--maxmemory/--evict (no LFU — its sampled eviction is the
		// closest equivalence, documented in the results caveats). Its Docker image
		// entrypoint IS pogocache, so the container takes the flags directly (no command).
		startCmd: func(m mode, s serverSpec) []string {
			flags := []string{
				"-p", fmt.Sprintf("%d", s.port),
				"--maxmemory", fmt.Sprintf("%d", maxmemoryBytes), "--evict", "yes",
			}
			if m == modeLocal {
				return append([]string{s.localBin}, flags...)
			}
			return append([]string{"docker", "run", "--rm", "-d", "--network", "host", s.image}, flags...)
		},
	},
}

// startRESP builds the argv for a server in the chosen mode: the local binary's flags, or
// `docker run --rm -d --network host <image> <serverArgs...>`.
func startRESP(m mode, s serverSpec, serverArgs []string) []string {
	if m == modeLocal {
		out := append([]string{s.localBin}, serverArgs[1:]...)
		return out
	}
	return append([]string{"docker", "run", "--rm", "-d", "--network", "host", s.image}, serverArgs...)
}
