.PHONY: build test fuzz image dashboard clean vet fmt lint help

# Default target
all: build

# Build the katamaran binary
build:
	go build -o bin/katamaran ./cmd/katamaran/

# Run go vet and gofmt checks
vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

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
	$(CE) build -t localhost/katamaran:dev .
	rm -f katamaran.tar
	$(CE) save localhost/katamaran:dev -o katamaran.tar

# Build the dashboard container image
dashboard:
	$(CE) build -t localhost/katamaran-dashboard:dev -f Dockerfile.dashboard .
	rm -f dashboard.tar
	$(CE) save localhost/katamaran-dashboard:dev -o dashboard.tar

# Remove build artifacts
clean:
	rm -rf bin/

# Show available targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  build       Build bin/katamaran (default)"
	@echo "  test        Run unit tests with race detector"
	@echo "  smoke       Run 64 smoke tests (no VMs required)"
	@echo "  fuzz        Run fuzz test seed corpus (instant)"
	@echo "  fuzz-long   Run actual fuzzing for 30s per target"
	@echo "  vet         Run go vet and gofmt checks"
	@echo "  image       Build katamaran container image"
	@echo "  dashboard   Build dashboard container image"
	@echo "  clean       Remove build artifacts"
