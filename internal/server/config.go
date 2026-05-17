package server

import "time"

// Default Config values. They mirror Redis so a cachemoney server behaves like a
// stock redis-server out of the box (default port, maxclients, timeout disabled).
const (
	defaultAddr         = ":6379"
	defaultMaxConns     = 10000
	defaultDrainTimeout = 5 * time.Second
	defaultVersion      = "0.1.0"
)

// Config holds connection-lifecycle tuning. The zero value is valid: defaults()
// fills Addr, MaxConns, DrainTimeout, Version, and Now, and leaves IdleTimeout at 0
// (disabled, Redis parity).
type Config struct {
	// Addr is the listen address used by ListenAndServe; default ":6379".
	Addr string

	// IdleTimeout bounds how long a connection may be idle between commands.
	// Zero (the default) disables the idle timeout.
	IdleTimeout time.Duration

	// MaxConns caps concurrent connections; default 10000. A value <= 0 means default.
	MaxConns int

	// DrainTimeout is the bounded graceful-shutdown window used by the binary's
	// run loop; default 5s.
	DrainTimeout time.Duration

	// Version is reported by the HELLO handshake's "version" field; default "0.1.0".
	Version string

	// Now is the clock used for absolute-expiry (EXAT/PXAT) conversion; default
	// time.Now. Tests inject a fake clock that is shared with the engine.
	Now func() time.Time
}

// defaults returns a copy of c with unset fields filled in. IdleTimeout is left as
// given (0 means disabled).
func (c Config) defaults() Config {
	if c.Addr == "" {
		c.Addr = defaultAddr
	}
	if c.MaxConns <= 0 {
		c.MaxConns = defaultMaxConns
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = defaultDrainTimeout
	}
	if c.Version == "" {
		c.Version = defaultVersion
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}
