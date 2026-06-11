package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/dopecoder/cachemoney/internal/bench"
)

// lookup answers Docker/image availability; injectable so the planner is unit-testable
// without a daemon.
type lookup struct {
	hasDocker func() bool
	hasImage  func(image string) bool
}

func realLookup() lookup {
	return lookup{
		hasDocker: func() bool {
			if _, err := exec.LookPath("docker"); err != nil {
				return false
			}
			return exec.Command("docker", "info").Run() == nil
		},
		hasImage: func(image string) bool {
			if exec.Command("docker", "image", "inspect", image).Run() == nil {
				return true
			}
			fmt.Fprintf(os.Stderr, "pulling %s (first run may take a while)...\n", image)
			pull := exec.Command("docker", "pull", image)
			pull.Stdout, pull.Stderr = os.Stderr, os.Stderr
			return pull.Run() == nil
		},
	}
}

// available reports whether Docker is usable and every given image is present/pullable.
func (lk lookup) available(images ...string) bool {
	if !lk.hasDocker() {
		return false
	}
	for _, img := range images {
		if img != "" && !lk.hasImage(img) {
			return false
		}
	}
	return true
}

type planned struct{ spec serverSpec }

type plan struct {
	run     []planned
	skipped []string
}

// makePlan partitions the servers into runnable vs skipped based on Docker + image
// availability. A server is skipped (never measured) when Docker is absent or any image
// it needs — its own, the redis tool/probe image, or the memtier image — is unavailable.
func makePlan(specs []serverSpec, lk lookup) plan {
	var p plan
	for _, s := range specs {
		if lk.available(s.images()...) {
			p.run = append(p.run, planned{s})
		} else {
			p.skipped = append(p.skipped, s.name)
		}
	}
	return p
}

type measurer func(p planned) (bench.Result, error)

// runCompare benchmarks each runnable server in sequence (one at a time: a server is
// started, measured, and stopped before the next begins) and collects a Suite. Servers
// that are unavailable, or whose measurement errors, are recorded as skipped rather than
// fabricated.
func runCompare(out io.Writer, specs []serverSpec, lk lookup, measure measurer) bench.Suite {
	p := makePlan(specs, lk)
	suite := bench.Suite{Skipped: append([]string(nil), p.skipped...)}
	for _, name := range p.skipped {
		_, _ = fmt.Fprintf(out, "skipping %s: Docker or its pinned image is unavailable\n", name)
	}
	for _, pl := range p.run {
		_, _ = fmt.Fprintf(out, "benchmarking %s...\n", pl.spec.name)
		res, err := measure(pl)
		if err != nil {
			_, _ = fmt.Fprintf(out, "skipping %s: %v\n", pl.spec.name, err)
			suite.Skipped = append(suite.Skipped, pl.spec.name)
			continue
		}
		suite.Results = append(suite.Results, res)
	}
	return suite
}

// ensureNetwork creates the bench bridge if it does not already exist.
func ensureNetwork(name string) error {
	if exec.Command("docker", "network", "inspect", name).Run() == nil {
		return nil
	}
	return exec.Command("docker", "network", "create", name).Run()
}

// removeNetwork tears the bench bridge down (best-effort).
func removeNetwork(name string) { _ = exec.Command("docker", "network", "rm", name).Run() }

// cleanupContainers force-removes any leftover bench server containers from a prior run
// (best-effort), so a re-run starts clean and the network can be removed.
func cleanupContainers(specs []serverSpec) {
	for _, s := range specs {
		_ = exec.Command("docker", "rm", "-f", containerName(s.name)).Run() //nolint:gosec // fixed internal container name, not external input
	}
}

// filterServers narrows specs to the comma-separated names in only (e.g. BENCH_ONLY=redis
// to benchmark a single product). An empty/blank value keeps every server. Names matching
// no server are reported to warn (a typo'd BENCH_ONLY otherwise silently runs nothing).
func filterServers(specs []serverSpec, only string, warn io.Writer) []serverSpec {
	only = strings.TrimSpace(only)
	if only == "" {
		return specs
	}
	known := map[string]bool{}
	for _, s := range specs {
		known[s.name] = true
	}
	want := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
			if !known[n] {
				_, _ = fmt.Fprintf(warn, "warning: BENCH_ONLY name %q matches no server (known: %s)\n", n, serverNames(specs))
			}
		}
	}
	out := make([]serverSpec, 0, len(specs))
	for _, s := range specs {
		if want[s.name] {
			out = append(out, s)
		}
	}
	return out
}

// serverNames joins spec names for diagnostics.
func serverNames(specs []serverSpec) string {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.name
	}
	return strings.Join(names, ", ")
}
