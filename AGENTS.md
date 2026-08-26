# katamaran

Zero-packet-drop live migration for Kata Containers on Kubernetes. Global rules
in `~/Desktop/Projects/maci0/AGENTS.md` apply; this file holds what is specific
to this repo.

## Layout

- `cmd/katamaran`: migration binary, `--mode source|dest`. Runs privileged in
  the migration Jobs.
- `cmd/katamaran-mgr`: Migration CRD controller plus the validating admission
  webhook that blocks replacement pods during adoption.
- `cmd/katamaran-dashboard`: web UI and JSON API over the Native orchestrator.
- `cmd/katamaran-orchestrator`: JSON-in / NDJSON-out CLI over the same package.
- `cmd/katamaran-factory`: gRPC VM cache server the Kata shim can adopt from.
- `cmd/containerd-shim-katamaran-adopted-v2`: containerd v2 shim that adopts a
  surviving migrated QEMU. Experimental, see its package doc.
- `internal/migration`: source and dest migration logic, QEMU cmdline replay,
  tunnel and qdisc setup.
- `internal/orchestrator`: Job rendering and submission. Job manifests live in
  `internal/orchestrator/templates/` and are `go:embed`ed; they are canonical,
  and `deploy/migrate.sh` renders those same files.
- `internal/qmp`: QMP client. Not safe for concurrent use.
- `internal/controller`, `internal/factory`, `internal/dashboard`,
  `internal/adopt`, `internal/logging`, `internal/buildinfo`, `internal/qmptest`.
- `config/crd/`: CRD and RuntimeClass manifests. `deploy/`: cluster manifests
  and the shell migration harness. `scripts/`: test and cluster tooling.

## Gate

`make vet test smoke fuzz lint-shell` must pass before a change is done. CI runs
the same targets on amd64 and arm64 plus `govulncheck` and multi-arch image
builds.

## Constraints

- Dashboard assets are vendored under `internal/dashboard/assets/` and served
  same-origin. No CDN references, no remote script loads.
- Every fuzz target added under `internal/` or `cmd/` gets a `fuzz-long` entry
  in the Makefile.
- Job template changes belong in `internal/orchestrator/templates/`, never in a
  copy under `deploy/`.
- Migration timeouts and buffer sizes are named constants, not literals. The
  controller's `StatusTimeout` and the Jobs' `activeDeadlineSeconds` must stay
  in agreement.

## Release

Bump the `[Unreleased]` section in `CHANGELOG.md` to the new version, add its
compare link, then push a `v*` tag. `.github/workflows/release.yml` re-runs the
gate, pushes multi-arch images to GHCR, and creates the GitHub Release from the
matching CHANGELOG section.
