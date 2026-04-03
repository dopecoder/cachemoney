# Contributing to cachemoney

cachemoney is built **test-first** and held to Google-style Go conventions.
This document is the working agreement for changes — including changes made by
the author.

## Prerequisites

- Go **1.22+**
- `make`
- One-time tool install: `make tools` (pins `golangci-lint` + `gofumpt`)

## The TDD loop (non-negotiable)

Every behavioral change follows **red → green → refactor**:

1. **RED** — write a failing test that describes the new behavior. Run it and
   watch it fail for the *right* reason.
2. **GREEN** — write the minimum code to make the test pass.
3. **TRIANGULATE** — add cases (edge, error, boundary) until the behavior is
   pinned down, not just the happy path.
4. **REFACTOR** — clean up names, duplication, and structure with tests green.

Tests use the standard library `testing` package with **table-driven** cases
and [`google/go-cmp`](https://github.com/google/go-cmp) (`cmp.Diff`) for
comparisons. No assertion frameworks.

```go
func TestThing(t *testing.T) {
 tests := map[string]struct {
  in   string
  want string
 }{
  "empty":  {in: "", want: ""},
  "simple": {in: "a", want: "A"},
 }
 for name, tc := range tests {
  t.Run(name, func(t *testing.T) {
   got := Thing(tc.in)
   if diff := cmp.Diff(tc.want, got); diff != "" {
    t.Errorf("Thing(%q) mismatch (-want +got):\n%s", tc.in, diff)
   }
  })
 }
}
```

Concurrent code **must** pass `go test -race`.

## Before you push

Run the same gate CI runs:

```bash
make ci   # tidy + vet + lint + race + coverage
```

Optionally install the pre-push hook so this runs automatically:

```bash
make hooks
```

## Code style

- `gofumpt` is authoritative for formatting (`make fmt`).
- `golangci-lint` must pass with zero issues (`make lint`).
- Exported symbols need doc comments (enforced by `revive`).
- Prefer the standard library; add a dependency only when it earns its place.
- Keep packages small and single-purpose — they are future standalone repos.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

```text
<type>(<optional scope>): <description>

feat(store): add per-key TTL with lazy expiration
fix(resp): handle inline commands without CRLF
test(store): cover concurrent Set/Get under -race
docs(readme): document M0 scope
```

Common types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `perf`,
`build`, `ci`. Keep the subject in the imperative mood, ≤ 72 characters.

## Pull requests

- One logical change per PR; keep diffs reviewable (aim for < 400 lines).
- Tests and docs ship **with** the code, not in a follow-up.
- CI (vet, lint, race tests, coverage) must be green before merge.
