# cachemoney AWS benchmark plan

The authoritative design that `benchmark/orchestrate.py` implements. Decided up front so
the results are defensible, not a pile of numbers. Methodology follows the
`benchmarking-discipline` skill (`~/.agents/skills/benchmarking-discipline/`).

## Why a two-machine rig

Every prior high-core number was a **rig artifact** — load generator co-located with the
server, softirq starvation, Docker bridge ceiling. The only fix is physical isolation:

- **Server-under-test:** `r8g.metal-24xl` — Graviton4, 96 cores, **bare metal** (no
  hypervisor jitter, headroom for kernel softirq processing, and the option to tune
  RPS/IRQ affinity if needed).
- **Load generator:** `c8g.24xlarge` — Graviton4, 96 cores, its own NIC and softirq budget.
- Same VPC, **cluster placement group**, one security group (all traffic between the two
  hosts). Benchmark traffic flows over the **private IP**; orchestration is SSH control-plane
  from the operator's laptop (low bandwidth, off the data path).

Start on small instances (e.g. `c8g.2xlarge` ×2) to smoke the whole pipeline cheaply, then
run the real sweep on the metal pair.

## Subjects (5, native installs, pinned versions)

| subject | what | how it uses cores |
|---|---|---|
| `cachemoney-std` | goroutine-per-connection | `GOMAXPROCS=N`, pinned `0..N-1` |
| `cachemoney-gnet` | epoll event loop (`CM_NET=gnet`) | `CM_GNET_LOOPS=N`; SO_REUSEPORT + edge-triggered + LockOSThread + big buffers on |
| `redis` 7.4 | — | `io-threads=1` (single) **and** `io-threads=N` (tuned) |
| `valkey` 8.1 | — | `io-threads=1` (single) **and** `io-threads=N` (tuned) |
| `pogocache` 1.3.1 | 2-random eviction | `-h 0.0.0.0 -p 6381 --threads N --evict yes` |

Ports (one server runs at a time, but fixed for clarity): std 6390, gnet 6391, redis 6379,
valkey 6380, pogocache 6381. cachemoney is built from repo HEAD for `arm64` (carries the gnet
spike) and uploaded; the rest are built from source on the server.

## Validity instrumentation — captured on EVERY run (non-negotiable)

| where | metric | tool |
|---|---|---|
| server | per-core CPU%, idle | `mpstat -P ALL` |
| server | softirq NET_RX/NET_TX delta | `/proc/softirqs` snapshots |
| server | NIC RX/TX Gbps, %line-rate | `sar -n DEV` |
| client | per-core CPU% (is the generator saturated?) | `mpstat -P ALL` |

Protocol: **warmup 10 s, measure 30 s, 3 repeats**; report median + min/max; flag if
coefficient of variation > 5 %. **Per-run validity gate** — a result is `valid` only if the
client is not saturated (busy cores < 85 %) and the NIC is below ~80 % of line rate;
otherwise the row is tagged `rig-limited` and excluded from headline conclusions.

## Tooling

- `memtier_benchmark` 2.4.2 — `--print-percentiles 50,99,99.9,99.99`, `--json-out-file`;
  **open-loop** via `--rate-limiting=<rps-per-connection>`; hit ratio from memtier's own GET
  Hits/Misses accounting (uniform across all servers, no per-server INFO needed).
- `redis-benchmark` — optional pipelined throughput ceiling (`-P 16`).

---

## E1 — Tail latency (headline)

*Hypothesis: the gnet event loop has a redis-class tail; std cachemoney's tail is fat because
of the per-request goroutine wakeup.*

- Server cores **fixed at 8** (`0-7`); client pinned to disjoint cores, over-provisioned.
- Concurrency **50** (`-t 5 -c 10`), **no pipelining**.
- Value sizes **64 B and 4 KB**; ratio set:get **1:10** (read-heavy, cache-typical).
- Cache **large (4 GiB)** → nothing evicts, isolating the network model.
- **Open-loop rate sweep** (total ops/s): 50k, 100k, 200k, 400k, … up to just below
  saturation (`--rate-limiting = rate / connections`). Yields a latency-vs-load curve free of
  coordinated omission.
- **Closed-loop saturation** point (no rate limit) for the max-throughput + saturation-tail.
- Metric: **p50/p99/p99.9/p99.99** per offered rate. Headline = the tail at the highest rate
  all five servers sustain.
- Subjects: std, gnet (loops=8), redis-single, valkey-single, pogocache.

## E2 — Vertical scaling (what the metal box is for)

*Hypothesis: gnet scales ~linearly via SO_REUSEPORT; std plateaus; single-thread redis is
flat; io-threads redis scales partway.*

- Server cores **N ∈ {1, 2, 4, 8, 16, 32, 48, 96}**, pinned `0..N-1`; the remaining
  `96-N` cores stay free for softirq. At large N that headroom shrinks even on a dedicated
  box — the instrumentation will **reveal the knee, and we report it** rather than hide it.
- Closed-loop saturation; concurrency is scaled to saturate each N (~100×N, capped) and
  saturation is **confirmed from the server-CPU instrumentation** rather than a full
  connection sweep (the orchestrator runs one saturating concurrency per N to keep the metal
  run affordable; widen it in `build_e2` if a point looks under-saturated). Value **64 B**
  (4 KB secondary). No pipelining for
  the saturation-latency read; optional `-P 16` for the throughput ceiling.
- Subjects per N: std (`GOMAXPROCS=N`), gnet (`loops=N`), redis-iothreads
  (`io-threads=N --io-threads-do-reads yes`, so reads are threaded too — the genuinely
  "tuned" config, not write-only threading), valkey-iothreads (`io-threads=N`; valkey 8
  threads reads automatically), plus redis-single & valkey-single as flat baselines.
- Metric: **max ops/s vs N** (the scaling curve) + p99/p99.9 at saturation + server
  CPU/softirq/NIC (to prove what the wall actually is at each N).

## E3 — Hit ratio under eviction

*Hypothesis: cachemoney W-TinyLFU admission beats redis/valkey LRU and pogocache sampling.*

- Cache **64 MiB**, `allkeys-lfu` (pogocache `--evict yes`). Server cores fixed at 8.
- `memtier --data-size=4096 --key-maximum=50000 --key-pattern=R:G --key-stddev=6000
  --key-median=25000 --ratio=1:2` — uniform-random SETs of 4 KB overflow the cache; Gaussian
  GETs form a hot set.
- Subjects: all five (gnet loops=8, redis/valkey default).
- Metric: **hit ratio** (memtier GET hits/(hits+misses)) + p50/p99.9 + ops/s.

---

## Outputs

- `results.csv` — one row per `subject × experiment × config × offered-rate × repeat`, with
  all latency/throughput/hit metrics **and** the validity columns (server CPU/softirq/NIC,
  client CPU, `valid|rig-limited`).
- `analyze.py` → per-experiment markdown comparison tables + the E2 scaling curve, headline
  rows restricted to `valid` rows only.
