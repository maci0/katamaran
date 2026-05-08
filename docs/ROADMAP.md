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

   **Research (May 2026):**

   ~~*Approach A: synthetic persist.json*~~ — **Dead end.** Kata's `fetchSandbox` calls `createSandbox` → `hypervisor.CreateVM()` unconditionally. It always starts a new QEMU regardless of persisted `HypervisorState`. The persist file rebuilds Go structs, not VM connections. The containerd restart recovery path works because **shims stay alive** (containerd reconnects to shims via ttrpc, shims never restart or re-adopt QEMU).

   ~~*Approach B: upstream Kata patch*~~ — Adding `CreateSandboxFromExisting(pid, qmpSocket, vsockCID)` to `virtcontainers` would work but requires Kata upstream buy-in. [Issue #1690](https://github.com/kata-containers/kata-containers/issues/1690) has been open since 2021 with no PRs or assignees. Kata's focus is on Confidential Containers and the Rust runtime rewrite — live migration is not on their active roadmap.

   **Approach C: katamaran adoption shim** — **Active development.** A lightweight containerd shim v2 binary (`containerd-shim-katamaran-v2`) that implements the containerd ttrpc API but connects to an existing migrated QEMU instead of starting one. The shim:
   1. Receives `CreateTask` from containerd (new pod creation)
   2. Instead of launching QEMU, connects to the migrated QEMU's QMP socket and kata-agent vsock
   3. Monitors the QEMU process (PID from migration metadata)
   4. Proxies container lifecycle calls to the existing kata-agent
   5. Reports container status back to containerd/kubelet

   This is equivalent to KubeVirt's `virt-launcher` — a thin wrapper that makes an existing VM look like a container to Kubernetes. No Kata upstream changes needed. The shim registers as a separate runtime class (e.g. `kata-migrated`) so it doesn't interfere with normal Kata pods.

   **Prior art:** [Exotanium](https://katacontainers.io/blog/kata-containers-exotanium-case-study/) modified Kata into a distributed runtime with live migration (Xen-based, details undisclosed). No other public implementation exists.

3. **Owner patching** — After the adoption shim creates a functioning pod on the destination, katamaran patches the source pod's owner (Deployment/ReplicaSet) or uses the admission webhook to redirect replacements to the dest node, keeping the replica count correct.

This is roughly what KubeVirt does with its VirtualMachineInstance lifecycle — `virt-controller` owns the virt-launcher pods and manages the source→dest handoff.

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
