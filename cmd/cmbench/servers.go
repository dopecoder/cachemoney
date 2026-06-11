package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Bench parameters + the shared bridge/port. Every server runs in its own container
// namespace and listens on serverPort, so the same port never conflicts — peers are
// addressed by container NAME on a private bridge (works under rootless / WSL2-mirrored /
// Docker Desktop, unlike --network host or host-published ports).
const (
	benchNetwork   = "cmbench-net"
	serverPort     = 6379
	maxmemoryBytes = 64 * 1024 * 1024 // 64 MiB ceiling, set identically on every server
	valueSize      = 64               // redis-benchmark value bytes (throughput axis)

	// memtier hit-ratio / eviction axis. Large values + a key space that exceeds the
	// 64 MiB ceiling force real eviction (the 64-byte/1M-key throughput shape never fills
	// the cache, so eviction never fires and every policy looks identical). SETs are
	// uniform (R) to populate broadly and overflow; GETs are Gaussian (G) onto a hot set,
	// so a frequency policy (W-TinyLFU / LFU) wins by retaining it. Validated to evict and
	// to separate W-TinyLFU > LFU-sampling > sampling.
	memtierValueBytes = 4096  // --data-size
	memtierKeyMax     = 50000 // --key-maximum (>> ~15k-entry cache at 4 KiB values)
	memtierKeyMedian  = 25000 // --key-median (center of the Gaussian GET hot set)
	memtierKeyStddev  = 6000  // --key-stddev
	memtierRatio      = "1:2" // --ratio SET:GET (enough SETs to fill+overflow)

	cachemoneyStaticPath = "bin/cachemoney-static" // CGO-free build, mounted into the base image
)

// Load sizes. Defaults give a representative run; BENCH_QUICK=1 shrinks them to a ~30s
// smoke, and each is individually overridable (e.g. BENCH_REPEATS=1) for diagnosis.
var (
	redisRequests  = envInt("BENCH_REDIS_REQUESTS", pick(100000, 5000)) // redis-benchmark -n
	redisClients   = envInt("BENCH_REDIS_CLIENTS", pick(20, 5))         // redis-benchmark -c
	memtierOps     = envInt("BENCH_MEMTIER_OPS", pick(50000, 2000))     // memtier -n per client
	memtierClients = envInt("BENCH_MEMTIER_CLIENTS", pick(10, 4))       // memtier -c
	memtierThreads = envInt("BENCH_MEMTIER_THREADS", 2)                 // memtier -t
	repeats        = envInt("BENCH_REPEATS", pick(3, 1))                // measured repeats (median); warmup discarded
)

func benchQuick() bool { return os.Getenv("BENCH_QUICK") != "" }

// pick returns the quick value under BENCH_QUICK, else the full value.
func pick(full, quick int) int {
	if benchQuick() {
		return quick
	}
	return full
}

// envInt reads a positive integer from key, falling back to def (warning to stderr on a
// present-but-invalid value so a fat-fingered override is not silently ignored).
func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a positive integer; using %d\n", key, v, def)
		return def
	}
	return n
}

// Pinned images (override any via bench/versions.env).
var (
	redisImage     = env("REDIS_IMAGE", "redis:7.4")
	valkeyImage    = env("VALKEY_IMAGE", "valkey/valkey:8.1")
	pogocacheImage = env("POGOCACHE_IMAGE", "pogocache/pogocache:1.3.1")
	memtierImage   = env("MEMTIER_IMAGE", "redislabs/memtier_benchmark:2.4.2")
	baseImage      = env("BASE_IMAGE", "alpine:3") // runs the static cachemoney binary
)

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func containerName(server string) string { return "cmbench-" + server }

// serverSpec declares how to run one RESP server as a container on the bench bridge.
type serverSpec struct {
	name      string
	image     string                    // the server image (cachemoney uses a base image)
	localFile string                    // host file that must exist to run this server (mounted in)
	runArgs   func(net string) []string // the full `docker run -d ...` argv
}

// images returns the images this server's measurement needs: its own + the shared redis
// image (redis-benchmark + the readiness probe) + the memtier image.
func (s serverSpec) images() []string {
	return []string{s.image, redisImage, memtierImage}
}

// redisServerArgs are the redis/valkey-server flags: protected-mode off so bridge peers
// can connect, and the same maxmemory + allkeys-lfu on every LFU-capable server.
func redisServerArgs(bin string) []string {
	return []string{
		bin, "--port", fmt.Sprintf("%d", serverPort), "--protected-mode", "no",
		"--maxmemory", fmt.Sprintf("%d", maxmemoryBytes), "--maxmemory-policy", "allkeys-lfu",
		"--save", "", "--appendonly", "no",
	}
}

// dockerRunBase is the common `docker run -d --rm --name <cmbench-name> --network <net>
// <image>` prefix.
func dockerRunBase(name, image, net string) []string {
	return []string{"docker", "run", "-d", "--rm", "--name", containerName(name), "--network", net, image}
}

// servers is the four-way matrix in canonical order.
var servers = []serverSpec{
	{
		name: "cachemoney", image: baseImage, localFile: cachemoneyStaticPath,
		runArgs: func(net string) []string {
			bin, _ := filepath.Abs(cachemoneyStaticPath)
			return []string{
				"docker", "run", "-d", "--rm", "--name", containerName("cachemoney"), "--network", net,
				"-v", bin + ":/cachemoney:ro", "--entrypoint", "/cachemoney", baseImage,
				fmt.Sprintf("-addr=:%d", serverPort),
				fmt.Sprintf("-maxmemory=%d", maxmemoryBytes), "-maxmemory-policy=allkeys-lfu",
			}
		},
	},
	{
		name: "redis", image: redisImage,
		runArgs: func(net string) []string {
			return append(dockerRunBase("redis", redisImage, net), redisServerArgs("redis-server")...)
		},
	},
	{
		name: "valkey", image: valkeyImage,
		runArgs: func(net string) []string {
			return append(dockerRunBase("valkey", valkeyImage, net), redisServerArgs("valkey-server")...)
		},
	},
	{
		name: "pogocache", image: pogocacheImage,
		// pogocache: -h 0.0.0.0 -p <port>; --evict yes evicts at the ceiling (it has no
		// LFU — the closest equivalence, documented in the results caveats).
		runArgs: func(net string) []string {
			return append(dockerRunBase("pogocache", pogocacheImage, net),
				"-h", "0.0.0.0", "-p", fmt.Sprintf("%d", serverPort),
				"--maxmemory", fmt.Sprintf("%d", maxmemoryBytes), "--evict", "yes")
		},
	},
}
