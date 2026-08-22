# Development commands for the Software Factory Go module.

GO ?= go
BINDIR ?= bin

BINARIES := \
	$(BINDIR)/factory \
	$(BINDIR)/factory-report \
	$(BINDIR)/factory-worker-attach

.DEFAULT_GOAL := help

.PHONY: help all build test test-race vet fmt fmt-check check deps tidy install run report attach worker-build clean

help: ## Show the available development commands.
	@awk 'BEGIN { FS = ":.*##"; print "Usage: make <target>\n"; print "Targets:" } /^[a-zA-Z0-9_.-]+:.*##/ { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: check ## Run formatting, static analysis, tests, and a build.

build: ## Build all command binaries into BINDIR (default: bin).
	@mkdir -p "$(BINDIR)"
	$(GO) build -o "$(BINDIR)/factory" ./cmd/factory
	$(GO) build -o "$(BINDIR)/factory-report" ./cmd/factory-report
	$(GO) build -o "$(BINDIR)/factory-worker-attach" ./cmd/factory-worker-attach

test: ## Run the complete Go test suite.
	$(GO) test ./...

test-race: ## Run the complete test suite with the race detector.
	$(GO) test -race ./...

vet: ## Run Go's static analysis checks.
	$(GO) vet ./...

fmt: ## Format all Go packages.
	$(GO) fmt ./...

fmt-check: ## Fail when any Go source file needs formatting.
	@test -z "$$(gofmt -l .)" || { \
		echo "Go files need formatting:"; \
		gofmt -l .; \
		exit 1; \
	}

check: fmt-check vet test build ## Run every repository verification gate.

deps: ## Download the module dependencies.
	$(GO) mod download

tidy: ## Synchronize go.mod and go.sum with the source tree.
	$(GO) mod tidy

install: ## Install all command binaries into Go's configured bin directory.
	$(GO) install ./cmd/...

run: ## Run the coordinator CLI; pass arguments with ARGS='status --help'.
	$(GO) run ./cmd/factory $(ARGS)

report: ## Run the structured-report CLI; pass arguments with ARGS='--help'.
	$(GO) run ./cmd/factory-report $(ARGS)

attach: ## Run the worker-attach CLI; pass arguments with ARGS='--help'.
	$(GO) run ./cmd/factory-worker-attach $(ARGS)

worker-build: ## Build and verify the pinned worker images, then print the config digest.
	./scripts/build-worker.sh

clean: ## Remove binaries built by the build target.
	rm -f $(BINARIES)
