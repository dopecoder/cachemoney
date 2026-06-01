package bench

import (
	"math"
	"os"
	"testing"
)

func TestParseMemtierFixture(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/memtier.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := ParseMemtier(raw)
	if err != nil {
		t.Fatalf("ParseMemtier: %v", err)
	}
	want := HitRatioResult{
		HitsPerSec:   81000.0,
		MissesPerSec: 9000.0,
		HitRatio:     0.9,
		OpsPerSec:    95234.5,
		Lat:          Latency{P50: 0.420, P99: 1.310, P999: 3.780},
	}
	if math.Abs(got.HitRatio-want.HitRatio) > 1e-9 {
		t.Fatalf("HitRatio = %v, want %v", got.HitRatio, want.HitRatio)
	}
	if got.OpsPerSec != want.OpsPerSec || got.HitsPerSec != want.HitsPerSec ||
		got.MissesPerSec != want.MissesPerSec || got.Lat != want.Lat {
		t.Fatalf("ParseMemtier = %+v, want %+v", got, want)
	}
}

func TestParseMemtierZeroHitsMissesIsError(t *testing.T) {
	t.Parallel()

	// No GET hits/misses → the ratio is undefined; that must be a typed error, not a
	// divide-by-zero NaN.
	if _, err := ParseMemtier([]byte(`{"ALL STATS":{"Totals":{"Ops/sec":1.0}}}`)); err == nil {
		t.Fatal("ParseMemtier with zero hits+misses: want error, got nil")
	}
}

func TestParseMemtierMalformed(t *testing.T) {
	t.Parallel()

	for _, in := range [][]byte{[]byte(""), []byte("not json"), []byte("[1,2,3]"), []byte("{")} {
		if _, err := ParseMemtier(in); err == nil {
			t.Errorf("ParseMemtier(%q): want error, got nil", in)
		}
	}
}
