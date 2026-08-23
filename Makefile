.PHONY: all build build-dashboard build-orchestrator build-mgr build-factory build-adopted-shim test smoke fuzz fuzz-long image dashboard mgr factory clean vet help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/maci0/katamaran/internal/buildinfo.Version=$(VERSION)

# Default target
all: build build-dashboard build-orchestrator build-mgr build-factory build-adopted-shim

# Build the katamaran binary
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran ./cmd/katamaran/

# Build the dashboard binary
build-dashboard:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-dashboard ./cmd/katamaran-dashboard/

# Build the orchestrator CLI (JSON-in / NDJSON-out wrapper around the
# orchestrator package). Used by scripts and local orchestration workflows.
build-orchestrator:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-orchestrator ./cmd/katamaran-orchestrator/

# Build the Migration CRD controller binary.
build-mgr:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-mgr ./cmd/katamaran-mgr/

# Build the VM factory server binary.
build-factory:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/katamaran-factory ./cmd/katamaran-factory/

# Build the containerd v2 adoption shim (Approach E). See
# cmd/containerd-shim-katamaran-adopted-v2/main.go package doc.
build-adopted-shim:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/containerd-shim-katamaran-adopted-v2 ./cmd/containerd-shim-katamaran-adopted-v2/

# Run go vet and gofmt checks (-s also enforces gofmt simplifications).
# gofmt covers every tracked .go file so it stays in sync with `./...`
# even when packages live outside cmd/ and internal/.
vet:
	go vet ./...
	@test -z "$$(gofmt -s -l $$(git ls-files '*.go'))" || (echo "gofmt needed on:"; gofmt -s -l $$(git ls-files '*.go'); exit 1)

# Run unit tests with race detector
test:
	go test ./... -count=1 -timeout 120s -race

# Run smoke tests (no VMs required)
smoke:
	./scripts/test.sh

# Run fuzz test seed corpus (instant, validates seeds across every package)
fuzz:
	go test ./... -run "^Fuzz" -count=1

# Run actual fuzzing for 30s per target
fuzz-long:
	go test ./internal/qmp/ -fuzz=FuzzResponseUnmarshal -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzClientProtocol -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzBlockJobInfoUnmarshal -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzMigrateInfoUnmarshal -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzErrorFormat -fuzztime=30s
	go test ./internal/qmp/ -fuzz=FuzzArgsSerialization -fuzztime=30s
	go test ./internal/migration/ -fuzz=FuzzFormatQEMUHost -fuzztime=30s
	go test ./internal/migration/ -fuzz=FuzzParseCmdlineBytes -fuzztime=30s
	go test ./internal/migration/ -fuzz=FuzzFindSrcSandboxDir -fuzztime=30s
	go test ./internal/migration/ -fuzz=FuzzParsePodRef -fuzztime=30s
	go test ./internal/orchestrator/ -fuzz=FuzzValidateSafeArgValue -fuzztime=30s
	go test ./internal/dashboard/ -fuzz=FuzzSplitTarget -fuzztime=30s
	go test ./internal/dashboard/ -fuzz=FuzzValidTargetPort -fuzztime=30s
	go test ./internal/dashboard/ -fuzz=FuzzValidFormValue -fuzztime=30s
	go test ./cmd/katamaran-orchestrator/ -fuzz=FuzzReadRequest -fuzztime=30s
	go test ./cmd/katamaran-mgr/ -fuzz=FuzzHandleAdmit -fuzztime=30s
	go test ./cmd/containerd-shim-katamaran-adopted-v2/ -fuzz=FuzzValidAdoptedSandboxID -fuzztime=30s

CE ?= $(shell command -v podman 2>/dev/null || echo docker)
GOARCH ?= $(shell go env GOARCH)

# Build the katamaran container image
image:
	$(CE) build --build-arg VERSION=$(VERSION) --build-arg TARGETARCH=$(GOARCH) -t localhost/katamaran:dev .
	$(CE) save localhost/katamaran:dev -o katamaran.tar.tmp && mv katamaran.tar.tmp katamaran.tar

# Build the dashboard container image
dashboard:
	$(CE) build --build-arg VERSION=$(VERSION) --build-arg TARGETARCH=$(GOARCH) -t localhost/katamaran-dashboard:dev -f Dockerfile.dashboard .
	$(CE) save localhost/katamaran-dashboard:dev -o dashboard.tar.tmp && mv dashboard.tar.tmp dashboard.tar

# Build the Migration controller container image
mgr:
	$(CE) build --build-arg VERSION=$(VERSION) --build-arg TARGETARCH=$(GOARCH) -t localhost/katamaran-mgr:dev -f Dockerfile.mgr .
	$(CE) save localhost/katamaran-mgr:dev -o mgr.tar.tmp && mv mgr.tar.tmp mgr.tar

# Build the VM factory server container image
factory:
	$(CE) build --build-arg VERSION=$(VERSION) --build-arg TARGETARCH=$(GOARCH) -t localhost/katamaran-factory:dev -f Dockerfile.factory .
	$(CE) save localhost/katamaran-factory:dev -o factory.tar.tmp && mv factory.tar.tmp factory.tar

# Remove build artifacts
clean:
	rm -rf bin/
	rm -f katamaran.tar dashboard.tar mgr.tar factory.tar *.tar.tmp coverage.out *_cover.out

# Show available targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all                 Build all binaries"
	@echo "  build               Build bin/katamaran"
	@echo "  build-dashboard     Build bin/katamaran-dashboard"
	@echo "  build-orchestrator  Build bin/katamaran-orchestrator"
	@echo "  build-mgr           Build bin/katamaran-mgr"
	@echo "  build-factory       Build bin/katamaran-factory"
	@echo "  build-adopted-shim  Build bin/containerd-shim-katamaran-adopted-v2"
	@echo "  test                Run unit tests with race detector"
	@echo "  smoke               Run smoke tests (no VMs required)"
	@echo "  fuzz                Run fuzz test seed corpus (instant)"
	@echo "  fuzz-long           Run actual fuzzing for 30s per target"
	@echo "  vet                 Run go vet and gofmt checks"
	@echo "  image               Build katamaran container image"
	@echo "  dashboard           Build dashboard container image"
	@echo "  mgr                 Build katamaran-mgr container image"
	@echo "  factory             Build katamaran-factory container image"
	@echo "  clean               Remove build artifacts"
