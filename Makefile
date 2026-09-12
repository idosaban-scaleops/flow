BINARY      := flow
MODULE      := github.com/idosaban-scaleops/flow
CMD         := ./cmd/flow

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Stamped into main so that `flow --version` (fang) and `flow version --json`
# always report the same values.
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.DEFAULT_GOAL := build

.PHONY: build
build: ## Build the binary into ./bin
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(CMD)

.PHONY: install
install: ## Install the binary into $GOBIN
	go install -trimpath -ldflags '$(LDFLAGS)' $(CMD)

.PHONY: test
test: ## Run the fast unit tests
	go test -race -short ./...

.PHONY: test-integration
test-integration: ## Run every test, including the ones that need a real git
	go test -race -tags integration ./...

.PHONY: lint
lint: ## Run golangci-lint over all build tags
	golangci-lint run --build-tags integration ./...

.PHONY: fmt
fmt: ## Format and organize imports
	golangci-lint fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: golden
golden: ## Regenerate golden files
	go test ./internal/ciwait ./internal/output -update

.PHONY: snapshot
snapshot: ## Build a local GoReleaser snapshot
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist

.PHONY: help
help: ## List the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
