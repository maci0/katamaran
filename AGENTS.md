# katamaran

Live migration for Kata Containers on Kubernetes. `CLAUDE.md` links to this file;
edit `AGENTS.md`.

## Gate

`make check` (go mod verify, vet, test, smoke, fuzz, lint-shell, lint-js, build
all binaries) must pass before a change is done. Fix failures without weakening
the gate.

## Constraints

- Binaries are built by the `build-%` pattern rule in the Makefile: every
  command under `cmd/` must have a matching name in `BINARIES`, and the flag
  set lives in that one rule.
- `make repro-check` builds everything twice under a different locale and
  timezone and diffs the result. The binaries must stay byte-identical; a diff
  means host state reached the build.
- Serialize calls on each `internal/qmp` client; it is not concurrency-safe.
- `cmd/containerd-shim-katamaran-adopted-v2` is experimental.
- Dashboard assets are vendored under `internal/dashboard/assets/` and served
  same-origin. No CDN references, no remote script loads.
- Every file under `internal/dashboard/assets/` is third-party code, so it
  carries a digest in `assets/SHA256SUMS` and an origin, version, and license
  entry in `assets/ATTRIBUTION.md`. Adding or bumping a bundle updates both;
  `TestVendoredAssetsMatchSHA256SUMS` fails otherwise. `newMux` serves each
  asset by name, so neither manifest has an HTTP route.
- Every fuzz target added under `internal/` or `cmd/` gets a `fuzz-long` entry
  in the Makefile.
- Edit Job manifests only in `internal/orchestrator/templates/`: Go embeds them
  and `deploy/migrate.sh` renders the same files. Do not create deploy copies.
- Migration timeouts and buffer sizes are named constants, not literals. Keep
  the controller's `StatusTimeout` and both Jobs' `activeDeadlineSeconds` equal
  in duration and above `storageSyncTimeout + migrationTimeout`, with headroom
  for Job startup and CNI convergence, to avoid aborting healthy migrations.

## Release

Only when explicitly asked to release: version the `[Unreleased]` section in
`CHANGELOG.md`, add its compare link, and push the matching `v*` tag after the
gate passes. `.github/workflows/release.yml` verifies, publishes multi-arch GHCR
images, and creates the GitHub Release from the matching changelog section.
