# Roadmap

Current release: **v0.2.0** (2026-05-01)

---

## Project Status

katamaran is feature-complete for single-pod, single-disk, single-NIC live migration within a single Kubernetes cluster. All 15 user stories are implemented and verified. The CRD controller, web dashboard, and CLI all drive the same Native orchestrator path.

| Area | Status |
|------|--------|
| 3-phase migration (storage → compute → network) | Done |
| Local storage (NBD drive-mirror) | Done |
| Shared storage (Ceph RBD, NFS — skip mirror) | Done |
| Zero-drop network cutover (sch_plug + IPIP/GRE) | Done |
| IPv4 and IPv6 | Done |
| Pod-picker mode (resolve sandbox at runtime) | Done |
| Cmdline replay (dest spawns its own QEMU) | Done |
| Auto-downtime from RTT | Done |
| Multifd parallel RAM channels | Done |
| Migration CRD + controller (HA, leader election) | Done |
| Web dashboard with live progress | Done |
| CI: lint, test, fuzz seeds, build, Docker, E2E | Done |
| Multi-arch release workflow (amd64 + arm64) | Done |
| E2E across CNIs (OVN, Cilium, Calico, Flannel) | Done |
| E2E across providers (minikube, kind) | Done |
| TCG (no-KVM) E2E on CI | Done |

---

## Short Term

Improvements that harden existing functionality without adding new migration capabilities.

### Observability

- **Prometheus metrics from migration progress** — expose storage sync %, RAM dirty rate, and actual downtime as Prometheus metrics on the controller's `/metrics` endpoint (currently only counters like dispatched/succeeded/failed are exposed)
- **Full per-pod log streaming for the dashboard** — the dashboard currently tails structured markers from the source pod log; full log streaming would show raw QEMU output in the UI log pane

### Encryption

- **Encrypted migration streams** — NBD drive-mirror and RAM pre-copy traffic are currently unencrypted. For cross-rack or cross-AZ deployments, wrap migration streams in WireGuard or IPsec tunnels. Evaluate whether QEMU's built-in TLS migration support (`tls-creds-x509`) is sufficient or if a host-level mesh is simpler.

### Multi-Disk VMs

- ~~**Parallel or sequential NBD mirrors for multi-disk pods**~~ — Done (v0.3.0+). `--drive-id` accepts comma-separated IDs. All mirrors run in parallel and must reach Ready before RAM pre-copy starts.

### Test Robustness

- **Fix QMP tests on macOS** — three QMP client tests fail on macOS due to Unix socket path length limits (`bind: invalid argument`). Use shorter temp dir paths or switch to abstract sockets on Linux with a fallback for Darwin.
- **E2E `--method direct`** — currently accepted but exits as not implemented. Either implement direct-mode E2E (katamaran binary invoked outside of Kubernetes Jobs) or remove the flag.

---

## Medium Term

Features that expand what can be migrated or how migration is triggered.

### Migration Scheduling

- ~~**Node selection policy**~~ — Done (v0.3.0+). `spec.destNode` is now optional. When omitted, source pod's nodeSelector and tolerations are copied to the dest Job with an anti-affinity to exclude the source node. `spec.destNodeSelector` allows label-based constraints without naming a specific node.

### Source Pod Lifecycle

> [!IMPORTANT]
> **Standalone pods work fully** — katamaran-mgr owns the migration
> lifecycle end-to-end: source cleanup, dest QEMU, network cutover.
> `sourceCleanup: orphan` or `delete` handles the source pod cleanly.
>
> **Controller-managed pods (Deployments, StatefulSets, Jobs) are
> limited.** These controllers expect to own pod lifecycle — when the
> source pod dies, they create a replacement. The migrated VM is a
> raw QEMU process on the destination, not a Kata pod visible to the
> controller. The Deployment sees N-1 healthy replicas and may
> reschedule. Workarounds: cordon the source node, use
> `sourceCleanup: orphan` (prevents rescheduling by removing
> ownerReferences), or accept the replica count discrepancy.
>
> Full controller-managed migration requires Kata sandbox adoption
> (see Long Term section) — creating a new Kata pod on the
> destination that wraps the migrated QEMU process.

- **Admission webhook for rescheduling prevention** — the current `spec.sourceCleanup: orphan` approach removes ownerReferences from the source pod and deletes it, preventing the owning Deployment/ReplicaSet from replacing it. However, there is a small race window: between migration completion and the orphan patch, the controller could create a replacement. A mutating or validating admission webhook would intercept replacement pod creation at the API level, closing this race. Not needed today (the race window is ~100ms vs. a 5-10s reconcile loop), but worth adding if katamaran operates on latency-sensitive workloads or high-churn Deployments.

### Pod Checkpoint / Restore

- **Rollback capability** — snapshot the pod spec, container state, and block device before migration starts. If migration fails after STOP, restore the source VM from the checkpoint instead of relying on `migrate-cancel` (which requires the source QEMU to still be alive).

### Preemption

- **Mid-flight migration cancellation** — if the destination node runs out of resources during transfer, cancel the migration gracefully. QEMU's `migrate-cancel` is already supported; the orchestrator needs to detect resource pressure and trigger it automatically.

### Dashboard Improvements

- **Migration history** — persist completed/failed migrations and surface them in the dashboard (currently only the active migration is shown)
- **Multi-migration view** — support concurrent migrations in the UI (the backend already supports concurrent migrations via distinct Job names)

---

## Long Term

Architectural extensions and new use cases.

### Cross-Cluster Migration

Migrate a Kata VM pod between Kubernetes clusters for cluster upgrades, region failover, or hybrid-cloud burst.

- Federation-aware orchestration (Cluster API, Admiralty)
- Cross-cluster data path (WireGuard, Submariner, Cilium ClusterMesh)
- IP address handoff via DNS / service mesh (pod CIDR will differ)
- Storage replication across clusters (stretched Ceph or NBD over WAN)
- Credential and secret migration
- RBAC and admission policy alignment

### Multi-NIC Pod Migration (Multus)

Kata supports Multus for multiple network interfaces including SR-IOV passthrough. Migrating multi-NIC pods requires per-interface tunnel and qdisc setup, handling non-migratable passthrough devices, and reconstructing PCI topology on the destination.

### Kata Sandbox Adoption (Controller-Managed Pod Migration)

Today katamaran migrates the QEMU process but the destination VM is not a Kubernetes-managed Kata pod. This limits migration to standalone pods — Deployments and StatefulSets will try to reschedule a replacement for the dead source pod, and the migrated VM is invisible to the cluster.

Full controller-managed migration requires three components working together:

1. **Pre-migration annotation + admission webhook** — Before migration starts, katamaran annotates the source pod's owner (Deployment/ReplicaSet) with `katamaran.io/migrating=true`. A validating admission webhook rejects replacement pod creation while the annotation is present, preventing the race between source pod death and cleanup. After migration, the annotation is removed.

2. **Kata sandbox adoption** — Make the migrated QEMU process appear as a regular Kata pod on the destination, visible to `kubectl`, manageable by Deployments, monitored by kubelet.

   **Research (May 2026):** Kata already has the internal machinery for this. The `fetchSandbox` path in `virtcontainers/sandbox.go` loads persisted `SandboxState` + `HypervisorState` (QEMU PID, UUID, vsock CID, hotplugged devices) from disk and reconnects to a running VM without recreating it. This is designed for containerd restart recovery (containerd restarts, finds still-running shims), but the same mechanism could be exploited for migration adoption.

   **Proposed approach (no Kata upstream changes):**
   1. Migration completes — dest QEMU is running with a known PID, QMP socket, vsock CID
   2. katamaran writes a synthetic `SandboxState` to the dest node's Kata persist directory (`/run/vc/sbs/<id>/`) with the migrated QEMU's `HypervisorState` (PID, UUID, vsock CID, QMP socket path)
   3. katamaran creates a new Kata pod spec on the dest node (via kubelet/containerd CRI)
   4. `containerd-shim-kata-v2` starts, discovers persisted state via `fetchSandbox`, and reconnects to the existing QEMU instead of launching a new one
   5. The kata-agent inside the VM responds on the vsock — the pod is now fully managed

   **Known challenges:**
   - `fetchSandbox` is keyed on sandbox ID — the synthetic state must use an ID that matches what containerd assigns to the new pod sandbox
   - The shim normally creates QEMU first, then persists state — the recovery path may not fire if the shim doesn't detect a pre-existing state at the right lifecycle point
   - vsock CID collision — the source and dest QEMU can't share a CID (already solved by katamaran's guest-cid rewriting)
   - virtiofsd socket and console socket paths must match the shim's expectations
   - The persist format is internal to Kata and may change between versions
   - kata-agent must be alive and responsive inside the migrated VM (it should be — it survived the live migration)

   **Alternative: upstream Kata changes.** A cleaner path would be adding a `CreateSandboxFromExisting(pid, qmpSocket, vsockCID)` API to the Kata shim. The shim would skip VM creation and directly adopt the provided QEMU process. This is a small, well-scoped change to `virtcontainers` — the `fetchSandbox` code already does most of the work. Filed as context on [kata-containers#1690](https://github.com/kata-containers/kata-containers/issues/1690).

   **Prior art:** [Exotanium](https://katacontainers.io/blog/kata-containers-exotanium-case-study/) modified Kata into a distributed runtime with live migration, but used Xen (not QEMU) and hasn't disclosed implementation details. No other public implementation exists. The upstream live migration issue (#1690) has been open since 2021 with no PRs or assignees.

3. **Owner patching** — After sandbox adoption creates a new pod on the destination, katamaran patches the owner's pod template or uses the webhook to redirect the replacement to the dest node, so the Deployment's replica count stays correct.

This is roughly what KubeVirt does with its VirtualMachineInstance lifecycle — `virt-controller` owns the virt-launcher pods and manages the source→dest handoff. katamaran would need equivalent integration with Kata's containerd shim, but the `fetchSandbox` recovery path suggests the gap is smaller than originally assumed.

### Live Migration Scheduling Operator

A full operator that watches node resource utilization, detects imbalance or maintenance events, and automatically triggers migrations to rebalance the cluster — similar to how vSphere DRS works for traditional VMs.

### GPU / Accelerator Passthrough

NVIDIA is exploring Kata + confidential containers for GPU workloads. If VFIO-passthrough GPUs become live-migratable (e.g. via NVIDIA's vGPU migration or future PCIe hot-migration support), katamaran would need to coordinate GPU device detach/attach alongside the existing 3-phase flow.

---

## Non-Goals

Things katamaran intentionally does not do:

- **Container checkpoint/restore (CRIU)** — katamaran migrates the VM, not the container process. CRIU-based migration is a different approach for runc containers.
- **CNI plugin development** — katamaran works with existing CNIs. IP tunnel + sch_plug bridges the convergence gap without modifying the CNI itself.
- **Hypervisor development** — katamaran orchestrates QEMU's existing migration primitives (drive-mirror, migrate, announce-self). It does not patch or extend QEMU.
