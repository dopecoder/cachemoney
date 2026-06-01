package bench

import (
	"fmt"
	"sort"
	"strings"
)

// canonicalOrder fixes the server row order so the rendered table is deterministic and
// reads in the project's narrative order; unknown servers sort after the known ones, then
// alphabetically.
var canonicalOrder = map[string]int{"cachemoney": 0, "redis": 1, "valkey": 2, "pogocache": 3}

func serverRank(name string) int {
	if r, ok := canonicalOrder[name]; ok {
		return r
	}
	return len(canonicalOrder)
}

// RenderTable renders a Suite as a deterministic markdown comparison table (servers ×
// metrics) with a footnote listing any skipped servers. The same Suite always renders
// byte-identically: results are sorted into the canonical server order and skipped names
// are sorted, independent of input order.
func RenderTable(s Suite) string {
	results := make([]Result, len(s.Results))
	copy(results, s.Results)
	sort.SliceStable(results, func(i, j int) bool {
		if ri, rj := serverRank(results[i].Server), serverRank(results[j].Server); ri != rj {
			return ri < rj
		}
		return results[i].Server < results[j].Server
	})

	var b strings.Builder
	b.WriteString("| Server | GET rps | SET rps | Hit ratio | GET p50 ms | GET p99 ms | p99.9 ms |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, r := range results {
		// p50/p99 are the raw redis-benchmark GET percentiles; p99.9 is memtier's
		// under-eviction tail (redis-benchmark does not emit p99.9) — design §5.3.
		p50, p99 := getLatency(r.Throughput, "GET")
		hit, p999 := "—", "—"
		if r.Hit != nil {
			hit = fmt.Sprintf("%.4f", r.Hit.HitRatio)
			p999 = fmtLat(r.Hit.Lat.P999)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			r.Server, commandRPS(r.Throughput, "GET"), commandRPS(r.Throughput, "SET"),
			hit, fmtLat(p50), fmtLat(p99), p999)
	}
	if len(s.Skipped) > 0 {
		skipped := make([]string, len(s.Skipped))
		copy(skipped, s.Skipped)
		sort.Strings(skipped)
		fmt.Fprintf(&b, "\n_Skipped (tooling unavailable): %s._\n", strings.Join(skipped, ", "))
	}
	return b.String()
}

// commandRPS renders the requests/sec for cmd, or an em dash when the command is absent.
func commandRPS(trs []ThroughputResult, cmd string) string {
	for _, tr := range trs {
		if tr.Command == cmd {
			return fmt.Sprintf("%.0f", tr.RPS)
		}
	}
	return "—"
}

// getLatency returns the p50/p99 of cmd from the redis-benchmark throughput results (0,0
// when absent — e.g. the minimal --csv form without percentiles).
func getLatency(trs []ThroughputResult, cmd string) (p50, p99 float64) {
	for _, tr := range trs {
		if tr.Command == cmd {
			return tr.P50, tr.P99
		}
	}
	return 0, 0
}

// fmtLat renders a latency in ms, or an em dash for a non-positive (absent) value.
func fmtLat(v float64) string {
	if v <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.3f", v)
}
