# katamaran

Live migration for Kata Containers on Kubernetes. `CLAUDE.md` links to this file;
edit `AGENTS.md`.

## Gate

`make vet test smoke fuzz lint-shell` must pass before a change is done. Fix
failures without weakening the gate.

## Constraints

- Serialize calls on each `internal/qmp` client; it is not concurrency-safe.
- `cmd/containerd-shim-katamaran-adopted-v2` is experimental.
- Dashboard assets are vendored under `internal/dashboard/assets/` and served
  same-origin. No CDN references, no remote script loads.
- Every fuzz target added under `internal/` or `cmd/` gets a `fuzz-long` entry
  in the Makefile.
- Edit Job manifests only in `internal/orchestrator/templates/`: Go embeds them
  and `deploy/migrate.sh` renders the same files. Do not create deploy copies.
- Migration timeouts and buffer sizes are named constants, not literals. The
  controller's `StatusTimeout` and the Jobs' `activeDeadlineSeconds` must stay
  in agreement.

## Release

Only when explicitly asked to release: version the `[Unreleased]` section in
`CHANGELOG.md`, add its compare link, and push the matching `v*` tag after the
gate passes. `.github/workflows/release.yml` verifies, publishes multi-arch GHCR
images, and creates the GitHub Release from the matching changelog section.
