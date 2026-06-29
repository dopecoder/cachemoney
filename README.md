# cachemoney

> A fast, sharded, Redis-compatible key–value cache in Go — built in the open,
> one composable, test-first piece at a time, and benchmarked honestly.

[![CI](https://github.com/dopecoder/cachemoney/actions/workflows/ci.yml/badge.svg)](https://github.com/dopecoder/cachemoney/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dopecoder/cachemoney.svg)](https://pkg.go.dev/github.com/dopecoder/cachemoney)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

`cachemoney` is a single-node, in-memory cache that speaks the **RESP wire
protocol** (so `redis-cli` and any Redis client just work) and is designed to
grow, milestone by milestone, into a distributed, sharded, replicated KV store.
It follows the small-sharp-composable-library model — ship a working flagship,
then extract each piece (`resp`, `shardmap`, `tinylfu`, `raft`, …) as a
standalone repo once its API stabilizes.

> **Status: M0 (single-node cache) — complete.** RESP2/RESP3 codec, TCP server,
> sharded engine, W-TinyLFU eviction, and a four-way benchmark harness are all
> built, tested, and measured. See the [Roadmap](#roadmap) for M1 (distribution)
> and beyond.

---

## TL;DR — the benchmarks

All numbers are **honest, reproducible, same-host** measurements. cachemoney is
an M0 **Go** prototype; Redis/Valkey are mature hand-tuned **C**. Methodology,
fairness controls, and caveats are summarized inline under each table below.

### cachemoney beats Redis, Valkey, and pogocache on this workload

With its experimental **event-loop backend** (`CM_NET=gnet`), on the four-way
eviction workload at moderate concurrency, cachemoney wins **every column** —
throughput, tail latency, **and** hit ratio:

| server | hit ratio | p50 | **p99.9** | **ops/s** |
|---|---:|---:|---:|---:|
| **cachemoney** (event-loop) | **0.287** | **0.143 ms** | **0.391 ms** | **139k** |
| cachemoney (goroutine)      | 0.287     | 0.175 ms     | 1.975 ms     | 61k      |
| redis 7.4                   | 0.266     | 0.287 ms     | 0.655 ms     | 68k      |
| valkey 8.1                  | 0.267     | 0.295 ms     | 0.679 ms     | 71k      |
| pogocache 1.3.1             | 0.251     | 0.327 ms     | 0.743 ms     | 51k      |

- **~2× the throughput** of Redis/Valkey and a **~1.7× lower p99.9** than Redis.
- The **best hit ratio in the field** — cachemoney's **W-TinyLFU** policy retains
  the hot set better than Redis/Valkey `allkeys-lfu` and pogocache's sampled LRU.

> **Honest caveat (read it).** This is a **co-located single-host** rig
> (server + load generator + 20 connections on one machine, Docker bridge). It is
> a genuine, repeatable result at low-to-moderate concurrency, **not** a settled
> high-core/high-concurrency claim — a dedicated **two-machine AWS rig**
> ([`bench/aws-rig/`](./bench/aws-rig/)) is built and pending to settle vertical
> scaling on isolated hardware. The default goroutine backend is within ~4% of
> Redis throughput (below); the event-loop backend is an opt-in **spike**, not yet
> productionized. Before landing on the root cause, every rival hypothesis was
> **refuted with data** — GC (`GOGC=off` changed nothing), synchronous eviction,
> Nagle (`TCP_NODELAY`), and value-copy size were each toggled and re-measured; a
> trivial 40-line goroutine-per-connection echo server reproduced the same ~2 ms
> tail, pinning it on Go's goroutine wakeup→run handoff, not the cache.

### The sharded map is 2–3.5× faster than `map` + `RWMutex`

The custom **sharded Robin Hood hashmap** is the one axis where Go competes on
raw speed. Under parallel contention (AMD Ryzen 9 7950X, 16c/32t):

| mix (Get/Set) | keys | shardmap `ns/op` | stdlib `map`+`RWMutex` `ns/op` | speedup |
|---|---|---:|---:|---:|
| 90 / 10 | 65 536 | **17.0** | 59.0 | ~3.5× |
| 90 / 10 | 4 096  | **15.1** | 39.9 | ~2.6× |
| 50 / 50 | 65 536 | **22.5** | 65.0 | ~2.9× |
| 50 / 50 | 4 096  | **21.1** | 46.3 | ~2.2× |

Throughput **scales with cores** (ns/op falls as cores rise 1→32) while the
single-lock baseline degrades — the cache-line padding works.
Details: [`internal/shardmap/BENCH.md`](./internal/shardmap/BENCH.md).

### Default backend: within ~4% of Redis, with a better hit ratio

The stock goroutine-per-connection server, measured with `redis-benchmark`
(throughput) + `memtier_benchmark` (hit ratio under a fixed 64 MiB `maxmemory`):

| Server | GET rps | SET rps | **Hit ratio** | GET p50 | GET p99 |
|---|---:|---:|---:|---:|---:|
| cachemoney | 61 501 | 61 690 | **0.2868** | 0.191 ms | 0.719 ms |
| redis 7.4  | 63 857 | 63 694 | 0.2764     | 0.167 ms | 0.359 ms |
| valkey 8.1 | 65 617 | 65 445 | 0.2768     | 0.167 ms | 0.343 ms |
| pogocache  | 30 572 | 30 294 | 0.2509     | 0.359 ms | 0.519 ms |

**Method:** `redis-benchmark --csv` drives throughput (uniform keys, 64-byte
values, no eviction); `memtier_benchmark` drives the hit ratio (values sized to
overflow a fixed 64 MiB `maxmemory` and force eviction, skewed GETs onto a hot
subset). Every server runs as a **pinned Docker image** (`redis:7.4`,
`valkey/valkey:8.1`, `pogocache/pogocache:1.3.1`, `memtier 2.4.2`) on one private
bridge network, configured identically (same `maxmemory`, closest-equivalent LFU
policy), warmup discarded, **median of 3** reported. Reproduce with
`make bench-compare` (needs Docker).

---

## What works today (M0, complete)

Every layer below is built, unit-tested, and race-clean. The six core library
packages are at **100% statement coverage**; the server is at 89%.

| Package | What it is |
|---|---|
| **`internal/cache`** | The embeddable, `net`-free **Engine** (`Get/Set/Del/TTL/Len`) — every op `context`-aware and error-returning, binary-safe values, defensive copies, lazy per-key TTL, and a byte-bounded heap with live `maxmemory` enforcement. |
| **`internal/shardmap`** | A generic, concurrency-safe **sharded Robin Hood hashmap** — high-fanout sharding with per-shard `RWMutex`, open addressing with backward-shift deletion, cache-line padding. Sampling hook for eviction. |
| **`internal/tinylfu`** | A full **W-TinyLFU** admission/eviction policy — count-min sketch with aging, doorkeeper bloom filter, lossy striped ring buffers, and a single-writer drainer. This is why the hit ratio wins. |
| **`internal/resp`** | A **RESP2/RESP3** wire codec (reader + writer) with strict limits and a fuzz suite — the protocol `redis-cli` and every Redis client speak. |
| **`internal/server`** | The **TCP server**: goroutine-per-connection listener, command dispatch, `redis-cli` `HELLO`/RESP3 handshake, graceful shutdown, live `CONFIG` — **plus an experimental epoll event-loop backend** (`CM_NET=gnet`) that reuses the engine, dispatch, and every handler unchanged. |
| **`internal/hash`** | A seeded, **HashDoS-resistant** `hash/maphash` seam. |
| **`cmd/cmbench` + `bench/`** | A four-way benchmark harness (cachemoney vs redis/valkey/pogocache over Docker) and a two-machine **AWS CDK rig** for isolated-hardware runs. |

**Command surface:** `PING`, `GET`, `SET` (with `EX`/`PX`/`EXAT` TTL options),
`DEL`, `EXISTS`, `TTL`, `PTTL`, `CONFIG GET/SET` (`maxmemory`,
`maxmemory-policy`), `HELLO`, `COMMAND`, `QUIT`.

## Quickstart

Requires **Go 1.22+**.

```bash
git clone https://github.com/dopecoder/cachemoney.git
cd cachemoney

make build            # build ./bin/cachemoney
./bin/cachemoney      # start the server (default :6380)
```

Talk to it with the Redis CLI you already have:

```bash
redis-cli -p 6380 PING            # PONG
redis-cli -p 6380 SET name money  # OK
redis-cli -p 6380 GET name        # "money"
redis-cli -p 6380 SET k v EX 10   # expires in 10s
redis-cli -p 6380 TTL k           # (integer) 10
```

Opt into the experimental event-loop backend with `CM_NET=gnet ./bin/cachemoney`.
`make help` lists every target.

## Architecture

The core idea, borrowed from
[tidwall/pogocache](https://github.com/tidwall/pogocache):
**the cache core knows nothing about sockets or wire formats** — protocols are
thin adapters over one Go `Engine` interface, so everything below the network line
is reused by the distribution layer (M1) and the durable storage tier (M3).

```text
        ┌──────────── wire-protocol adapters (thin) ────────────┐
        │   resp (M0)   │   http (M4)   │   memcache (M4)  │ …   │
        └───────────────────────────┬───────────────────────────┘
                                     │  Engine interface (Get/Set/Del/TTL/…)
                        ┌────────────▼────────────┐
                        │          engine         │   internal/cache
                        │  sharded store · TTL ·   │
                        │  W-TinyLFU eviction ·    │
                        │  binary-safe []byte      │
                        └────────────┬────────────┘
               ┌─────────────────────┼─────────────────────┐
               ▼                     ▼                       ▼
          shardmap              maphash (seed)          tinylfu policy
     (sharded Robin Hood)    (HashDoS-resistant)     (admission/eviction)
```

Why Go, and what it costs: Go buys a first-class distributed ecosystem (etcd,
CockroachDB, Temporal) at the price of raw single-node speed vs hand-tuned C — GC
on a pointer-dense heap and goroutine-per-connection networking. The sharded map
is the one axis Go fully recovers; the rest is an honest, measured trade-off.

## Project layout

```text
cmd/cachemoney/        # server binary entrypoint
cmd/cmbench/           # four-way benchmark harness (redis/valkey/pogocache)
internal/cache/        # the Engine: Get/Set/Del/TTL/Len + byte-bounded eviction
internal/shardmap/     # generic sharded Robin Hood hashmap (+ BENCH.md)
internal/tinylfu/      # W-TinyLFU admission/eviction policy
internal/resp/         # RESP2/RESP3 wire codec
internal/server/       # TCP server + gnet event-loop backend
internal/hash/         # seeded maphash seam
bench/aws-rig/         # two-machine AWS CDK benchmark rig
```

Packages live under `internal/` until their API stabilizes; mature libraries get
promoted to standalone repositories (the "extract as you go" model).

## Development

Built **test-first** (TDD: red → green → triangulate → refactor) and held to
Google-style Go conventions. ~9,400 lines of tests to ~4,800 lines of code.
Testing stack: standard-library `testing` + table-driven tests +
[`google/go-cmp`](https://github.com/google/go-cmp), plus property tests, fuzzers,
and a race-clean suite. See [CONTRIBUTING.md](./CONTRIBUTING.md).

| Command | What it does |
| --- | --- |
| `make test` | unit tests — `go test ./...` |
| `make race` | tests under the race detector |
| `make cover` | race tests + coverage summary (→ `coverage.txt`) |
| `make bench` | sharded map vs stdlib `map`+`RWMutex` microbenchmarks |
| `make bench-compare` | four-way comparison vs redis/valkey/pogocache (needs Docker) |
| `make fuzz` | shardmap + RESP model-equivalence fuzzers |
| `make lint` | golangci-lint (expect `0 issues`) |
| `make ci` | full local gate: tidy + vet + lint + race + cover |

`make help` prints every target.

## Roadmap

| Milestone | Scope | Status |
| --- | --- | :---: |
| **M0** | Single-node cache: RESP codec, TCP server, sharded map, W-TinyLFU eviction, `GET/SET/DEL/TTL`, benchmarks vs redis/valkey/pogocache | ✅ complete |
| **M1** | Distributed: Raft (election + log replication), consistent hashing, SWIM membership; 3-node cluster | planned |
| **M2** | Operable: Prometheus `/metrics`, control CLI, Helm/StatefulSet for Kubernetes | planned |
| **M3** | Durability: disaggregated C++ storage node (WAL, index, bloom, compaction) over the wire | planned |
| **M4** | Polish: rate limiter, more wire protocols, LSM storage upgrade | planned |

## License

[MIT](./LICENSE) © 2026 Nithin Rao
