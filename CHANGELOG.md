# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.2] - 2026-04-30

### Added

- `Request.CNIConvergenceDelaySeconds` /
  `.spec.cniConvergenceDelaySeconds` (and source CLI flag
  `--cni-convergence-delay`) — per-migration override for how long
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

- `internal/orchestrator/script.go` — the old `migrate.sh` wrapper. The
  shell script remains in `deploy/` for ad-hoc manual testing only.
- `deploy/job-source.yaml` / `deploy/job-dest.yaml` — same bytes as the
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

[Unreleased]: https://github.com/maci0/katamaran/compare/v0.1.2...HEAD
[0.1.0]: https://github.com/maci0/katamaran/releases/tag/v0.1.0
