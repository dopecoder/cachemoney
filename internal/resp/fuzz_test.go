package resp_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/dopecoder/cachemoney/internal/resp"
)

// fuzzSeeds spans the valid frames (§5/§8), the malformed inputs (§10),
// truncations, and huge-length declarations — the corpus the robustness contract
// must survive.
var fuzzSeeds = []string{
	// valid frames
	"*1\r\n$4\r\nPING\r\n",
	"*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nEX\r\n$2\r\n30\r\n",
	"*1\r\n$0\r\n\r\n",
	"*0\r\n",
	"*2\r\n$4\r\nECHO\r\n$2\r\nhi\r\n",
	"*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nPING\r\n",
	// malformed / typed-error inputs
	"PING\r\n",
	"*1\r\n:5\r\n",
	"*1\r\n$xy\r\n",
	"*1\r\n$-1\r\n",
	"*-1\r\n",
	"*1\r\n$4\r\nPINGXX",
	"\r\n",
	"*1\n",
	// truncations
	"*1",
	"*1\r\n$4",
	"*1\r\n$4\r\nPI",
	"*1\r\n$4\r\nPING",
	"*1\r\n$4\r\nPING\r",
	// huge-length declarations
	"*1\r\n$2000000000\r\n",
	"*100000000\r\n",
	"*99999999999999999999\r\n",
	"$" + "9999999999999999999999999999\r\n",
}

// drain loops ReadCommand to completion, asserting the robustness contract: no
// panic (implicit), every terminal error is one of io.EOF / io.ErrUnexpectedEOF /
// *ProtocolError, the loop makes progress and terminates, and small limits bound
// allocation.
func drain(t *testing.T, src io.Reader, budget int) {
	t.Helper()
	r := resp.NewReader(src, resp.WithMaxBulkLen(4096), resp.WithMaxMultibulkLen(1024))
	for i := 0; i <= budget+2; i++ {
		args, err := r.ReadCommand()
		if err == nil {
			if args == nil {
				t.Fatalf("ReadCommand returned nil args with a nil error")
			}
			continue
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return
		}
		var pe *resp.ProtocolError
		if errors.As(err, &pe) {
			return
		}
		t.Fatalf("unexpected error type %T: %v", err, err)
	}
	t.Fatalf("decoder did not terminate within %d reads", budget+2)
}

// FuzzReadCommand is the executable form of the decoder robustness contract. Each
// input is driven to completion via a bytes.Reader and, in a second pass, via a
// 1-byte chunked reader to exercise every resumption boundary.
func FuzzReadCommand(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		drain(t, bytes.NewReader(data), len(data))
		drain(t, &chunkedReader{data: data, chunk: 1}, len(data))
	})
}
