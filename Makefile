VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/hasankaradmir/karga-server/internal/version.Version=$(VERSION)

.PHONY: build dist test test-integration test-e2e scenario test-linux lint fmt envoy-image

build:
	go build -ldflags "$(LDFLAGS)" -o bin/karga-server ./cmd/karga-server

# dist builds the release archives without a release: the names and contents goreleaser gives them,
# and checksums.txt, so scripts/install.sh can install them from file://.
DIST_TARGETS ?= linux/amd64 linux/arm64
dist:
	rm -rf dist && mkdir -p dist
	set -e; for t in $(DIST_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; name=karga-server_$(VERSION:v%=%)_$${os}_$${arch}; \
		mkdir -p dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o dist/$$name/karga-server ./cmd/karga-server; \
		COPYFILE_DISABLE=1 tar --no-xattrs -czf dist/$$name.tar.gz -C dist/$$name karga-server -C $(CURDIR) README.md CHANGELOG.md LICENSE deploy/systemd/karga-server.service; rm -rf dist/$$name; \
	done
	cd dist && if command -v sha256sum >/dev/null; then sha256sum *.tar.gz; else shasum -a 256 *.tar.gz; fi > checksums.txt
	@echo "install: KARGA_VERSION=$(VERSION) KARGA_DOWNLOAD_URL=file://$(CURDIR)/dist KARGA_SKIP_SIGNATURE=1 sh scripts/install.sh"

test:
	go test -race ./...

# Needs a reachable Docker daemon (DOCKER_HOST or the active docker context).
test-integration:
	go test -tags integration -race -count=1 ./...

# Builds karga-server and the karga CLI (from KARGA_CLI_SRC, default ../karga-cli) and drives them
# against Docker.
test-e2e:
	go test -tags e2e -count=1 ./tests/e2e/...

# Drives karga-server and the karga CLI (KARGA_CLI_SRC, default ../karga-cli) through installs,
# upgrades, backups, repairs, a crash and its resume against Docker (Linux: make test-linux TARGETS=scenario).
scenario:
	scripts/scenario.sh

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

# Runs TARGETS in a Linux VM (OrbStack or Lima): the gateway and release proxy are Linux-only.
TARGETS ?= test-integration test-e2e
test-linux:
	scripts/linux-vm.sh $(TARGETS)

# envoy-image prints the Envoy image karga-server runs (CI pulls it ahead of the tests).
envoy-image:
	@sed -n 's/^const DefaultImage = "\(.*\)"$$/\1/p' internal/infrastructure/envoy/envoy.go
