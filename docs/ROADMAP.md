# Roadmap

Current release: **v0.3.0** (2026-05-07)

---

## Project Status

katamaran is feature-complete for single-pod, multi-disk, single-NIC live migration within a single Kubernetes cluster. All 15 user stories are implemented and verified. The CRD controller, web dashboard, and CLI all drive the same Native orchestrator path.

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

- **Storage and dirty-rate metrics** — controller `/metrics` already exposes phase, RAM transfer, downtime, applied downtime, and RTT; add storage sync percentage and RAM dirty-page rate when those signals are emitted by the source job
- **Full per-pod log streaming for the dashboard** — the dashboard currently tails structured markers from the source pod log; full log streaming would show raw QEMU output in the UI log pane

### Encryption

- **Encrypted migration streams** — NBD drive-mirror and RAM pre-copy traffic are currently unencrypted. For cross-rack or cross-AZ deployments, wrap migration streams in WireGuard or IPsec tunnels. Evaluate whether QEMU's built-in TLS migration support (`tls-creds-x509`) is sufficient or if a host-level mesh is simpler.

### Multi-Disk VMs

- ~~**Parallel or sequential NBD mirrors for multi-disk pods**~~ — Done in the current branch. `--drive-id` accepts comma-separated IDs. All mirrors run in parallel and must reach Ready before RAM pre-copy starts.

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

- **Durable migration history** — the dashboard keeps the last 100 completed/failed migrations in memory; persist them across dashboard restarts if operators need historical audit data
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

   ~~*Approach C: standalone adoption shim*~~ — Implements containerd ttrpc TaskService from scratch. Works but loses all Kata features (exec, logs, metrics, agent integration).

   **Approach D: Kata VM Factory** — **Active development.** Kata has a built-in Factory interface (`virtcontainers.Factory`) with `GetVM(ctx, config) (*VM, error)` designed to provide pre-existing VMs to sandboxes. The factory is used for VM caching/templating but can be repurposed for migration adoption:

   1. Migration completes — dest QEMU running with known PID, QMP socket, vsock CID
   2. katamaran runs a **factory gRPC server** on the dest node that implements Kata's Factory protocol
   3. `GetVM` returns a `*VM` constructed via `NewVMFromGrpc()` wrapping the migrated QEMU
   4. New Kata pod created → shim calls factory → gets the migrated VM → connects to kata-agent via vsock
   5. Full Kata functionality: exec, logs, metrics, container lifecycle — all work because the real Kata shim manages the VM

   No custom shim. No Kata patches. No system file modification. Uses Kata's designed extension point. The katamaran DaemonSet configures `containerd` to point the factory endpoint to katamaran's factory server on install.

   **Implementation status (May 2026):** Factory server built, DaemonSet sidecar deployed, migration-meta.json written, controller creates adoption pods. **Verified on real KVM:** the Kata shim calls the factory's Config() and proceeds to config validation. **Remaining blocker:** `"hypervisor config does not match"` — the factory must return a VMConfig that matches the node's actual Kata configuration field-for-field. A minimal stub causes fallback to direct VM creation. Needs:
   - Capturing VMConfig from the first Kata sandbox persist.json on the node (works when a sandbox exists, fails on fresh nodes)
   - Or importing Kata's TOML config parser to construct the exact VMConfig struct
   - Or having the Kata shim skip the config comparison when the factory is a "migration factory" (upstream change)

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
