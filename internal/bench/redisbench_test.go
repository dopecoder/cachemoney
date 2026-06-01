package bench

import (
	"os"
	"testing"
)

// The parsers are the strict-TDD core: captured tool output → exact structured values,
// a typed error (never a panic) on anything malformed. Fixtures in testdata/ pin the
// pinned-version output formats.

func TestParseRedisBenchFixture(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/redis-benchmark.csv")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := ParseRedisBench(raw)
	if err != nil {
		t.Fatalf("ParseRedisBench: %v", err)
	}
	want := []ThroughputResult{
		{Command: "GET", RPS: 185874.16, P50: 0.207, P99: 0.567},
		{Command: "SET", RPS: 178253.12, P50: 0.215, P99: 0.583},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("result %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestParseRedisBenchMinimalNoHeader(t *testing.T) {
	t.Parallel()

	// The historical minimal --csv form: just "test","rps" with no header and no
	// percentiles. rps must still parse; percentiles default to 0 (sourced from memtier).
	got, err := ParseRedisBench([]byte("\"GET\",\"123456.78\"\r\n\"SET\",\"99000.00\"\r\n"))
	if err != nil {
		t.Fatalf("ParseRedisBench minimal: %v", err)
	}
	if len(got) != 2 || got[0].Command != "GET" || got[0].RPS != 123456.78 || got[0].P50 != 0 {
		t.Fatalf("minimal parse = %+v", got)
	}
}

func TestParseRedisBenchErrors(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"empty":        []byte(""),
		"blank":        []byte("   \n"),
		"non-numeric":  []byte("\"GET\",\"notanumber\"\r\n"),
		"too-few-cols": []byte("\"GET\"\r\n"),
		"header-only":  []byte("\"test\",\"rps\"\r\n"),
	}
	for name, in := range cases {
		if _, err := ParseRedisBench(in); err == nil {
			t.Errorf("ParseRedisBench(%s): want error, got nil", name)
		}
	}
}
