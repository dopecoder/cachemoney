package resp_test

import (
	"io"
	"testing"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// Benchmark methodology: results are reproducible on a quiescent machine at the
// default GOMAXPROCS (NumCPU). Run with:
//
//	go test -bench=. -benchmem -run=^$ ./internal/resp
//
// Reported figures are honest steady-state numbers; ns/op and allocs/op vary with
// hardware. The decode path's per-argument copy-out allocations are the figure a
// future buffer-pool optimization (design §6) would have to beat.

// loopReader yields frame's bytes endlessly, wrapping at the boundary, so a single
// Reader decodes the same frame repeatedly with no per-iteration setup allocation.
type loopReader struct {
	frame []byte
	pos   int
}

func (l *loopReader) Read(p []byte) (int, error) {
	if len(l.frame) == 0 {
		return 0, io.EOF
	}
	n := copy(p, l.frame[l.pos:])
	l.pos += n
	if l.pos == len(l.frame) {
		l.pos = 0
	}
	return n, nil
}

func BenchmarkDecode(b *testing.B) {
	frame := []byte("*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n$2\r\n30\r\n")
	r := resp.NewReader(&loopReader{frame: frame})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.ReadCommand(); err != nil {
			b.Fatalf("ReadCommand() error = %v", err)
		}
	}
}

// BenchmarkEncode emits a representative reply mix (a simple string, a bulk, and a
// small array) into an io.Discard-backed Writer, flushing each iteration.
func BenchmarkEncode(b *testing.B) {
	w := resp.NewWriter(io.Discard)
	bulk := []byte("value")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.WriteSimpleString("OK")
		_ = w.WriteBulk(bulk)
		_ = w.WriteArrayHeader(2)
		_ = w.WriteInt(1)
		_ = w.WriteBulkString("x")
		if err := w.Flush(); err != nil {
			b.Fatalf("Flush() error = %v", err)
		}
	}
}
