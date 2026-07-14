.PHONY: build test test-unit test-e2e test-e2e-overlay test-e2e-controlplane test-e2e-gpu test-e2e-inference test-e2e-inference-gpu test-e2e-unit lint fmt fmt-check install-hooks clean

# Load .env if present (e.g. DIGITALOCEAN_TOKEN for e2e). .env is gitignored.
-include .env
export

BINARY := bin/forge
GO := go
GOBUILDFLAGS := -trimpath

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/nunocgoncalves/forge/internal/version.version=$(VERSION) \
           -X github.com/nunocgoncalves/forge/internal/version.commit=$(COMMIT) \
           -X github.com/nunocgoncalves/forge/internal/version.date=$(DATE)

build:
	$(GO) build $(GOBUILDFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/forge

test: test-unit

test-unit:
	$(GO) test -race -count=1 ./...

test-e2e:
	cd test/e2e && go test -race -count=1 -timeout 25m -run '^TestE2E$$' .

test-e2e-overlay:
	cd test/e2e && go test -race -count=1 -timeout 25m -run '^TestE2EOverlay$$' .

test-e2e-controlplane:
	cd test/e2e && go test -race -count=1 -timeout 15m -run '^TestControlPlaneIdentity$$' ./...

test-e2e-gpu:
	cd test/e2e && go test -race -count=1 -timeout 45m -run '^TestGPUE2E$$' .

test-e2e-inference:
	cd test/e2e && go test -race -count=1 -timeout 15m -run '^TestInferenceFlowContract$$' .

test-e2e-inference-gpu:
	cd test/e2e && go test -race -count=1 -timeout 45m -run '^TestInferenceFlowGPU$$' .

# Pure unit tests for the e2e harness internals (kindtest): fast, no network,
# no cluster. Covers the chart auto-resolution helpers (HOR-321). The e2e
# module is a separate Go module, so this is scoped to ./internal/...
# (the real e2e tests need Kind/DO and run via test-e2e-*).
test-e2e-unit:
	cd test/e2e && go test -race -count=1 ./internal/...

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install-hooks:
	git config core.hooksPath .githooks

clean:
	rm -rf bin/
	$(GO) clean -testcache
