package resp

import "testing"

// White-box coverage for the unexported error-message lookup, including the
// out-of-range fallback that the black-box decode tests cannot reach (no decode
// path ever produces an out-of-range ErrKind).
func TestErrKindMessage(t *testing.T) {
	kinds := []ErrKind{
		KindExpectedArray,
		KindExpectedBulk,
		KindBadLength,
		KindMissingCRLF,
		KindBulkTooLong,
		KindMultibulkTooLong,
		KindLineTooLong,
	}
	for _, k := range kinds {
		if k.message() == "" {
			t.Errorf("message() empty for kind %d", k)
		}
	}
	if got := ErrKind(200).message(); got != "malformed frame" {
		t.Errorf("out-of-range message() = %q, want %q", got, "malformed frame")
	}
}

// TestParseInt locks the canonical non-negative length grammar: a lone "0" and
// 1-9-led runs are valid; empty, signs, leading zeros, stray non-digits, and
// overflowing values are rejected. White-box because parseInt is unexported and a
// few branches (e.g. a non-digit after a valid leading digit) are awkward to reach
// through a full decode.
func TestParseInt(t *testing.T) {
	valid := map[string]int{"0": 0, "1": 1, "123": 123, "512000000": 512000000}
	for in, want := range valid {
		if got, ok := parseInt([]byte(in)); !ok || got != want {
			t.Errorf("parseInt(%q) = (%d, %v), want (%d, true)", in, got, ok, want)
		}
	}
	invalid := []string{"", "-1", "-0", "+1", "00", "007", "x", "1x", "99999999999999999999"}
	for _, in := range invalid {
		if got, ok := parseInt([]byte(in)); ok {
			t.Errorf("parseInt(%q) = (%d, true), want (_, false)", in, got)
		}
	}
}
