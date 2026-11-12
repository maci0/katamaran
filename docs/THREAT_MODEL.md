# Threat model

Last reviewed: 2026-11-12, against the code on that date. Owner and
review cadence: not recorded; both are an organizational decision and this
file does not invent either.

Scope: the katamaran binaries, the dashboard, the manager, the factory, the
orchestrator, the adoption shim, and the manifests under `deploy/` and
`config/`. The Kata runtime, containerd, and the Kubernetes apiserver are
trusted components this model does not review. An attacker who already
controls the apiserver, or who already has root on a node, is outside scope:
every control below assumes those hold.

## Risk-ranked summary

| ID | Threat | Boundary | Severity | Reachability |
|---|---|---|---|---|
| T1 | The dashboard HTTP API has no authentication. Any caller that reaches it can start a `privileged`, `hostPID` migration Job that runs cluster-wide, and can delete or orphan any pod in the cluster. The default install publishes that API as the cluster-wide ClusterIP `katamaran-dashboard-metrics` | B1, B4 | Critical | Trivial from any pod in the cluster |
| T2 | A `Migration` CR in any namespace drives the same `privileged`, `hostPID` Job into `kube-system`. This path never touches the dashboard | B9, B4 | Critical | Needs `create` on `katamaran.io/migrations` in one namespace |
| T3 | Load-generator targets are screened only against loopback, link-local, multicast, and three pinned metadata IPs, so cluster-internal and RFC1918 addresses stay reachable (SSRF) | B1 | High | Trivial |
| T4 | The migration data plane is cleartext: guest RAM on TCP 4444, the NBD disk mirror on TCP 10809, and the IPIP or GRE redirect tunnel all cross the node network unencrypted | B7 | High | Needs a network position between or on the nodes |
| T5 | One unauthenticated HTTP request becomes a `privileged`, `hostNetwork`, `hostPID` Job running the cluster-scoped `katamaran-source` ServiceAccount | B1, B4 | High | Follows directly from T1 |
| T6 | The admission webhook fails open: `failurePolicy: Ignore`, and its self-signed certificate is regenerated on every leader start | B3 | Medium | Any webhook fault, leader churn, or a port-forwarded spoof |
| T7 | Destination QEMU arguments are scraped from a Kubernetes pod log and replayed. The binary itself is pinned, but the argument vector, the nvdimm path, and the guest memory contents are not | B6 | Medium | Needs pod-log write, or a spoofed source pod |
| T8 | `/metrics`, `/debug/vars`, and `/debug/pprof/` under `--enable-debug` are unauthenticated and served over cleartext HTTP, on the dashboard and on the controller | B1, B8 | Medium | Needs in-cluster reach |
| T9 | The factory gRPC socket on the node has no peer authentication. `GetBaseVM` hands over a migrated VM and `Quit` stops the daemon, for any local caller | B5 | Medium | Needs code execution on the node |
| T10 | `KATAMARAN_MIGRATION_IMAGE` is the only gate on which image becomes a privileged Job, and it is deployment configuration, not a policy | B4 | Medium | Needs write access to the deployment or its config |
| T11 | The node DaemonSet's privileged init container copies three binaries onto the host, rewrites the host's containerd and Kata configuration, and restarts containerd. A compromised DaemonSet image is host root | B11 | Medium | Needs control of the image the DaemonSet pulls |
| T12 | Denial of service: the load generator spawns unbounded per-target work from unauthenticated callers, and factory `Quit` is callable by any socket peer | B1, B5 | Low | Trivial, contained to the affected process |

## 1. Attack surface inventory

Network listeners, all cleartext except the webhook:

- Dashboard HTTP on `--addr` (default `:8080`), `ReadHeaderTimeout` 10s and
  `MaxHeaderBytes` 64 KiB
  (`internal/dashboard/server.go:157`, `internal/dashboard/server.go:243`).
  `deploy/dashboard.yaml` ships no Service of its own, but the default
  install applies `deploy/metrics-services.yaml`, which creates the
  cluster-wide ClusterIP `katamaran-dashboard-metrics` on 8080
  (`deploy/metrics-services.yaml:36`). That port serves the whole UI and
  API, not only `/metrics`: the dashboard Deployment's own comment names
  `kubectl port-forward` of this Service as the admin path
  (`deploy/dashboard.yaml:191`). `deploy/monitoring.yaml` holds only the
  ServiceMonitors, and those are the objects that need a Prometheus
  Operator; the Service itself applies on any cluster.
  Routes (`internal/dashboard/server.go:290`, `internal/dashboard/server.go:333`):
  `GET /`, `GET /assets/`, `GET /healthz`, `GET /readyz`, `GET /metrics`,
  `GET /api/pods`, `GET /api/nodes`, `GET /api/history`, `GET /api/status`,
  `POST /api/migrate`, `POST /api/migrate/stop`, `POST /api/ping`,
  `POST /api/ping/stop`, `GET /api/ping/results`, `POST /api/httpgen`,
  `POST /api/httpgen/stop`, `GET /api/httpgen/results`. `/debug/vars` and
  `/debug/pprof/` are added only with `--enable-debug`
  (`internal/dashboard/server.go:349`).
- Controller debug HTTP on `--addr` (default `:8081`): `/healthz`, `/readyz`,
  `/metrics`, `/debug/vars` (`cmd/katamaran-mgr/debug.go:60`,
  `cmd/katamaran-mgr/debug.go:91`). The route table is `debugMux`;
  `serveDebug` only binds it (`cmd/katamaran-mgr/debug.go:30`).
- Admission webhook HTTPS on `--webhook-addr` (default `:9443`), self-signed,
  `failurePolicy: Ignore` (`cmd/katamaran-mgr/webhook.go:172`,
  `deploy/manager.yaml:270`).
- Factory gRPC over a Unix socket, default `/var/run/katamaran/factory.sock`,
  mode `0600` (`cmd/katamaran-factory/main.go:187`,
  `cmd/katamaran-factory/main.go:196`). RPCs: `Config`,
  `GetBaseVM`, `Status`, `Quit` (`internal/factory/server.go:100`,
  `internal/factory/server.go:147`, `internal/factory/server.go:192`,
  `internal/factory/server.go:212`). The mode limits peers to the owning
  UID; it does not authenticate a peer.
- Migration data plane, opened on the destination node: QEMU incoming
  migration on TCP 4444 and the NBD server on TCP 10809
  (`internal/migration/config.go:22`, `internal/migration/config.go:25`).
- QMP client dials a Unix socket whose path comes from the request or from pod
  discovery (`internal/qmp/client.go:173`). The CLI default is
  `/run/vc/vm/extra-monitor.sock` (`internal/katamaran/cli.go:183`,
  `internal/migration/config.go:31`).
- Adoption shim ttrpc listener, inherited from containerd over an FD named by
  the environment (`cmd/containerd-shim-katamaran-adopted-v2/main.go:268`).
  Experimental, not a user CLI.
- Directory watcher over `/run/vc/vm/` reading `migration-meta.json`
  (`cmd/katamaran-factory/main.go:142`, `internal/factory/watcher.go:99`).
  The file contents become the VM config the factory serves, so anything
  that can write that directory on the node feeds the factory.

Files and objects the processes read or write:

- `migration-meta.json` plus `VMConfig` payloads, polled from `--watch-dir`
  (default `/run/vc/vm/`) by the factory watcher
  (`cmd/katamaran-factory/main.go:214`).
- The source pod log, scraped for the `KATAMARAN_CMDLINE_B64` marker and
  replayed as QEMU arguments (`internal/migration/cmdlinefetch.go:156`).
- The `Migration` CRD, `migrations.katamaran.io`, namespaced
  (`config/crd/migration.yaml:13`).
- Host paths mounted by the migration Jobs: `/run/vc`, `/sys`,
  `/lib/modules`, `/tmp/katamaran-cmdlines`, and on the destination also
  `/dev/shm`, `/run/kata-containers/shared/sandboxes`, and `/opt/kata`
  (`internal/orchestrator/templates/job-source.yaml:99`,
  `internal/orchestrator/templates/job-dest.yaml:102`).
- Host paths the DaemonSet writes: `/usr/local/bin` for three binaries,
  the Kata `configuration-qemu.toml`, and containerd's `config.toml` plus
  `config.d` (`deploy/daemonset.yaml:74`, `deploy/daemonset.yaml:152`).

Configuration and environment:

- Dashboard: `--kubeconfig`, `--addr`, `--enable-debug`, `--log-format`;
  `KATAMARAN_MIGRATION_IMAGE` (required), `KATAMARAN_ALLOWED_NAMESPACES`
  (`internal/dashboard/server.go:191`, `internal/dashboard/server.go:202`).
- Controller: `--kubeconfig`, `--addr`, `--webhook-addr`,
  `--leader-namespace`, `--leader-name`, `--disable-leader-election`,
  `--pod-wait-timeout`; `KATAMARAN_MIGRATION_IMAGE` (required),
  `KATAMARAN_POD_WAIT_TIMEOUT` (`cmd/katamaran-mgr/main.go:189`,
  `cmd/katamaran-mgr/main.go:355`).
- `katamaran` CLI: `--mode`, `--dest-ip`, `--vm-ip`, `--qmp`,
  `--replay-cmdline-from-pod`, `--tunnel-mode`, and the rest of the migration
  flags (`internal/katamaran/cli.go`). `KATAMARAN_MIGRATION_ID` overrides the
  generated ID (`internal/katamaran/cli.go:243`).
- Factory: `--listen`, `--watch-dir` (`cmd/katamaran-factory/main.go:141`).
- In-cluster: `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT` select
  the apiserver (`internal/migration/podresolve.go:447`).
- Shim: `KATAMARAN_SHIM_LISTENER_FD`, `KATAMARAN_SHIM_SOCKET_PATH`
  (`cmd/containerd-shim-katamaran-adopted-v2/main.go:268`).

Untrusted input:

- HTTP request bodies, headers (`X-Request-Id`, `Origin`, `Referer`,
  `Sec-Fetch-Site`) and form fields on the dashboard
  (`internal/dashboard/middleware.go:104`,
  `internal/dashboard/middleware.go:169`,
  `internal/dashboard/migrate.go:86`). Bodies are capped at 1 MiB
  (`internal/dashboard/response.go:52`).
- `AdmissionReview` JSON from the apiserver, capped at 3 MiB
  (`cmd/katamaran-mgr/webhook.go:174`).
- QEMU JSON responses over the QMP socket (`internal/qmp/client.go:226`).
- Source pod logs and `/proc/<pid>/cmdline` captured from another pod
  (`internal/migration/cmdlinefetch.go:156`,
  `internal/migration/destspawn.go:427`).
- `katamaran-orchestrator` reads one JSON `orchestrator.Request` from stdin
  and submits it (`cmd/katamaran-orchestrator/main.go:163`). It checks
  `orchestrator.Validate` and nothing else: no image allowlist, no
  namespace pin. It is a third route to the privileged Job, alongside the
  dashboard and the CRD.
- `deploy/migrate.sh` takes `--image`, node names, pod names and QMP paths
  as arguments and renders the same Job templates
  (`deploy/migrate.sh:176`). It checks shell-safe characters
  (`deploy/migrate.sh:286`) but applies no image allowlist, so the image
  the caller names is the image that runs privileged. It is the fourth
  route.
- Kubernetes objects: the namespaced `Migration` CRD, which is itself an entry
  point for anyone holding `create` on it (`config/crd/migration.yaml:13`),
  plus the node and pod listings the dashboard and manager read.

## 2. Trust boundaries and data flow

```
caller --HTTP--> dashboard --apiserver--> Jobs in kube-system
tenant --CRD--> controller --apiserver--> the same Jobs
operator --stdin--> katamaran-orchestrator --apiserver--> the same Jobs
operator --shell--> deploy/migrate.sh --kubectl--> the same Jobs (B10)
DaemonSet init --host filesystem--> containerd, Kata, /usr/local/bin (B11)
Job --pod log--> QEMU argv on the destination node
source node --TCP 4444 / 10809 / IPIP|GRE--> destination node
node process --unix socket--> factory and QMP
apiserver --HTTPS--> admission webhook
```

**B1, caller to dashboard.** The dashboard authenticates nobody. Trust is
entirely a property of who can open the port. The shipped default publishes
that port as a cluster-wide ClusterIP Service
(`deploy/metrics-services.yaml:36`), so every pod in the cluster can open it.
An operator who deletes the Service and reaches the dashboard only through
`kubectl port-forward` narrows the caller set to whoever holds
`pods/portforward` in `kube-system`, but that is an operator choice the
default install does not make. `cmd/katamaran-dashboard/README.md` still says
no Service fronts the UI; the manifest contradicts it, and the manifest is
what an install applies.

**B2, dashboard to apiserver.** The dashboard's ServiceAccount holds a
cluster-scoped Role: `get`, `list`, `patch`, and `delete` on every pod, and
`get`, `list` on nodes (`deploy/dashboard.yaml:63`). A compromise of the
dashboard process is a compromise of those verbs, not just of the HTTP API.
The ClusterRole grants `patch` and `delete` on every pod in the cluster, not
only reads, so a compromise of the dashboard pod can alter or remove any pod,
not only read its log. The reconciliation of that asymmetry belongs to the
authorization review.

**B3, apiserver to webhook.** The webhook is the only TLS listener, but it
fails open (`deploy/manager.yaml:270`) and its certificate is generated in
process and replaced on every leader election
(`cmd/katamaran-mgr/webhook.go:46`). A caller who can port-forward the webhook
port presents an `AdmissionReview` the handler accepts, because the handler
checks the body shape, not the peer
(`cmd/katamaran-mgr/webhook.go:172`).

**B4, orchestrator to Job.** Elevation is the boundary's purpose: the Job
template hardcodes `privileged`, `hostPID`, and `hostNetwork`, and the image
comes from the request subject to the single allowlist entry (T10). Tampering
of the rendered manifest is constrained by `Validate`
(`internal/orchestrator/validation.go:28`) and by the template being embedded
rather than fetched. Repudiation: Job creation is attributable only through
apiserver audit logs; the dashboard attributes requests by `remote_addr`
only, with no user identity (`internal/dashboard/middleware.go:136`).

**B9, Kubernetes user to the Migration CRD.** The CRD is namespaced
(`config/crd/migration.yaml:13`), so RBAC can grant `create` on it to a
tenant in a single namespace, but the resulting action is cluster-wide.
Elevation: the author does not choose a pod spec, only spec fields, but the
controller renders the same `privileged: true`, `hostNetwork: true`,
`hostPID: true` Job into `kube-system`
(`internal/orchestrator/orchestrator.go:100`,
`internal/orchestrator/templates/job-source.yaml:60`) using its own
cluster-wide ServiceAccount, and the Job mounts hostPaths `/run/vc`, `/sys`
and `/lib/modules` (`internal/orchestrator/templates/job-source.yaml:106`,
`internal/orchestrator/templates/job-source.yaml:110`,
`internal/orchestrator/templates/job-source.yaml:114`).
Reaching a privileged, hostPID, host-network pod that mounts `/sys` and
`/lib/modules` is equivalent to node compromise, so a CR author in namespace
`team-a` gets node-root through a namespace-scoped grant. The repo narrows the
blast radius in two places: pod references are pinned to the CR's own namespace
(`internal/controller/reconciler.go:997`,
`internal/controller/reconciler.go:1003`) so the author cannot name a
pod in someone else's namespace, and `spec.image` must equal the operator's
configured image (`internal/controller/reconciler.go:972`). Both leave the
privileged Job itself in reach.

Tampering within that grant: `spec.destNode` and `spec.destNodeSelector` place
the privileged Job on a node the author picks, so an author can steer a
hostPID pod onto a chosen node. `spec.tunnelMode` selects the encapsulation
from a closed set (`internal/orchestrator/validation.go:85`) and
`spec.sourceCleanup` selects `none`, `delete` or `orphan`
(`internal/orchestrator/validation.go:131`); `delete` removes the source pod
through the controller's cluster-wide `pods: delete`
(`internal/controller/reconciler.go:627`), which is a workload-disruption
primitive for anyone holding only the CR grant. Denial of service: nothing
limits how many Migrations a namespace may hold, so a CR author can queue
privileged Jobs until the nodes are saturated.

**B10, operator workstation.** Elevation relative to the other two paths:
`katamaran-orchestrator` and `deploy/migrate.sh` reach the same privileged Job
with none of the image or namespace checks that B1 and B9 apply. The caller
already holds credentials that can create Jobs in `kube-system`, so this adds
no privilege the caller did not have; what it removes is the last check in
front of it. A scripted caller, or a credential shared more widely than the
operator intended, can run an image of its choosing as `privileged` with
`hostPID` and `hostNetwork`.

**B11, DaemonSet to host.** The init container rewrites
`configuration-qemu.toml`, writes a containerd drop-in, appends to
`config.toml`'s `imports`, and runs `systemctl restart containerd`
(`deploy/daemonset.yaml:74`, `deploy/daemonset.yaml:183`). A tampered image
gets host root without exploiting anything else (T11). The containerd drop-in
is not removed on DaemonSet deletion (`deploy/daemonset.yaml:163`), so a
removed install leaves containerd loading a shim that no longer exists.

**B5, node to factory and QMP.** No peer authentication, so spoofing is free
for any local process that can open the socket. `Quit` is the
denial-of-service path (T12) and `GetBaseVM` is the disclosure and tampering
path (T9). The same holds for the QMP socket: any process on the node that
can open it controls the guest.

**B6, pod log to QEMU.** Tampering: a writer of the source pod log controls
the argument vector the destination execs. The binary path is pinned to the
configured or bundled QEMU (`internal/migration/destspawn.go:607`) and the
nvdimm copy refuses symlinks and non-regular files
(`internal/migration/destspawn.go:651`), which closes the "exec an arbitrary
binary" and "read an arbitrary host file through a symlink" variants. The
argument vector itself is still attacker-controlled (T7). The read is bounded
by `maxPodLogLineSize`, `maxPodLogScanBytes` and `cmdlineStreamTTL`
(`internal/migration/cmdlinefetch.go:20`,
`internal/migration/cmdlinefetch.go:26`,
`internal/migration/cmdlinefetch.go:41`), and the destination execs without a
shell, but a workload that can emit a crafted cmdline marker still controls
the destination process arguments.

**B7, node to node.** Disclosure and tampering: anyone on the node network
path can read or modify guest RAM and disk state in flight (T4). There is no
authentication of the migration peer, so a host that can answer on 4444 or
10809 can impersonate the destination. The redirect tunnel adds a host route
that any process able to create tunnels on the source node can interfere
with. This is tracked as a known gap in `docs/ROADMAP.md:43`.

**B8, operator to observability.** expvar exposes runtime internals and
`--enable-debug` exposes pprof, which allows CPU profiling and forced dumps
(T8). Without the flag the manager serves `/metrics` only.

Recurring classes: the repository's own history shows repeated hardening at
the HTTP input layer (CSRF, security headers, request-ID character filtering,
SSRF screening), so regressions in `internal/dashboard` validation are the
class most likely to recur. The privileged-job and pod-log boundaries have no
equivalent validation depth.

## 3. Assets and impact

- **Guest memory and disk contents.** The whole point of the data plane. A
  reader on the node network path gets the guest's RAM and the blocks being
  mirrored (T4).
- **Node root.** The migration Job is `privileged` with `hostPID` and
  `hostNetwork` (`internal/orchestrator/templates/job-source.yaml:46`),
  and the DaemonSet init container is `privileged` with the host root mounted
  (`deploy/daemonset.yaml:37`, `deploy/daemonset.yaml:296`). Either one is
  host compromise.
- **Cluster credentials.** The controller's ClusterRole can create and delete
  Jobs and pods cluster-wide (`deploy/manager.yaml:38`). The dashboard's
  ClusterRole can patch and delete any pod (`deploy/dashboard.yaml:63`).
  The Job's own ServiceAccount can read any pod and any pod log
  (`deploy/job-rbac.yaml:37`, `deploy/job-rbac.yaml:43`).
- **Pod logs.** The destination Job reads the source pod's logs to recover
  the QEMU command line (`internal/migration/cmdlinefetch.go:156`). Pod
  logs commonly contain credentials, tokens and startup arguments, and the
  dest Job reads them.
- **Workload placement.** `POST /api/migrate` and `spec.destNode` schedule
  privileged work on a caller-chosen node.
- **Workload disruption.** `spec.sourceCleanup: delete` lets a caller tear
  down a running pod as the tail of a migration the caller started
  (`internal/controller/reconciler.go:627`), and the dashboard's stop
  endpoints cancel the migration itself.
- **VM adoption.** `GetBaseVM` hands a caller the identity of a live migrated
  VM (`internal/factory/server.go:147`).
- **Availability.** Migration aborts, daemon `Quit`, and load-generator
  processes all degrade the cluster's migration capability. A namespace holding
  only the CR grant can also queue privileged Jobs until nodes are saturated.

Impact is not a generic "data breach": the concrete outcome is guest memory
exfiltration or tampering in flight, node compromise through a privileged Job,
or cluster-wide secret disclosure through pod logs.

## 4. Mitigations mapping

| Control | Covers | Where |
|---|---|---|
| `KATAMARAN_ALLOWED_NAMESPACES` allowlist, defaulting to all namespaces | Narrows T1's read and migrate scope when set | `internal/dashboard/authorization.go:34` |
| Exact-match image allowlist from `KATAMARAN_MIGRATION_IMAGE` | T10, partially: one image, operator-chosen | `internal/dashboard/migrate.go:104`, `cmd/katamaran-mgr/main.go:189` |
| Form field allowlist, boolean literal check, shell-metacharacter rejection, 1 MiB body cap | Stops injection into the rendered Job command | `internal/dashboard/migrate.go:86`, `internal/orchestrator/validation.go:200` |
| Target screening: loopback, unspecified, link-local, multicast, and three pinned metadata addresses, re-checked in the dialer, redirects refused | The screened part of T3 only | `internal/dashboard/validation.go:57`, `internal/dashboard/loadgen.go:36`, `internal/dashboard/loadgen.go:303` |
| CSRF check on `Origin`, `Referer`, and `Sec-Fetch-Site` | Browser cross-site requests; not non-browser callers | `internal/dashboard/middleware.go:169` |
| Security headers and a same-origin CSP, with `'unsafe-inline'` because the page uses inline scripts | Browser confinement of the dashboard UI | `internal/dashboard/middleware.go:222` |
| Cross-namespace pod reference rejection | Keeps B9 inside the CR's namespace | `internal/controller/reconciler.go:997` |
| One-migration-per-source-pod guard, fail-closed when the Job list fails | Stops a second destructive copy of a live VM | `internal/orchestrator/inflight.go:57`, `internal/orchestrator/inflight.go:115` |
| QEMU binary pinned; nvdimm copy uses `O_NOFOLLOW` and requires a regular file | The arbitrary-exec and symlink-read variants of T7 | `internal/migration/destspawn.go:607`, `internal/migration/destspawn.go:651` |
| Pod-log scan caps: 8 MiB lines, 16 MiB total, 60 s stream | Bounds B6's read | `internal/migration/cmdlinefetch.go:20`, `internal/migration/cmdlinefetch.go:26`, `internal/migration/cmdlinefetch.go:41` |
| Webhook body capped at 3 MiB | Bounds B3's read | `cmd/katamaran-mgr/webhook.go:174` |
| Factory socket mode `0600` | Local-user separation for B5, nothing more | `cmd/katamaran-factory/main.go:196` |
| Leader election, 15 s lease | One reconciler, one webhook certificate publisher | `cmd/katamaran-mgr/main.go:299` |
| Non-root, read-only root filesystem, dropped capabilities and `RuntimeDefault` seccomp on the manager and dashboard workloads | Containment of the control-plane processes, not of the Jobs | `deploy/manager.yaml:212`, `deploy/dashboard.yaml:142` |
| Panic recovery and per-request logging with a validated request ID | Bounds a handler crash to one request; attributes by address only | `internal/dashboard/middleware.go:147`, `internal/dashboard/middleware.go:83` |
| Fuzz targets on QMP parsing, cmdline and pod-ref parsing, orchestrator argument validation, dashboard form and target validation, the admission webhook handler, and the adoption shim's sandbox-id check | Regression net for the parsers at these boundaries | `internal/qmp/fuzz_test.go:17`, `internal/migration/parsers_fuzz_test.go:13`, `internal/orchestrator/fuzz_test.go:13`, `internal/dashboard/validation_fuzz_test.go:15`, `cmd/katamaran-mgr/webhook_test.go:129`, `cmd/containerd-shim-katamaran-adopted-v2/main_test.go:49` |

The CSRF row is not authentication: requests with no Origin, Referer or
`Sec-Fetch-Site` pass (`internal/dashboard/middleware.go:176`), which is every
non-browser client. The namespace row is opt-in and empty by default
(`deploy/dashboard.yaml:129`), and it does nothing for the CRD path (T2).
The cross-namespace row keeps a CR author inside their own namespace; it does
not constrain which node the privileged Job lands on. The image row on the
CRD path is a second copy, `authorizeImage`
(`internal/controller/reconciler.go:972`), and the orchestrator and
`deploy/migrate.sh` routes consult neither copy. CR fields that pick node
placement, encapsulation and source teardown are closed enums
(`internal/orchestrator/validation.go:85`,
`internal/orchestrator/validation.go:131`).

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
- **T8.** No authentication on the metrics and debug listeners.
- **T9.** No peer authentication on the factory socket.
- **T11.** No signing or digest pin on the DaemonSet image, and no admission
  policy on the DaemonSet itself.
- **T12.** No rate limit on the load-generator endpoints.

Single point of failure: `KATAMARAN_MIGRATION_IMAGE` is the sole control
standing between an API caller and a privileged Job, and it is a plain
environment variable (`deploy/dashboard.yaml:124`, `deploy/manager.yaml:164`).

## 5. Abuse cases

- **A1, resource exhaustion from an authenticated-by-network caller.**
  `POST /api/ping` and `POST /api/httpgen` spawn a goroutine per
  target with no global cap (`internal/dashboard/loadgen.go:176`,
  `internal/dashboard/loadgen.go:280`). Each target is re-resolved and dialed
  from the dashboard pod, so one caller can aim the pod's bandwidth and file
  descriptors at arbitrary in-cluster addresses (T3 plus T12).
- **A2, destructive actions through the dashboard.** `POST /api/migrate` with
  `source_cleanup=delete` or `orphan` ends in the controller deleting the
  source pod (`internal/controller/reconciler.go:627`). Nothing in the
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

## 6. Residual risk

The controls in section 4 hold only as far as their references say, and each
one has a boundary it does not cross:

- The CSRF checks do not authenticate. A request carrying none of `Origin`,
  `Referer` or `Sec-Fetch-Site` is served (`internal/dashboard/middleware.go:176`),
  so every non-browser caller is trusted once it can reach the port.
- The namespace allowlist is empty unless an operator sets it
  (`deploy/dashboard.yaml:129`). Empty means every namespace
  (`internal/dashboard/authorization.go:23`).
- The image allowlist is an environment variable on two Deployments
  (`deploy/dashboard.yaml:124`, `deploy/manager.yaml:164`). It is not a
  signature check, and the two CLI routes do not consult it at all.
- Namespace containment on the CRD path is enforced by the controller
  (`internal/controller/reconciler.go:997`), not by RBAC. The repository
  ships no Role granting `create` on `migrations`, so who holds that grant
  in a real cluster is a cluster-admin decision this repository does not
  record.
- Node routing and pod selection are validated only for syntax and
  distinctness (`internal/orchestrator/validation.go:28`); whether the caller
  is entitled to move a given pod is not decided anywhere in this repository.
- The factory socket mode keeps other UIDs out
  (`cmd/katamaran-factory/main.go:196`) and says nothing about a process
  running as the factory's own UID.
- `failurePolicy: Ignore` (`deploy/manager.yaml:270`) means every
  admission-time control is best-effort.

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
