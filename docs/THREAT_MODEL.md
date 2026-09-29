# Threat model

Last reviewed: 2026-09-29. Owner and review cadence: not recorded.

Scope: the code in this repository and the manifests under `deploy/` and
`internal/orchestrator/templates/`. Dependencies and container base images are
covered by the dependency review, not here. Every claim below carries a
`path:line` reference so a later pass can re-verify or re-date it.

## Risk-ranked summary

| # | Threat | Boundary | Severity | Exploitability |
|---|---|---|---|---|
| T1 | Dashboard HTTP API has no authentication: any caller that reaches it can request a privileged, host-network migration Job cluster-wide | B1 | Critical | Trivial from any pod in the cluster |
| T2 | A `Migration` CR created in any namespace drives a `privileged`, `hostPID` Job into `kube-system` through a cluster-wide ServiceAccount. This path does not go through the dashboard | B9 | Critical | Needs `create` on `katamaran.io/migrations` in one namespace |
| T3 | Load-generator targets are screened against loopback, link-local, multicast and metadata IPs only, so cluster-internal and RFC1918 addresses stay reachable (SSRF) | B1 | High | Trivial |
| T4 | Migration data plane is unencrypted: guest RAM, NBD disk mirror and the IPIP/GRE redirect tunnel cross the node network in clear | B7 | High | Requires network position between or on the nodes |
| T5 | Privilege transition from an unauthenticated HTTP request to a `privileged`, `hostNetwork`, `hostPID` Job running the cluster-scoped `katamaran-source` ServiceAccount | B1, B4 | High | Follows directly from T1 |
| T6 | Admission webhook fails open: `failurePolicy: Ignore`, plus a self-signed certificate regenerated on every leader start | B3 | Medium | Triggered by any webhook fault, leader churn, or a port-forwarded spoof |
| T7 | Destination QEMU argv is taken from a Kubernetes pod log and executed; anyone who can write to that log controls the destination process | B6 | Medium | Needs pod-log write or a spoofed source pod |
| T8 | Observability endpoints (`/metrics`, `/debug/vars`, and `/debug/pprof/` under `--enable-debug`) are unauthenticated and served in clear HTTP | B1, B8 | Medium | Requires in-cluster reach |
| T9 | Factory gRPC socket on the node host has no peer authentication; `GetBaseVM` hands over a migrated VM and `Quit` stops the daemon to any local caller | B5 | Medium | Requires code execution on the node |
| T10 | `KATAMARAN_MIGRATION_IMAGE` is the only gate on which image becomes a privileged Job; it is deployment configuration, not a policy | B4 | Medium | Needs write access to the deployment or its config |
| T11 | Denial of service: load generator spawns unbounded per-target work from unauthenticated callers; factory `Quit` is callable by any socket peer | B1, B5 | Low | Trivial, contained to the affected process |

## 1. Attack surface inventory

Network listeners and endpoints:

- Dashboard HTTP server, default `:8080`
  (`internal/dashboard/server.go:128`, `internal/dashboard/server.go:225`).
  `deploy/dashboard.yaml` ships no Service, so the default install is reached
  by `kubectl port-forward` only; `deploy/monitoring.yaml` adds a ClusterIP
  `katamaran-dashboard-metrics` Service on 8080, applied only where a
  Prometheus Operator is installed, and that Service does reach the UI
  (`deploy/monitoring.yaml:30`).
  Routes: `GET /healthz`, `GET /readyz`,
  `GET /metrics`, `GET /{$}`, four `/assets/*` handlers, `GET /api/status`,
  `/api/pods`, `/api/nodes`, `/api/history`, and the state-changing
  `POST /api/migrate`, `/api/migrate/stop`, `/api/ping`, `/api/ping/stop`,
  `/api/httpgen`, `/api/httpgen/stop` (`internal/dashboard/server.go:273`).
- Manager observability server, plain HTTP: `GET /healthz`, `GET /readyz`,
  `GET /metrics`, `GET /debug/vars` (`cmd/katamaran-mgr/debug.go:44`).
- Admission webhook server, HTTPS with an in-memory self-signed cert:
  `POST /admit`, `GET /healthz` (`cmd/katamaran-mgr/webhook.go:138`,
  `cmd/katamaran-mgr/webhook.go:161`).
- Dashboard debug routes, behind `--enable-debug`: `/debug/pprof/` and
  `/debug/vars` (`internal/dashboard/server.go:336`).

IPC and host sockets:

- Factory gRPC server on a Unix socket, default
  `/var/run/katamaran/factory.sock` (`cmd/katamaran-factory/main.go:116`,
  `internal/factory/server.go`). RPCs: `Config`, `GetBaseVM`, `Status`,
  `Quit` (`internal/factory/server.go:94`, `:141`, `:175`, `:195`).
- QMP client against the VM's QEMU monitor socket
  (`internal/qmp/client.go:112`, default path set at
  `internal/katamaran/cli.go:134`).
- Directory watcher over `/run/vc/vm/` reading `migration-meta.json`
  (`cmd/katamaran-factory/main.go:117`, `internal/factory/watcher.go`).
- containerd shim adoption listener, path and inherited FD from the
  environment (`cmd/containerd-shim-katamaran-adopted-v2/main.go:264`,
  `:267`).

Inputs from outside the trust boundary:

- HTTP request bodies, headers (`X-Request-Id`, `Origin`, `Referer`,
  `Sec-Fetch-Site`) and form fields on the dashboard
  (`internal/dashboard/middleware.go:104`, `:169`,
  `internal/dashboard/migrate.go:86`).
- `AdmissionReview` JSON from the apiserver, capped at 3 MiB
  (`cmd/katamaran-mgr/webhook.go:176`).
- QEMU JSON responses over the QMP socket (`internal/qmp/client.go:219`).
- Source pod logs and `/proc/<pid>/cmdline` captured from another pod
  (`internal/migration/cmdlinefetch.go:114`,
  `internal/katamaran/cli.go:181`).
- Environment: `KATAMARAN_MIGRATION_IMAGE` (`internal/dashboard/server.go:174`,
  `cmd/katamaran-mgr/main.go:173`), `KATAMARAN_ALLOWED_NAMESPACES`
  (`internal/dashboard/server.go:185`),
  `KATAMARAN_POD_WAIT_TIMEOUT`
  (`cmd/katamaran-mgr/main.go:339`), `KATAMARAN_MIGRATION_ID`
  (`internal/katamaran/cli.go:213`), `KATAMARAN_SHIM_LISTENER_FD`,
  `KATAMARAN_SHIM_SOCKET_PATH`, `KUBECONFIG`, `KUBERNETES_SERVICE_HOST`,
  `KUBERNETES_SERVICE_PORT` (`internal/migration/podresolve.go:301`).
- CLI flags on all binaries, notably the migration agent's mode, QMP path,
  tunnel mode, drive list and cmdline replay paths
  (`internal/katamaran/cli.go:133`).
- Kubernetes objects: the namespaced `Migration` CRD, which is itself an entry
  point for anyone holding `create` on it (`config/crd/migration.yaml:13`),
  node and pod listings the dashboard and manager read.

Surface added by deployment:

- Node daemon runs `hostPID: true`, a privileged container, with hostPaths
  `/run/vc`, `/sys`, `/lib/modules` (`deploy/daemonset.yaml:20`, `:26`,
  `:259`).
- Migration Jobs run `privileged: true`, `hostNetwork: true`, with
  `automountServiceAccountToken: true` and hostPaths
  (`internal/orchestrator/templates/job-source.yaml:33`, `:41`,
  `internal/orchestrator/templates/job-dest.yaml:31`, `:39`).
- `deploy/metrics-services.yaml` adds two ClusterIP Services publishing the
  manager's `:8081` and the dashboard's `:8080` to the Prometheus
  ServiceMonitors (`deploy/metrics-services.yaml:14`, `:36`). Both serve
  unauthenticated `/metrics`, so this widens the reach of T8 beyond the pod
  itself.
- `config/crd/runtimeclass-adopted.yaml` registers the `katamaran-adopted`
  runtime that routes an adopted-VM pod to the experimental shim
  (`internal/controller/reconciler.go:1256`).

## 2. Trust boundaries and data flow

- **B1 User to dashboard.** Browser or script to the dashboard HTTP API.
  No identity is established; `csrfCheck` is the only browser-facing control
  and it inspects origin, not the caller (`internal/dashboard/middleware.go:169`).
- **B2 Dashboard to apiserver.** The dashboard holds a ServiceAccount bound to
  a namespaced Role for Jobs plus a ClusterRole for pods and nodes
  (`deploy/dashboard.yaml:12`, `:38`). It reads pod logs cluster-wide.
- **B3 Apiserver to admission webhook.** `POST /admit` over TLS with a CA
  bundle the manager patches into the ValidatingWebhookConfiguration itself
  (`cmd/katamaran-mgr/webhook.go:93`). TLS proves the endpoint holds the key
  generated at startup, nothing more; there is no client identity check.
- **B4 Control plane to node workloads.** A migration request becomes a
  privileged Job carrying the cluster-scoped `katamaran-source` ServiceAccount
  (`get` on pods, `pods/log`, `nodes`; `deploy/dashboard.yaml:69`). This is
  the largest privilege transition in the system.
- **B5 Node host to guest VM.** QMP gives full control of a running QEMU.
  Access control is filesystem permission on the socket only.
- **B6 Pod log to destination process.** `--replay-cmdline-from-pod` fetches
  the source QEMU argv from a pod log and executes it
  (`internal/katamaran/cli.go:155`, `internal/migration/destspawn.go:665`).
- **B7 Node to node migration data plane.** RAM pre-copy, NBD mirror and the
  IPIP/GRE redirect tunnel between source and destination nodes
  (`internal/migration/tunnel.go:24`, `internal/migration/source.go`).
  No authentication, no encryption, no integrity protection.
- **B8 Operator to observability endpoints.** Metrics and debug handlers
  inherit no authentication from the servers above.
- **B9 Kubernetes user to the Migration CRD.** A namespaced custom resource
  (`config/crd/migration.yaml:13`) that the cluster-scoped `katamaran-mgr`
  reconciles. This is a second, independent route to the same privileged Job
  as B1, and it does not pass through the dashboard. The controller's
  ClusterRole grants `jobs: create` cluster-wide and `pods: delete`
  (`deploy/manager.yaml:50`, `:40`), so the privilege a CR author gains is the
  controller's, not the author's. T2 is this boundary.

Secrets flow:

- In: service-account tokens projected into the manager, dashboard and
  migration Job pods; the `KATAMARAN_MIGRATION_IMAGE` value (deployment
  configuration, not a secret); guest secrets, which live only inside VM
  memory and disk.
- Live: the webhook private key is generated per process start and held in
  memory only (`cmd/katamaran-mgr/webhook.go:47`).
- Leave: guest memory and disk contents cross node boundaries unencrypted
  (B7); pod log contents cross from the source pod to the destination Job
  (B6).

## 3. Assets and impact

- **Guest memory and disk images.** In-flight migration traffic carries
  everything the workload holds: application data, in-memory credentials,
  session tokens. Readable or modifiable by anyone with a network position
  between the two nodes, and on any node that runs a privileged katamaran pod.
- **Node host.** A privileged Job with hostPaths `/run/vc`, `/sys`,
  `/lib/modules` and `hostPID` is effectively root on the node
  (`internal/orchestrator/templates/job-source.yaml:87`).
- **Cluster-wide pod and pod-log read.** The `katamaran-source` and dashboard
  ClusterRoles permit reading every pod's log, which commonly contains
  credentials, tokens, and startup arguments
  (`deploy/dashboard.yaml:80`, `internal/migration/cmdlinefetch.go:149`).
- **Cluster workload placement.** `POST /api/migrate` schedules privileged
  work on an operator-chosen node, so an unauthenticated caller influences
  where code lands.
- **Workload disruption.** `spec.sourceCleanup: delete` and the dashboard's
  stop endpoints let a caller tear down a running pod, either directly or as
  the tail of a migration the caller started
  (`internal/controller/reconciler.go:552`).
- **VM adoption.** `GetBaseVM` hands a caller the identity of a live migrated
  VM (`internal/factory/server.go:141`).
- **Availability.** Migration aborts, daemon `Quit`, and load-generator
  processes all degrade the cluster's migration capability. A namespace holding
  only the CR grant can also queue privileged Jobs until nodes are saturated.

Impact is not a generic "data breach": the concrete outcome is guest memory
exfiltration or tampering in flight, node compromise through a privileged Job,
or cluster-wide secret disclosure through pod logs.

## 4. Threats per boundary

**B1 (user to dashboard).** Spoofing: no identity exists to spoof, so every
caller is trusted as the operator (T1). Tampering: `POST /api/migrate` accepts
attacker-chosen source, destination, image and node fields, all of which reach
Job templates (`internal/dashboard/migrate.go:86`). Information disclosure:
`/api/pods`, `/api/nodes`, `/api/history` expose the cluster inventory to any
reachable caller. SSRF: the target screen
(`internal/dashboard/validation.go:61`) blocks loopback, link-local,
multicast and cloud metadata addresses but not RFC1918 or cluster service
addresses, so an authenticated-free caller can drive the HTTP load generator
at the apiserver, a kubelet, or another pod's port (T3). DoS: `/api/ping` and
`/api/httpgen` each start a long-running subprocess or request loop from an
unauthenticated caller (T11). Elevation: any of the above reaches Job
creation (T5).

**B2 (dashboard to apiserver).** The dashboard's cluster-scoped read access
is broader than its namespaced write access, so a compromise of the dashboard
pod reads every pod log in the cluster. The reconciliation of that asymmetry
belongs to the authorization review.

**B3 (apiserver to webhook).** The webhook exists to deny race-window Pod
replacements (`internal/controller/admission.go:12`). It fails open on every
error path (`cmd/katamaran-mgr/webhook.go:176`, `:183`, `:193`) and
`failurePolicy: Ignore` means the cluster accepts Pods when it is unreachable
(`deploy/manager.yaml:216`). With a per-start self-signed key, any process
that can reach the Service port can present a different certificate and the
CA bundle is rewritten by the leader on every acquisition, so the webhook's
identity is only as trustworthy as the port binding (T6).

**B4 (control plane to node).** Tampering: image selection is gated only by
exact match against `KATAMARAN_MIGRATION_IMAGE` (`internal/dashboard/migrate.go:103`,
`cmd/katamaran-mgr/main.go:173`), a value read from the environment at
startup. Elevation: the resulting Job is privileged and host-networked on both
sides (`internal/orchestrator/templates/job-dest.yaml:39`). Repudiation: Job
creation is attributable only through apiserver audit logs; the dashboard
attributes requests by `remote_addr` only, with no user identity
(`internal/dashboard/middleware.go:137`).

**B9 (Kubernetes user to the Migration CRD).** The CRD is namespaced
(`config/crd/migration.yaml:13`), so RBAC can grant `create` on it to a
tenant in a single namespace, but the resulting action is cluster-wide.
Elevation: the author does not choose a pod spec, only spec fields, but the
controller renders the same `privileged: true`, `hostNetwork: true`,
`hostPID: true` Job into `kube-system`
(`internal/orchestrator/orchestrator.go:100`,
`internal/orchestrator/templates/job-source.yaml:43`) using its own
cluster-wide ServiceAccount, and the Job mounts hostPaths `/run/vc`, `/sys`
and `/lib/modules` (`internal/orchestrator/templates/job-source.yaml:97`,
`:101`, `:105`).
Reaching a privileged, hostPID, host-network pod that mounts `/sys` and
`/lib/modules` is equivalent to node compromise, so a CR author in namespace
`team-a` gets node-root through a namespace-scoped grant. The repo narrows the
blast radius in two places: pod references are pinned to the CR's own namespace
(`internal/controller/reconciler.go:914`, `:920`) so the author cannot name a
pod in someone else's namespace, and `spec.image` must equal the operator's
configured image (`internal/controller/reconciler.go:889`). Both leave the
privileged Job itself in reach.

Tampering within that grant: `spec.destNode` and `spec.destNodeSelector` place
the privileged Job on a node the author picks, so an author can steer a
hostPID pod onto a chosen node. `spec.tunnelMode` selects the encapsulation
from a closed set (`internal/orchestrator/validation.go:76`) and
`spec.sourceCleanup` selects `none`, `delete` or `orphan`
(`internal/orchestrator/validation.go:112`); `delete` removes the source pod
through the controller's cluster-wide `pods: delete`
(`internal/controller/reconciler.go:552`), which is a workload-disruption
primitive for anyone holding only the CR grant. Denial of service: nothing
limits how many Migrations a namespace may hold, so a CR author can queue
privileged Jobs until the nodes are saturated.

Repudiation: Job creation is attributable only through apiserver audit logs;
the CRD records the author's name on the object but the Job, and the guest it
carries, carry no trace of which CR produced them.

**B5 (node host to VM).** Any process on the node that can open the QMP or
factory socket controls the guest or claims the migrated VM. `Quit` is a
remote stop switch for the daemon (T9, T11).

**B6 (pod log to destination process).** The destination argv comes from a
pod log line. Bounded by `maxPodLogLineSize`, `maxPodLogScanBytes` and
`cmdlineStreamTTL` (`internal/migration/cmdlinefetch.go:20`, `:26`, `:41`),
and executed without a shell, but a workload that can emit a crafted cmdline
marker controls the destination process arguments (T7).

**B7 (node to node data plane).** Tampering and disclosure on unencrypted
RAM and disk streams, plus a host route through the redirect tunnel that any
process able to create tunnels on the source node can interfere with. This is
tracked as a known gap in `docs/ROADMAP.md:43`.

**B8 (operator to observability).** expvar exposes runtime internals and
`--enable-debug` exposes pprof, which allows CPU profiling and forced dumps
(T8).

Recurring classes: the repository's own history shows repeated hardening at
the HTTP input layer (CSRF, security headers, request-ID character filtering,
SSRF screening), so regressions in `internal/dashboard` validation are the
class most likely to recur. The privileged-job and pod-log boundaries have no
equivalent validation depth.

## 5. Mitigations mapping

Present in code:

- Origin, Referer and `Sec-Fetch-Site` checks on state-changing requests
  (`internal/dashboard/middleware.go:169`). Covers browser CSRF against
  `/api/migrate` and the load-generator endpoints. It is not authentication:
  requests with no Origin, Referer or `Sec-Fetch-Site` pass
  (`internal/dashboard/middleware.go:181`), which is every non-browser client.
- Response security headers, including a CSP with `frame-ancestors 'none'`
  and same-origin-only asset sourcing (`internal/dashboard/middleware.go:222`).
- Target screening for the load generators, with DNS revalidation in the
  dialer and redirects disabled
  (`internal/dashboard/validation.go:61`, `internal/dashboard/loadgen.go:278`).
  Does not cover RFC1918 or in-cluster service addresses.
- Shell-safe validation of every migrate form field and an allowlist of
  accepted form keys (`internal/dashboard/migrate.go:86`,
  `internal/dashboard/validation.go:155`,
  `internal/orchestrator/validation.go:20`).
- Exact image allowlist for privileged Jobs
  (`internal/dashboard/migrate.go:103`, `cmd/katamaran-mgr/main.go:173`).
- HTTP server timeouts and header size caps on the dashboard and manager
  servers (`internal/dashboard/server.go:223`, `cmd/katamaran-mgr/debug.go:19`).
- Bounded webhook request body and bounded pod-log scanning
  (`cmd/katamaran-mgr/webhook.go:176`,
  `internal/migration/cmdlinefetch.go:20`).
- Non-root, read-only root filesystem, dropped capabilities and
  `RuntimeDefault` seccomp on the manager and dashboard workloads
  (`deploy/manager.yaml:198`, `deploy/dashboard.yaml:197`).
- Leader election so only one manager patches the webhook CA bundle
  (`deploy/manager.yaml:43`).
- Panic recovery and per-request logging with a validated request ID
  (`internal/dashboard/middleware.go:147`, `:83`).
- Namespace pinning on the CRD path: `spec.sourcePod.namespace` and
  `spec.destPod.namespace` must equal the CR's own namespace, because the
  controller acts on pods cluster-wide
  (`internal/controller/reconciler.go:914`, `:920`). This is the control that
  keeps a namespace-scoped `create` grant from reaching another tenant's pod;
  it does not constrain where the privileged Job lands.
- Image allowlist on the CRD path, separate from the dashboard's copy:
  `authorizeImage` rejects an empty configured image and any `spec.image`
  that is not an exact match (`internal/controller/reconciler.go:889`).
- Closed-enum validation for the CR fields that pick node placement,
  encapsulation and source teardown (`internal/orchestrator/validation.go:76`,
  `:112`).
- Dashboard namespace allowlist: `KATAMARAN_ALLOWED_NAMESPACES` filters
  `/api/pods` and answers 403 on `/api/migrate` for a namespace outside the
  list (`internal/dashboard/authorization.go:50`, `:71`,
  `internal/dashboard/migrate.go:151`). It is opt-in and empty by default:
  `deploy/dashboard.yaml:181` ships the variable commented out, so the
  deployed dashboard is cluster-wide until an operator sets it. With it set,
  it narrows T1 and T3 within the dashboard path only; it does nothing for the
  CRD path (T2).

Missing, ranked:

- Nothing constrains a namespaced `create` on `katamaran.io/migrations` to the
  privilege such a grant implies. The author reaches a `privileged`, `hostPID`
  Job in `kube-system` mounting `/sys` and `/lib/modules` (T2). The namespace
  pin and the image allowlist bound the blast radius but do not close it, and
  no admission policy in `deploy/` or `config/` restricts who may create the CR
  or which nodes it may target.
- No authentication or authorization of any kind on the dashboard API (T1,
  T5). Nothing between the socket and Job creation.
- No authentication on the observability endpoints (T8), and
  `deploy/metrics-services.yaml` now publishes two of them as Services.
- No confidentiality or integrity protection on the migration data plane
  (T4), acknowledged in `docs/ROADMAP.md:43`.
- No peer authentication on the factory gRPC socket (T9).
- Webhook availability and identity both fail open (T6).

Single points of failure:

- The `katamaran-mgr` ClusterRole is the only thing standing between a CR grant
  and a node-compromising pod. It carries `jobs: create`, `pods: delete` and
  `pods/log` cluster-wide (`deploy/manager.yaml:35`, `:48`), so it is one
  binding serving T2, T5 and the pod-log disclosure in section 3 at once.
- `KATAMARAN_MIGRATION_IMAGE` is the only control between a request and an
  arbitrary image running privileged on a node, on both the dashboard path
  (T10) and the CRD path.
- The CSRF check is the only browser-facing control on a mutating API that has
  no authentication of its own (T1).
- The self-signed webhook certificate and `failurePolicy: Ignore` are the only
  things standing between a webhook fault and a cluster-wide race window (T6).

Documentation claims checked against code:

- The image allowlist claim in `README.md:720` matches
  `internal/dashboard/migrate.go:103` and `internal/controller/reconciler.go:889`.
- The unencrypted-stream claim in `docs/ROADMAP.md:43` matches the tunnel and
  NBD paths.
- `cmd/katamaran-dashboard/README.md:39` describes `/api/pods` as narrowed to
  `KATAMARAN_ALLOWED_NAMESPACES` "when that variable is set". That matches
  `internal/dashboard/server.go:454`, and the same README states the unset
  default is every namespace. Accurate as written; the deployed manifest
  leaves it unset (`deploy/dashboard.yaml:181`).
- `docs/TESTING.md:139` claims fuzz coverage of the QMP, cmdline, webhook and
  dashboard input parsers; those fuzz targets are present under `internal/`
  and `cmd/`. This statement is accurate.
- `SECURITY.md` states that no private disclosure channel is configured, that
  only the current release line is supported, and that the dashboard assumes
  admin-only reach. All three match the repository as of this date. Its
  supported-versions table is derived from `CHANGELOG.md:148`; a new release
  makes it stale.

## 6. Abuse cases

A principal holding only `create` on `katamaran.io/migrations` in its own
namespace, with no other privilege, can:

- Cause a `privileged`, `hostPID`, host-networked pod to run in `kube-system`
  on a node it selects through `spec.destNode` or `spec.destNodeSelector`,
  mounting hostPaths `/run/vc`, `/sys` and `/lib/modules`
  (`internal/orchestrator/templates/job-source.yaml:43`, `:97`, `:105`).
- Queue privileged Jobs without limit, since nothing caps the number of
  Migrations a namespace may hold.
- Delete a pod in its own namespace after migration with
  `spec.sourceCleanup: delete` (`internal/controller/reconciler.go:552`).
- Choose the encapsulation of the unencrypted migration tunnel
  (`cmd/katamaran-mgr/main.go:281`).

A caller who can reach the dashboard, without any credentials, can:

- Trigger a migration of any pod it can name to any node it can name
  (`POST /api/migrate`, `internal/dashboard/migrate.go:74`). The pod list
  endpoint supplies the inventory to do this
  (`internal/dashboard/server.go:441`).
- Enumerate every pod and node in the cluster (`/api/pods`, `/api/nodes`).
- Use the load generator to probe cluster-internal services for open ports and
  to generate sustained traffic against a chosen target
  (`internal/dashboard/loadgen.go:150`).
- Read the migration history, including the node pairs and images used
  (`/api/history`).

Trust placed in client-side or non-code enforcement:

- The browser UI is the only place a caller would be told which form fields
  are valid; the server re-validates (`internal/dashboard/migrate.go:81`), so
  this is stated as a resolved item, not a gap.
- The image allowlist is enforced server-side but its value is deployment
  configuration, so the control is only as strong as the Deployment's
  environment (T10).
- Node routing and pod selection are validated only for syntax and
  distinctness (`internal/orchestrator/validation.go:20`); whether the caller
  is entitled to move a given pod is not decided anywhere in this repository.
- Namespace containment is the only tenant boundary on the CRD path, and it is
  enforced in the controller rather than by RBAC
  (`internal/controller/reconciler.go:914`). The repository ships no
  Role granting `create` on `migrations`, so who holds that grant in a real
  cluster is a cluster-admin decision this repository does not record.

## 7. Model maintenance

This file was written from the code on the date above. Re-verify every
`path:line` reference when re-reading; a reference that no longer resolves
means the model has drifted and the surrounding claim is stale.

Known gaps in this pass: no risk-owner or review cadence is recorded, and
`SECURITY.md` records that no private disclosure channel is configured. The
adopted-VM shim (`cmd/containerd-shim-katamaran-adopted-v2`) is experimental
per `AGENTS.md` and was not modelled in detail.
