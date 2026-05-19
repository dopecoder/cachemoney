// Command cachemoney is the entrypoint for the cachemoney key-value store: a
// Redis-wire-compatible, in-memory cache server.
//
// main is a thin shell: it parses flags/environment into a server.Config
// (parseConfig), wires SIGINT/SIGTERM to a context, and hands the listen →
// serve → graceful-shutdown lifecycle to run. The two pieces are split so the
// pure configuration mapping is unit-testable and the lifecycle is
// integration-tested against a real server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/server"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg, showVersion, err := parseConfig(os.Args[1:], os.LookupEnv)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	case showVersion:
		fmt.Println(version)
		return
	}

	cfg.Version = version // report the build version in the HELLO handshake
	srv := server.New(cache.New(), cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	log.Printf("cachemoney %s listening on %s", version, cfg.Addr)
	err = run(ctx, srv, cfg.DrainTimeout)
	stop() // release the signal handler before any fatal exit

	if err != nil {
		log.Fatalf("cachemoney: %v", err)
	}
}

// parseConfig maps command-line arguments and the environment into a
// server.Config plus the -version flag. It does no I/O — the environment is read
// through the injected lookupEnv — so it is a pure unit. Precedence is
// flag > environment > built-in default for each knob.
func parseConfig(args []string, lookupEnv func(string) (string, bool)) (server.Config, bool, error) {
	fs := flag.NewFlagSet("cachemoney", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	addr := fs.String("addr", envString(lookupEnv, "CACHEMONEY_ADDR", ":6379"),
		"listen address")
	idle := fs.Duration("idle-timeout", envDuration(lookupEnv, "CACHEMONEY_IDLE_TIMEOUT", 0),
		"idle connection timeout (0 disables)")
	maxConns := fs.Int("max-conns", envInt(lookupEnv, "CACHEMONEY_MAX_CONNS", 10000),
		"maximum concurrent connections")
	drain := fs.Duration("drain-timeout", envDuration(lookupEnv, "CACHEMONEY_DRAIN_TIMEOUT", 5*time.Second),
		"graceful-shutdown drain timeout")
	showVersion := fs.Bool("version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return server.Config{}, false, err
	}
	cfg := server.Config{
		Addr:         *addr,
		IdleTimeout:  *idle,
		MaxConns:     *maxConns,
		DrainTimeout: *drain,
	}
	return cfg, *showVersion, nil
}

// envString returns the environment value for key, or def when unset.
func envString(lookupEnv func(string) (string, bool), key, def string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	return def
}

// envInt returns the integer environment value for key, or def when unset or
// unparseable (a malformed override falls back rather than failing startup).
func envInt(lookupEnv func(string) (string, bool), key string, def int) int {
	if v, ok := lookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envDuration returns the duration environment value for key, or def when unset or
// unparseable.
func envDuration(lookupEnv func(string) (string, bool), key string, def time.Duration) time.Duration {
	if v, ok := lookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// run owns the listen → serve → graceful-shutdown lifecycle. It serves until ctx
// is cancelled (a SIGINT/SIGTERM delivered by main's signal context) and then
// drains bounded by drain; a listener that fails to bind is returned immediately.
// A clean stop (ErrServerClosed) is reported as nil. run is integration-tested
// against a real server.
func run(ctx context.Context, srv *server.Server, drain time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if errors.Is(err, server.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), drain)
		defer cancel()
		return srv.Shutdown(shCtx)
	}
}
