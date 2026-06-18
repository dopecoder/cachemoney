#!/usr/bin/env python3
"""Summarize results.csv from orchestrate.py into per-experiment comparison tables.

Headline rows use only `valid` runs; rig-limited/noisy rows are listed separately so a
bad data point never silently becomes a conclusion (per BENCHMARK-PLAN.md).

Usage:  python3 analyze.py results.csv
"""

from __future__ import annotations

import csv
import sys
from collections import defaultdict


def load(path: str) -> list[dict]:
    with open(path, newline="") as fh:
        return list(csv.DictReader(fh))


def num(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def fmt(v, nd=3):
    n = num(v)
    return f"{n:.{nd}f}" if n is not None else "-"


def table(rows: list[dict], cols: list[tuple[str, str]]) -> str:
    head = "| " + " | ".join(h for _, h in cols) + " |"
    sep = "|" + "|".join("---" for _ in cols) + "|"
    out = [head, sep]
    for row in rows:
        out.append("| " + " | ".join(str(row.get(k, "-")) for k, _ in cols) + " |")
    return "\n".join(out)


def section_e1(rows: list[dict]) -> str:
    rows = [r for r in rows if r["experiment"] == "E1"]
    if not rows:
        return ""
    out = ["## E1 — Tail latency (valid rows)\n"]
    # group by data_size + rate, compare subjects on the tail
    groups = defaultdict(list)
    for r in rows:
        if r["valid"] != "valid":
            continue
        groups[(r["data_size"], r["rate_limit_pc"])].append(r)
    for (ds, rl), grp in sorted(
        groups.items(), key=lambda kv: (int(kv[0][0]), int(kv[0][1]))
    ):
        mode = (
            "closed-loop saturation [tail is coordinated-omission-affected]"
            if rl == "0"
            else f"open-loop ~{rl}rps/conn [CO-free]"
        )
        out.append(f"\n**value={ds}B, {mode}** — sorted by p99.9\n")
        grp.sort(key=lambda r: num(r["p999"]) or 9e9)
        view = [
            {
                "subject": r["subject"],
                "p50": fmt(r["p50"]),
                "p99": fmt(r["p99"]),
                "p99.9": fmt(r["p999"]),
                "p99.99": fmt(r["p9999"]),
                "ops/s": r["ops"],
                "srvCPU%": r["srv_cpu_avg"],
            }
            for r in grp
        ]
        out.append(
            table(
                view,
                [
                    ("subject", "server"),
                    ("p50", "p50"),
                    ("p99", "p99"),
                    ("p99.9", "p99.9"),
                    ("p99.99", "p99.99"),
                    ("ops/s", "ops/s"),
                    ("srvCPU%", "srv CPU%"),
                ],
            )
        )
    return "\n".join(out)


def section_e2(rows: list[dict]) -> str:
    rows = [r for r in rows if r["experiment"] == "E2" and r["valid"] == "valid"]
    if not rows:
        return ""
    out = ["\n## E2 — Vertical scaling (valid rows): max ops/s vs cores\n"]
    out.append(
        "_p99.9 at saturation is closed-loop and coordinated-omission-affected; "
        "read it as a relative comparison, not an absolute SLA number._\n"
    )
    subjects = sorted({r["subject"] for r in rows})
    cores = sorted({int(r["cores"]) for r in rows})
    header = ["server \\ cores"] + [str(c) for c in cores]
    out.append("| " + " | ".join(header) + " |")
    out.append("|" + "|".join("---" for _ in header) + "|")
    for subj in subjects:
        cells = [subj]
        for c in cores:
            match = [r for r in rows if r["subject"] == subj and int(r["cores"]) == c]
            cells.append(str(match[0]["ops"]) if match else "-")
        out.append("| " + " | ".join(cells) + " |")
    out.append(
        "\n_Tail + bottleneck at each point (p99.9 ms / server CPU% / softirq net delta):_\n"
    )
    view = sorted(rows, key=lambda r: (r["subject"], int(r["cores"])))
    out.append(
        table(
            [
                {
                    "s": r["subject"],
                    "cores": r["cores"],
                    "ops": r["ops"],
                    "p999": fmt(r["p999"]),
                    "cpu": r["srv_cpu_avg"],
                    "soft": r["srv_softirq_net"],
                    "nic": r["srv_nic_util"],
                }
                for r in view
            ],
            [
                ("s", "server"),
                ("cores", "cores"),
                ("ops", "ops/s"),
                ("p999", "p99.9"),
                ("cpu", "srvCPU%"),
                ("soft", "softirq Δ"),
                ("nic", "NIC%"),
            ],
        )
    )
    return "\n".join(out)


def section_e3(rows: list[dict]) -> str:
    rows = [r for r in rows if r["experiment"] == "E3"]
    if not rows:
        return ""
    rows.sort(key=lambda r: num(r["hit_ratio"]) or 0, reverse=True)
    out = ["\n## E3 — Hit ratio under eviction (sorted by hit ratio)\n"]
    out.append(
        table(
            [
                {
                    "s": r["subject"],
                    "hit": fmt(r["hit_ratio"], 4),
                    "p50": fmt(r["p50"]),
                    "p999": fmt(r["p999"]),
                    "ops": r["ops"],
                    "v": r["valid"],
                }
                for r in rows
            ],
            [
                ("s", "server"),
                ("hit", "hit ratio"),
                ("p50", "p50"),
                ("p999", "p99.9"),
                ("ops", "ops/s"),
                ("v", "validity"),
            ],
        )
    )
    return "\n".join(out)


def excluded(rows: list[dict]) -> str:
    bad = [r for r in rows if r["valid"] != "valid"]
    if not bad:
        return ""
    out = ["\n## Excluded / flagged runs (NOT used in headline tables)\n"]
    out.append(
        table(
            [
                {
                    "e": r["experiment"],
                    "l": r["label"],
                    "v": r["valid"],
                    "cliCPU": r["cli_cpu_max"],
                    "nic": r["srv_nic_util"],
                }
                for r in bad
            ],
            [
                ("e", "exp"),
                ("l", "config"),
                ("v", "reason"),
                ("cliCPU", "client CPU max%"),
                ("nic", "srv NIC%"),
            ],
        )
    )
    return "\n".join(out)


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: analyze.py results.csv", file=sys.stderr)
        return 2
    rows = load(sys.argv[1])
    print(
        f"# cachemoney benchmark results\n\n_{len(rows)} runs from `{sys.argv[1]}`._\n"
    )
    for section in (section_e1, section_e2, section_e3, excluded):
        s = section(rows)
        if s:
            print(s)
    return 0


if __name__ == "__main__":
    sys.exit(main())
