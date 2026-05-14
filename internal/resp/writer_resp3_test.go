package resp_test

import (
	"bytes"
	"math"
	"math/big"
	"testing"

	"github.com/dopecoder/cachemoney/internal/resp"
)

func bigFromString(t *testing.T, s string) *big.Int {
	t.Helper()
	x, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("invalid big number %q", s)
	}
	return x
}

// ---- Requirement: RESP3 reply encoding (byte-exact) -------------------------

func TestRESP3GoldenTable(t *testing.T) {
	t.Parallel()
	bignum := "123456789012345678901234567890"
	cases := []struct {
		name string
		want string
		emit func(t *testing.T, e ew)
	}{
		{"null", "_\r\n", func(_ *testing.T, e ew) { e.null() }},
		{
			"map",
			"%1\r\n$5\r\nproto\r\n:3\r\n",
			func(_ *testing.T, e ew) {
				e.mapHdr(1)
				e.bulkStr("proto")
				e.integer(3)
			},
		},
		{"bool true", "#t\r\n", func(_ *testing.T, e ew) { e.boolean(true) }},
		{"bool false", "#f\r\n", func(_ *testing.T, e ew) { e.boolean(false) }},
		{"double", ",3.14\r\n", func(_ *testing.T, e ew) { e.double(3.14) }},
		{
			"big number",
			"(" + bignum + "\r\n",
			func(t *testing.T, e ew) { e.bignum(bigFromString(t, bignum)) },
		},
		{
			"HELLO map",
			"%2\r\n$6\r\nserver\r\n$10\r\ncachemoney\r\n$5\r\nproto\r\n:3\r\n",
			func(_ *testing.T, e ew) {
				e.mapHdr(2)
				e.bulkStr("server")
				e.bulkStr("cachemoney")
				e.bulkStr("proto")
				e.integer(3)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encode(t, 3, func(e ew) { tc.emit(t, e) })
			if !bytes.Equal([]byte(tc.want), got) {
				t.Fatalf("bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRESP3DoubleSpecialValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    float64
		want string
	}{
		{"pos inf", math.Inf(1), ",inf\r\n"},
		{"neg inf", math.Inf(-1), ",-inf\r\n"},
		{"nan", math.NaN(), ",nan\r\n"},
		{"integral double", 3.0, ",3\r\n"},
		{"negative double", -0.5, ",-0.5\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encode(t, 3, func(e ew) { e.double(tc.f) })
			if !bytes.Equal([]byte(tc.want), got) {
				t.Fatalf("bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRESP3NegativeBigNumber(t *testing.T) {
	t.Parallel()
	got := encode(t, 3, func(e ew) {
		e.bignum(bigFromString(t, "-987654321098765432109876543210"))
	})
	want := "(-987654321098765432109876543210\r\n"
	if !bytes.Equal([]byte(want), got) {
		t.Fatalf("bytes = %q, want %q", got, want)
	}
}

// ---- RESP2 downgrades (same logical reply, version-selected bytes) ----------

func TestRESP2Downgrades(t *testing.T) {
	t.Parallel()
	bignum := "123456789012345678901234567890"
	cases := []struct {
		name string
		want string
		emit func(t *testing.T, e ew)
	}{
		{
			"map -> flat array",
			"*2\r\n$5\r\nproto\r\n:3\r\n",
			func(_ *testing.T, e ew) {
				e.mapHdr(1)
				e.bulkStr("proto")
				e.integer(3)
			},
		},
		{"bool true -> :1", ":1\r\n", func(_ *testing.T, e ew) { e.boolean(true) }},
		{"bool false -> :0", ":0\r\n", func(_ *testing.T, e ew) { e.boolean(false) }},
		{"double -> bulk", "$4\r\n3.14\r\n", func(_ *testing.T, e ew) { e.double(3.14) }},
		{
			"big number -> bulk",
			"$30\r\n" + bignum + "\r\n",
			func(t *testing.T, e ew) { e.bignum(bigFromString(t, bignum)) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := encode(t, 2, func(e ew) { tc.emit(t, e) })
			if !bytes.Equal([]byte(tc.want), got) {
				t.Fatalf("bytes = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- The dialect-switch oracle ----------------------------------------------

// One logical reply serialized under each flag on the SAME writer proves the
// per-connection version selects the bytes (§7.2/§8).
func TestDialectSwitchOracle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		emit      func(e ew)
		wantRESP2 string
		wantRESP3 string
	}{
		{"null", func(e ew) { e.null() }, "$-1\r\n", "_\r\n"},
		{"bool true", func(e ew) { e.boolean(true) }, ":1\r\n", "#t\r\n"},
		{"map header", func(e ew) { e.mapHdr(1) }, "*2\r\n", "%1\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			w := resp.NewWriter(&buf)
			e := ew{w}

			e.setProto(2)
			tc.emit(e)
			if err := w.Flush(); err != nil {
				t.Fatalf("RESP2 Flush() error = %v", err)
			}
			if got := buf.String(); got != tc.wantRESP2 {
				t.Fatalf("RESP2 bytes = %q, want %q", got, tc.wantRESP2)
			}

			buf.Reset()
			e.setProto(3)
			tc.emit(e)
			if err := w.Flush(); err != nil {
				t.Fatalf("RESP3 Flush() error = %v", err)
			}
			if got := buf.String(); got != tc.wantRESP3 {
				t.Fatalf("RESP3 bytes = %q, want %q", got, tc.wantRESP3)
			}
		})
	}
}

func TestSetProtoClamps(t *testing.T) {
	t.Parallel()
	// Below 2 clamps to RESP2, above 3 clamps to RESP3.
	low := encode(t, 0, func(e ew) {
		e.setProto(1)
		e.null()
	})
	if !bytes.Equal(low, []byte("$-1\r\n")) {
		t.Fatalf("SetProto(1) null = %q, want RESP2 $-1", low)
	}
	high := encode(t, 0, func(e ew) {
		e.setProto(9)
		e.null()
	})
	if !bytes.Equal(high, []byte("_\r\n")) {
		t.Fatalf("SetProto(9) null = %q, want RESP3 _", high)
	}
}
