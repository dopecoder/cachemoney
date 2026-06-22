#!/usr/bin/env python3
"""cachemoney two-machine benchmark orchestrator.

Runs from the operator's laptop as an SSH control-plane: it starts each DB on the
server-under-test (pinned to N cores), drives load with memtier_benchmark from the load
generator against the server's PRIVATE IP, and captures validity instrumentation
(server CPU/softirq/NIC + client CPU) on every run so we can prove what the bottleneck is.

Implements bench/aws-rig/BENCHMARK-PLAN.md: E1 tail latency (open-loop + closed-loop),
E2 vertical scaling (core sweep, redis/valkey single + io-threads), E3 hit ratio.

Usage:
  python3 orchestrate.py --server <pub> --client <pub> --server-private <priv> \
      --key ~/.ssh/cm-rig.pem [--experiments e1,e2,e3] [--quick] [--out results.csv]

Server/DB launch flags live in SERVER_CMDS — verify them against `--help` on the box the
first time (pogocache/valkey flag names are the likeliest to need a tweak).
"""

from __future__ import annotations

import argparse
import json
import statistics
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

# ---------------------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------------------

PORTS = {
    "cachemoney-std": 6390,
    "cachemoney-gnet": 6391,
    "redis-single": 6379,
    "redis-iothreads": 6379,
    "valkey-single": 6380,
    "valkey-iothreads": 6380,
    "pogocache": 6381,
}

MAXMEMORY_BIG = (
    4 * 1024 * 1024 * 1024
)  # E1/E2: nothing evicts, isolate the network model
MAXMEMORY_EVICT = 64 * 1024 * 1024  # E3: small cache, force eviction

# Server launch templates. {pin}=numactl prefix, {n}=core count, {port}, {mm}=maxmemory bytes,
# {iot}=io-threads, {loops}=gnet loops, {et}=edge-trigger env. Verify on first run.
SERVER_CMDS = {
    "cachemoney-std": "{pin} env GOMAXPROCS={n} CM_NET=std "
    "/usr/local/bin/cachemoney -addr :{port} -maxmemory {mm} -maxmemory-policy allkeys-lfu",
    "cachemoney-gnet": "{pin} env GOMAXPROCS={n} CM_NET=gnet CM_GNET_LOOPS={loops} {et}"
    "/usr/local/bin/cachemoney -addr :{port} -maxmemory {mm} -maxmemory-policy allkeys-lfu",
    "redis-single": "{pin} /usr/local/bin/redis-server --port {port} --bind 0.0.0.0 "
    "--protected-mode no --save '' --appendonly no --maxmemory {mm} "
    "--maxmemory-policy allkeys-lfu --io-threads 1",
    "redis-iothreads": "{pin} /usr/local/bin/redis-server --port {port} --bind 0.0.0.0 "
    "--protected-mode no --save '' --appendonly no --maxmemory {mm} "
    "--maxmemory-policy allkeys-lfu --io-threads {iot} --io-threads-do-reads yes",
    "valkey-single": "{pin} /usr/local/bin/valkey-server --port {port} --bind 0.0.0.0 "
    "--protected-mode no --save '' --appendonly no --maxmemory {mm} "
    "--maxmemory-policy allkeys-lfu --io-threads 1",
    "valkey-iothreads": "{pin} /usr/local/bin/valkey-server --port {port} --bind 0.0.0.0 "
    "--protected-mode no --save '' --appendonly no --maxmemory {mm} "
    "--maxmemory-policy allkeys-lfu --io-threads {iot}",
    # pogocache: default host is 127.0.0.1 and port 9401 -> -h/-p are mandatory for remote
    # access; --maxmemory wants a human size (e.g. 64m/4gb), NOT raw bytes; --threads matches
    # the pinned core count; --evict yes is the eviction toggle.
    "pogocache": "{pin} /usr/local/bin/pogocache -h 0.0.0.0 -p {port} "
    "--threads {n} --maxmemory {mm_human} --evict yes",
}

SERVER_BINARIES = ["cachemoney", "redis-server", "valkey-server", "pogocache"]


@dataclass
class Spec:
    experiment: str
    subject: str
    label: str
    core_count: int
    cores: str  # numactl -C list, e.g. "0-7"
    port: int
    maxmemory: int
    io_threads: int = 1
    gnet_loops: int = 0
    etrigger: bool = False
    # workload
    data_size: int = 64
    ratio: str = "1:10"
    key_pattern: str = "R:R"
    key_maximum: int = 1_000_000
    key_extra: str = ""  # e.g. "--key-stddev=6000 --key-median=25000"
    threads: int = 4
    conns: int = 13  # per thread; total = threads*conns
    rate_limit: int = 0  # per-connection rps; 0 = closed-loop


# ---------------------------------------------------------------------------------------
# SSH helpers
# ---------------------------------------------------------------------------------------


class Remote:
    def __init__(self, key: str, user: str):
        self.key = key
        self.user = user

    def run(self, host: str, cmd: str, timeout: int = 120) -> tuple[int, str]:
        full = [
            "ssh",
            "-i",
            self.key,
            "-o",
            "StrictHostKeyChecking=accept-new",
            "-o",
            "ConnectTimeout=10",
            f"{self.user}@{host}",
            cmd,
        ]
        try:
            p = subprocess.run(full, capture_output=True, text=True, timeout=timeout)
            return p.returncode, p.stdout + p.stderr
        except subprocess.TimeoutExpired:
            return 124, "ssh timeout"


# ---------------------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------------------


def stop_servers(r: Remote, server: str) -> None:
    for b in SERVER_BINARIES:
        r.run(server, f"pkill -x {b} 2>/dev/null; true")
    time.sleep(1.0)


def _human_bytes(n: int) -> str:
    """Render a byte count as a compact size string (e.g. 64m, 4gb) for tools like pogocache
    whose --maxmemory wants a human size rather than raw bytes."""
    if n % (1024 * 1024 * 1024) == 0:
        return f"{n // (1024 * 1024 * 1024)}gb"
    return f"{n // (1024 * 1024)}m"


def start_server(r: Remote, server: str, spec: Spec) -> None:
    pin = f"numactl -C {spec.cores}" if spec.cores else ""
    et = "CM_GNET_ETRIGGER=1 " if spec.etrigger else ""
    cmd = SERVER_CMDS[spec.subject].format(
        pin=pin,
        n=spec.core_count,
        port=spec.port,
        mm=spec.maxmemory,
        mm_human=_human_bytes(spec.maxmemory),
        iot=spec.io_threads,
        loops=spec.gnet_loops,
        et=et,
    )
    launch = f"nohup setsid {cmd} >/tmp/cm-srv-{spec.port}.log 2>&1 </dev/null & echo launched-$!"
    r.run(server, launch)


def wait_ready(
    r: Remote, client: str, private_ip: str, port: int, tries: int = 40
) -> bool:
    probe = (
        f"(redis6-cli -h {private_ip} -p {port} ping 2>/dev/null || "
        f"redis-cli -h {private_ip} -p {port} ping 2>/dev/null)"
    )
    for _ in range(tries):
        rc, out = r.run(client, probe)
        if "PONG" in out.upper():
            return True
        time.sleep(0.5)
    return False


# ---------------------------------------------------------------------------------------
# Validity instrumentation (server + client)
# ---------------------------------------------------------------------------------------

STATS_SCRIPT = r"""
mpstat -P ALL {m} 1 > /tmp/cm-mp.txt 2>/dev/null &
sar -n DEV {m} 1 > /tmp/cm-sar.txt 2>/dev/null &
cat /proc/softirqs > /tmp/cm-si1
wait
cat /proc/softirqs > /tmp/cm-si2
echo '@@MPSTAT@@'; cat /tmp/cm-mp.txt
echo '@@SAR@@'; cat /tmp/cm-sar.txt
echo '@@SI1@@'; cat /tmp/cm-si1
echo '@@SI2@@'; cat /tmp/cm-si2
"""


def _parse_mpstat(text: str) -> tuple[float, float, dict[int, float]]:
    """Return (avg_busy%, max_core_busy%, {cpu_id: busy%}) from an mpstat -P ALL Average block."""
    avg_busy, max_busy = 0.0, 0.0
    percore: dict[int, float] = {}
    for line in text.splitlines():
        if not line.startswith("Average:"):
            continue
        parts = line.split()
        if len(parts) < 3 or parts[1] == "CPU":
            continue
        try:
            idle = float(parts[-1])
        except ValueError:
            continue
        busy = 100.0 - idle
        if parts[1] == "all":
            avg_busy = busy
        else:
            max_busy = max(max_busy, busy)
            if parts[1].isdigit():
                percore[int(parts[1])] = busy
    return avg_busy, max_busy, percore


def _parse_sar_dev(text: str) -> tuple[float, float, float]:
    """Return (rx_mbps, tx_mbps, %ifutil) for the busiest non-lo interface."""
    cols: dict[str, int] = {}
    best = (0.0, 0.0, 0.0)
    best_total = -1.0
    for line in text.splitlines():
        if "IFACE" in line and "rxkB/s" in line:
            hdr = line.split()
            cols = {name: i for i, name in enumerate(hdr)}
            continue
        if not line.startswith("Average:") or not cols:
            continue
        parts = line.split()
        try:
            iface = parts[cols["IFACE"]]
            if iface == "lo":
                continue
            rx = float(parts[cols["rxkB/s"]]) * 8 / 1000.0  # kB/s -> Mbps
            tx = float(parts[cols["txkB/s"]]) * 8 / 1000.0
            util = float(parts[cols["%ifutil"]]) if "%ifutil" in cols else 0.0
        except (KeyError, ValueError, IndexError):
            continue
        if rx + tx > best_total:
            best_total = rx + tx
            best = (rx, tx, util)
    return best


def _parse_softirq_net(si1: str, si2: str) -> int:
    def total(text: str) -> int:
        s = 0
        for line in text.splitlines():
            if line.strip().startswith(("NET_RX", "NET_TX")):
                s += sum(int(x) for x in line.split()[1:] if x.isdigit())
        return s

    return total(si2) - total(si1)


def _expand_cpu_list(spec: str) -> set[int]:
    """Expand a cpu list like '0-79' or '0,2,4-7' into a set of cpu ids."""
    out: set[int] = set()
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            a, b = part.split("-", 1)
            out.update(range(int(a), int(b) + 1))
        else:
            out.add(int(part))
    return out


def capture_stats(
    r: Remote, host: str, secs: int, pinned_cores: set[int] | None = None
) -> dict:
    rc, out = r.run(host, STATS_SCRIPT.format(m=secs), timeout=secs + 60)
    blocks = {}
    cur = None
    for line in out.splitlines():
        if line in ("@@MPSTAT@@", "@@SAR@@", "@@SI1@@", "@@SI2@@"):
            cur = line.strip("@")
            blocks[cur] = []
        elif cur:
            blocks[cur].append(line)

    def g(k: str) -> str:
        return "\n".join(blocks.get(k, []))

    avg_busy, max_busy, percore = _parse_mpstat(g("MPSTAT"))
    rx, tx, util = _parse_sar_dev(g("SAR"))
    soft = _parse_softirq_net(g("SI1"), g("SI2"))
    # Mean busy over ONLY the pinned cores is the honest "is the load generator out of
    # headroom?" signal — a single hot softirq core must not invalidate the whole run (M1).
    if pinned_cores:
        vals = [percore[c] for c in pinned_cores if c in percore]
        cpu_pinned = round(sum(vals) / len(vals), 1) if vals else avg_busy
    else:
        cpu_pinned = avg_busy
    return {
        "cpu_avg": round(avg_busy, 1),
        "cpu_max": round(max_busy, 1),
        "cpu_pinned": cpu_pinned,
        "nic_rx_mbps": round(rx, 1),
        "nic_tx_mbps": round(tx, 1),
        "nic_util": round(util, 1),
        "softirq_net": soft,
    }


# ---------------------------------------------------------------------------------------
# Load generation (memtier)
# ---------------------------------------------------------------------------------------


def memtier_cmd(
    spec: Spec, private_ip: str, secs: int, client_cores: str, out_json: str
) -> str:
    pin = f"numactl -C {client_cores} " if client_cores else ""
    args = [
        f"rm -f {out_json} && {pin}memtier_benchmark",
        f"-s {private_ip}",
        f"-p {spec.port}",
        "--protocol=redis",
        f"-t {spec.threads}",
        f"-c {spec.conns}",
        f"--data-size={spec.data_size}",
        f"--ratio={spec.ratio}",
        f"--key-pattern={spec.key_pattern}",
        f"--key-maximum={spec.key_maximum}",
        spec.key_extra,
        f"--test-time={secs}",
        "--hide-histogram",
        "--print-percentiles 50,99,99.9,99.99",
        f"--json-out-file={out_json}",
    ]
    if spec.rate_limit > 0:
        args.append(f"--rate-limiting={spec.rate_limit}")
    return " ".join(a for a in args if a)


def parse_memtier(text: str) -> dict:
    data = json.loads(text)
    allstats = data["ALL STATS"]
    totals = allstats["Totals"]
    pct = totals.get("Percentile Latencies")
    if not pct or "p99.90" not in pct:
        raise KeyError(
            "memtier JSON missing 'Percentile Latencies' p99.90 (unexpected schema)"
        )

    def lat(key: str):
        return pct.get(key)

    hits = totals.get("Hits/sec")
    misses = totals.get("Misses/sec")
    if hits is None:
        gets = allstats.get("Gets", {})
        hits, misses = gets.get("Hits/sec"), gets.get("Misses/sec")
    hit_ratio = None
    if hits is not None and misses is not None and (hits + misses) > 0:
        hit_ratio = round(hits / (hits + misses), 4)
    return {
        "ops": round(totals.get("Ops/sec", 0.0), 0),
        "p50": lat("p50.00"),
        "p99": lat("p99.00"),
        "p999": lat("p99.90"),
        "p9999": lat("p99.99"),
        "hit_ratio": hit_ratio,
    }


def _empty_result(reason: str) -> dict:
    return {
        "ops": 0,
        "p50": None,
        "p99": None,
        "p999": None,
        "p9999": None,
        "hit_ratio": None,
        "error": reason,
    }


def run_memtier(
    r: Remote, client: str, spec: Spec, private_ip: str, secs: int, client_cores: str
) -> dict:
    # Unique output file per invocation + explicit existence check: never parse a stale file
    # from a previous run and misattribute its numbers to this subject (B1).
    out_json = f"/tmp/cm-mt-{spec.port}-{int(time.time() * 1000)}.json"
    cmd = memtier_cmd(spec, private_ip, secs, client_cores, out_json)
    r.run(client, cmd, timeout=secs + 120)
    rc, out = r.run(client, f"cat {out_json} 2>/dev/null; rm -f {out_json}")
    if not out.strip():
        return _empty_result("memtier-failed:no-output")
    try:
        return parse_memtier(out)
    except (json.JSONDecodeError, KeyError) as e:
        return _empty_result(f"memtier-parse:{e}")


# ---------------------------------------------------------------------------------------
# Run executor
# ---------------------------------------------------------------------------------------


def execute(r: Remote, args, spec: Spec) -> dict:
    pinned = _expand_cpu_list(args.client_cores) if args.client_cores else None
    stop_servers(r, args.server)
    start_server(r, args.server, spec)
    if not wait_ready(r, args.client, args.server_private, spec.port):
        stop_servers(r, args.server)
        return {**_row_meta(spec), "valid": "no-start", "ops": 0}

    # warmup (discarded)
    run_memtier(
        r, args.client, spec, args.server_private, args.warmup, args.client_cores
    )

    reps_ops, reps = [], []
    for _ in range(args.repeats):
        with ThreadPoolExecutor(max_workers=3) as pool:
            f_mt = pool.submit(
                run_memtier,
                r,
                args.client,
                spec,
                args.server_private,
                args.measure,
                args.client_cores,
            )
            f_srv = pool.submit(capture_stats, r, args.server, args.measure)
            f_cli = pool.submit(capture_stats, r, args.client, args.measure, pinned)
            m = f_mt.result()
            srv_stats = f_srv.result()
            cli_stats = f_cli.result()
        m["_srv"], m["_cli"] = srv_stats, cli_stats
        reps.append(m)
        reps_ops.append(m["ops"])
    stop_servers(r, args.server)

    best = _median_rep(reps, reps_ops)
    srv, cli = best["_srv"], best["_cli"]
    cov = (
        (statistics.pstdev(reps_ops) / statistics.mean(reps_ops))
        if statistics.mean(reps_ops)
        else 0
    )
    failed = [m for m in reps if m.get("error") or m["ops"] == 0]
    # Client saturation is judged on the MEAN busy of the pinned load cores, not the busiest
    # single core (M1). NIC saturation uses both %ifutil AND measured throughput vs the
    # instance line rate, because ENA NICs often report %ifutil=0 (M2).
    client_saturated = cli.get("cpu_pinned", cli["cpu_avg"]) >= 85.0
    nic_line = float(args.nic_line_mbps)
    srv_nic_total = srv["nic_rx_mbps"] + srv["nic_tx_mbps"]
    cli_nic_total = cli["nic_rx_mbps"] + cli["nic_tx_mbps"]
    nic_hot = (
        srv["nic_util"] >= 80.0
        or cli["nic_util"] >= 80.0
        or srv_nic_total >= 0.8 * nic_line
        or cli_nic_total >= 0.8 * nic_line
    )
    valid = "valid"
    if failed:
        valid = "memtier-failed"
    elif client_saturated:
        valid = "rig-limited:client-cpu"
    elif nic_hot:
        valid = "rig-limited:nic"
    elif cov > 0.05:
        valid = "noisy:cov>5%"

    return {
        **_row_meta(spec),
        "ops": int(best["ops"]),
        "p50": best["p50"],
        "p99": best["p99"],
        "p999": best["p999"],
        "p9999": best["p9999"],
        "hit_ratio": best["hit_ratio"],
        "cov": round(cov, 3),
        "srv_cpu_avg": srv["cpu_avg"],
        "srv_cpu_max": srv["cpu_max"],
        "srv_nic_rx_mbps": srv["nic_rx_mbps"],
        "srv_nic_tx_mbps": srv["nic_tx_mbps"],
        "srv_nic_util": srv["nic_util"],
        "srv_softirq_net": srv["softirq_net"],
        "cli_cpu_max": cli["cpu_max"],
        "cli_cpu_pinned": cli.get("cpu_pinned"),
        "cli_nic_util": cli["nic_util"],
        "valid": valid,
    }


def _row_meta(spec: Spec) -> dict:
    return {
        "experiment": spec.experiment,
        "subject": spec.subject,
        "label": spec.label,
        "cores": spec.core_count,
        "io_threads": spec.io_threads,
        "gnet_loops": spec.gnet_loops,
        "data_size": spec.data_size,
        "ratio": spec.ratio,
        "total_conns": spec.threads * spec.conns,
        "rate_limit_pc": spec.rate_limit,
    }


def _median_rep(reps: list[dict], ops: list[float]) -> dict:
    median_ops = statistics.median(ops)
    return min(reps, key=lambda m: abs(m["ops"] - median_ops))


# ---------------------------------------------------------------------------------------
# Experiment matrices (see BENCHMARK-PLAN.md)
# ---------------------------------------------------------------------------------------


def cores_str(n: int) -> str:
    return "0" if n == 1 else f"0-{n - 1}"


def build_e1(quick: bool) -> list[Spec]:
    """Tail latency: 8 cores, 50 conns, read-heavy, no eviction. Open-loop rate sweep + closed-loop."""
    subjects = [
        "cachemoney-std",
        "cachemoney-gnet",
        "redis-single",
        "valkey-single",
        "pogocache",
    ]
    rates = (
        [0] if quick else [50_000, 100_000, 200_000, 0]
    )  # 0 = closed-loop saturation
    sizes = [4096] if quick else [64, 4096]
    specs = []
    for subj in subjects:
        for ds in sizes:
            for total_rate in rates:
                conns = 50
                rl = (total_rate // conns) if total_rate else 0
                specs.append(
                    Spec(
                        experiment="E1",
                        subject=subj,
                        label=f"{subj} ds={ds} rate={total_rate or 'sat'}",
                        core_count=8,
                        cores="0-7",
                        port=PORTS[subj],
                        maxmemory=MAXMEMORY_BIG,
                        gnet_loops=8 if subj == "cachemoney-gnet" else 0,
                        data_size=ds,
                        ratio="1:10",
                        key_pattern="R:R",
                        key_maximum=1_000_000,
                        threads=5,
                        conns=10,
                        rate_limit=rl,
                    )
                )
    return specs


def build_e2(quick: bool, server_cores: int) -> list[Spec]:
    """Vertical scaling: sweep server cores, saturation, redis/valkey single + io-threads."""
    ladder = [1, 2, 4, 8] if quick else [1, 2, 4, 8, 16, 32, 48, 96]
    ns = [n for n in ladder if n <= server_cores]
    subjects = [
        "cachemoney-std",
        "cachemoney-gnet",
        "redis-single",
        "redis-iothreads",
        "valkey-single",
        "valkey-iothreads",
    ]
    specs = []
    for n in ns:
        total = min(100 * n, 4000)
        threads = min(n * 4, 64)
        conns = max(total // threads, 1)
        for subj in subjects:
            specs.append(
                Spec(
                    experiment="E2",
                    subject=subj,
                    label=f"{subj} cores={n}",
                    core_count=n,
                    cores=cores_str(n),
                    port=PORTS[subj],
                    maxmemory=MAXMEMORY_BIG,
                    io_threads=n if subj.endswith("iothreads") else 1,
                    gnet_loops=n if subj == "cachemoney-gnet" else 0,
                    data_size=64,
                    ratio="1:10",
                    key_pattern="R:R",
                    key_maximum=1_000_000,
                    threads=threads,
                    conns=conns,
                    rate_limit=0,
                )
            )
    return specs


def build_e3(quick: bool) -> list[Spec]:
    """Hit ratio under eviction: 64MiB cache, 4KB overflow workload, 8 cores."""
    subjects = [
        "cachemoney-std",
        "cachemoney-gnet",
        "redis-single",
        "valkey-single",
        "pogocache",
    ]
    specs = []
    for subj in subjects:
        specs.append(
            Spec(
                experiment="E3",
                subject=subj,
                label=f"{subj} eviction",
                core_count=8,
                cores="0-7",
                port=PORTS[subj],
                maxmemory=MAXMEMORY_EVICT,
                gnet_loops=8 if subj == "cachemoney-gnet" else 0,
                data_size=4096,
                ratio="1:2",
                key_pattern="R:G",
                key_maximum=50_000,
                key_extra="--key-stddev=6000 --key-median=25000",
                threads=4,
                conns=25,
                rate_limit=0,
            )
        )
    return specs


# ---------------------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------------------

CSV_FIELDS = [
    "experiment",
    "subject",
    "label",
    "cores",
    "io_threads",
    "gnet_loops",
    "data_size",
    "ratio",
    "total_conns",
    "rate_limit_pc",
    "ops",
    "p50",
    "p99",
    "p999",
    "p9999",
    "hit_ratio",
    "cov",
    "srv_cpu_avg",
    "srv_cpu_max",
    "srv_nic_rx_mbps",
    "srv_nic_tx_mbps",
    "srv_nic_util",
    "srv_softirq_net",
    "cli_cpu_max",
    "cli_cpu_pinned",
    "cli_nic_util",
    "valid",
]


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--server", required=True, help="server-under-test public IP")
    ap.add_argument("--client", required=True, help="load-generator public IP")
    ap.add_argument(
        "--server-private", required=True, help="server private IP (load target)"
    )
    ap.add_argument("--key", required=True, help="path to the EC2 private key (.pem)")
    ap.add_argument("--user", default="ubuntu")
    ap.add_argument(
        "--client-cores", default="", help="numactl -C list to pin memtier, e.g. 0-79"
    )
    ap.add_argument(
        "--nic-line-mbps",
        type=int,
        default=25000,
        help="instance NIC line rate in Mbps for the NIC-saturation gate (e.g. 30000-50000 on metal)",
    )
    ap.add_argument("--experiments", default="e1,e2,e3")
    ap.add_argument(
        "--quick", action="store_true", help="reduced matrix for smoke runs"
    )
    ap.add_argument("--warmup", type=int, default=10)
    ap.add_argument("--measure", type=int, default=30)
    ap.add_argument("--repeats", type=int, default=3)
    ap.add_argument("--out", default="results.csv")
    args = ap.parse_args()

    if args.quick:
        args.warmup, args.measure, args.repeats = 5, 10, 1

    r = Remote(args.key, args.user)
    rc, out = r.run(args.server, "nproc")
    server_cores = int(out.strip()) if out.strip().isdigit() else 8
    print(f"server reports {server_cores} cores; verifying provisioning sentinel...")
    rc, sent = r.run(
        args.server, "test -f /opt/cm-setup-done && echo READY || echo PENDING"
    )
    if "READY" not in sent:
        print(
            "WARNING: /opt/cm-setup-done not present — server provisioning may be incomplete.",
            file=sys.stderr,
        )

    selected = [e.strip().lower() for e in args.experiments.split(",")]
    specs: list[Spec] = []
    if "e1" in selected:
        specs += build_e1(args.quick)
    if "e2" in selected:
        specs += build_e2(args.quick, server_cores)
    if "e3" in selected:
        specs += build_e3(args.quick)

    # Clamp every spec to the server's actual core count: the fixed-8-core experiments (E1/E3)
    # and any gnet-loops/io-threads would otherwise emit `numactl -C 0-7` on a 2-core smoke box
    # and fail to start. core_count drives the numactl range, GOMAXPROCS, loops, and io-threads.
    for spec in specs:
        if spec.core_count > server_cores:
            spec.core_count = server_cores
            spec.cores = cores_str(server_cores)
        if spec.gnet_loops > server_cores:
            spec.gnet_loops = server_cores
        if spec.io_threads > server_cores:
            spec.io_threads = server_cores

    print(f"running {len(specs)} configurations -> {args.out}")
    import csv

    with open(args.out, "w", newline="") as fh:
        w = csv.DictWriter(fh, fieldnames=CSV_FIELDS, extrasaction="ignore")
        w.writeheader()
        for i, spec in enumerate(specs, 1):
            t0 = time.time()
            row = execute(r, args, spec)
            w.writerow(row)
            fh.flush()
            print(
                f"[{i}/{len(specs)}] {row.get('experiment')} {row['label']}: "
                f"ops={row.get('ops')} p99.9={row.get('p999')} hit={row.get('hit_ratio')} "
                f"srvCPU={row.get('srv_cpu_avg')}% {row.get('valid')} ({time.time() - t0:.0f}s)"
            )
    print(f"done. analyze with: python3 analyze.py {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
