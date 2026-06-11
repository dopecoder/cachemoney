package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dopecoder/cachemoney/internal/bench"
)

// execMeasurer is the os/exec seam: it starts a server on the bridge, probes readiness,
// runs redis-benchmark (throughput + p50/p99) and memtier (hit ratio + p99.9), each with
// a discarded warmup followed by `repeats` measured runs whose median is reported. It is
// exercised by the opt-in end-to-end smoke and by `make bench-compare`.
func execMeasurer(net string) measurer {
	return func(p planned) (bench.Result, error) {
		stop, err := startServer(p.spec, net)
		if err != nil {
			return bench.Result{}, err
		}
		defer stop()

		cname := containerName(p.spec.name)
		if err := waitReady(net, cname, 20*time.Second); err != nil {
			return bench.Result{}, err
		}
		tr, err := medianRedisBench(net, cname)
		if err != nil {
			return bench.Result{}, err
		}
		hit, err := medianMemtier(net, cname)
		if err != nil {
			return bench.Result{}, err
		}
		return bench.Result{Server: p.spec.name, Throughput: tr, Hit: &hit}, nil
	}
}

// startServer launches the server container and returns a stop func that removes it. A
// server declaring a localFile (e.g. cachemoney's mounted static binary) fails fast with a
// clear message when that file is absent, rather than starting a container that exits
// immediately and burning the readiness timeout.
func startServer(s serverSpec, net string) (func(), error) {
	if s.localFile != "" {
		if _, err := os.Stat(s.localFile); err != nil {
			return nil, fmt.Errorf("%s: required file %s not found (build it, e.g. via `make bench-compare`): %w", s.name, s.localFile, err)
		}
	}
	argv := s.runArgs(net)
	if err := exec.Command(argv[0], argv[1:]...).Run(); err != nil { //nolint:gosec // argv from fixed internal serverSpec, not external input
		return nil, fmt.Errorf("start %s: %w", s.name, err)
	}
	cname := containerName(s.name)
	return func() { _ = exec.Command("docker", "stop", cname).Run() }, nil //nolint:gosec // cname is a fixed internal container name, not external input
}

// waitReady probes the server by name from a throwaway redis-cli container on the bridge
// until it answers PONG or the timeout elapses.
func waitReady(net, cname string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "run", "--rm", "--network", net, redisImage, //nolint:gosec // fixed argv from internal config
			"redis-cli", "-h", cname, "-p", fmt.Sprintf("%d", serverPort), "ping").Output()
		if strings.Contains(string(out), "PONG") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("server %s not ready within %s", cname, timeout)
}

func medianRedisBench(net, cname string) ([]bench.ThroughputResult, error) {
	if _, err := runRedisBenchOnce(net, cname); err != nil { // warmup, discarded
		return nil, err
	}
	runs := make([][]bench.ThroughputResult, 0, repeats)
	for range repeats {
		trs, err := runRedisBenchOnce(net, cname)
		if err != nil {
			return nil, err
		}
		runs = append(runs, trs)
	}
	return bench.MedianThroughput(runs), nil
}

func runRedisBenchOnce(net, cname string) ([]bench.ThroughputResult, error) {
	out, err := exec.Command("docker", "run", "--rm", "--network", net, redisImage, //nolint:gosec // fixed argv from internal config
		"redis-benchmark", "-h", cname, "-p", fmt.Sprintf("%d", serverPort),
		"-t", "get,set", "-n", fmt.Sprintf("%d", redisRequests),
		"-c", fmt.Sprintf("%d", redisClients), "-d", fmt.Sprintf("%d", valueSize), "--csv").Output()
	if err != nil {
		return nil, fmt.Errorf("redis-benchmark: %w", err)
	}
	return bench.ParseRedisBench(out)
}

func medianMemtier(net, cname string) (bench.HitRatioResult, error) {
	if _, err := runMemtierOnce(net, cname); err != nil { // warmup, discarded
		return bench.HitRatioResult{}, err
	}
	runs := make([]bench.HitRatioResult, 0, repeats)
	for range repeats {
		hit, err := runMemtierOnce(net, cname)
		if err != nil {
			return bench.HitRatioResult{}, err
		}
		runs = append(runs, hit)
	}
	return bench.MedianHitRatio(runs), nil
}

func runMemtierOnce(net, cname string) (bench.HitRatioResult, error) {
	dir, err := os.MkdirTemp("", "memtier")
	if err != nil {
		return bench.HitRatioResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cmd := exec.Command("docker", "run", "--rm", "--network", net, "-v", dir+":/out", memtierImage, //nolint:gosec // fixed argv from internal config
		"-s", cname, "-p", fmt.Sprintf("%d", serverPort), "--protocol=redis",
		"--ratio="+memtierRatio, "--key-pattern=R:G",
		fmt.Sprintf("--data-size=%d", memtierValueBytes),
		fmt.Sprintf("--key-maximum=%d", memtierKeyMax),
		fmt.Sprintf("--key-median=%d", memtierKeyMedian),
		fmt.Sprintf("--key-stddev=%d", memtierKeyStddev),
		"-n", fmt.Sprintf("%d", memtierOps), "-c", fmt.Sprintf("%d", memtierClients),
		"-t", fmt.Sprintf("%d", memtierThreads),
		"--print-percentiles", "50,99,99.9", "--json-out-file=/out/out.json")
	if err := cmd.Run(); err != nil {
		return bench.HitRatioResult{}, fmt.Errorf("memtier: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "out.json")) //nolint:gosec // temp file this process just created
	if err != nil {
		return bench.HitRatioResult{}, err
	}
	return bench.ParseMemtier(raw)
}
