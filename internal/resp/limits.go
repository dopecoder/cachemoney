package resp

const (
	// DefaultMaxBulkLen is the default ceiling on a single bulk-string payload,
	// 512 MB — matching Redis proto-max-bulk-len.
	DefaultMaxBulkLen = 512 * 1024 * 1024
	// DefaultMaxMultibulkLen is the default ceiling on a top-level array's element
	// count, 1 Mi — matching Redis's hard-coded multibulk cap.
	DefaultMaxMultibulkLen = 1024 * 1024

	// defaultBufferSize is the default bufio buffer size backing a Reader.
	defaultBufferSize = 16 * 1024
	// maxHeaderLine bounds a single '$'/'*' header line (prefix + decimal length +
	// CRLF). A valid line is at most ~22 bytes. Once a line terminator is seen, a
	// line longer than this cap is rejected; an attacker sending digits with no
	// terminator is bounded earlier by the bufio buffer (ErrBufferFull). Both paths
	// return KindLineTooLong, so pre-parse buffering is always bounded.
	maxHeaderLine = 64
)

// Limits bounds the resources a single ReadCommand may be asked to allocate. A
// non-positive field resolves to its documented default. The limits are the
// untrusted-input ceilings: a declared size over the limit is rejected before any
// allocation sized to the (untrusted) declared value.
type Limits struct {
	// MaxBulkLen caps a single bulk-string payload; <= 0 resolves to DefaultMaxBulkLen.
	MaxBulkLen int
	// MaxMultibulkLen caps the top-level array element count; <= 0 resolves to
	// DefaultMaxMultibulkLen.
	MaxMultibulkLen int
}

// Option configures a Reader at construction. Omitted options resolve to the
// Redis-compatible defaults.
type Option func(*config)

// config is the resolved Reader construction settings.
type config struct {
	limits     Limits
	bufferSize int
}

// WithMaxBulkLen sets the maximum accepted bulk-string payload length. A
// non-positive value resolves to DefaultMaxBulkLen.
func WithMaxBulkLen(n int) Option {
	return func(c *config) { c.limits.MaxBulkLen = n }
}

// WithMaxMultibulkLen sets the maximum accepted top-level array length. A
// non-positive value resolves to DefaultMaxMultibulkLen.
func WithMaxMultibulkLen(n int) Option {
	return func(c *config) { c.limits.MaxMultibulkLen = n }
}

// WithBufferSize sets the internal bufio buffer size. A non-positive value
// resolves to the 16 KiB default; bufio enforces its own 16-byte minimum.
func WithBufferSize(n int) Option {
	return func(c *config) { c.bufferSize = n }
}

// resolveConfig applies the options over the defaults and normalizes any
// non-positive value back to its default.
func resolveConfig(opts []Option) config {
	c := config{
		limits: Limits{
			MaxBulkLen:      DefaultMaxBulkLen,
			MaxMultibulkLen: DefaultMaxMultibulkLen,
		},
		bufferSize: defaultBufferSize,
	}
	for _, opt := range opts {
		opt(&c)
	}
	if c.limits.MaxBulkLen <= 0 {
		c.limits.MaxBulkLen = DefaultMaxBulkLen
	}
	if c.limits.MaxMultibulkLen <= 0 {
		c.limits.MaxMultibulkLen = DefaultMaxMultibulkLen
	}
	if c.bufferSize <= 0 {
		c.bufferSize = defaultBufferSize
	}
	return c
}
