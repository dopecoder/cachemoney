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

- `internal/store`: a thread-safe in-memory key–value store with per-key TTL
  (lazy expiration) and binary-safe values. This is the core the network
  server and sharding layer will be built on.

That's deliberately small. Each milestone below adds one defensible capability
and ships a benchmarkable, blog-able artifact — never an empty repo.

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
cmd/cachemoney/      # binary entrypoint
internal/store/      # in-memory KV store (Get/Set/Del/TTL)  ← M0 core
docs/architecture/   # ARCHITECTURE.md + decision records (ADRs)
.github/workflows/   # CI (vet, race tests, coverage, lint)
scripts/git-hooks/   # optional pre-push gate (make hooks)
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

This project is built **test-first** (TDD: red → green → refactor) and held to
Google-style Go conventions. The full local gate mirrors CI:

```bash
make tools    # install pinned golangci-lint + gofumpt (one-time)
make ci       # tidy + vet + lint + race + coverage
make hooks    # install the pre-push hook (optional)
```

Testing stack: standard-library `testing` + table-driven tests +
[`google/go-cmp`](https://github.com/google/go-cmp). See
[CONTRIBUTING.md](./CONTRIBUTING.md) for the workflow and commit conventions.

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
