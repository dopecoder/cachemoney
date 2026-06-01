package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/dopecoder/cachemoney/internal/bench"
)

// mode is how a server (or tool) can be run.
type mode int

const (
	modeSkip mode = iota
	modeLocal
	modeDocker
)

// lookup answers availability questions; it is injectable so the plan/skip logic is
// unit-testable without Docker or any installed binary.
type lookup struct {
	hasBinary func(name string) bool
	hasDocker func() bool
	hasImage  func(image string) bool
}

// realLookup probes the actual environment.
func realLookup() lookup {
	return lookup{
		hasBinary: func(n string) bool {
			if _, err := exec.LookPath(n); err == nil {
				return true
			}
			_, err := os.Stat(n) // also accept a relative path like bin/cachemoney
			return err == nil
		},
		hasDocker: func() bool { _, err := exec.LookPath("docker"); return err == nil },
		hasImage: func(image string) bool {
			return exec.Command("docker", "image", "inspect", image).Run() == nil
		},
	}
}

// serverMode chooses how a server can run: a present local binary wins; else a runnable
// pinned Docker image; else skip.
func (lk lookup) serverMode(s serverSpec) mode {
	if s.localBin != "" && lk.hasBinary(s.localBin) {
		return modeLocal
	}
	if s.image != "" && lk.hasDocker() && lk.hasImage(s.image) {
		return modeDocker
	}
	return modeSkip
}

// toolMode chooses how a bench tool can run: a present local binary, else its image via
// Docker, else skip.
func (lk lookup) toolMode(bin, image string) mode {
	if lk.hasBinary(bin) {
		return modeLocal
	}
	if image != "" && lk.hasDocker() && lk.hasImage(image) {
		return modeDocker
	}
	return modeSkip
}

type planned struct {
	spec serverSpec
	mode mode
}

// plan is the decided run set + the skipped server names.
type plan struct {
	run     []planned
	skipped []string
}

// makePlan decides, per server, whether it runs (and how) or is skipped — the pure core
// of skip-when-absent (spec Req 1).
func makePlan(specs []serverSpec, lk lookup) plan {
	var p plan
	for _, s := range specs {
		if m := lk.serverMode(s); m != modeSkip {
			p.run = append(p.run, planned{s, m})
		} else {
			p.skipped = append(p.skipped, s.name)
		}
	}
	return p
}

// measurer runs the bench tools against a started server and returns its result. It is the
// os/exec orchestration seam; tests inject a fake, and the real one (execMeasurer) runs only
// when the tooling is present (smoke-covered).
type measurer func(p planned) (bench.Result, error)

// runCompare produces the comparison Suite: it skips unavailable servers with a clear
// message (exit-0 contract) and measures the available ones via measure. With an all-absent
// lookup, measure is never called — no Docker/exec needed (the skip-when-absent path).
func runCompare(out io.Writer, specs []serverSpec, lk lookup, measure measurer) bench.Suite {
	p := makePlan(specs, lk)
	suite := bench.Suite{Skipped: append([]string(nil), p.skipped...)}
	for _, name := range p.skipped {
		_, _ = fmt.Fprintf(out, "skipping %s: no local binary and no runnable pinned Docker image\n", name)
	}
	for _, pl := range p.run {
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
