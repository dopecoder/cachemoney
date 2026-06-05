# cachemoney — developer workflow.
# Run `make help` for the list of targets.

# Pinned tool versions (kept in sync with .github/workflows/ci.yml).
GOLANGCI_LINT_VERSION ?= v2.1.6
GOFUMPT_VERSION       ?= latest

GO            ?= go
BIN_DIR       := bin
BINARY        := $(BIN_DIR)/cachemoney
PKG           := ./...
MAIN_PKG      := ./cmd/cachemoney
COVER_PROFILE := coverage.txt
BENCHTIME     ?= 300ms
FUZZTIME      ?= 20s

# Inject version metadata at build time.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

GOBIN := $(shell $(GO) env GOPATH)/bin

.DEFAULT_GOAL := help

## help: Print available targets.
.PHONY: help
help:
	@grep -E '^## [a-zA-Z0-9_-]+:' $(MAKEFILE_LIST) | \
		sed -e 's/## //' | awk -F': ' '{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## tools: Install pinned dev tools (golangci-lint, gofumpt).
.PHONY: tools
tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)

## tidy: Sync go.mod/go.sum.
.PHONY: tidy
tidy:
	$(GO) mod tidy

## fmt: Format code with gofumpt.
.PHONY: fmt
fmt:
	$(GOBIN)/gofumpt -l -w .

## vet: Run go vet.
.PHONY: vet
vet:
	$(GO) vet $(PKG)

## lint: Run golangci-lint.
.PHONY: lint
lint:
	$(GOBIN)/golangci-lint run

## test: Run unit tests.
.PHONY: test
test:
	$(GO) test $(PKG)

## race: Run tests with the race detector.
.PHONY: race
race:
	$(GO) test -race $(PKG)

## cover: Run tests with coverage and print a summary.
.PHONY: cover
cover:
	$(GO) test -race -covermode=atomic -coverprofile=$(COVER_PROFILE) $(PKG)
	$(GO) tool cover -func=$(COVER_PROFILE) | tail -1

## cover-html: Write an HTML coverage report to coverage.html.
.PHONY: cover-html
cover-html: cover
	$(GO) tool cover -html=$(COVER_PROFILE) -o coverage.html
	@echo "wrote coverage.html"

## bench: Run benchmarks (sharded map vs stdlib map+RWMutex). BENCHTIME?=300ms.
.PHONY: bench
bench:
	$(GO) test -run='^$$' -bench=. -benchmem -benchtime=$(BENCHTIME) ./internal/shardmap

## fuzz: Run the shardmap model-equivalence fuzzer. FUZZTIME?=20s.
.PHONY: fuzz
fuzz:
	$(GO) test -run='^$$' -fuzz=FuzzMapModelEquivalence -fuzztime=$(FUZZTIME) ./internal/shardmap

## bench-compare: Four-way RESP benchmark (cachemoney vs Redis/Valkey/pogocache); skips absent servers.
.PHONY: bench-compare
bench-compare: build
	$(GO) build -o $(BIN_DIR)/cmbench ./cmd/cmbench
	set -a; . ./bench/versions.env; set +a; \
		BENCH_DOC=docs/benchmarks/bench-vs-redis.md $(BIN_DIR)/cmbench

## build: Build the cachemoney binary.
.PHONY: build
build:
	mkdir -p $(BIN_DIR)
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BINARY) $(MAIN_PKG)

## run: Build and run cachemoney.
.PHONY: run
run: build
	$(BINARY)

## ci: Run the full local gate (what CI runs).
.PHONY: ci
ci: tidy vet lint race cover

## hooks: Install the git pre-push hook.
.PHONY: hooks
hooks:
	git config core.hooksPath scripts/git-hooks
	@echo "git hooks path set to scripts/git-hooks"

## clean: Remove build and coverage artifacts.
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) $(COVER_PROFILE) coverage.html
