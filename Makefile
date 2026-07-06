.PHONY: build test test-unit test-e2e lint fmt fmt-check install-hooks clean

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
	cd test/e2e && go test -race -count=1 .

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
