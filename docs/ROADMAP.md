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

- **Parallel or sequential NBD mirrors for multi-disk pods** — katamaran currently mirrors a single drive. Kata pods with multiple virtio-blk devices (rootfs + data disk) need either parallel NBD mirrors (one per drive) or sequential mirroring with coordinated drive-mirror completion before RAM pre-copy starts.

### Test Robustness

- **Fix QMP tests on macOS** — three QMP client tests fail on macOS due to Unix socket path length limits (`bind: invalid argument`). Use shorter temp dir paths or switch to abstract sockets on Linux with a fallback for Darwin.
- **E2E `--method direct`** — currently accepted but exits as not implemented. Either implement direct-mode E2E (katamaran binary invoked outside of Kubernetes Jobs) or remove the flag.

---

## Medium Term

Features that expand what can be migrated or how migration is triggered.

### Migration Scheduling

- **Node selection policy** — today the operator or user picks the destination node. Add a scheduler-aware component that selects a target node based on resource headroom, storage locality, network topology, and anti-affinity rules. Could plug into the Kubernetes scheduler framework or be a standalone admission webhook.

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
