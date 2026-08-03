.PHONY: build test test-unit test-e2e test-e2e-overlay test-e2e-secrets test-e2e-flux test-e2e-cert-issuers test-e2e-controlplane test-e2e-gpu test-e2e-inference test-e2e-inference-gpu test-e2e-internal-tls test-e2e-unit lint fmt fmt-check install-hooks clean

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

# The E2E module has one top-level TestE2E runner. -run selects an isolated
# scenario; -v is required so capacity skips and named stages remain visible.
test-e2e:
	cd test/e2e && go test -v -race -count=1 -timeout 60m -run '^TestE2E$$/^digitalocean-cpu$$' .

# Compatibility aliases: overlay, secret-sync, and Flux now compose on the one
# CPU fixture instead of provisioning three additional droplets.
test-e2e-overlay test-e2e-secrets test-e2e-flux: test-e2e

test-e2e-gpu:
	cd test/e2e && go test -v -race -count=1 -timeout 60m -run '^TestE2E$$/^digitalocean-gpu$$' .

# Compatibility alias: real inference now follows GPU smoke on the same VM.
test-e2e-inference-gpu: test-e2e-gpu

test-e2e-cert-issuers:
	cd test/e2e && go test -v -race -count=1 -timeout 15m -run '^TestE2E$$/^kind-cert-issuers$$' .

test-e2e-controlplane:
	cd test/e2e && go test -v -race -count=1 -timeout 15m -run '^TestE2E$$/^kind-controlplane-identity$$' .

test-e2e-internal-tls:
	cd test/e2e && go test -v -race -count=1 -timeout 20m -run '^TestE2E$$/^kind-internal-tls$$' .

test-e2e-inference:
	cd test/e2e && go test -v -race -count=1 -timeout 15m -run '^TestE2E$$/^kind-inference-contract$$' .

# Compile the scenario package without infrastructure, then run pure harness
# unit tests (runner + kind/chart helpers). The nested module is outside the
# root module's ./... and therefore needs this explicit CI target.
test-e2e-unit:
	cd test/e2e && go test -run '^$$' .
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
