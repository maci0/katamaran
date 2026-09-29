.PHONY: all build $(BUILD_TARGETS) check check-node test smoke fuzz fuzz-long repro-check image dashboard mgr factory clean vet lint-shell lint-js help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/maci0/katamaran/internal/buildinfo.Version=$(VERSION)

# Every binary is the main package of cmd/<name>/ and lands in bin/<name>, so
# one pattern rule builds them all. Adding a command is one entry in
# BINARIES, and no binary can pick up different build flags than the others.
BINARIES := katamaran katamaran-dashboard katamaran-orchestrator katamaran-mgr \
            katamaran-factory containerd-shim-katamaran-adopted-v2
BUILD_TARGETS := $(addprefix build-,$(BINARIES))

# Default target
all: $(BUILD_TARGETS)

build: build-katamaran

# -buildvcs=false keeps host VCS state out of the binary; -trimpath keeps the
# build directory out. Both are what make two builds byte-identical.
# -mod=readonly makes a build fail rather than silently edit go.mod/go.sum.
build-%:
	go build -trimpath -buildvcs=false -mod=readonly -ldflags "$(LDFLAGS)" -o "bin/$*" "./cmd/$*/"

# Run go vet and gofmt checks (-s also enforces gofmt simplifications).
# gofmt covers every tracked .go file so it stays in sync with `./...`
# even when packages live outside cmd/ and internal/.
vet:
	go vet -composites.whitelist=false ./...
	@set -e; files=$$(git ls-files '*.go'); \
	if [ -z "$$files" ]; then \
		printf 'git ls-files returned no .go files; run from a git checkout\n' >&2; exit 1; fi; \
	unformatted=$$(gofmt -s -l $$files); \
	test -z "$$unformatted" || { printf 'gofmt needed on:\n%s\n' "$$unformatted"; exit 1; }

# Lint every tracked shell script. git ls-files keeps this in sync with the
# tree (same rationale as the gofmt check above) so a script added outside
# scripts/ cannot silently escape analysis.
lint-shell:
	@set -e; files=$$(git ls-files '*.sh'); \
	if [ -z "$$files" ]; then \
		printf 'git ls-files returned no .sh files; run from a git checkout\n' >&2; exit 1; fi; \
	shellcheck -x --enable=add-default-case,avoid-negated-conditions,avoid-nullary-conditions,deprecate-which,quote-safe-variables,require-double-brackets,useless-use-of-cat $$files

# Lint the hand-written dashboard JS (biome.json scopes the file set and
# excludes the vendored bundles under internal/dashboard/assets). The
# formatter is off in biome.json: the checked-in file predates biome and
# reformatting it is separate work.
lint-js: check-biome
	biome lint

# biome is the one gate tool no manifest governs: the repo ships no
# package.json, so the linter comes from the local toolchain while CI fetches
# the exact version named in biome.json's $schema. Say so when it is missing
# instead of leaving "biome: command not found" as the only signal.
check-biome:
	@set -e; version=$$(sed -n 's|.*biomejs.dev/schemas/\([^/]*\)/.*|\1|p' biome.json); \
	if [ -z "$$version" ]; then \
		printf 'could not read the pinned biome version from biome.json\n' >&2; exit 1; fi; \
	command -v biome >/dev/null 2>&1 || { \
		printf 'biome not found on PATH; the dashboard lint requires %s:\n' "$$version" >&2; \
		printf '  npx --yes "@biomejs/biome@%s" lint\n' "$$version" >&2; \
		exit 1; }

check:
	go mod verify
	$(MAKE) vet test smoke fuzz lint-shell lint-js all

# Build every binary twice from the same tree and diff the results. Two
# builds of the same source must be byte-identical; a diff means the build
# reads host state (VCS metadata, the build directory, the clock). The
# second pass runs under a different locale and timezone, because neither
# may reach a Go binary. Run this from a differently named checkout as
# well to cover the build-path case that -trimpath handles.
repro-check:
	@set -eu; \
	rm -rf bin .repro-a .repro-b; \
	$(MAKE) -s all; cp -R bin .repro-a; rm -rf bin; \
	LC_ALL=C TZ=UTC $(MAKE) -s all; cp -R bin .repro-b; rm -rf bin; \
	if diff -r .repro-a .repro-b; then \
		echo "reproducible: $(words $(BINARIES)) binaries byte-identical across two builds"; \
	else \
		echo "NOT reproducible: see the diff above" >&2; exit 1; \
	fi; \
	rm -rf .repro-a .repro-b

# Run unit tests with race detector
test: check-node
	go test ./... -count=1 -timeout 120s -race
	node --test internal/dashboard/*.test.cjs

# internal/dashboard's asset and form parser tests are Node's built-in test
# runner. Node is the only non-Go toolchain the gate needs, so fail loud
# here rather than at the `node --test` line with a bare "not found".
check-node:
	@command -v node >/dev/null 2>&1 || { \
		printf 'node not found on PATH; the dashboard unit tests require it (see .node-version)\n' >&2; exit 1; }

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
	rm -rf bin/ .repro-a .repro-b
	rm -f katamaran.tar dashboard.tar mgr.tar factory.tar *.tar.tmp coverage.out *_cover.out

# Show available targets
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all                 Build all binaries"
	@echo "  build               Build bin/katamaran"
	@echo "  build-<name>        Build bin/<name> from cmd/<name> (one of: $(BINARIES))"
	@echo "  check               Verify modules, run local checks, build all binaries"
	@echo "  repro-check         Build everything twice and diff the binaries"
	@echo "  test                Run unit tests with race detector"
	@echo "  smoke               Run smoke tests (no VMs required)"
	@echo "  fuzz                Run fuzz test seed corpus (instant)"
	@echo "  fuzz-long           Run actual fuzzing for 30s per target"
	@echo "  vet                 Run go vet and gofmt checks"
	@echo "  lint-shell          Run shellcheck over every tracked .sh file"
	@echo "  image               Build katamaran container image"
	@echo "  dashboard           Build dashboard container image"
	@echo "  mgr                 Build katamaran-mgr container image"
	@echo "  factory             Build katamaran-factory container image"
	@echo "  clean               Remove build artifacts"
