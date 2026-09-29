# katamaran

Live migration for Kata Containers on Kubernetes. `CLAUDE.md` links to this file;
edit `AGENTS.md`.

## Gate

`make check` (go mod verify, vet, test, smoke, fuzz, lint-shell, lint-js,
lint-yaml, build all binaries) must pass before a change is done. Fix failures
without weakening the gate.

`node` (version in `.node-version`), `biome` (version in `biome.json`'s
`$schema`), and `yamllint` (version in the `lint-yaml` CI job's
`YAMLLINT_VERSION`) must be on `PATH`: the repo ships no `package.json`, so
nothing installs them, and `test`, `lint-js`, and `lint-yaml` fail loud when
one is absent.

## Constraints

- Binaries are built by the `build-%` pattern rule in the Makefile: every
  command under `cmd/` must have a matching name in `BINARIES`, and the flag
  set lives in that one rule.
- `cmd/qmp-hotplug-disk` is an E2E tool. `scripts/e2e.sh` builds it and copies
  it to the test node, so the node needs no Python or QEMU tooling. It is in
  `BINARIES` for build and repro coverage only; release.yml ships the other
  four images and must not gain a matrix entry for it.
- `make repro-check` builds everything twice under a different locale and
  timezone and diffs the result. The binaries must stay byte-identical; a diff
  means host state reached the build.
- Serialize calls on each `internal/qmp` client; it is not concurrency-safe.
- The four shipped images (`Dockerfile`, `Dockerfile.dashboard`,
  `Dockerfile.factory`, `Dockerfile.mgr`) are checked by
  `internal/images/images_test.go`: digest-pinned bases, exec-form ENTRYPOINT,
  the OCI title/source/documentation/description/version labels, and a `USER` in
  the runtime stage unless the file carries a `No USER:` rationale.
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
- Every tracked `.yml`/`.yaml` file is linted by `make lint-yaml` against
  `.yamllint`, which keeps the defect rules (syntax, key-duplicates, brackets,
  trailing-spaces) and disables only the stylistic ones, each with a stated
  reason. `scripts/manifests/kata-pod.yaml` is excluded there: it is a shell
  template, valid only after `scripts/e2e.sh` substitutes its `${...}`
  placeholders.
- Migration timeouts and buffer sizes are named constants, not literals. Keep
  the controller's `StatusTimeout` and both Jobs' `activeDeadlineSeconds` equal
  in duration and above `storageSyncTimeout + migrationTimeout`, with headroom
  for Job startup and CNI convergence, to avoid aborting healthy migrations.

## Release

Only when explicitly asked to release: rename the `## [Unreleased]` heading in
`CHANGELOG.md` to `## [<version>]` (bare semver, no `v`, the exact string
`release.yml` matches), move the compare link to the same version, and push the
matching `v*` tag after the gate passes. `.github/workflows/release.yml`
verifies, publishes multi-arch GHCR images, and creates the GitHub Release from
the matching changelog section.
