# cachemoney

> A distributed, sharded, replicated key–value store in Go — built in the open,
> one composable piece at a time.

[![CI](https://github.com/dopecoder/cachemoney/actions/workflows/ci.yml/badge.svg)](https://github.com/dopecoder/cachemoney/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dopecoder/cachemoney.svg)](https://pkg.go.dev/github.com/dopecoder/cachemoney)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

`cachemoney` starts as a fast single-node cache and grows into a distributed KV
store: in-memory sharding and eviction first, then Raft consensus, replication,
and a disaggregated durable storage tier. The design follows the small,
sharp, composable-library model — ship a working flagship, then extract each
piece (`resp`, `shardmap`, `lru`, `raft`, …) as a standalone repo once it
stabilizes.

> **Status: M0 (single-node cache), in progress.** This README documents only
> what is actually built. See [Roadmap](#roadmap) for what is planned.

## What works today

The single-node cache **engine** and its building blocks are complete, tested,
and benchmarked:

- **`internal/cache`** — the embeddable, `net`-free `Engine`
  (`Get/Set/Del/TTL/Len`), every operation `context`-aware and error-returning,
  with binary-safe values, defensive copies, and lazy per-key TTL.
- **`internal/shardmap`** — a generic, concurrency-safe **sharded Robin Hood
  hashmap**: high-fanout sharding with a per-shard `RWMutex`, open addressing with
  backward-shift deletion, and cache-line padding. ~2–3.8× faster than a stdlib
  `map`+`RWMutex` under contention ([BENCH.md](./internal/shardmap/BENCH.md)).
- **`internal/hash`** — a seeded, HashDoS-resistant `hash/maphash` seam.

All three are at 100% test coverage and run clean under the race detector. The
network server (RESP codec + TCP) is the next slice — see [Roadmap](#roadmap).

## Quickstart

Requires **Go 1.22+**.

```bash
git clone https://github.com/dopecoder/cachemoney.git
cd cachemoney

make test     # run unit tests
make cover    # tests with race detector + coverage summary
make build    # build ./bin/cachemoney
make run      # build and run
```

`make help` lists every target.

## Project layout

```text
cmd/cachemoney/        # binary entrypoint
internal/cache/        # the Engine: Get/Set/Del/TTL/Len  ← M0 core
internal/shardmap/     # generic sharded Robin Hood hashmap (+ BENCH.md)
internal/hash/         # seeded maphash seam
docs/architecture/     # ARCHITECTURE.md + decision records (ADRs)
docs/knowledge.md      # from-scratch concept deep dives
openspec/              # spec-driven-development artifacts (living specs + archive)
.github/workflows/     # CI (vet, race tests, coverage, lint)
scripts/git-hooks/     # optional pre-push gate (make hooks)
```

Packages live under `internal/` until their API stabilizes; mature libraries
get promoted to standalone repositories (the "extract as you go" model).

## Architecture

The design — the engine/protocol spine, why Go (and what it costs vs pogocache),
sharding, eviction, and the M0–M4 roadmap — lives in
[`docs/architecture/ARCHITECTURE.md`](./docs/architecture/ARCHITECTURE.md). Every
consequential decision and its trade-offs are recorded as
[ADRs](./docs/architecture/decisions/).

New to the concepts (Robin Hood hashing, sharding, HashDoS, Go GC, eviction,
RESP, Raft, …)? [`docs/knowledge.md`](./docs/knowledge.md) is a from-scratch
reference explaining every piece and its trade-offs.

## Development

Built **test-first** (TDD: red → green → triangulate → refactor) and held to
Google-style Go conventions. Requires **Go 1.22+**. Testing stack: standard-library
`testing` + table-driven tests + [`google/go-cmp`](https://github.com/google/go-cmp).
See [CONTRIBUTING.md](./CONTRIBUTING.md) for the workflow and commit conventions.

### Setup (one-time)

```bash
make tools     # install pinned golangci-lint + gofumpt into $(go env GOPATH)/bin
make hooks     # optional: install the pre-push hook (runs vet + race)
```

### Everyday commands

| Command | What it does |
| --- | --- |
| `make test` | unit tests — `go test ./...` |
| `make race` | tests under the race detector |
| `make cover` | race tests + coverage summary (→ `coverage.txt`) |
| `make cover-html` | HTML coverage report (→ `coverage.html`) |
| `make bench` | benchmarks: sharded map vs stdlib `map`+`RWMutex` (`BENCHTIME=300ms`) |
| `make fuzz` | shardmap model-equivalence fuzzer (`FUZZTIME=20s`) |
| `make lint` | golangci-lint (expect `0 issues`) |
| `make fmt` | format with gofumpt |
| `make vet` | `go vet ./...` |
| `make build` / `make run` | build / build-and-run `./bin/cachemoney` |
| `make ci` | full local gate: tidy + vet + lint + race + cover |
| `make clean` | remove build/coverage artifacts |

`make help` prints this list. Override knobs, e.g. `make fuzz FUZZTIME=2m`.

### Targeting a package, test, or benchmark

Use raw `go test` to scope to one package/test/benchmark:

```bash
go test ./internal/cache                                 # one package
go test -v -run TestCache_TTLExpiry ./internal/cache     # one test, verbose
go test -race -run 'TestMap_' ./internal/shardmap        # name prefix, race

# benchmarks (methodology + reference numbers in internal/shardmap/BENCH.md)
go test -run='^$' -bench=BenchmarkConcurrent -benchmem -benchtime=300ms ./internal/shardmap
go test -run='^$' -bench=BenchmarkScaling -cpu=1,2,4,8,16,32 ./internal/shardmap

# fuzzing (bounded)
go test -run='^$' -fuzz=FuzzMapModelEquivalence -fuzztime=15s ./internal/shardmap
```

Quick health check: `make race && make lint` → every package `ok`, `0 issues`.

### Spec-driven development

Non-trivial changes are planned and tracked under [`openspec/`](./openspec): living
capability specs in `openspec/specs/`, and completed change proposals/designs/tasks
archived under `openspec/changes/archive/`.

## Roadmap

| Milestone | Scope | Language |
| --- | --- | --- |
| **M0** | Single-node cache: RESP codec, TCP server, sharded map, LRU eviction, `GET/SET/DEL/TTL`, benchmarks vs redis/pogocache | Go |
| **M1** | Distributed: Raft (election + log replication), consistent hashing, SWIM membership; 3-node cluster | Go |
| **M2** | Operable: Prometheus `/metrics`, control CLI, Helm/StatefulSet for Kubernetes | Go |
| **M3** | Durability: disaggregated C++ storage node (WAL, index, bloom, compaction) over the wire | C++ |
| **M4** | Polish: rate limiter, more wire protocols, LSM storage upgrade | Go / C++ |

## License

[MIT](./LICENSE) © 2026 Nithin Rao
