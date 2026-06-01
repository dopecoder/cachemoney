package bench

import (
	"strings"
	"testing"
)

// These tests close the remaining branches: the typed-error message, the negative/invalid
// inputs, the optional-percentile fallback, and the table's unknown-server / missing-data
// paths.

func TestParseErrorMessage(t *testing.T) {
	t.Parallel()

	_, err := ParseMemtier([]byte(""))
	if err == nil || !strings.Contains(err.Error(), "memtier") {
		t.Fatalf("ParseError message = %v, want it to name the tool", err)
	}
}

func TestParseRedisBenchNegativeRPSIsError(t *testing.T) {
	t.Parallel()

	if _, err := ParseRedisBench([]byte("\"GET\",\"-5\"\r\n")); err == nil {
		t.Fatal("negative rps: want error, got nil")
	}
}

func TestParseRedisBenchInvalidCSVIsError(t *testing.T) {
	t.Parallel()

	// A bare quote in a non-quoted field makes encoding/csv's ReadAll fail.
	if _, err := ParseRedisBench([]byte("a\"b\n")); err == nil {
		t.Fatal("invalid CSV: want error, got nil")
	}
}

func TestParseRedisBenchNonNumericPercentileDefaultsToZero(t *testing.T) {
	t.Parallel()

	in := []byte("\"test\",\"rps\",\"p50_latency_ms\"\r\n\"GET\",\"100\",\"notanumber\"\r\n")
	got, err := ParseRedisBench(in)
	if err != nil {
		t.Fatalf("ParseRedisBench: %v", err)
	}
	if len(got) != 1 || got[0].RPS != 100 || got[0].P50 != 0 {
		t.Fatalf("non-numeric percentile not defaulted to 0: %+v", got)
	}
}

func TestParseMemtierNegativeRateIsError(t *testing.T) {
	t.Parallel()

	if _, err := ParseMemtier([]byte(`{"ALL STATS":{"Totals":{"Hits/sec":-1,"Misses/sec":5}}}`)); err == nil {
		t.Fatal("negative hit/miss rate: want error, got nil")
	}
}

func TestParseRedisBenchNonNumericDataRowIsError(t *testing.T) {
	t.Parallel()

	// A valid header followed by a data row whose rps is non-numeric → error (the
	// data-row parse branch, distinct from a headerless first row).
	in := []byte("\"test\",\"rps\"\r\n\"GET\",\"notanumber\"\r\n")
	if _, err := ParseRedisBench(in); err == nil {
		t.Fatal("non-numeric rps in a data row: want error, got nil")
	}
}

func TestParseRedisBenchNonFiniteRPSIsError(t *testing.T) {
	t.Parallel()

	// strconv.ParseFloat accepts nan/inf; a non-finite rps must still be a typed error,
	// not a NaN/Inf silently in the table.
	for _, in := range []string{"\"GET\",\"nan\"\r\n", "\"GET\",\"inf\"\r\n", "\"GET\",\"+Inf\"\r\n"} {
		if _, err := ParseRedisBench([]byte(in)); err == nil {
			t.Errorf("ParseRedisBench(%q): want error for non-finite rps, got nil", in)
		}
	}
}

func TestRenderTableUnknownServerAndMissingData(t *testing.T) {
	t.Parallel()

	s := Suite{
		Results: []Result{
			{Server: "memcached"}, // unknown → sorts last; no Hit, no throughput → em dashes
			{Server: "aerospike"}, // a second unknown → same-rank alpha tiebreaker
			{Server: "cachemoney", Throughput: []ThroughputResult{{Command: "GET", RPS: 1}}}, // known, no SET, no Hit
		},
	}
	out := RenderTable(s)
	ci, mi := strings.Index(out, "cachemoney"), strings.Index(out, "memcached")
	if ci < 0 || mi < 0 || ci > mi {
		t.Fatalf("unknown server should sort after known ones:\n%s", out)
	}
	if ai := strings.Index(out, "aerospike"); ai < 0 || ai > mi {
		t.Fatalf("same-rank servers should sort alphabetically:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Fatalf("missing-data cells should render as em dashes:\n%s", out)
	}
}
