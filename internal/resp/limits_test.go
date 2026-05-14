package resp_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// allocBytes reports the heap bytes allocated while running fn once (after a GC
// to settle the baseline). It is the rigorous "no over-allocation" proof: a
// declared-size allocation of the untrusted length would dwarf the threshold.
func allocBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// ---- Requirement: Bounded, DoS-aware parsing --------------------------------

func TestOverLimitBulkRejectedWithoutAllocation(t *testing.T) {
	t.Parallel()
	// ~2 GB declared bulk length against the 512 MB default — on a tiny buffer.
	const input = "*1\r\n$2000000000\r\n"

	r := resp.NewReader(strings.NewReader(input), resp.WithBufferSize(64))
	_, err := r.ReadCommand()
	var pe *resp.ProtocolError
	if !errors.As(err, &pe) || pe.Kind != resp.KindBulkTooLong {
		t.Fatalf("error = %v, want *ProtocolError{KindBulkTooLong}", err)
	}

	// Proof it never allocated the ~2 GB declared buffer.
	bytesUsed := allocBytes(func() {
		rr := resp.NewReader(strings.NewReader(input), resp.WithBufferSize(64))
		_, _ = rr.ReadCommand()
	})
	if bytesUsed > 1<<20 {
		t.Fatalf("allocated %d bytes for an over-limit bulk; want < 1 MiB", bytesUsed)
	}

	allocs := testing.AllocsPerRun(50, func() {
		rr := resp.NewReader(strings.NewReader(input), resp.WithBufferSize(64))
		_, _ = rr.ReadCommand()
	})
	if allocs > 8 {
		t.Fatalf("over-limit bulk path made %.1f allocs/run; want a small bounded count", allocs)
	}
}

func TestOverLimitMultibulkRejectedWithoutAllocation(t *testing.T) {
	t.Parallel()
	// 100M elements against the 1 Mi default — on a tiny buffer.
	const input = "*100000000\r\n"

	r := resp.NewReader(strings.NewReader(input), resp.WithBufferSize(64))
	_, err := r.ReadCommand()
	var pe *resp.ProtocolError
	if !errors.As(err, &pe) || pe.Kind != resp.KindMultibulkTooLong {
		t.Fatalf("error = %v, want *ProtocolError{KindMultibulkTooLong}", err)
	}

	bytesUsed := allocBytes(func() {
		rr := resp.NewReader(strings.NewReader(input), resp.WithBufferSize(64))
		_, _ = rr.ReadCommand()
	})
	if bytesUsed > 1<<20 {
		t.Fatalf("allocated %d bytes for an over-limit multibulk; want < 1 MiB", bytesUsed)
	}
}

func TestLimitsAreConfigurable(t *testing.T) {
	t.Parallel()
	// Custom limits far below the input's declared sizes.
	r := resp.NewReader(strings.NewReader("*1\r\n$11\r\n"),
		resp.WithMaxBulkLen(10), resp.WithMaxMultibulkLen(4))
	_, err := r.ReadCommand()
	var pe *resp.ProtocolError
	if !errors.As(err, &pe) || pe.Kind != resp.KindBulkTooLong {
		t.Fatalf("custom bulk limit: error = %v, want KindBulkTooLong", err)
	}

	r2 := resp.NewReader(strings.NewReader("*5\r\n"), resp.WithMaxMultibulkLen(4))
	_, err = r2.ReadCommand()
	if !errors.As(err, &pe) || pe.Kind != resp.KindMultibulkTooLong {
		t.Fatalf("custom multibulk limit: error = %v, want KindMultibulkTooLong", err)
	}
}

func TestLimitBoundaryAtVsOneOver(t *testing.T) {
	t.Parallel()
	// At the limit: the bulk header passes the limit check (it then hits
	// ErrUnexpectedEOF because the tiny buffer lacks the payload — NOT a limit
	// rejection).
	at := resp.NewReader(strings.NewReader("*1\r\n$4\r\n"), resp.WithMaxBulkLen(4))
	_, err := at.ReadCommand()
	var pe *resp.ProtocolError
	if errors.As(err, &pe) && pe.Kind == resp.KindBulkTooLong {
		t.Fatalf("at-limit length was rejected by the limit; want acceptance, got %v", err)
	}

	// One over the limit: rejected before any declared-size allocation.
	over := resp.NewReader(strings.NewReader("*1\r\n$5\r\n"), resp.WithMaxBulkLen(4))
	_, err = over.ReadCommand()
	if !errors.As(err, &pe) || pe.Kind != resp.KindBulkTooLong {
		t.Fatalf("one-over length: error = %v, want KindBulkTooLong", err)
	}
}

func TestZeroOptionValuesResolveToDefaults(t *testing.T) {
	t.Parallel()
	// Non-positive option values must resolve to the documented defaults, so a
	// length under the default is accepted and one over the default is rejected.
	r := resp.NewReader(strings.NewReader("*1\r\n$2000000000\r\n"),
		resp.WithMaxBulkLen(0), resp.WithMaxMultibulkLen(-1), resp.WithBufferSize(0))
	_, err := r.ReadCommand()
	var pe *resp.ProtocolError
	if !errors.As(err, &pe) || pe.Kind != resp.KindBulkTooLong {
		t.Fatalf("error = %v, want KindBulkTooLong against the default limit", err)
	}
	if resp.DefaultMaxBulkLen != 512*1024*1024 {
		t.Fatalf("DefaultMaxBulkLen = %d", resp.DefaultMaxBulkLen)
	}
	if resp.DefaultMaxMultibulkLen != 1024*1024 {
		t.Fatalf("DefaultMaxMultibulkLen = %d", resp.DefaultMaxMultibulkLen)
	}
}
