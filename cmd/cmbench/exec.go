package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dopecoder/cachemoney/internal/bench"
)

// toolset records how the bench tools can be run (local binary or via a pinned image).
// redis-benchmark ships inside the Redis image, so it reuses REDIS_IMAGE.
type toolset struct {
	redisBench   mode
	memtier      mode
	redisImage   string
	memtierImage string
}

// realTools probes the environment for redis-benchmark + memtier_benchmark.
func realTools(lk lookup) toolset {
	redisImage := env("REDIS_IMAGE", "redis:7.4")
	memtierImage := env("MEMTIER_IMAGE", "redislabs/memtier_benchmark:2.1.1")
	return toolset{
		redisBench:   lk.toolMode("redis-benchmark", redisImage),
		memtier:      lk.toolMode("memtier_benchmark", memtierImage),
		redisImage:   redisImage,
		memtierImage: memtierImage,
	}
}

// execMeasurer returns a measurer that starts a server, runs whichever tools are available
// R times (median reported), and parses the output. It is the os/exec seam — exercised only
// when the tooling is present (smoke-covered); the skip-when-absent path never reaches it.
func execMeasurer(tools toolset) measurer {
	return func(p planned) (bench.Result, error) {
		if tools.redisBench == modeSkip && tools.memtier == modeSkip {
			return bench.Result{}, fmt.Errorf("no redis-benchmark or memtier_benchmark available")
		}
		stop, err := startServer(p)
		if err != nil {
			return bench.Result{}, err
		}
		defer stop()

		addr := fmt.Sprintf("localhost:%d", p.spec.port)
		if err := waitReady(addr, 10*time.Second); err != nil {
			return bench.Result{}, err
		}

		res := bench.Result{Server: p.spec.name}
		if tools.redisBench != modeSkip {
			tr, err := medianRedisBench(tools, p.spec.port)
			if err != nil {
				return bench.Result{}, err
			}
			res.Throughput = tr
		}
		if tools.memtier != modeSkip {
			hit, err := medianMemtier(tools, p.spec.port)
			if err != nil {
				return bench.Result{}, err
			}
			res.Hit = &hit
		}
		return res, nil
	}
}

// startServer launches the server (local process or detached container) and returns a
// teardown. For a container the teardown stops it; for a local process it kills it.
func startServer(p planned) (stop func(), err error) {
	argv := p.spec.startCmd(p.mode, p.spec)
	if p.mode == modeDocker {
		out, err := exec.Command(argv[0], argv[1:]...).Output() //nolint:gosec // argv from fixed internal serverSpec, not external input
		if err != nil {
			return nil, fmt.Errorf("docker run %s: %w", p.spec.name, err)
		}
		id := strings.TrimSpace(string(out))
		return func() { _ = exec.Command("docker", "stop", id).Run() }, nil //nolint:gosec // id is the container this process just started
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv from fixed internal serverSpec, not external input
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", p.spec.name, err)
	}
	return func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }, nil
}

// waitReady dials addr and PINGs over RESP until it gets +PONG or the timeout elapses.
func waitReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pingOK(addr) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("server at %s not ready within %s", addr, timeout)
}

func pingOK(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return false
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "+PONG")
}

// medianRedisBench discards a warmup pass, then runs R measured repeats and returns the
// per-command median of rps/p50/p99 (spec Req 4: discard a warmup, median the repeats).
func medianRedisBench(tools toolset, port int) ([]bench.ThroughputResult, error) {
	if _, err := runRedisBenchOnce(tools, port); err != nil { // warmup pass, discarded
		return nil, err
	}
	runs := make([][]bench.ThroughputResult, 0, repeats)
	for i := 0; i < repeats; i++ {
		trs, err := runRedisBenchOnce(tools, port)
		if err != nil {
			return nil, err
		}
		runs = append(runs, trs)
	}
	return bench.MedianThroughput(runs), nil
}

func runRedisBenchOnce(tools toolset, port int) ([]bench.ThroughputResult, error) {
	argv := redisBenchArgv(tools, port)
	out, err := exec.Command(argv[0], argv[1:]...).Output() //nolint:gosec // argv from fixed internal tool config, not external input
	if err != nil {
		return nil, fmt.Errorf("redis-benchmark: %w", err)
	}
	return bench.ParseRedisBench(out)
}

// medianMemtier discards a warmup pass (which primes the cache), then runs R measured
// repeats and returns the repeat whose hit ratio is the median.
func medianMemtier(tools toolset, port int) (bench.HitRatioResult, error) {
	if _, err := runMemtierParsed(tools, port); err != nil { // warmup pass, discarded
		return bench.HitRatioResult{}, err
	}
	runs := make([]bench.HitRatioResult, 0, repeats)
	for i := 0; i < repeats; i++ {
		hit, err := runMemtierParsed(tools, port)
		if err != nil {
			return bench.HitRatioResult{}, err
		}
		runs = append(runs, hit)
	}
	return bench.MedianHitRatio(runs), nil
}

func runMemtierParsed(tools toolset, port int) (bench.HitRatioResult, error) {
	out, err := runMemtier(tools, port)
	if err != nil {
		return bench.HitRatioResult{}, err
	}
	return bench.ParseMemtier(out)
}

// redisBenchArgv builds the redis-benchmark command (local or via the Redis image).
func redisBenchArgv(tools toolset, port int) []string {
	base := []string{
		"redis-benchmark", "-h", "localhost", "-p", fmt.Sprintf("%d", port),
		"-t", "get,set", "-n", fmt.Sprintf("%d", redisRequests),
		"-c", fmt.Sprintf("%d", redisClients), "-d", fmt.Sprintf("%d", valueSize), "--csv",
	}
	if tools.redisBench == modeDocker {
		return append([]string{"docker", "run", "--rm", "--network", "host", tools.redisImage}, base...)
	}
	return base
}

// runMemtier runs memtier with a host-mounted output dir (so the JSON lands on the host even
// when running via Docker) and returns the JSON bytes.
func runMemtier(tools toolset, port int) ([]byte, error) {
	dir, err := os.MkdirTemp("", "memtier")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	jsonPath := filepath.Join(dir, "out.json")

	stddev := fmt.Sprintf("%d", memtierKeyMax/100) // ~1% hot set
	args := []string{
		"-s", "localhost", "-p", fmt.Sprintf("%d", port), "--protocol=redis",
		"--ratio=1:10", "--key-pattern=G:G", "--key-stddev=" + stddev,
		fmt.Sprintf("--key-maximum=%d", memtierKeyMax),
		"-n", fmt.Sprintf("%d", memtierOps), "-c", fmt.Sprintf("%d", memtierClients),
		"-t", fmt.Sprintf("%d", memtierThreads), "--hide-histogram",
	}
	var argv []string
	if tools.memtier == modeDocker {
		argv = append([]string{
			"docker", "run", "--rm", "--network", "host",
			"-v", dir + ":/out", tools.memtierImage,
		}, args...)
		argv = append(argv, "--json-out-file=/out/out.json")
	} else {
		argv = append([]string{"memtier_benchmark"}, args...)
		argv = append(argv, "--json-out-file="+jsonPath)
	}
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil { //nolint:gosec // argv from fixed internal tool config, not external input
		return nil, fmt.Errorf("memtier: %w", err)
	}
	return os.ReadFile(jsonPath) //nolint:gosec // jsonPath is a temp file this process just created
}
