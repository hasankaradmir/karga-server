VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga-server/internal/version.Version=$(VERSION)

.PHONY: build test test-integration test-e2e test-linux lint fmt

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga-server ./cmd/karga-server

test:
	go test -race ./...

# Needs a reachable Docker daemon (DOCKER_HOST or the active docker context).
test-integration:
	go test -tags integration -race -count=1 ./...

# Builds karga-server and the karga CLI (from KARGA_CLI_SRC, default ../karga) and drives them
# against Docker.
test-e2e:
	go test -tags e2e -count=1 ./tests/e2e/...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

# Runs TARGETS in a Linux VM (OrbStack or Lima): the gateway and release proxy are Linux-only.
TARGETS ?= test-integration test-e2e
test-linux:
	scripts/linux-vm.sh $(TARGETS)
