// Command cachemoney is the entrypoint for the cachemoney key-value store.
//
// At milestone M0 the network server is not yet wired up; this binary reports
// build metadata and exits. The RESP codec and TCP server are the next slice.
package main

import (
	"flag"
	"fmt"
	"runtime"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	fmt.Printf(
		"cachemoney %s (%s/%s)\n"+
			"M0: in-memory store ready; network server not yet wired up.\n"+
			"Next: RESP codec + TCP server. See README.md roadmap.\n",
		version, runtime.GOOS, runtime.GOARCH,
	)
}
