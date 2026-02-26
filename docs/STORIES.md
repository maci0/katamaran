# User Stories

User stories for katamaran — zero-packet-drop live migration for Kata Containers.

### TL;DR

15 stories across 5 areas: **core migration** (local storage, shared storage, zero-drop network cutover), **IPv4/IPv6** support, **graceful shutdown** (SIGINT/SIGTERM cleanup, idempotent setup), **error handling** (storage/RAM failure detection, CLI validation), **destination ops** (packet buffering, GARP), and **testing** (smoke, single-node QMP, two-node E2E).

---

## Core Migration

### US-1: Live migrate a Kata VM with local storage

> **As a** cluster operator,
> **I want to** live-migrate a Kata Containers VM from one node to another using NBD drive-mirror,
> **so that** the VM continues running on the destination with its local block device intact and zero downtime perceived by the workload.

**Acceptance criteria:**
- [x] Source VM pauses for no more than 50ms (QEMU downtime limit)
- [x] The entire block device is replicated to the destination via NBD before RAM migration begins
- [x] The source VM's block job is cancelled with `force:true` after migration completes
- [x] The destination VM resumes and is fully operational

### US-2: Live migrate a Kata VM with shared storage

> **As a** cluster operator using Ceph RBD or NFS,
> **I want to** skip the NBD drive-mirror phase entirely,
> **so that** migration completes in seconds instead of minutes, since both nodes already share the storage backend.

**Acceptance criteria:**
- [x] Passing `-shared-storage` skips NBD server start, drive-mirror, and NBD server stop
- [x] Only RAM pre-copy and network cutover are performed
- [x] Migration time is dominated by RAM convergence, not disk size

### US-3: Zero-packet-drop network cutover

> **As a** workload owner running a latency-sensitive service,
> **I want** zero in-flight packets dropped during VM migration,
> **so that** active TCP connections and UDP streams survive the cutover without retransmission or data loss.

**Acceptance criteria:**
- [x] A `tc sch_plug` qdisc buffers packets on the destination tap interface during the STOP→RESUME window
- [x] An IPIP tunnel on the source forwards packets arriving at the stale IP to the destination
- [x] After RESUME, the queue is flushed (`release_indefinite`) delivering all buffered packets in order
- [x] GARP (`announce-self`) updates switch MAC tables to the destination port

---

## IPv4 and IPv6

### US-4: Migrate VMs with IPv4 pod IPs

> **As a** cluster operator using an IPv4 CNI,
> **I want** the IPIP tunnel and traffic redirection to work with IPv4 addresses,
> **so that** in-flight IPv4 packets are forwarded to the destination during CNI convergence.

**Acceptance criteria:**
- [x] `setupTunnel` creates a tunnel with `mode ipip` for IPv4 addresses
- [x] Host route uses `ip route add <vmIP> dev <tunnel>`
- [x] Both `-dest-ip` and `-vm-ip` are validated with `netip.ParseAddr`

### US-5: Migrate VMs with IPv6 pod IPs

> **As a** cluster operator using a dual-stack or IPv6-only CNI,
> **I want** the tunnel and traffic redirection to work with IPv6 addresses,
> **so that** IPv6-only workloads can be live-migrated with the same zero-drop guarantee.

**Acceptance criteria:**
- [x] `setupTunnel` creates a tunnel with `mode ip6ip6` for IPv6 addresses
- [x] Host route uses `ip -6 route add <vmIP> dev <tunnel>`
- [x] Mixed address families (IPv4 dest + IPv6 vm or vice versa) are rejected with a clear error
- [x] IPv6 addresses are validated at the CLI level before migration begins

---

## Graceful Shutdown and Cleanup

### US-6: Abort migration gracefully on SIGINT/SIGTERM

> **As a** cluster operator who accidentally started a migration or needs to cancel it,
> **I want** `Ctrl+C` or `SIGTERM` to gracefully abort all in-progress operations and clean up resources,
> **so that** the host networking and QEMU state are left clean without manual intervention.

**Acceptance criteria:**
- [x] Signal handler cancels the context, which propagates to all in-progress operations
- [x] Deferred cleanup removes `tc sch_plug` qdisc, stops NBD server, cancels block jobs, and tears down IPIP tunnel
- [x] Cleanup uses `context.Background()` with a 10s timeout so it runs even after main context cancellation
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
- [x] If the block job disappears unexpectedly, report that it "disappeared unexpectedly"
- [x] If the block job doesn't appear within 30s, report it "did not appear" (likely silent drive-mirror failure)
- [x] If the block job enters a terminal state (`concluded`, `null`) without `ready`, report the state
- [x] If storage sync exceeds `storageSyncTimeout` (2h), report a timeout with the job ID
- [x] Progress is logged as a percentage during sync

### US-9: Detect and report RAM migration failure

> **As a** cluster operator monitoring a migration,
> **I want** clear error messages if RAM migration fails, is cancelled, or times out,
> **so that** I can understand the root cause and decide whether to retry.

**Acceptance criteria:**
- [x] Migration status is logged at each poll interval
- [x] `failed` status includes QEMU's `error-desc` when available
- [x] `cancelled` status returns a distinct sentinel error
- [x] Migration polling is bounded by `migrationTimeout` (1h) to prevent infinite loops
- [x] On failure, `migrate_cancel` is sent to QEMU to resume the source VM

### US-10: Validate CLI inputs before migration begins

> **As a** cluster operator,
> **I want** invalid IP addresses and flag combinations to be rejected immediately at startup,
> **so that** I don't discover configuration errors deep into a multi-hour storage mirror.

**Acceptance criteria:**
- [x] Invalid `-dest-ip` and `-vm-ip` are rejected with `netip.ParseAddr` before any QMP connection
- [x] Missing required flags (`-dest-ip`, `-vm-ip` in source mode) print a clear error and usage
- [x] Unexpected positional arguments are rejected
- [x] Invalid `-mode` values are rejected with the invalid value shown in the error

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
- [x] If `sch_plug` is unavailable (kernel module missing), migration proceeds without buffering (degraded but functional)
- [x] If tap interface is not specified, network queue setup is skipped entirely

### US-12: Broadcast GARP after VM resumes

> **As a** network administrator,
> **I want** the destination VM to broadcast Gratuitous ARP with the correct guest MAC address,
> **so that** L2 switches and CNI plugins update their forwarding tables immediately.

**Acceptance criteria:**
- [x] GARP is sent via QEMU's `announce-self` (not host-side `arping`)
- [x] Uses the guest's actual MAC address on all NICs
- [x] Sends 5 rounds with exponential backoff (50ms initial, 100ms step, 550ms max)
- [x] GARP failure is logged as a warning, not a fatal error

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
- [x] `minikube-e2e.sh` creates a two-node minikube cluster with Kata Containers
- [x] Installs katamaran on both nodes
- [x] Runs a full migration (dest first, then source)
- [x] Validates the VM is running on the destination after migration
- [x] Supports `teardown` subcommand for manual cleanup

