package bench

import (
	"bytes"
	"encoding/csv"
	"math"
	"strconv"
	"strings"
)

// ParseRedisBench parses redis-benchmark --csv output into per-command throughput
// results. It handles both the minimal `"test","rps"` form and the extended,
// header-prefixed form with latency columns (p50/p99 are filled when present, else 0 —
// the comparison table sources latency from memtier). Malformed, empty, or unexpected
// input yields a *ParseError; it never panics.
func ParseRedisBench(data []byte) ([]ThroughputResult, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // rows may differ; we validate column counts ourselves
	records, err := r.ReadAll()
	if err != nil {
		return nil, &ParseError{Tool: "redis-benchmark", Msg: "invalid CSV: " + err.Error()}
	}
	records = nonEmptyRecords(records)
	if len(records) == 0 {
		return nil, &ParseError{Tool: "redis-benchmark", Msg: "no records"}
	}

	rpsCol, p50Col, p99Col, start := 1, -1, -1, 0
	if isHeaderRow(records[0]) {
		var ok bool
		if rpsCol, p50Col, p99Col, ok = mapColumns(records[0]); !ok {
			return nil, &ParseError{Tool: "redis-benchmark", Msg: "header has no rps column"}
		}
		start = 1
	}
	if start >= len(records) {
		return nil, &ParseError{Tool: "redis-benchmark", Msg: "no data rows"}
	}

	out := make([]ThroughputResult, 0, len(records)-start)
	for _, rec := range records[start:] {
		if len(rec) <= rpsCol {
			return nil, &ParseError{Tool: "redis-benchmark", Msg: "row has too few columns"}
		}
		rps, err := strconv.ParseFloat(strings.TrimSpace(rec[rpsCol]), 64)
		if err != nil {
			return nil, &ParseError{Tool: "redis-benchmark", Msg: "non-numeric rps: " + rec[rpsCol]}
		}
		if rps < 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
			return nil, &ParseError{Tool: "redis-benchmark", Msg: "rps is not a finite non-negative number"}
		}
		out = append(out, ThroughputResult{
			Command: strings.TrimSpace(rec[0]),
			RPS:     rps,
			P50:     optFloat(rec, p50Col),
			P99:     optFloat(rec, p99Col),
		})
	}
	return out, nil
}

// nonEmptyRecords drops blank lines (a single whitespace-only field).
func nonEmptyRecords(records [][]string) [][]string {
	out := records[:0]
	for _, rec := range records {
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// isHeaderRow reports whether rec is a header (its rps field is non-numeric).
func isHeaderRow(rec []string) bool {
	if len(rec) < 2 {
		return false
	}
	_, err := strconv.ParseFloat(strings.TrimSpace(rec[1]), 64)
	return err != nil
}

// mapColumns finds the rps/p50/p99 column indices in a header by name; ok is false when
// no rps column is present.
func mapColumns(header []string) (rps, p50, p99 int, ok bool) {
	rps, p50, p99 = -1, -1, -1
	for i, h := range header {
		switch h = strings.ToLower(strings.TrimSpace(h)); {
		case h == "rps" || h == "throughput" || strings.Contains(h, "requests"):
			rps = i
		case strings.Contains(h, "p50"):
			p50 = i
		case strings.Contains(h, "p99") && !strings.Contains(h, "p99.9"):
			p99 = i
		}
	}
	return rps, p50, p99, rps >= 0
}

// optFloat parses an optional numeric column, returning 0 when absent or unparseable.
func optFloat(rec []string, col int) float64 {
	if col < 0 || col >= len(rec) {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(rec[col]), 64)
	if err != nil {
		return 0
	}
	return v
}
