# User Stories

User stories for katamaran: zero-packet-drop live migration for Kata Containers.

### TL;DR

22 stories across 8 areas: **core migration** (local storage, shared storage, multi-disk pods, zero-drop network cutover, pod-picker mode, cmdline replay, auto-downtime from RTT, multifd RAM channels), **IPv4/IPv6** support, **graceful shutdown** (SIGINT/SIGTERM cleanup, idempotent setup), **error handling** (storage/RAM failure detection, CLI validation), **destination ops** (packet buffering, GARP), **declarative orchestration** (Migration CRD + HA controller), the **web dashboard**, and **testing** (smoke, single-node QMP, two-node E2E).

---

## Core Migration

### US-1: Live migrate a Kata VM with local storage

> **As a** cluster operator,
> **I want to** live-migrate a Kata Containers VM from one node to another using NBD drive-mirror,
> **so that** the VM continues running on the destination with its local block device intact and zero downtime perceived by the workload.

**Acceptance criteria:**
- [x] Source VM pauses for no more than 25ms (QEMU downtime limit)
- [x] The entire block device is replicated to the destination via NBD before RAM migration begins
- [x] The source VM's block job is cancelled with `force:true` after migration completes
- [x] The destination VM resumes and is fully operational

### US-2: Live migrate a Kata VM with shared storage

> **As a** cluster operator using Ceph RBD or NFS,
> **I want to** skip the NBD drive-mirror phase entirely,
> **so that** migration completes in seconds instead of minutes, since both nodes already share the storage backend.

**Acceptance criteria:**
- [x] Passing `--shared-storage` skips NBD server start, drive-mirror, and NBD server stop
- [x] Only RAM pre-copy and network cutover are performed
- [x] Migration time is dominated by RAM convergence, not disk size

### US-3: Zero-packet-drop network cutover

> **As a** workload owner running a latency-sensitive service,
> **I want** zero in-flight packets dropped during VM migration,
> **so that** active TCP connections and UDP streams survive the cutover without retransmission or data loss.

**Acceptance criteria:**
- [x] A `tc sch_plug` qdisc buffers packets on the destination tap interface during the STOP→RESUME window
- [x] An IPIP tunnel on the source forwards packets arriving at the stale IP to the destination
- [x] After RESUME, the queue is switched to `release_indefinite`, flushing all buffered packets in order
- [x] GARP (`announce-self`) updates switch MAC tables to the destination port
- [x] The tunnel stays up after RESUME for `--cni-convergence-delay` (default 5s) while the CNI propagates the destination binding, then is torn down

### US-16: Migrate a multi-disk Kata VM

> **As a** cluster operator running Kata pods with more than one block device,
> **I want to** mirror all disks during migration,
> **so that** multi-disk workloads move without leaving any volume behind on the source node.

**Acceptance criteria:**
- [x] `--drive-id` accepts comma-separated QEMU block device IDs for multi-disk pods
- [x] All drive-mirror jobs are started before any is waited on, so mirrors run in parallel (job ID `mirror-<drive-id>` per disk)
- [x] The destination adds an NBD export for every drive before mirroring begins
- [x] RAM pre-copy starts only after every mirror job has reached Ready

### US-17: Resolve sandboxes at runtime (pod-picker mode)

> **As a** cluster operator who cannot know the sandbox UUID at scheduling time,
> **I want to** pass pod names instead of raw QMP socket paths and VM IPs,
> **so that** katamaran works from inside Jobs where the sandbox ID is only discoverable at runtime.

**Acceptance criteria:**
- [x] `--pod-name` / `--pod-namespace` replace `--qmp` and `--vm-ip` on the source side; the pod IP is looked up via the in-cluster apiserver
- [x] The source sandbox is resolved by matching the sandbox whose network namespace contains that pod IP; zero matches and multiple matches are errors (it refuses to guess)
- [x] `--dest-pod-name` / `--dest-pod-namespace` resolve the destination QMP socket the same way, overriding the placeholder path baked into Job templates

### US-18: Replay the source QEMU cmdline on the destination

> **As an** operator migrating without a pre-created destination pod,
> **I want** the dest side to spawn its own QEMU using the exact source command line,
> **so that** disk topology, memory layout, and device configuration match without a hand-maintained dest config.

**Acceptance criteria:**
- [x] Source mode captures `/proc/<qemu_pid>/cmdline` via `--emit-cmdline-to` before migration starts and prints it base64-encoded as a `KATAMARAN_CMDLINE_B64=` log marker
- [x] Dest mode fetches the marker from the source pod's log (`--replay-cmdline-from-pod <ns>/<name>`), decodes it, and spawns QEMU with `-incoming defer`
- [x] A file-based `--replay-cmdline <path>` variant remains for manual testing; user-staged files are never deleted (only fetched ones are cleaned up)

### US-19: Auto-calculate downtime from measured RTT

> **As a** cluster operator deploying across networks with varying latency,
> **I want** the downtime limit derived from the measured RTT to the destination instead of a static flag default,
> **so that** migrations converge on both low-latency LANs and higher-latency inter-AZ links without manual tuning.

**Acceptance criteria:**
- [x] `--auto-downtime` measures RTT to the destination via ICMP echo (5s timeout per probe)
- [x] The applied limit is 2× RTT plus a floor; the floor defaults to 25ms and is overridden by `--auto-downtime-floor-ms`
- [x] On RTT measurement failure, the static `--downtime` value is used and the marker reports `auto=false`
- [x] The applied limit surfaces in the `KATAMARAN_DOWNTIME_LIMIT applied_ms=… rtt_ms=… auto=…` marker so orchestrators can record it

### US-20: Parallelize RAM transfer over multifd channels

> **As a** cluster operator migrating memory-heavy VMs over bandwidth-limited links,
> **I want** RAM pre-copy spread across parallel TCP channels,
> **so that** migration throughput is not capped by a single connection.

**Acceptance criteria:**
- [x] QEMU's `multifd` capability is enabled with N channels when `--multifd-channels` > 0 (default 4)
- [x] Setting 0 disables multifd; negative values are rejected before any side effect
- [x] The channel count is passed via migrate parameters on both source and destination

---

## IPv4 and IPv6

### US-4: Migrate VMs with IPv4 pod IPs

> **As a** cluster operator using an IPv4 CNI,
> **I want** the IPIP tunnel and traffic redirection to work with IPv4 addresses,
> **so that** in-flight IPv4 packets are forwarded to the destination during CNI convergence.

**Acceptance criteria:**
- [x] `setupTunnel` creates a tunnel with `mode ipip` for IPv4 addresses
- [x] Host route uses `ip route replace <vmIP> dev <tunnel>`
- [x] Both `--dest-ip` and `--vm-ip` are validated with `netip.ParseAddr`
- [x] `--tunnel-mode gre` creates a GRE tunnel instead; `--tunnel-mode none` skips tunnel creation entirely

### US-5: Migrate VMs with IPv6 pod IPs

> **As a** cluster operator using a dual-stack or IPv6-only CNI,
> **I want** the tunnel and traffic redirection to work with IPv6 addresses,
> **so that** IPv6-only workloads can be live-migrated with the same zero-drop guarantee.

**Acceptance criteria:**
- [x] `setupTunnel` creates a tunnel with `mode ip6ip6` for IPv6 addresses
- [x] Host route uses `ip -6 route replace <vmIP> dev <tunnel>`
- [x] Mixed address families (IPv4 dest + IPv6 vm or vice versa) are rejected with a clear error
- [x] IPv6 addresses are validated at the CLI level before migration begins
- [x] `--tunnel-mode gre` creates an `ip6gre` tunnel instead

---

## Graceful Shutdown and Cleanup

### US-6: Abort migration gracefully on SIGINT/SIGTERM

> **As a** cluster operator who accidentally started a migration or needs to cancel it,
> **I want** `Ctrl+C` or `SIGTERM` to gracefully abort all in-progress operations and clean up resources,
> **so that** the host networking and QEMU state are left clean without manual intervention.

**Acceptance criteria:**
- [x] Signal handler cancels the context, which propagates to all in-progress operations
- [x] Deferred cleanup removes `tc sch_plug` qdisc, stops NBD server, cancels block jobs, and tears down IPIP tunnel
- [x] Cleanup uses `context.WithoutCancel` with a 10s timeout so it runs even after main context cancellation while preserving parent values
- [x] Exit code is 130 (standard SIGINT exit code)

### US-7: Idempotent tunnel and qdisc setup

> **As a** cluster operator re-running katamaran after a partial failure,
> **I want** tunnel and qdisc setup to be idempotent,
> **so that** stale resources from a previous run are cleaned up automatically before creating new ones.

**Acceptance criteria:**
- [x] `setupTunnel` deletes any existing tunnel with the same name before creation
- [x] Destination qdisc setup removes any existing root qdisc before adding a new one
- [x] NBD server setup stops any existing server before starting a new one

---

## Error Handling and Observability

### US-8: Detect and report storage sync failure

> **As a** cluster operator monitoring a migration,
> **I want** clear error messages if storage mirroring fails or stalls,
> **so that** I can diagnose and resolve the issue without inspecting QEMU internals.

**Acceptance criteria:**
- [x] If the block job disappears unexpectedly, report that it "disappeared"
- [x] If the block job doesn't appear within 30s, report it "did not appear" (likely silent drive-mirror failure)
- [x] If the block job enters a terminal state (`concluded`, `null`) without `ready`, report the state
- [x] If storage sync exceeds `storageSyncTimeout` (2h), report a timeout with the job ID
- [x] Progress is logged as a percentage during sync

### US-9: Detect and report RAM migration failure

> **As a** cluster operator monitoring a migration,
> **I want** clear error messages if RAM migration fails, is cancelled, or times out,
> **so that** I can understand the root cause and decide whether to retry.

**Acceptance criteria:**
- [x] Migration status is logged on status change or significant progress (remaining bytes halved) during polling
- [x] `failed` status includes QEMU's `error-desc` when available
- [x] `cancelled` status returns a distinct sentinel error
- [x] Migration polling is bounded by `migrationTimeout` (1h) to prevent infinite loops
- [x] On failure, `migrate-cancel` is sent to QEMU to resume the source VM

### US-10: Validate CLI inputs before migration begins

> **As a** cluster operator,
> **I want** invalid IP addresses and flag combinations to be rejected immediately at startup,
> **so that** I don't discover configuration errors deep into a multi-hour storage mirror.

**Acceptance criteria:**
- [x] Invalid `--dest-ip` and `--vm-ip` are rejected with `netip.ParseAddr` before any QMP connection
- [x] Missing required flags (`--dest-ip`, `--vm-ip` in source mode) print a clear error and usage
- [x] Unexpected positional arguments are rejected
- [x] Invalid `--mode` values are rejected with the invalid value shown in the error

---

## Destination-Side Operations

### US-11: Buffer and flush packets across RESUME

> **As a** workload with active network connections,
> **I want** the destination to buffer all arriving packets before RESUME and flush them immediately after,
> **so that** no packets are lost during the brief VM pause.

**Acceptance criteria:**
- [x] `sch_plug` qdisc is installed in pass-through mode initially (so pre-migration traffic flows normally)
- [x] Queue is switched to `block` mode before the expected RESUME
- [x] On RESUME, queue is switched to `release_indefinite`, flushing all buffered packets
- [x] If `sch_plug` is unavailable (kernel module missing) and the tap interface exists, migration fails with a clear error suggesting `sch_plug` may not be loaded
- [x] If tap interface is not specified, network queue setup is skipped entirely

### US-12: Broadcast GARP after VM resumes

> **As a** network administrator,
> **I want** the destination VM to broadcast Gratuitous ARP with the correct guest MAC address,
> **so that** L2 switches and CNI plugins update their forwarding tables immediately.

**Acceptance criteria:**
- [x] GARP is sent via QEMU's `announce-self` (not host-side `arping`)
- [x] Uses the guest's actual MAC address on all NICs
- [x] Sends 5 rounds with incremental backoff (20ms initial, +100ms step, 550ms max)
- [x] GARP failure is warn-only: it is logged and the destination run continues (RESUME has already fired by then, so failing the whole job over a convergence accelerator would mark a completed migration failed)

---

## Declarative Orchestration

### US-21: Drive migrations declaratively via a Migration CRD

> **As a** platform engineer managing migrations across a fleet,
> **I want to** declare a Migration resource and have a controller run the whole flow,
> **so that** migrations survive leader restarts, integrate with RBAC, and do not depend on someone babysitting two shell jobs.

**Acceptance criteria:**
- [x] A `Migration` CRD declares source/dest pod refs, image, node selection (`spec.destNode`, optional `spec.destNodeSelector`), cleanup policy (`spec.sourceCleanup`), and adoption (`spec.adoptVM`)
- [x] katamaran-mgr runs as multiple replicas safely: Lease-based leader election ensures only the leader reconciles
- [x] The controller dispatches source and dest Jobs, watches them to a terminal phase, and patches CR status along the way
- [x] If the leader restarts between source-Job and dest-Job creation, recovery re-submits the missing dest Job idempotently
- [x] Deleted Migration CRs are cleaned up through a finalizer
- [x] With `adoptVM: true`, a validating admission webhook denies replacement pod creation by the source controller for 5 minutes after migration success, and an adoption pod inheriting the source's labels and ownerReferences is created

---

## Web Dashboard

### US-22: Track live migration progress from a web dashboard

> **As a** cluster operator driving migrations during a maintenance window,
> **I want** a web UI showing live migration phase, RAM progress, and history,
> **so that** I can watch and trigger migrations from a browser without kubectl access to the internals.

**Acceptance criteria:**
- [x] `POST /api/migrate` starts a migration; the `KATAMARAN_MIGRATION_IMAGE` env pins the single allowed image and the server warns when it is unset
- [x] Live progress is derived from structured markers tailed off the source pod log (`KATAMARAN_PROGRESS`, `KATAMARAN_DOWNTIME_LIMIT`)
- [x] `/api/history` returns the last 100 completed/failed migrations kept in memory
- [x] The server exposes `/metrics`, `/healthz`, and `/readyz`

---

## Testing

### US-13: Smoke test without VMs

> **As a** developer working on katamaran,
> **I want** a fast smoke test suite that validates compilation, formatting, and CLI behavior without requiring VMs or KVM,
> **so that** I can iterate quickly on code changes.

**Acceptance criteria:**
- [x] `test.sh` validates `go vet`, `gofmt`, and `go build`
- [x] Tests exercise all flag combinations, error messages, and edge cases
- [x] Tests validate both IPv4 and IPv6 address parsing
- [x] Tests check shell script syntax for all `.sh` files
- [x] All tests pass in under 30 seconds on any Linux machine

### US-14: Single-node Kata QMP smoke test

> **As a** developer with KVM access,
> **I want** to test katamaran against a real Kata Containers QMP socket,
> **so that** I can verify the QMP handshake and command execution work with a real QEMU instance.

**Acceptance criteria:**
- [x] `minikube-test.sh` creates a single-node minikube cluster with Kata Containers
- [x] Deploys a Kata pod and locates its QMP socket
- [x] Runs katamaran in dest mode against the live QMP socket
- [x] Cleans up automatically (or preserves with `--keep`)

### US-15: Two-node E2E migration test

> **As a** developer validating the full migration flow,
> **I want** an end-to-end test that performs a real live migration between two nodes,
> **so that** I can verify all three phases (storage, compute, network) work together.

**Acceptance criteria:**
- [x] `e2e.sh` creates a two-node minikube or kind cluster with Kata Containers
- [x] Installs katamaran on both nodes
- [x] Runs a full migration (dest first, then source)
- [x] Validates the VM is running on the destination after migration
- [x] Supports `--teardown` flag for manual cleanup

