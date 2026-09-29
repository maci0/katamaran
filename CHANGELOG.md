# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The project is pre-1.0 and carries no API stability promise. A minor release
(`0.x.0`) may break users, and every such change is listed under Breaking
changes or Removed; a patch release (`0.x.y`) carries fixes only. The release
workflow refuses to publish a tag that has no section below.

## [Unreleased]

### Added

- `SECURITY.md`: supported versions and in-scope issues, stating plainly that
  no private disclosure channel is configured rather than naming one.
- `KATAMARAN_ALLOWED_NAMESPACES` on the dashboard: a comma-separated
  namespace allowlist. `/api/pods` omits pods outside it and
  `POST /api/migrate` answers 403 for a `source_pod_namespace` or
  `dest_pod_namespace` outside it, matching the pin the Migration CRD
  applies to `spec.sourcePod` and `spec.destPod`. Unset leaves the
  dashboard cluster-wide, as before.
- `deploy/monitoring.yaml`, holding the mgr and dashboard metrics
  Services and their ServiceMonitors.
- `make lint-js`: Biome lints the dashboard's hand-written JavaScript, and
  `make check` plus a CI job run it. The file set is scoped in `biome.json`
  so the vendored bundles under `internal/dashboard/assets/` stay out.
- `docs/THREAT_MODEL.md`: a risk-ranked threat model naming the trust
  boundaries and the file that implements each control.
- The recommended `app.kubernetes.io/*` labels on the DaemonSet, the mgr
  and dashboard Deployments and Services, and both Job templates.
- `docs/INSTALL.md` documents pulling the four published GHCR images for a
  release, and the retag the DaemonSet install expects.

### Breaking changes

- `deploy/dashboard.yaml` no longer creates a ClusterIP Service in front of
  the dashboard UI. The dashboard ServiceAccount can list, patch, and delete
  pods in every namespace and creates hostPID migration Jobs, so the Service
  handed an admin surface to every pod in the cluster. Upgrade: any
  `kubectl port-forward -n kube-system svc/katamaran-dashboard 8080:8080`
  or Service-backed ingress, monitoring rule, or `curl` that named that
  Service must be repointed at `svc/katamaran-dashboard-metrics`, which
  now serves the UI on the same port 8080 through the apiserver proxy.
  Applying the new manifest on an existing deployment leaves the old
  Service in place until it is deleted by hand.

### Changed

- `GET /api/status` answers `400` naming the field when `logs_after` or
  `pings_after` is not a non-negative integer. It previously fell back to a
  full snapshot, so a client stuck on a bad cursor never learned why its
  incremental view stopped advancing. Omitting a cursor still returns the
  documented full snapshot.
- `/api/ping` and `/api/httpgen` apply the same `target`-only allowlist to
  the query string they already applied to the form body. A misspelled
  query parameter now answers `400` instead of being dropped while the
  request starts a generator with a defaulted target.
- `deploy/migrate.sh --help` lists its exit codes, matching every binary in
  the project: 0 on a completed migration, 1 on a runtime error, 2 on an
  argument or configuration error.
- `make lint-shell` also enables ShellCheck's `add-default-case` and
  `quote-safe-variables`.
- The dashboard's `/api` route table is now a single declaration. The 405
  `Allow` header is derived from the registered mux patterns instead of a
  hand-maintained copy, so a newly added endpoint can no longer answer 404
  to a client that probes it with the wrong method.
- `deploy/dashboard.yaml` no longer creates a ClusterIP Service in front
  of the dashboard UI. The dashboard ServiceAccount can list, patch, and
  delete pods in every namespace and creates hostPID migration Jobs, so
  the Service handed an admin surface to every pod in the cluster. Reach
  the UI with `kubectl port-forward -n kube-system
  svc/katamaran-dashboard-metrics 8080:8080`.
- The build flags live in one Makefile pattern rule, with
  `-buildvcs=false`, `-trimpath`, and `-mod=readonly` applied to every
  binary, so no command can pick up different flags than the others.
- The release pipeline runs `make repro-check` in its verify job. A build
  that reads host state is now caught before images are pushed to GHCR
  rather than shipped silently.
- `scripts/e2e.sh` builds the katamaran image with `make image` instead of
  invoking the container engine directly, matching the katamaran-mgr path
  it already used. Both images are built with the same flags and both
  archives land in the repository root instead of the caller's cwd.
- The `katamaran-factory` image owns `/var/run/katamaran`, so the
  non-root user it runs as can still create the runtime socket directory.
- The node sandbox scan matches needles in a single pass over `/proc`
  instead of one pass per needle.

### Fixed

- The in-flight migration guard no longer matches a same-named pod in another
  namespace. Migration Jobs from every namespace land in `kube-system` and
  carried only `katamaran.io/source-pod` (the pod name), so a Migration for
  `web` in one namespace joined the running migration of `web` in another and
  that migration's status was written to the wrong Migration CR. Jobs now also
  carry `katamaran.io/source-pod-namespace` and the guard selects on both.
- Replaying the `--replay-cmdline` dest Job submit no longer fails a live
  migration. `stageThenStartDest` treated an `AlreadyExists` create as a hard
  error, so a duplicated staging pass (or a create whose response was lost
  after the API server persisted it) ran the failure path and deleted the
  still-running source Job. It now reuses the existing dest Job, the same
  rule `Resume` already applied.
- `deploy/migrate.sh` no longer deletes a running migration when it is
  re-run. The pre-flight cleanup removed both Jobs for the `JOB_SUFFIX`
  unconditionally, so a second invocation targeting a migration still in
  flight aborted it. It now reaps only Jobs that reached a terminal
  condition and stops with an error when one is still running. Re-running
  after a finished migration is unchanged.
- Fake QMP test servers bind in a short `os.MkdirTemp` directory
  (`qmptest.TempDir`) instead of `t.TempDir()`. On macOS the long
  `/var/folders/...` names overrun the 104-byte `sun_path` limit and the
  listen failed with a bare `bind: invalid argument`. An over-long path is
  now reported with the offending path.
- The dest side probes for `cgroup.controllers` before writing
  `cgroup.procs`. On a cgroup v1 host the mkdir succeeded on a tmpfs and
  the write created a regular file the kernel ignores, logging a
  re-parent that never happened; it now warns and skips the move.
- `scripts/build-minikube-modules.sh` uses `sed -i.bak` instead of the
  GNU-only bare `-i`, which BSD sed rejects.
- `/proc` paths are built with `filepath.Join` rather than string
  concatenation, matching the rest of the tree.
- A rejected `katamaran` flag value exits 2, the documented argument-error
  code, instead of 1. `--tap`, `--tap-netns`, `--drive-id`,
  `--multifd-channels`, and `--emit-cmdline-to` were validated inside the
  migration, after the run had started, so a typo looked to a script like a
  failed migration. The checks are now shared between the CLI and the
  migration package and run before anything starts.
- A QMP line above the 4 MiB cap now latches the client into a desynced
  state and fails every later command with `qmp.ErrDesynced`. Previously the
  oversized fragment was consumed while the rest of that line stayed queued,
  so each subsequent read returned its tail and the source's STOP-poll loop
  retried until the migration budget ran out.
- The source migration binary checks every stdout marker write. A failed
  `KATAMARAN_CMDLINE_AT` or `KATAMARAN_CMDLINE_B64` write now aborts the
  migration at once instead of letting the dest side discover the gap five
  minutes later, after the guest is paused; a failed progress, phase, result
  or VMConfig marker is logged.
- The dashboard bounds a migration it starts with `migrateRunTimeout`. A
  wedged apiserver used to block the poll loop forever, leaving `isMigrating`
  set and answering 409 to every later `POST /api/migrate`.
- The adopted shim recovers from a panic in the ttrpc server and in the exit
  watcher. Either one previously killed the process and left containerd
  without a task to Wait on or Kill while QEMU kept running.
- The adopted shim reports an unreadable OCI `config.json` instead of
  falling back to `katamaran-dest`, which could hand it a different pod's
  QEMU pid, and it logs cgroup removal failures that leave a directory the
  next pid lookup scans.
- The dest binary returns an error when a stale socket cannot be removed,
  rather than letting `waitForSocket` report the dead path as ready.
- The factory watcher no longer marks a migration metadata file as seen when
  it fails to parse, so a transient read anomaly no longer hides that
  migration from every later scan.
- The controller logs when `spec.sourceCleanup` is requested but no discoverer
  is configured, instead of silently leaving the source pod running after a
  successful migration.
- The orchestrator's job-scheduling timeout joins the last list error with the
  deadline error, so `errors.Is` and `errors.As` see both causes.
- `scripts/build-minikube-modules.sh` no longer pipes the container build
  into `grep | head`. A build whose output matched nothing failed the
  script under `pipefail`, and a real build failure printed none of its
  own error. The full log is kept and the last 40 lines are shown on
  failure.
- The dashboard's ICMP ping subprocess runs under `LC_ALL=C`, so a host
  locale can no longer change the output it parses, and the HTML it
  serves declares `charset=utf-8`.
- Migration, webhook, and shim error paths log the failure they used to
  drop: an unreadable `persist.json`, a failed cgroup move, a stuck-in-Running
  pod found by the node scan, an AdmissionReview with no request, and a
  dropped shim log record.
- The destination's writable nvdimm copy now reports a failed `close`. The
  copy is a multi-hundred-MB write whose result dest QEMU maps as VM memory,
  so a close that could not flush was silently handing the VM a truncated
  image; it now fails the copy and removes the temp file like any other
  write error.
- A duplicate migration registration no longer builds a run it discards.
  `startRun` claims the migration id before creating its cancel context and
  update channel, so a join racing a live run leaves nothing behind.

### Security

- Cmdline replay no longer lets a captured source QEMU argv name a host path
  on the destination node. Every `-qmp` socket is repinned to the dest sandbox
  dir, `-monitor` and `-pidfile` are dropped, and a `-chardev file` backend is
  dropped, closing a VM-monitor takeover and an arbitrary root file write in
  the privileged dest job.

### Fixed

- Dashboard list endpoints answer `[]` instead of `null` when empty.
  `GET /api/history` and the `history` array in `GET /api/status` served a
  JSON null on a dashboard that had not completed a migration, while
  `logs` and `pings` in the same response already served `[]`; a client
  iterating `history` threw. `GET /api/nodes` and `GET /api/pods` now
  normalize an empty result the same way at the handler.
- Developer tooling on arm64 hosts, which the release images and the
  arm64 CI job support. `scripts/sweep.sh` hardcoded
  `qemu-system-x86_64` and the x86-only `q35,kernel_irqchip=split` machine
  type for its destination helper VM, so the sweep could not run on an
  aarch64 node; both now come from the node's `uname -m`.
  `scripts/build-minikube-modules.sh` built the kernel modules with an
  x86_64 Buildroot toolchain regardless of the node's architecture, and a
  module that failed to `insmod` was only a warning, so the mismatch
  surfaced later as a `sch_plug` qdisc error. It now selects the toolchain
  from the node architecture, rejects architectures it cannot build for, and
  fails when a module does not load. `scripts/build-minikube-iso.sh` is
  amd64-only upstream, so it now refuses to run on another host, and
  `scripts/e2e.sh` only offers the custom ISO on x86_64.
- `KATAMARAN_POD_WAIT_TIMEOUT` on the mgr now fails at startup (exit 2)
  when it is not a valid Go duration or is not positive, instead of
  logging a warning and running on the 60s default. The resolution also
  moved ahead of the Kubernetes client construction, so a bad value is
  reported before any apiserver connection. An explicitly set
  `--pod-wait-timeout` still wins over the variable, as documented.
- `KUBERNETES_SERVICE_PORT` is validated as a number before it is
  concatenated into the in-cluster apiserver URL. A non-numeric value
  previously surfaced as a TLS or "no such host" error that named
  neither the variable nor its value.
- The rendered Jobs' `activeDeadlineSeconds` and the controller's
  `StatusTimeout` now come from one constant, `migration.JobActiveDeadline`,
  with a test that fails if either Job template drifts from it. A Job
  deadline shorter than the constant silently truncates migrations the
  controller still believes are running.
- `docs/USAGE.md` documents all five runtime environment variables, with
  which binary reads each and whether it is required, instead of only
  `KATAMARAN_MIGRATION_ID`.
- `deploy/monitoring.yaml` extraction. The metrics Services and
  ServiceMonitors moved out of `deploy/dashboard.yaml` so that manifest
  applies on a cluster without a Prometheus Operator, and the mgr's
  scrape objects no longer ship in the dashboard's file.
- `deploy/migration-example.yaml` named a source pod (`kata-demo`) and
  dest node (`kata-worker-b`) that no manifest in the tree creates. It
  now points at the pod the tutorial deploys, `nginx-kata` in the
  default namespace, and at that profile's second node.

## [0.6.0] - 2026-09-28

### Changed

- Docker base image digests for golang 1.27-alpine and alpine 3.24.
- GitHub Actions: docker/setup-qemu-action v4.4.0,
  docker/setup-buildx-action v4.4.1, docker/build-push-action v7.4.0,
  softprops/action-gh-release v3.0.3, and a refreshed
  actions/dependency-review-action pin.
- `go.mod` on k8s.io v0.37.1, google.golang.org/grpc v1.84.0,
  github.com/containerd/containerd/api v1.12.0, and
  github.com/containerd/ttrpc v1.2.10.

## [0.5.0] - 2026-09-18

### Added

- Prometheus scrape targets for mgr and dashboard counters: metrics
  Services plus ServiceMonitors in `deploy/dashboard.yaml`.
- License notices for the vendored Chart.js 4.5.1 and Tailwind 3.4.17
  dashboard assets.
- `make check`, one local contributor target that runs the same
  vet/test/smoke/fuzz/lint-shell gate CI uses, then builds every binary.

### Changed

- `KATAMARAN_MIGRATION_IMAGE` is required on katamaran-mgr and the
  dashboard. They refuse to start when it is unset, and every Migration
  `spec.image` must match it exactly, including after a controller
  restart. `deploy/manager.yaml` and `deploy/dashboard.yaml` set
  `localhost/katamaran:dev`; replace that with a trusted image (prefer
  a digest) before deploying.
- Deployment shutdown for mgr and dashboard: HTTP servers keep serving
  until in-flight requests finish, main waits before exiting, and
  `/readyz` returns 503 once termination starts. Both deploy manifests
  add a 5s preStop sleep and 40s `terminationGracePeriodSeconds`.
- Controller Migration list reads are paginated.
- Dashboard Chart.js is loaded only when a history series exists, and
  form controls use the hull palette.
- Source and dest Job templates `exec` katamaran so Kubernetes SIGTERM
  reaches the binary instead of the wrapper shell.
- `go.mod` on k8s.io v0.37.0, golang.org/x/net v0.59.0,
  golang.org/x/sys v0.48.0, and google.golang.org/grpc v1.83.2.
- All four Dockerfiles pin the golang builder at 1.27-alpine and the
  runtime at alpine 3.24.
- GitHub Actions: checkout v7.0.1, setup-go v7.0.0, cache v6.1.0,
  docker setup/build/login v4, and softprops/action-gh-release v3.0.2.
- Go builds use `-mod=readonly -buildvcs=false` in the Makefile and
  Dockerfiles. `make vet` fails on gofmt drift and tool errors, and
  `make lint-shell` enables extra ShellCheck checks.

### Fixed

- Adoption pods keep the source workload's selector labels, so the
  admission webhook does not deny the replacement it just created.
- Storage mirroring fails closed on stale or terminal ready Jobs,
  cleans up after partial startup, and reports a stable error order.
- Migration cleanup is retry-safe after a controller restart; dest
  staging failure deletes the source Job; `migrate.sh` exits non-zero
  when the dest Job wait fails.
- Recovery poll budget is bounded so a stalled Job list fails closed,
  and discovery is bounded by the dispatch `StatusTimeout`.
- Status channel shutdown is synchronized; phase event time is the
  event time, not the delivery time; completion logs keep the original
  failure.
- Cmdline replay preserves QEMU argument bytes and applies tap
  defaults independently of other replayed flags.
- Auto-downtime: dashboard validation and latency stats honor the
  checkbox, the downtime field stays enabled after unchecking it, and
  sub-millisecond RTT rounds up in the source budget.
- Shim log growth is bounded at runtime; capped log and command output
  stay on UTF-8 rune boundaries.
- Dashboard unknown-field errors are reported in sorted key order;
  loadgen reaps ping processes when scanning ends.
- Smoke tests no longer clobber `bin/katamaran`, exercise stale
  binaries, or fail spuriously under `pipefail`.
- Hot-plug helper help and usage exits are correct.

### Security

- `google.golang.org/grpc` v1.83.2, which fixes GO-2026-6443 (server
  panic on missing authority or Host headers). The factory gRPC server
  is on that path.

## [0.4.2] - 2026-08-26

v0.4.0 and v0.4.1 were tagged but their release runs failed before
publishing anything: no images, no GitHub Release. Everything below
ships under v0.4.2.

### Added

- Kata sandbox adoption. A migrated VM can now be handed back to
  Kubernetes as a running pod instead of being left as a bare QEMU
  process. `spec.adoptVM: true` on a Migration makes the controller
  create an adoption pod on the destination node once the migration
  succeeds. Three pieces make that work:
  - `katamaran-factory`, a gRPC server implementing Kata's
    `CacheService` (`GetBaseVM` / `Config` / `Status` / `Quit`). It
    watches `/run/vc/vm/` for the `migration-meta.json` the dest Job
    writes and serves the migrated VM's state to the Kata shim. Runs
    as a DaemonSet sidecar; the init container points Kata's
    `vm_cache_endpoint` at its socket.
  - `internal/migration`'s `surviveContainerExit`, which re-parents the
    migrated QEMU (and its KVM helper kernel threads) into
    `/sys/fs/cgroup/katamaran-adopted/<sandbox-id>` so it outlives the
    dest Job container's cgroup.
  - `containerd-shim-katamaran-adopted-v2`, a containerd v2 shim that
    attaches to that surviving QEMU and exposes it as a containerd task.
    It serves both the v2 and v3 TTRPC TaskService APIs (containerd
    2.2.x dispatches to v2), resolves the pid from the surviving cgroup,
    and implements Create/Start/State/Wait/Kill/Delete/Connect/Pids.
    Enabled by `config/crd/runtimeclass-adopted.yaml`. Exec, checkpoint,
    and QMP pause/resume are not wired.
- Validating admission webhook on katamaran-mgr that denies replacement
  pods while an adoption is pending, closing the race where a workload
  controller sees zero replicas between the source-pod delete and the
  adoption-pod create and dispatches a cold replacement. Covers
  ReplicaSet (so Deployments), StatefulSet, DaemonSet, and Job owners.
  Certificates are self-signed in-process (ECDSA P-256) and the
  `caBundle` is patched by the leader only, so no cert-manager
  dependency. `failurePolicy: Ignore` keeps the cluster usable when mgr
  is down. New flags: `--webhook-addr`, `--webhook-service`,
  `--webhook-namespace`, `--disable-webhook`.
- Multi-disk migration. `--drive-id` accepts a comma-separated list.
  Source issues a `drive-mirror` per drive and waits for every mirror to
  reach Ready before RAM pre-copy; dest exports each drive from a single
  NBD server.
- Automatic destination node selection. `spec.destNode` is now optional.
  When omitted the controller copies the source pod's nodeSelector and
  tolerations onto the dest Job, adds anti-affinity excluding the source
  node, and lets the scheduler place it. New `spec.destNodeSelector`
  constrains that choice by label without naming a node.
- `spec.sourceCleanup` controls the source pod after a successful
  migration: `none` (default), `delete`, or `orphan` (strip
  ownerReferences, then delete, so the owner does not reschedule).
- Mid-flight controller-restart recovery via
  `Orchestrator.Resume(ctx, id, req) (created bool, err error)`. If the
  mgr leader dies between source-Job and dest-Job creation in
  ReplayCmdline mode, the new leader re-runs the staging step on every
  reconcile tick instead of letting the migration stall until
  StatusTimeout. Counted by `katamaran_migrations_resumed_total`.
  `orchestrator.SourceJobName(id)` / `DestJobName(id)` expose the
  `katamaran-{source,dest}-<id>` naming the recovery path needs.
- Per-migration Prometheus gauges on the controller's `/metrics`,
  labeled by `migration_id`: `katamaran_migration_ram_transferred_bytes`,
  `_ram_total_bytes`, `_phase`, `_downtime_ms`, `_applied_downtime_ms`,
  `_rtt_ms`. Sourced from the existing progress markers, no new QEMU
  queries.
- `dest-starting`, `src-starting`, and `cutover` phases in the status
  stream, so a migration's progress is visible before RAM transfer
  starts and at the VM pause.
- Dashboard migration history: last 100 completed migrations in memory,
  served by `GET /api/history` and included in `GET /api/status`, with a
  UI table. Resets on restart.
- `spec.cniConvergenceDelaySeconds` and the `--cni-convergence-delay`
  flag, a per-migration override for how long the source holds the IP
  tunnel open after cutover while the CNI rebinds the pod.
- `make lint-shell`, which shellchecks every tracked `.sh` file via
  `git ls-files` so scripts added outside `scripts/` cannot escape
  linting. CI runs it, plus a native arm64 job for vet, tests, and fuzz
  seeds.

### Changed

- Dashboard Tailwind and Chart.js are vendored under
  `internal/dashboard/assets/` and served same-origin via `go:embed`
  with versioned filenames. The CSP no longer lists any CDN origin. This
  removes unverified remote script execution from an origin that can
  start and stop live VM migrations, and makes the UI work on air-gapped
  clusters. Chart.js bytes were verified against the SRI hash already
  pinned in-tree.
- `deploy/manager.yaml` is the manager manifest's home; it moved out of
  `config/crd/`. The dashboard command was renamed to match its binary.
- Dest Job memory limit raised 4Gi to 8Gi to cover the `/dev/shm`-backed
  memory-backend-file allocation, which is charged to the container's
  cgroup. The previous limit OOMKilled larger VMs.
- Source's wait for the dest QEMU raised 25s to 60s. On a freshly added
  worker, dest scheduling plus image pull plus virtiofsd bind plus QEMU
  spawn overran the old budget and the first `migrate` hit connection
  refused.
- Job `activeDeadlineSeconds` and the controller's `StatusTimeout` both
  rounded up to 4h to cover drive-mirror on large disks plus CNI
  convergence. The previous 900s silently capped mirroring at 15
  minutes.
- Controller and dashboard reads come from the apiserver watch cache
  rather than quorum reads, source pod logs are followed rather than
  re-fetched on a rolling window, and transferring-phase status patches
  are coalesced to one per 10s. One poll tick of staleness in exchange
  for far less apiserver and etcd load.
- A source-side QMP stall after the VM pause is treated as a successful
  handover rather than an error: kata-shim routinely tears the source
  QEMU down once the destination has resumed.
- RBAC: katamaran-mgr gains pod `create`/`patch`/`delete` for adoption
  and source cleanup, and scoped `get`/`update` on its own
  ValidatingWebhookConfiguration so it can patch its `caBundle` and
  nothing else. The webhook Service routes only to the leader replica.
- Byte sizes in the dashboard use IEC units, and migration duration is
  measured on the monotonic clock.
- `go.mod` on k8s.io v0.36.4 and protobuf v1.36.12.

### Removed

- `orchestrator.ErrReplayCmdlineNotSupported` and the `*rest.Config`
  field on `native`. Both were load-bearing only for the v0.1.x
  SPDY/stager-pod cmdline-replay pipeline removed in v0.2.0.
  `NewFromClient(cs)` is now fully featured, tests included.
- `katamaran-dashboard` Role: `pods/exec create` and namespaced `pods
  create`/`delete`. The in-process orchestrator needs only `jobs
  create/delete/get/list/watch`, `pods get/list/watch`, and `pods/log
  get`. Cluster-scope `pods delete/patch` stays for source-pod cleanup.
  Apply the updated `deploy/dashboard.yaml` when upgrading.
- v0.1.x SPDY-era RBAC grants and the doc lines describing them.

### Fixed

- Dest Job cleanup runs on a cancellation-proof context, so a Migration
  CR deleted while Apply is still waiting for dest scheduling no longer
  orphans Jobs until their `activeDeadlineSeconds` expires.
- CR deletion is deferred until the worker registers its cancel func,
  and in-flight dispatch is cancelled on delete.
- Abandoned status streams and the config poller exit instead of
  wedging; orchestrator shutdown is bounded; QMP timeouts are reported
  as timeouts rather than generic failures.
- The migrate stop endpoint actually calls `Orchestrator.Stop`.
- Errors that previously vanished are surfaced: poller failures, stop
  path failures, pending-mark failures, dest migration cleanup, and shim
  failures.
- Marker integers that are malformed or out of range are rejected in
  `parseInt64` instead of being silently coerced.
- Pod references in the Migration CRD are validated against DNS-1123 and
  pinned to their own namespace. Source pod log URL paths are
  URL-encoded so a name containing `/` cannot address `/exec` or `/log`.
- Temporary cmdline files are removed at exit and on failure paths, the
  factory VM queue is bounded, and shim log growth is capped.
- `GetBaseVM` does not block when the queue is empty; it returns
  Unavailable so kata-shim falls back to cold VM creation instead of
  stalling every fresh sandbox.
- Arch-specific Kata QEMU binary is picked for cmdline replay.
- Certificate validity uses calendar years and timestamps are UTC.
- Stale shim sockets are cleaned up, and the dest Job is removed when
  re-rendering fails.
- `go.mod` go directive on 1.26.6, clearing the stdlib advisories
  govulncheck flagged against 1.26.2 (GO-2026-6218, -6091, -6090, -6089,
  GO-2026-5972, -5856, GO-2026-5039, -5038).
- All four Dockerfiles pin the golang builder at go 1.26.7 (previously
  1.26.2), so the shipped binaries carry the patched stdlib and not just
  the analysis. The old pin could not build against the raised go
  directive at all: the image sets `GOTOOLCHAIN=local`.
- `make lint-shell` passes on the shellcheck build CI runs, which reports
  a trap-invoked cleanup as SC2317 where newer builds report SC2329.

## [0.3.0] - 2026-05-07

### Added

- E2E `--tcg` flag for macOS Apple Silicon (experimental): full live
  migration under QEMU software emulation on kind + Podman, no KVM
  required. Arch-aware TCG wrapper patches (accel swap on x86_64;
  gic-version, CPU model, nvdimm, PMU fixes on aarch64), reduced TCG VM
  memory to prevent OOM, and vhost-vsock/net device mounts plus kernel
  modules for kind nodes. Documented in TESTING.md section 12.
- Configurable orchestrator pod-wait timeout with a three-layer
  override chain: `--pod-wait-timeout` flag, `KATAMARAN_POD_WAIT_TIMEOUT`
  env var, and `spec.podWaitTimeoutSeconds` in the Migration CRD
  (`Request.PodWaitTimeoutSeconds`).
- CI: multi-arch container image builds (linux/amd64 + linux/arm64) via
  QEMU user-mode emulation.

### Fixed

- E2E destination QEMU cmdline replay now mirrors
  `internal/migration/destspawn.go`: strips `fd=` / `vhostfd=` / `fds=`
  args, adds `ifname` / `script=no` for tap, and assigns a fresh vsock
  guest-cid.
- E2E scripts are bash 3.2 compatible (macOS default shell); qmp disk
  hotplug uses a python3 helper instead of netcat; test disk images are
  created with `truncate` instead of `qemu-img`.
- E2E harness applies RBAC for migration Jobs and detects the container
  engine by daemon reachability rather than binary presence.
- Makefile passes `TARGETARCH=$(GOARCH)` so native ARM64 image builds work.

[0.3.0]: https://github.com/maci0/katamaran/releases/tag/v0.3.0

## [0.2.0] - 2026-05-01

Architectural refactor: cmdline replay no longer needs a stager pod
or SPDY exec. The source binary stamps the captured QEMU cmdline as
a `KATAMARAN_CMDLINE_B64=<base64>` marker on its own pod log; the
dest binary fetches that line via the in-cluster apiserver and
replays normally. Existing `--replay-cmdline=<file>` flag still
works for manual `deploy/migrate.sh` runs.

### Added

- `katamaran --replay-cmdline-from-pod <namespace>/<pod>` (dest mode):
  the dest binary fetches the source pod's log via the apiserver,
  scans for `KATAMARAN_CMDLINE_B64=`, decodes, and replays.
  Implemented in `internal/migration/cmdlinefetch.go`.
- `KATAMARAN_CMDLINE_B64` marker emitted by the source binary
  alongside the existing `KATAMARAN_CMDLINE_AT` path marker.
- New unit tests `TestInjectReplayFromPod_AppendsFlag` and
  `TestInjectReplayFromPod_NoKatamaranContainer` cover the Native
  orchestrator's argv-patch helper.

### Changed

- `internal/orchestrator/native_replay.go` shrunk from ~290 lines to
  ~17. The whole SPDY/stager/hostPath pipeline (`stageCmdline`,
  `podCat`, `podWrite`, `podStream`, `createStagerPod`,
  `waitPodReady`, `limitWriter`, `dirOf`, `hostPathType`,
  `int64ptr`, `boolptr`) is gone.
- `katamaran-mgr` RBAC: dropped `pods/exec` create + `pods` create /
  delete. Only `pods get/list/watch` and `pods/log get` remain on
  the controller's SA. **Note for upgraders:** apply the new
  `deploy/manager.yaml` to align RBAC; the controller will not
  attempt the removed verbs but stale grants are harmless.
- `katamaran-source` SA gains `pods/log get` so the dest job's
  binary can read the source pod's log over the apiserver. Apply
  the updated `deploy/dashboard.yaml` when upgrading.
- `internal/orchestrator/native.go` Apply path no longer adds
  `--replay-cmdline=<file>` to the dest job's args. Dest gets
  `--replay-cmdline-from-pod=<ns>/<pod>` patched in by
  `injectReplayFromPod` once the source pod's name is known.

### Fixed

- The dest binary's pod-log GET originally set
  `Accept: text/plain`, which the apiserver's pod-log subresource
  rejects with HTTP 406. Drop the header; Go's default `*/*` works.
  Surfaced + fixed live during the v0.2.0 verification run.

[0.2.0]: https://github.com/maci0/katamaran/releases/tag/v0.2.0

## [0.1.2] - 2026-04-30

### Added

- `Request.CNIConvergenceDelaySeconds` /
  `.spec.cniConvergenceDelaySeconds` (and source CLI flag
  `--cni-convergence-delay`): a per-migration override for how long
  the source keeps the IP tunnel alive after the cutover so the
  cluster's CNI can propagate the pod's new node binding. Zero
  falls back to the compile-time default (5s). Cilium /
  OVN-Kubernetes converge sub-second; Calico / Flannel often want
  5-10s. Surfaced when a live HTTP loadgen test against a
  kata-nginx pod showed connection-refused traffic for ~58s after
  cutover with the previous fixed 5s delay.

### Fixed

- `dashboard.Run()` panicked on second invocation in the test
  process: `expvar.NewString("version")` rejects duplicate
  registration. Replaced with an idempotent `publishExpvars`
  helper that reuses already-registered vars and rebinds the
  underlying counter functions on subsequent calls. Surfaced by
  `go test -count=3 -race ./...`.

### Changed

- `nativeRun.send`: dropped a dead first `select` that had empty
  case bodies and always exited via the `default`. The actual
  short-circuit on a closed run lives in the second select. The
  recovered-panic path now logs at Debug instead of being
  silently swallowed.

[0.1.2]: https://github.com/maci0/katamaran/releases/tag/v0.1.2

## [0.1.1] - 2026-04-29

### Added

- Release workflow (`.github/workflows/release.yml`): on every `v*`
  tag push, build multi-arch images (linux/amd64 + linux/arm64) for
  `katamaran`, `katamaran-dashboard`, and `katamaran-mgr`; push to
  `ghcr.io/<owner>/<image>:<vN.M.P>` and `:latest`; create a GitHub
  Release whose body is the CHANGELOG section for the tag. v0.1.0
  was tagged before this workflow existed; v0.1.1 is the first run.

[0.1.1]: https://github.com/maci0/katamaran/releases/tag/v0.1.1

## [0.1.0] - 2026-04-29

First tagged release. Zero-packet-drop live migration of Kata Containers
through QMP, driven from a CRD or a web dashboard.

### Added

#### Migration core (`katamaran` binary)
- Source / destination QMP-driven live-migration loops with auto-converge,
  multifd RAM channels, and IPIP / GRE / none tunnel modes.
- Pod-mode resolver: `--pod-name` / `--pod-namespace` resolves a kata pod's
  sandbox UUID, QEMU PID, and VM IP from the apiserver at runtime so
  callers don't need to hand-stitch the QMP socket path or VM IP.
- `--replay-cmdline`: dest binary spawns its own QEMU by replaying the
  source's captured `/proc/<pid>/cmdline` with `-incoming defer`. Removes
  the need for a placeholder kata pod on the destination node.
- `--auto-downtime`: ICMP-based RTT measurement to dest, downtime limit
  programmed as `max(rtt × 2 + floor, floor)`. `--auto-downtime-floor-ms`
  overrides the floor (default 25 ms).
- Structured stdout markers (`KATAMARAN_PROGRESS`, `KATAMARAN_RESULT`,
  `KATAMARAN_DOWNTIME_LIMIT`, `KATAMARAN_CMDLINE_AT`) the orchestrator
  scrapes from pod logs to drive UI / CR status updates without parsing
  slog text or JSON.

#### Native orchestrator (`internal/orchestrator`)
- In-cluster client-go path: renders the embedded source/dest Job
  templates, submits via apiserver, polls Job conditions for status.
- Per-migration Job names (`katamaran-source-<id>` /
  `katamaran-dest-<id>`) so concurrent migrations across different
  destination nodes don't collide.
- ReplayCmdline support: SPDY-execs into the source pod to read the
  cmdline file, creates a transient stager Pod on the dest node to land
  it via hostPath, then submits the dest Job.
- Final synchronous KATAMARAN_RESULT scrape on PhaseSucceeded so the
  terminal StatusUpdate carries actual downtime and final RAM totals
  even when poll fires between tailProgress ticks.

#### Migration CRD (`katamaran.io/v1alpha1`)
- `Migration` custom resource: `spec.sourcePod`, `spec.destNode`,
  `spec.image`, `spec.sharedStorage`, `spec.replayCmdline`,
  `spec.tunnelMode`, `spec.downtimeMS`, `spec.autoDowntime`,
  `spec.autoDowntimeFloorMS`, `spec.multifdChannels`.
- `.status` carries `phase`, `migrationID`, `startedAt`, `completedAt`,
  `ramTransferred`, `ramTotal`, `actualDowntimeMS`, `appliedDowntimeMS`,
  `rttMS`, `autoDowntime`, plus `message` / `error`.
- `kubectl get migration` printer columns: Source, Dest, Phase,
  Downtime (priority 1), Age.

#### Controller (`katamaran-mgr`)
- Polling reconciler with three paths: dispatch new CRs, recover
  in-flight CRs after a controller restart by inspecting the labelled
  Jobs, and run the deletion finalizer (`katamaran.io/finalizer`) so
  `kubectl delete migration` cancels the underlying Jobs.
- Lease-based leader election (15s lease, 10s renew). Default
  Deployment runs 2 replicas + PodDisruptionBudget (minAvailable=1)
  with soft pod-anti-affinity across nodes.
- Hardened pod securityContext: runAsNonRoot, drop ALL caps,
  readOnlyRootFilesystem, seccomp RuntimeDefault.
- HTTP server on `:8081` exposing `/healthz`, `/readyz`, `/debug/vars`
  (Go expvar JSON), and `/metrics` (Prometheus text-format).
- Counters: `katamaran_migrations_dispatched_total`, `_succeeded_total`,
  `_failed_total`, `_recovered_total`, `_deleted_total`, `_inflight`,
  `_reconcile_errors_total`, `_watch_lost_total`.

#### Dashboard
- Pod-picker UX: dropdowns auto-populate from `/api/pods` /
  `/api/nodes`. Pick a kata-qemu source pod and a destination node; the
  backend resolves QMP socket, sandbox UUID, QEMU PID, pod IP, and node
  internal IP from the apiserver.
- Live RAM transfer progress widget (percentage bar mid-flight, green
  "done" bar with actual VM downtime once the dest job completes).
- Auto-downtime checkbox; manual downtime field auto-disables when
  checked.
- ICMP / HTTP loadgen panels with a Chart.js latency chart that shows
  buffered packets during cutover as RTT spikes.
- Final succeeded log line includes wall-clock + setup/xfer breakdown
  and the limit recap, e.g.
  `>>> succeeded: 2.25 GB transferred, 18ms downtime (limit 25ms, auto), 30s wall (2s setup + 28s xfer)`.

#### Operations
- Multi-arch container images (`localhost/katamaran:dev`,
  `katamaran-dashboard:dev`, `katamaran-mgr:dev`) built via buildx with
  `BUILDPLATFORM` / `TARGETOS` / `TARGETARCH`.
- DaemonSet (`deploy/daemonset.yaml`) installs the katamaran binary
  onto kata-runtime-labelled nodes and loads the required kernel
  modules (`sch_plug`, `ipip`, `ip6_tunnel`, `ip_gre`, `ip6_gre`).
- `katamaran-orchestrator` CLI: NDJSON-streaming wrapper that consumes
  a JSON-encoded `orchestrator.Request` from stdin and emits StatusUpdate
  events to stdout. Useful for bash / CI pipelines that want a
  structured runner.
- E2E harness `scripts/e2e.sh` supports `--method=job` (legacy shell
  path) and `--method=crd` (CRD + controller path), both running on
  minikube and kind, with calico, cilium, flannel, OVN-Kubernetes, and
  kindnet CNIs, and `--storage=none|local|nfs`.

### Removed

- `internal/orchestrator/script.go`: the old `migrate.sh` wrapper. The
  shell script remains in `deploy/` for ad-hoc manual testing only.
- `deploy/job-source.yaml` / `deploy/job-dest.yaml`: same bytes as the
  embedded templates in `internal/orchestrator/templates/`. `migrate.sh`
  now reads the canonical files via a relative path.

### Security

- Source pod log URL paths are URL-encoded so a namespace or pod name
  containing `/` cannot accidentally address `/pods/<name>/exec` or
  `/log` instead of the pod itself.
- Dashboard handlers reject requests whose `Sec-Fetch-Site` is anything
  other than `same-origin` or `none`. SSRF blocklist covers loopback,
  link-local, multicast, and known cloud metadata IPs (AWS IMDS v4 + v6).
- `go.mod` go directive bumped to 1.26.2 to pull in the patched
  `crypto/tls` and `crypto/x509` (GO-2026-4870 / GO-2026-4946 /
  GO-2026-4947).

[Unreleased]: https://github.com/maci0/katamaran/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/maci0/katamaran/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/maci0/katamaran/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/maci0/katamaran/compare/v0.3.0...v0.4.2
[0.3.0]: https://github.com/maci0/katamaran/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/maci0/katamaran/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/maci0/katamaran/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/maci0/katamaran/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/maci0/katamaran/releases/tag/v0.1.0
