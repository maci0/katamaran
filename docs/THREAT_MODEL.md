# Threat model

Last reviewed: 2026-11-12. Owner and review cadence: not recorded.

Scope: the code in this repository and the manifests under `deploy/` and
`internal/orchestrator/templates/`. Dependency and base-image CVEs belong to
the dependency review, not here. Every claim carries a `path:line` reference
so a later pass can re-verify it.

The deployment posture these ratings assume is in `SECURITY.md`: the dashboard
reached by a cluster admin over `kubectl port-forward`, and
`KATAMARAN_MIGRATION_IMAGE` set to an image the operator trusts. Publishing the
dashboard to every pod in the cluster moves T1 and T5 from "needs a cluster
foothold" to "needs any pod", and the ratings below get worse.

## Risk-ranked summary

| # | Threat | Boundary | Severity | Exploitability |
|---|---|---|---|---|
| T1 | The dashboard HTTP API has no authentication. Any caller that reaches it can start a `privileged`, `hostPID` migration Job that runs cluster-wide, and can delete or orphan any pod in the cluster | B1, B4 | Critical | Trivial once the port is reachable |
| T2 | A `Migration` CR in any namespace drives the same `privileged`, `hostPID` Job into `kube-system`. This path never touches the dashboard | B9, B4 | Critical | Needs `create` on `katamaran.io/migrations` in one namespace |
| T3 | Load-generator targets are screened only against loopback, link-local, multicast, and three pinned metadata IPs, so cluster-internal and RFC1918 addresses stay reachable (SSRF) | B1 | High | Trivial |
| T4 | The migration data plane is cleartext: guest RAM on TCP 4444, the NBD disk mirror on TCP 10809, and the IPIP or GRE redirect tunnel all cross the node network unencrypted | B7 | High | Needs a network position between or on the nodes |
| T5 | One unauthenticated HTTP request becomes a `privileged`, `hostNetwork`, `hostPID` Job running the cluster-scoped `katamaran-source` ServiceAccount | B1, B4 | High | Follows directly from T1 |
| T6 | The admission webhook fails open: `failurePolicy: Ignore`, and its self-signed certificate is regenerated on every leader start | B3 | Medium | Any webhook fault, leader churn, or a port-forwarded spoof |
| T7 | Destination QEMU arguments are scraped from a Kubernetes pod log and replayed. The binary itself is pinned, but the argument vector, the nvdimm path, and the guest memory contents are not | B6 | Medium | Needs pod-log write, or a spoofed source pod |
| T8 | `/metrics`, `/debug/vars`, and `/debug/pprof/` under `--enable-debug` are unauthenticated and served over cleartext HTTP, on the dashboard and on the controller | B1, B8 | Medium | Needs in-cluster reach |
| T9 | The factory gRPC socket on the node has no peer authentication. `GetBaseVM` hands over a migrated VM and `Quit` stops the daemon, for any local caller | B5 | Medium | Needs code execution on the node |
| T10 | `KATAMARAN_MIGRATION_IMAGE` is the only gate on which image becomes a privileged Job, and it is deployment configuration, not a policy | B4 | Medium | Needs write access to the deployment or its config |
| T11 | The node DaemonSet's privileged init container copies three binaries onto the host, rewrites the host's containerd and Kata configuration, and restarts containerd. A compromised DaemonSet image is host root | B10 | Medium | Needs control of the image the DaemonSet pulls |
| T12 | Denial of service: the load generator spawns unbounded per-target work from unauthenticated callers, and factory `Quit` is callable by any socket peer | B1, B5 | Low | Trivial, contained to the affected process |

## 1. Attack surface inventory

Network listeners, all cleartext except the webhook:

- Dashboard HTTP on `--addr` (default `:8080`), `ReadHeaderTimeout` 10s and
  `MaxHeaderBytes` 64 KiB
  (`internal/dashboard/server.go:157`, `internal/dashboard/server.go:243`).
  Routes (`internal/dashboard/server.go:290`, `internal/dashboard/server.go:333`):
  `GET /`, `GET /assets/`, `GET /healthz`, `GET /readyz`, `GET /metrics`,
  `GET /api/pods`, `GET /api/nodes`, `GET /api/history`, `GET /api/status`,
  `POST /api/migrate`, `POST /api/migrate/stop`, `POST /api/ping/start`,
  `POST /api/ping/stop`, `GET /api/ping/results`, `POST /api/http/start`,
  `POST /api/http/stop`, `GET /api/http/results`. `/debug/vars` and
  `/debug/pprof/` are added only with `--enable-debug`
  (`internal/dashboard/server.go:349`).
- Controller debug HTTP on `--addr` (default `:8081`): `/healthz`, `/readyz`,
  `/metrics`, `/debug/vars` (`cmd/katamaran-mgr/debug.go:60`,
  `cmd/katamaran-mgr/debug.go:91`).
- Admission webhook HTTPS on `--webhook-addr` (default `:9443`), self-signed,
  `failurePolicy: Ignore` (`cmd/katamaran-mgr/webhook.go:172`,
  `deploy/manager.yaml:270`).
- Factory gRPC over a Unix socket, default `/var/run/katamaran/factory.sock`,
  mode `0600` (`cmd/katamaran-factory/main.go:187`). RPCs: `Config`,
  `GetBaseVM`, `Status`, `Quit` (`internal/factory/server.go:94`,
  `internal/factory/server.go:141`, `internal/factory/server.go:175`,
  `internal/factory/server.go:195`).
- Migration data plane, opened on the destination node: QEMU incoming
  migration on TCP 4444 and the NBD server on TCP 10809
  (`internal/migration/config.go:22`, `internal/migration/config.go:25`).
- QMP client dials a Unix socket whose path comes from the request or from pod
  discovery (`internal/qmp/client.go:173`).
- Adoption shim ttrpc listener, inherited from containerd over an FD named by
  the environment (`cmd/containerd-shim-katamaran-adopted-v2/main.go:268`).
  Experimental, not a user CLI.

Files and runtime config consumed as input:

- `migration-meta.json` plus `VMConfig` payloads, polled from `--watch-dir`
  (default `/run/vc/vm/`) by the factory watcher
  (`cmd/katamaran-factory/main.go:214`).
- The source pod log, scraped for the `KATAMARAN_CMDLINE_B64` marker and
  replayed as QEMU arguments (`internal/migration/cmdlinefetch.go:114`).
- The `Migration` CRD, `migrations.katamaran.io`, namespaced
  (`config/crd/migration.yaml:1`).

CLI flags and environment variables that change authority:

- Dashboard: `--kubeconfig`, `--addr`, `--enable-debug`, `--log-format`;
  `KATAMARAN_MIGRATION_IMAGE` (required), `KATAMARAN_ALLOWED_NAMESPACES`
  (`internal/dashboard/server.go:191`, `internal/dashboard/server.go:202`).
- Controller: `--kubeconfig`, `--addr`, `--webhook-addr`,
  `--leader-namespace`, `--leader-name`, `--disable-leader-election`,
  `--pod-wait-timeout`; `KATAMARAN_MIGRATION_IMAGE` (required),
  `KATAMARAN_POD_WAIT_TIMEOUT` (`cmd/katamaran-mgr/main.go:189`,
  `cmd/katamaran-mgr/main.go:355`).
- `katamaran` CLI: `--mode`, `--dest-ip`, `--vm-ip`, `--qmp-socket`,
  `--replay-cmdline-from-pod`, `--tunnel-mode`, and the rest of the migration
  flags (`internal/katamaran/cli.go`). `KATAMARAN_MIGRATION_ID` overrides the
  generated ID (`internal/katamaran/cli.go:243`).
- Factory: `--listen`, `--watch-dir` (`cmd/katamaran-factory/main.go:141`).
- In-cluster: `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT` select
  the apiserver (`internal/migration/podresolve.go:447`).

Scheduled and background work: the controller polls Migration CRs
(`internal/controller/reconciler.go`), the orchestrator tails source pod logs
for progress markers (`internal/orchestrator/progress.go:19`), and the factory
watcher polls its directory (`internal/factory/watcher.go:45`).

Nothing here listens on a message bus or accepts file uploads.

## 2. Trust boundaries and data flow

- **B1, caller to dashboard.** No authentication. The only authorization is
  `KATAMARAN_ALLOWED_NAMESPACES`, which defaults to every namespace
  (`internal/dashboard/authorization.go:50`). CSRF checks apply to
  browser-sent requests only; a request carrying no `Origin`, `Referer`, or
  `Sec-Fetch-Site` is accepted (`internal/dashboard/middleware.go:170`).
- **B2, dashboard and controller to the apiserver.** In-cluster config first,
  then `KUBECONFIG` (`internal/orchestrator/native.go:112`). The apiserver is
  trusted for everything it returns, including pod logs.
- **B3, apiserver to the admission webhook.** TLS with a self-signed
  certificate minted at startup (`cmd/katamaran-mgr/webhook.go:47`) and
  patched into the `ValidatingWebhookConfiguration` by the leader
  (`cmd/katamaran-mgr/main.go:317`). `failurePolicy: Ignore`
  (`deploy/manager.yaml:270`).
- **B4, orchestrator to the migration Job.** The privilege transition. A
  validated `orchestrator.Request` becomes two Jobs in `kube-system` running
  as `katamaran-source`
  (`internal/orchestrator/orchestrator.go:100`,
  `internal/orchestrator/templates/job-source.yaml:22`,
  `internal/orchestrator/templates/job-dest.yaml:22`).
- **B5, node-local caller to the factory socket.** Any process that can
  connect to the socket is a full peer; the socket mode is the only control
  (`cmd/katamaran-factory/main.go:196`).
- **B6, source pod log to the destination QEMU.** The dest job reads the
  source pod's log through the apiserver and treats the decoded marker as
  QEMU arguments (`internal/migration/cmdlinefetch.go:339`).
- **B7, node to node.** Guest RAM, the NBD mirror, and the redirect tunnel
  cross the network with no encryption and no peer authentication
  (`internal/migration/config.go:21`, `internal/migration/tunnel.go:89`).
- **B8, scraper to the controller debug port.** Same posture as the
  dashboard's observability routes: no authentication
  (`cmd/katamaran-mgr/debug.go:28`).
- **B9, tenant to the `Migration` CRD.** Any subject that can `create` a
  Migration in one namespace can name a pod in that namespace and have the
  controller act on it cluster-wide. Cross-namespace pod references are
  rejected (`internal/controller/reconciler.go:998`).
- **B10, DaemonSet image to the node host.** The init container is
  `privileged`, mounts the host root, and writes host configuration
  (`deploy/daemonset.yaml:36`, `deploy/daemonset.yaml:295`).

Secrets: the only credentials are the Kubernetes service-account tokens. The
dashboard and controller mount one implicitly; the migration Jobs mount
`katamaran-source`'s (`internal/orchestrator/templates/job-source.yaml:22`);
the DaemonSet sets `automountServiceAccountToken: false`
(`deploy/daemonset.yaml:30`). No credential is written to a log or a CR
status by this code. Rotation is Kubernetes' own token rotation; this
repository adds none. `KATAMARAN_MIGRATION_IMAGE` is configuration, not a
secret, but it is the image-trust anchor (T10).

## 3. Assets and impact

- **Guest memory and disk contents.** Streamed in clear between nodes (B7)
  and copied into a writable nvdimm image on the destination
  (`internal/migration/destspawn.go:650`). Disclosure or tampering of a
  tenant's live workload.
- **Node root.** The migration Jobs are `privileged` with `hostPID`,
  `hostNetwork`, and host mounts of `/run/vc` and the containerd socket
  (`internal/orchestrator/templates/job-source.yaml:30`). Compromise of the
  Job image is compromise of every node a migration runs on.
- **Cluster control plane actions.** `katamaran-mgr` holds cluster-wide
  `create`, `patch`, and `delete` on pods and `create` and `delete` on Jobs
  (`deploy/manager.yaml:36`). The dashboard holds cluster-wide `patch` and
  `delete` on pods (`deploy/dashboard.yaml:63`).
- **Source pod lifecycle.** `spec.sourceCleanup` of `delete` or `orphan`
  removes the source pod after a successful migration
  (`internal/controller/reconciler.go:626`).
- **Migrated VM custody.** `GetBaseVM` returns a VM the factory is holding
  for adoption (`internal/factory/server.go:141`).
- **Availability of the migration service.** One controller leader
  (`cmd/katamaran-mgr/main.go:297`); webhook downtime fails open (T6).

## 4. Threats per boundary

**B1, caller to dashboard.** Spoofing: there is no identity to spoof, because
there is no authentication (T1). Tampering: form fields are checked against an
allowlist and for shell metacharacters
(`internal/dashboard/migrate.go:82`, `internal/dashboard/validation.go:149`),
and the body is capped at 1 MiB (`internal/dashboard/server.go:61`); the gap
is what a *valid* request is allowed to do. Information disclosure: `/api/pods`
and `/api/nodes` return cluster-wide pod and node data
(`internal/dashboard/server.go:470`, `internal/dashboard/server.go:484`), and
`/metrics` exports the orchestrator's counters. Denial of service: see A1.
Elevation: T5.

**B3, apiserver to webhook.** Spoofing: the certificate is regenerated per
leader with no stable identity, so a client that dials the webhook port
directly presents nothing the server checks. Tampering and disclosure: the
body is capped at 3 MiB (`cmd/katamaran-mgr/webhook.go:174`). Denial of
service and the elevation that follows from it: `failurePolicy: Ignore` means
a wedged, restarting, or unreachable webhook admits everything (T6).

**B4, orchestrator to Job.** Elevation is the boundary's purpose: the Job
template hardcodes `privileged`, `hostPID`, and `hostNetwork`, and the image
comes from the request subject to the single allowlist entry (T10). Tampering
of the rendered manifest is constrained by `Validate`
(`internal/orchestrator/validation.go:28`) and by the template being embedded
rather than fetched.

**B5, node to factory.** No peer authentication, so spoofing is free for any
local process. `Quit` is the denial-of-service path (T12) and `GetBaseVM` is
the disclosure and tampering path (T9).

**B6, pod log to QEMU.** Tampering: a writer of the source pod log controls
the argument vector the destination execs. The binary path is pinned to the
configured or bundled QEMU (`internal/migration/destspawn.go:607`) and the
nvdimm copy refuses symlinks and non-regular files
(`internal/migration/destspawn.go:654`), which closes the "exec an arbitrary
binary" and "read an arbitrary host file through a symlink" variants. The
argument vector itself is still attacker-controlled (T7).

**B7, node to node.** Disclosure and tampering: anyone on the node network
path can read or modify guest RAM and disk state in flight (T4). There is no
authentication of the migration peer, so a host that can answer on 4444 or
10809 can impersonate the destination.

**B9, tenant to CRD.** The namespace boundary is enforced for pod references
(`internal/controller/reconciler.go:998`), but the effect is not
namespace-scoped: the resulting Job runs in `kube-system` with host
privileges (T2). A tenant who can create a Migration can therefore consume
host-level resources on any node and, through `sourceCleanup`, delete their
own pods via the controller's authority.

**B10, DaemonSet to host.** The init container rewrites
`configuration-qemu.toml`, writes a containerd drop-in, appends to
`config.toml`'s `imports`, and runs `systemctl restart containerd`
(`deploy/daemonset.yaml:160`, `deploy/daemonset.yaml:178`). A tampered image
gets host root without exploiting anything else (T11). The containerd drop-in
is not removed on DaemonSet deletion (`deploy/daemonset.yaml:148`), so a
removed install leaves containerd loading a shim that no longer exists.

## 5. Mitigations mapping

| Control | Covers | Where |
|---|---|---|
| `KATAMARAN_ALLOWED_NAMESPACES` allowlist, defaulting to all namespaces | Narrows T1's read and migrate scope when set | `internal/dashboard/authorization.go:34` |
| Exact-match image allowlist from `KATAMARAN_MIGRATION_IMAGE` | T10, partially: one image, operator-chosen | `internal/dashboard/migrate.go:104`, `cmd/katamaran-mgr/main.go:194` |
| Form field allowlist, boolean literal check, shell-metacharacter rejection, 1 MiB body cap | Stops injection into the rendered Job command | `internal/dashboard/migrate.go:82`, `internal/orchestrator/validation.go:200` |
| Target screening: loopback, unspecified, link-local, multicast, and three pinned metadata addresses, re-checked in the dialer, redirects refused | The screened part of T3 only | `internal/dashboard/validation.go:56`, `internal/dashboard/loadgen.go:36`, `internal/dashboard/loadgen.go:295` |
| CSRF check on `Origin`, `Referer`, and `Sec-Fetch-Site` | Browser cross-site requests; not non-browser callers | `internal/dashboard/middleware.go:170` |
| Security headers and a same-origin CSP, with `'unsafe-inline'` because the page uses inline scripts | Browser confinement of the dashboard UI | `internal/dashboard/middleware.go:221` |
| Cross-namespace pod reference rejection | Keeps B9 inside the CR's namespace | `internal/controller/reconciler.go:998` |
| One-migration-per-source-pod guard, fail-closed when the Job list fails | Stops a second destructive copy of a live VM | `internal/orchestrator/inflight.go:57`, `internal/orchestrator/inflight.go:128` |
| QEMU binary pinned; nvdimm copy uses `O_NOFOLLOW` and requires a regular file | The arbitrary-exec and symlink-read variants of T7 | `internal/migration/destspawn.go:607`, `internal/migration/destspawn.go:654` |
| Pod-log scan caps: 8 MiB lines, 16 MiB total, 60 s stream | Bounds B6's read | `internal/migration/cmdlinefetch.go:20`, `internal/migration/cmdlinefetch.go:26`, `internal/migration/cmdlinefetch.go:41` |
| Webhook body capped at 3 MiB | Bounds B3's read | `cmd/katamaran-mgr/webhook.go:174` |
| Factory socket mode `0600` | Local-user separation for B5, nothing more | `cmd/katamaran-factory/main.go:196` |
| Leader election, 15 s lease | One reconciler, one webhook certificate publisher | `cmd/katamaran-mgr/main.go:297` |
| Fuzz targets on QMP parsing, cmdline and pod-ref parsing, orchestrator argument validation, dashboard form and target validation, the admission webhook handler, and the adoption shim's sandbox-id check | Regression net for the parsers at these boundaries | `internal/qmp/fuzz_test.go:17`, `internal/migration/parsers_fuzz_test.go:13`, `internal/orchestrator/fuzz_test.go:13`, `internal/dashboard/validation_fuzz_test.go:15`, `cmd/katamaran-mgr/webhook_test.go:129`, `cmd/containerd-shim-katamaran-adopted-v2/main_test.go:49` |

Not mitigated, in the order the summary ranks them:

- **T1, T5.** No authentication on the dashboard. The namespace allowlist is
  opt-in and the CSRF check does not apply to non-browser clients.
- **T2.** No admission policy restricts who may create a Migration, and the
  webhook does not review Migration CRs at all. RBAC is the only gate.
- **T3.** RFC1918, cluster service CIDRs, and pod CIDRs are all dialable.
- **T4.** No TLS and no peer authentication on 4444, 10809, or the tunnel.
  `README.md` and `docs/ROADMAP.md` both say so and point at WireGuard,
  IPsec, or QEMU `tls-creds-x509` as future work.
- **T6.** `failurePolicy: Ignore` is a deliberate availability choice, and it
  is load-bearing: every other admission-time control fails open with it.
- **T7.** Argument-vector validation beyond the binary pin and the nvdimm
  file-type checks.
- **T11.** No signing or digest pin on the DaemonSet image, and no admission
  policy on the DaemonSet itself.
- **T12.** No rate limit on the load-generator endpoints.

Single point of failure: `KATAMARAN_MIGRATION_IMAGE` is the sole control
standing between an API caller and a privileged Job, and it is a plain
environment variable (`deploy/dashboard.yaml:124`, `deploy/manager.yaml:164`).

## 6. Abuse cases

- **A1, resource exhaustion from an authenticated-by-network caller.**
  `POST /api/ping/start` and `POST /api/http/start` spawn a goroutine per
  target with no global cap (`internal/dashboard/loadgen.go:150`,
  `internal/dashboard/loadgen.go:256`). Each target is re-resolved and dialed
  from the dashboard pod, so one caller can aim the pod's bandwidth and file
  descriptors at arbitrary in-cluster addresses (T3 plus T12).
- **A2, destructive actions through the dashboard.** `POST /api/migrate` with
  `source_cleanup=delete` or `orphan` ends in the controller deleting the
  source pod (`internal/controller/reconciler.go:626`). Nothing in the
  dashboard confirms the pod identity beyond the namespace allowlist, so a
  caller who can reach the port can delete pods they could not otherwise
  touch.
- **A3, workflow gaming of the dedup guard.** The one-migration-per-pod guard
  keys on labels that are dropped when the pod name exceeds 63 characters
  (`internal/orchestrator/orchestrator.go:141`). A Migration whose source pod
  name is longer than that gets no duplicate protection, so two Migrations
  for it both run to completion against one live VM. The CRD schema allows
  pod names up to 253 characters (`config/crd/migration.yaml:44`).
- **A4, tenant quota bypass.** Migration Jobs land in `kube-system`
  regardless of the requesting namespace
  (`internal/orchestrator/orchestrator.go:100`), so a namespace's resource
  quota never sees them. The only bound is the controller's own admission,
  which fails open (T6).

## 7. Model maintenance

This document drifts. The previous revision cited mitigations and line
numbers the code has since outgrown, and it described the destination as
executing an attacker-chosen binary, which `internal/migration/destspawn.go`
no longer does. Re-verify the references rather than trusting them.

`SECURITY.md` records that no private disclosure channel is configured. The
supported-version table there is the only statement of what receives security
fixes. There is no documented path from a reported vulnerability to a shipped
fix beyond the release workflow in `.github/workflows/release.yml`.

Audit trail: the dashboard logs each request with method, path, status, and a
client-supplied `X-Request-Id` that is validated before use
(`internal/dashboard/middleware.go:104`, `internal/dashboard/middleware.go:83`).
The controller logs admission decisions and migration phase changes. Neither
is a tamper-evident audit log, and neither attributes a dashboard action to a
person, because the dashboard has no user identity.
