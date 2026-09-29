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
| T2 | Load-generator targets are screened against loopback, link-local, multicast and metadata IPs only, so cluster-internal and RFC1918 addresses stay reachable (SSRF) | B1 | High | Trivial |
| T3 | Migration data plane is unencrypted: guest RAM, NBD disk mirror and the IPIP/GRE redirect tunnel cross the node network in clear | B7 | High | Requires network position between or on the nodes |
| T4 | Privilege transition from an unauthenticated HTTP request to a `privileged`, `hostNetwork`, `hostPID` Job running the cluster-scoped `katamaran-source` ServiceAccount | B1, B4 | High | Follows directly from T1 |
| T5 | Admission webhook fails open: `failurePolicy: Ignore`, plus a self-signed certificate regenerated on every leader start | B3 | Medium | Triggered by any webhook fault, leader churn, or a port-forwarded spoof |
| T6 | Destination QEMU argv is taken from a Kubernetes pod log and executed; anyone who can write to that log controls the destination process | B6 | Medium | Needs pod-log write or a spoofed source pod |
| T7 | Observability endpoints (`/metrics`, `/debug/vars`, and `/debug/pprof/` under `--enable-debug`) are unauthenticated and served in clear HTTP | B1, B8 | Medium | Requires in-cluster reach |
| T8 | Factory gRPC socket on the node host has no peer authentication; `GetBaseVM` hands over a migrated VM and `Quit` stops the daemon to any local caller | B5 | Medium | Requires code execution on the node |
| T9 | `KATAMARAN_MIGRATION_IMAGE` is the only gate on which image becomes a privileged Job; it is deployment configuration, not a policy | B4 | Medium | Needs write access to the deployment or its config |
| T10 | Denial of service: load generator spawns unbounded per-target work from unauthenticated callers; factory `Quit` is callable by any socket peer | B1, B5 | Low | Trivial, contained to the affected process |

## 1. Attack surface inventory

Network listeners and endpoints:

- Dashboard HTTP server, default `:8080`, exposed as a ClusterIP Service
  (`internal/dashboard/server.go:207`, `internal/dashboard/server.go:256`,
  `deploy/dashboard.yaml:182`). Routes: `GET /healthz`, `GET /readyz`,
  `GET /metrics`, `GET /{$}`, four `/assets/*` handlers, `GET /api/status`,
  `/api/pods`, `/api/nodes`, `/api/history`, and the state-changing
  `POST /api/migrate`, `/api/migrate/stop`, `/api/ping`, `/api/ping/stop`,
  `/api/httpgen`, `/api/httpgen/stop` (`internal/dashboard/server.go:259`).
- Manager observability server, plain HTTP: `GET /healthz`, `GET /readyz`,
  `GET /metrics`, `GET /debug/vars` (`cmd/katamaran-mgr/debug.go:44`).
- Admission webhook server, HTTPS with an in-memory self-signed cert:
  `POST /admit`, `GET /healthz` (`cmd/katamaran-mgr/webhook.go:138`,
  `cmd/katamaran-mgr/webhook.go:161`).
- Dashboard debug routes, behind `--enable-debug`: `/debug/pprof/` and
  `/debug/vars` (`internal/dashboard/server.go:279`).

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
  `internal/katamaran/cli.go:153`).
- Environment: `KATAMARAN_MIGRATION_IMAGE` (`internal/dashboard/server.go:169`,
  `cmd/katamaran-mgr/main.go:173`), `KATAMARAN_POD_WAIT_TIMEOUT`
  (`cmd/katamaran-mgr/main.go:207`), `KATAMARAN_MIGRATION_ID`
  (`internal/katamaran/cli.go:213`), `KATAMARAN_SHIM_LISTENER_FD`,
  `KATAMARAN_SHIM_SOCKET_PATH`, `KUBECONFIG`, `KUBERNETES_SERVICE_HOST`,
  `KUBERNETES_SERVICE_PORT` (`internal/migration/podresolve.go:301`).
- CLI flags on all binaries, notably the migration agent's mode, QMP path,
  tunnel mode, drive list and cmdline replay paths
  (`internal/katamaran/cli.go:133`).
- Kubernetes objects: the `Migration` CRD (`config/crd/migration.yaml:19`),
  node and pod listings the dashboard and manager read.

Surface added by deployment:

- Node daemon runs `hostPID: true`, a privileged container, with hostPaths
  `/run/vc`, `/sys`, `/lib/modules` (`deploy/daemonset.yaml:20`, `:26`,
  `:259`).
- Migration Jobs run `privileged: true`, `hostNetwork: true`, with
  `automountServiceAccountToken: true` and hostPaths
  (`internal/orchestrator/templates/job-source.yaml:33`, `:41`,
  `internal/orchestrator/templates/job-dest.yaml:31`, `:39`).

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
- **VM adoption.** `GetBaseVM` hands a caller the identity of a live migrated
  VM (`internal/factory/server.go:141`).
- **Availability.** Migration aborts, daemon `Quit`, and load-generator
  processes all degrade the cluster's migration capability.

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
at the apiserver, a kubelet, or another pod's port (T2). DoS: `/api/ping` and
`/api/httpgen` each start a long-running subprocess or request loop from an
unauthenticated caller (T10). Elevation: any of the above reaches Job
creation (T4).

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
identity is only as trustworthy as the port binding (T5).

**B4 (control plane to node).** Tampering: image selection is gated only by
exact match against `KATAMARAN_MIGRATION_IMAGE` (`internal/dashboard/migrate.go:103`,
`cmd/katamaran-mgr/main.go:173`), a value read from the environment at
startup. Elevation: the resulting Job is privileged and host-networked on both
sides (`internal/orchestrator/templates/job-dest.yaml:39`). Repudiation: Job
creation is attributable only through apiserver audit logs; the dashboard
attributes requests by `remote_addr` only, with no user identity
(`internal/dashboard/middleware.go:137`).

**B5 (node host to VM).** Any process on the node that can open the QMP or
factory socket controls the guest or claims the migrated VM. `Quit` is a
remote stop switch for the daemon (T8, T10).

**B6 (pod log to destination process).** The destination argv comes from a
pod log line. Bounded by `maxPodLogLineSize`, `maxPodLogScanBytes` and
`cmdlineStreamTTL` (`internal/migration/cmdlinefetch.go:20`, `:26`, `:41`),
and executed without a shell, but a workload that can emit a crafted cmdline
marker controls the destination process arguments (T6).

**B7 (node to node data plane).** Tampering and disclosure on unencrypted
RAM and disk streams, plus a host route through the redirect tunnel that any
process able to create tunnels on the source node can interfere with. This is
tracked as a known gap in `docs/ROADMAP.md:43`.

**B8 (operator to observability).** expvar exposes runtime internals and
`--enable-debug` exposes pprof, which allows CPU profiling and forced dumps
(T7).

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
  (`internal/dashboard/validation.go:61`, `internal/dashboard/loadgen.go:275`).
  Does not cover RFC1918 or in-cluster service addresses.
- Shell-safe validation of every migrate form field and an allowlist of
  accepted form keys (`internal/dashboard/migrate.go:86`,
  `internal/dashboard/validation.go:152`,
  `internal/orchestrator/validation.go:20`).
- Exact image allowlist for privileged Jobs
  (`internal/dashboard/migrate.go:103`, `cmd/katamaran-mgr/main.go:173`).
- HTTP server timeouts and header size caps on the dashboard and manager
  servers (`internal/dashboard/server.go:209`, `cmd/katamaran-mgr/debug.go:19`).
- Bounded webhook request body and bounded pod-log scanning
  (`cmd/katamaran-mgr/webhook.go:176`,
  `internal/migration/cmdlinefetch.go:20`).
- Non-root, read-only root filesystem, dropped capabilities and
  `RuntimeDefault` seccomp on the manager and dashboard workloads
  (`deploy/manager.yaml:168`, `deploy/dashboard.yaml:143`).
- Leader election so only one manager patches the webhook CA bundle
  (`deploy/manager.yaml:43`).
- Panic recovery and per-request logging with a validated request ID
  (`internal/dashboard/middleware.go:147`, `:83`).

Missing, ranked:

- No authentication or authorization of any kind on the dashboard API (T1,
  T4). Nothing between the socket and Job creation.
- No authentication on the observability endpoints (T7).
- No confidentiality or integrity protection on the migration data plane
  (T3), acknowledged in `docs/ROADMAP.md:43`.
- No peer authentication on the factory gRPC socket (T8).
- Webhook availability and identity both fail open (T5).

Single points of failure:

- `KATAMARAN_MIGRATION_IMAGE` is the only control between an HTTP request and
  an arbitrary image running privileged on a node (T9).
- The CSRF check is the only browser-facing control on a mutating API that has
  no authentication of its own (T1).
- The self-signed webhook certificate and `failurePolicy: Ignore` are the only
  things standing between a webhook fault and a cluster-wide race window (T5).

Documentation claims checked against code:

- The image allowlist claim in `README.md:720` matches
  `internal/dashboard/migrate.go:103`.
- The unencrypted-stream claim in `docs/ROADMAP.md:43` matches the tunnel and
  NBD paths.
- There is no `SECURITY.md` in this repository, so there is no disclosure
  contact or supported-versions statement to correct. Recording the absence
  here rather than inventing one.
- `docs/TESTING.md:139` claims fuzz coverage of the QMP, cmdline, webhook and
  dashboard input parsers; those fuzz targets are present under `internal/`
  and `cmd/`. This statement is accurate.

## 6. Abuse cases

A caller who can reach the dashboard, without any credentials, can:

- Trigger a migration of any pod it can name to any node it can name
  (`POST /api/migrate`, `internal/dashboard/migrate.go:74`). The pod list
  endpoint supplies the inventory to do this
  (`internal/dashboard/server.go:268`).
- Enumerate every pod and node in the cluster (`/api/pods`, `/api/nodes`).
- Use the load generator to probe cluster-internal services for open ports and
  to generate sustained traffic against a chosen target
  (`internal/dashboard/loadgen.go:134`).
- Read the migration history, including the node pairs and images used
  (`/api/history`).

Trust placed in client-side or non-code enforcement:

- The browser UI is the only place a caller would be told which form fields
  are valid; the server re-validates (`internal/dashboard/migrate.go:81`), so
  this is stated as a resolved item, not a gap.
- The image allowlist is enforced server-side but its value is deployment
  configuration, so the control is only as strong as the Deployment's
  environment (T9).
- Node routing and pod selection are validated only for syntax and
  distinctness (`internal/orchestrator/validation.go:20`); whether the caller
  is entitled to move a given pod is not decided anywhere in this repository.

## 7. Model maintenance

This file was written from the code on the date above. Re-verify every
`path:line` reference when re-reading; a reference that no longer resolves
means the model has drifted and the surrounding claim is stale.

Known gaps in this pass: no risk-owner or review cadence is recorded, and no
vulnerability disclosure path exists in the repository.
