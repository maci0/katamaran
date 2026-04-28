.PHONY: all build build-dashboard build-orchestrator build-mgr test smoke fuzz fuzz-long image dashboard mgr clean vet help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/maci0/katamaran/internal/buildinfo.Version=$(VERSION)

# Default target
all: build build-dashboard build-orchestrator build-mgr

# Build the katamaran binary
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran ./cmd/katamaran/

# Build the dashboard binary
build-dashboard:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-dashboard ./cmd/dashboard/

# Build the orchestrator CLI (JSON-in / NDJSON-out wrapper around the
# orchestrator package). Used by scripts and local orchestration workflows.
build-orchestrator:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-orchestrator ./cmd/katamaran-orchestrator/

# Build the Migration CRD controller binary.
build-mgr:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-mgr ./cmd/katamaran-mgr/

# Run go vet and gofmt checks
vet:
	go vet ./cmd/... ./internal/...
	@test -z "$$(gofmt -l cmd internal)" || (echo "gofmt needed on:"; gofmt -l cmd internal; exit 1)

# Run unit tests with race detector
test:
	go test ./... -count=1 -timeout 120s -race

# Run smoke tests (no VMs required)
smoke:
	./scripts/test.sh

# Run fuzz test seed corpus (instant, validates seeds)
fuzz:
	go test ./internal/qmp/ -run "^Fuzz" -count=1
	go test ./internal/migration/ -run "^Fuzz" -count=1

# Run actual fuzzing for 30s per target
fuzz-long:
	go test ./internal/qmp/ -fuzz=FuzzResponseUnmarshal -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzClientProtocol -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzBlockJobInfoUnmarshal -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzMigrateInfoUnmarshal -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzErrorFormat -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzArgsSerialization -fuzztime=30s
	go test ./internal/migration/ -fuzz=FuzzFormatQEMUHost -fuzztime=30s

CE ?= $(shell command -v podman 2>/dev/null || echo docker)

# Build the katamaran container image
image:
	$(CE) build --build-arg VERSION=$(VERSION) -t localhost/katamaran:dev .
	$(CE) save localhost/katamaran:dev -o katamaran.tar.tmp && mv katamaran.tar.tmp katamaran.tar

# Build the dashboard container image
dashboard:
	$(CE) build --build-arg VERSION=$(VERSION) -t localhost/katamaran-dashboard:dev -f Dockerfile.dashboard .
	$(CE) save localhost/katamaran-dashboard:dev -o dashboard.tar.tmp && mv dashboard.tar.tmp dashboard.tar

# Build the Migration controller container image
mgr:
	$(CE) build --build-arg VERSION=$(VERSION) -t localhost/katamaran-mgr:dev -f Dockerfile.mgr .
	$(CE) save localhost/katamaran-mgr:dev -o mgr.tar.tmp && mv mgr.tar.tmp mgr.tar

# Remove build artifacts
clean:
	rm -rf bin/
	rm -f katamaran.tar dashboard.tar mgr.tar *.tar.tmp coverage.out *_cover.out

# Show available targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build            Build bin/katamaran"
	@echo "  build-dashboard  Build bin/katamaran-dashboard"
	@echo "  build-orchestrator Build bin/katamaran-orchestrator"
	@echo "  build-mgr        Build bin/katamaran-mgr"
	@echo "  test             Run unit tests with race detector"
	@echo "  smoke            Run smoke tests (no VMs required)"
	@echo "  fuzz             Run fuzz test seed corpus (instant)"
	@echo "  fuzz-long        Run actual fuzzing for 30s per target"
	@echo "  vet              Run go vet and gofmt checks"
	@echo "  image            Build katamaran container image"
	@echo "  dashboard        Build dashboard container image"
	@echo "  mgr              Build katamaran-mgr container image"
	@echo "  clean            Remove build artifacts"
