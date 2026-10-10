# Development commands for the Software Factory Go module.

GO ?= go
BINDIR ?= bin

# The go.mod toolchain line names the approved, patched Go release. Every Go
# command started here runs that exact release, and Go downloads it when the
# Go on PATH is a different version. toolchain-check refuses any other result.
GO_TOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.mod)
export GOTOOLCHAIN := $(GO_TOOLCHAIN)

# factory onboard hands this checkout to the onboarding agent, so the installed
# binary records where it was built from. The single quotes keep a checkout
# path with spaces in one link flag.
LDFLAGS := -X 'github.com/Stevie1704/sw-factory/internal/cli.factoryCheckout=$(CURDIR)'

BINARIES := \
	$(BINDIR)/factory \
	$(BINDIR)/factory-report \
	$(BINDIR)/factory-worker-headless

.DEFAULT_GOAL := help

.PHONY: help all toolchain-check build test test-race vet fmt fmt-check check vuln-check deps tidy install run report worker-build worker-scan worker-publish clean

help: ## Show the available development commands.
	@awk 'BEGIN { FS = ":.*##"; print "Usage: make <target>\n"; print "Targets:" } /^[a-zA-Z0-9_.-]+:.*##/ { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: check ## Run formatting, static analysis, tests, and a build.

toolchain-check: ## Refuse to continue unless Go runs the approved go.mod toolchain.
	@actual="$$($(GO) env GOVERSION)" || exit 1; \
	if [ -z "$(GO_TOOLCHAIN)" ] || [ "$$actual" != "$(GO_TOOLCHAIN)" ]; then \
		echo "Go runs $$actual, but go.mod approves '$(GO_TOOLCHAIN)'." >&2; \
		echo "Keep the go.mod toolchain line, and let Go download that release:" >&2; \
		echo "do not set GOTOOLCHAIN on the make command line, and allow GOPROXY to serve golang.org/toolchain." >&2; \
		exit 1; \
	fi

build: toolchain-check ## Build all command binaries into BINDIR (default: bin).
	@mkdir -p "$(BINDIR)"
	$(GO) build -ldflags "$(LDFLAGS)" -o "$(BINDIR)/factory" ./cmd/factory
	$(GO) build -o "$(BINDIR)/factory-report" ./cmd/factory-report
	$(GO) build -o "$(BINDIR)/factory-worker-headless" ./cmd/factory-worker-headless

test: toolchain-check ## Run the complete Go test suite.
	$(GO) test ./...

test-race: toolchain-check ## Run the complete test suite with the race detector.
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

vuln-check: build ## Scan the module source and the built binaries with pinned govulncheck.
	./scripts/scan-go-artifacts.sh --source $(BINARIES)

deps: ## Download the module dependencies.
	$(GO) mod download

tidy: ## Synchronize go.mod and go.sum with the source tree.
	$(GO) mod tidy

install: toolchain-check ## Install all command binaries into Go's configured bin directory.
	$(GO) install -ldflags "$(LDFLAGS)" ./cmd/...

run: ## Run the coordinator CLI; pass arguments with ARGS='status --help'.
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/factory $(ARGS)

report: ## Run the structured-report CLI; pass arguments with ARGS='--help'.
	$(GO) run ./cmd/factory-report $(ARGS)

worker-build: ## Build and verify the pinned worker images, then print the config digest.
	./scripts/build-worker.sh

worker-scan: ## Scan a worker image; pass WORKER_REFERENCE=image@digest (default: factory.yaml).
	./scripts/scan-worker-image.sh $(WORKER_REFERENCE)

worker-publish: ## Scan, push, and verify a locally built worker; pass WORKER_REFERENCE=image:tag.
	./scripts/publish-worker.sh $(WORKER_REFERENCE)

clean: ## Remove binaries built by the build target.
	rm -f $(BINARIES)
