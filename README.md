# katamaran

```
     ~             ~              ~
  ~     ~       ~     ~        ~     ~
~    _____|_____    |    _____|_____    ~
    /     |     \   |   /     |     \
===/_____\|/_____|==|==/_____\|/_____|===
   \_____|_____/    |   \_____|_____/
    |    |  |       |    |    |  |
    |    |  |  KATA | MARAN  |  |
    |    |  |       |    |    |  |
~~~~|~~~~|~~|~~~~~~~|~~~~|~~~~|~~|~~~~~~~~
  ~~~~~~~~~~~~~~~~~~|~~~~~~~~~~~~~~~~~~~~~~~~
                    |
        zero-drop live migration
          for Kata Containers
```

Zero-packet-drop live migration for [Kata Containers](https://katacontainers.io/).

Supports both **local storage** (NBD drive-mirror) and **shared storage** (Ceph, NFS — skip mirroring with `-shared-storage`).

Traditional QEMU live migration assumes shared storage. In Kubernetes with Kata Containers, pods typically use local virtio-blk disks — meaning the entire block device must be migrated alongside RAM and network state. `katamaran` orchestrates all three phases in the correct order while guaranteeing **zero in-flight packet drops** during the cutover.

> *Like a catamaran glides between two hulls, katamaran glides your VM between two nodes — smoothly, with nothing lost overboard.*

---

## Architecture Overview

Migration proceeds in three sequential phases:

```
Source Node                                    Destination Node
┌──────────────────────┐                      ┌──────────────────────┐
│  VM running           │   Phase 1: Storage   │  NBD server listening │
│  drive-mirror ───────────── NBD ──────────►  │  (block device ready) │
│  (background sync)    │                      │                       │
│                       │   Phase 2: Compute   │                       │
│  migrate ────────────────── TCP ──────────►  │  (RAM pre-copy)       │
│  VM pauses (STOP)     │                      │  VM resumes (RESUME)  │
│                       │   Phase 3: Network   │                       │
│  IPIP tunnel ─────────────────────────────►  │  sch_plug unplug      │
│  (redirect traffic)   │                      │  (flush buffered pkts)│
└──────────────────────┘                      └──────────────────────┘
```

### Phase 1 — Storage Mirroring (NBD + drive-mirror)

The destination QEMU starts an NBD server exporting the target block device. The source issues a `drive-mirror` QMP command that copies every block to the remote NBD target in the background while the VM keeps running. Dirty blocks are re-synced continuously until the mirror reports `ready` (fully synchronized).

### Phase 2 — Compute Migration (RAM Pre-Copy & Final Incremental Copy)

Once storage is synchronized, the source starts standard QEMU RAM pre-copy migration (`migrate` QMP command) with `auto-converge` enabled. QEMU iteratively copies dirty RAM pages while the VM continues to run.

To achieve true "zero downtime" perception, `katamaran` explicitly configures QEMU with a strict **50ms downtime limit** and uncaps the migration bandwidth to 10GB/s. This forces QEMU to keep iterating until the remaining dirty RAM can be transferred in under 50 milliseconds.

Once the remaining dirty RAM set is small enough to transfer within this 50ms budget, the VM pauses (emitting the `STOP` event). **At this very last bit, QEMU performs a final incremental copy** of the remaining dirty RAM pages and in-flight storage blocks. Only after this final incremental copy completes does the destination VM resume (emitting the `RESUME` event).

### Phase 3 — Zero-Drop Network Cutover (tc sch_plug + IPIP Tunnel)

The critical downtime window — between `STOP` on the source and `RESUME` on the destination — is where packets would normally be lost. `katamaran` eliminates this:

1. **Source side**: Immediately after `STOP`, an IPIP tunnel is created pointing at the destination node. A host route for the VM IP is added through the tunnel, forwarding any packets that arrive at the (now stale) source to the destination.
2. **Destination side**: A `tc sch_plug` qdisc on the destination tap interface buffers all arriving packets (including those forwarded through the tunnel). The qdisc is installed in pass-through mode (`release_indefinite`) and only switched to buffering (`block`) just before the expected RESUME. When the VM resumes, the queue is unplugged with `release_indefinite`, flushing all buffered packets into the now-running VM in order. QEMU's `announce-self` QMP command then broadcasts Gratuitous ARP using the guest's actual MAC address, ensuring switches learn the correct port binding immediately.

The result: packets that arrive during the switchover are queued, not dropped. After the CNI control plane converges (seconds later), new traffic flows directly to the destination and the tunnel is torn down.

---

## Prerequisites

| Component | Minimum Version | Notes |
|-----------|----------------|-------|
| **QEMU** | 6.2+ | Must support `drive-mirror`, `nbd-server-start`, `announce-self`, QMP |
| **Kata Containers** | 3.x | QMP socket must be accessible |
| **iproute2** | any | `tc` (sch_plug qdisc) + `ip tunnel` (IPIP) |
| **Go** | 1.22+ | Install system-wide |

### CNI Considerations

| CNI | Compatibility | Notes |
|-----|--------------|-------|
| **Kube-OVN** | Excellent | OVN handles port-chassis rebinding and GARP automatically. The IPIP tunnel bridges the gap until OVN converges. |
| **Cilium** | Good | Works with IPIP tunnel + GARP. eBPF datapath reconverges after endpoint re-registration. |
| **Calico** | Good | IPIP/VXLAN modes work. BGP route propagation delay is covered by the tunnel. |
| **Flannel** | Basic | VXLAN FDB entries may need manual nudging. IPIP tunnel covers the gap. |

---

## Project Structure

```
go.mod                  # Go module declaration
main.go                 # CLI entry point — flag parsing and dispatch
config.go               # Constants and runCmd helper
qmp.go                  # QMP client (connect, execute, wait for events)
dest.go                 # Destination-side migration logic
source.go               # Source-side migration logic and polling
tunnel.go               # IPIP tunnel setup and teardown
README.md               # This file
TESTING.md              # Step-by-step test environment guide
testenv/                # Test VM infrastructure
  test.sh               # Smoke tests (40 tests)
  setup.sh              # Provisions two-node QEMU environment
  start-node-a.sh       # Launches source VM
  start-node-b.sh       # Launches destination VM
  minikube-test.sh      # Minikube + Kata smoke tests (requires KVM)
  k3s-e2e.sh            # k3s E2E migration test (uses QEMU VMs)
  cloud-init/           # Cloud-init templates
    user-data.yaml      # Base user-data (shared)
    network-config.yaml # Network config for both nodes
```

---

## Usage

Build the tool:

```bash
go build -o katamaran .
```

`katamaran` handles graceful shutdowns safely. If you send a `SIGINT` (`Ctrl+C`) or `SIGTERM` during an active migration, it will gracefully abort the QMP operations, automatically delete any `tc` queue disciplines, and tear down any temporary `ipip` tunnels to leave the host networking state completely clean.

### Destination Node (run first)

```bash
sudo ./katamaran \
  -mode dest \
  -qmp /run/vc/vm/<sandbox-id>/qmp.sock \
  -tap tap0_kata \
  -drive-id drive-virtio-disk0
```

This will:
1. Install a `sch_plug` qdisc on the tap interface in pass-through mode (non-fatal if unavailable)
2. Start an NBD server on port `10809` for storage mirroring (skipped with `-shared-storage`)
3. Plug the network queue to buffer in-flight packets (skipped if step 1 failed)
4. Wait for the VM to resume (`RESUME` event)
5. Flush all buffered packets via `release_indefinite` (skipped if step 1 failed)
6. Stop the NBD server (skipped with `-shared-storage`)
7. Send Gratuitous ARP via QEMU `announce-self` (uses correct guest MAC)

### Source Node (run after destination is ready)

```bash
sudo ./katamaran \
  -mode source \
  -qmp /run/vc/vm/<sandbox-id>/qmp.sock \
  -dest-ip 10.0.1.42 \
  -vm-ip 10.244.1.15 \
  -drive-id drive-virtio-disk0
```

This will:
1. Start `drive-mirror` to the destination NBD server (skipped with `-shared-storage`)
2. Wait for storage to fully synchronize (skipped with `-shared-storage`)
3. Configure and begin RAM pre-copy migration with auto-converge
4. Wait for VM pause (`STOP` event — downtime window begins)
5. Create an IPIP tunnel to redirect in-flight traffic to destination
6. Monitor migration until completion (bounded by `migrationTimeout`)
7. Cancel migration via `migrate_cancel` if it failed or timed out
8. Abort the block mirror with `force:true` cancel (skipped with `-shared-storage`)
9. Tear down the IPIP tunnel after CNI convergence delay

### Shared Storage Mode

If both nodes share a storage backend (e.g., Ceph RBD, NFS), skip the NBD drive-mirror phase entirely with `-shared-storage`:

```bash
# Destination
sudo ./katamaran -mode dest -shared-storage \
  -qmp /run/vc/vm/<sandbox-id>/qmp.sock -tap tap0_kata

# Source
sudo ./katamaran -mode source -shared-storage \
  -qmp /run/vc/vm/<sandbox-id>/qmp.sock \
  -dest-ip 10.0.1.42 -vm-ip 10.244.1.15
```

### CLI Flags

| Flag | Default | Required | Description |
|------|---------|----------|-------------|
| `-mode` | *(none)* | Yes | `source` or `dest` |
| `-qmp` | `/run/vc/vm/qmp.sock` | No | Path to the QEMU QMP unix socket |
| `-tap` | `tap0` | dest only | Tap interface attached to the VM |
| `-dest-ip` | *(none)* | source only | IP address of the destination node |
| `-vm-ip` | *(none)* | source only | The VM's pod IP (for traffic redirection) |
| `-drive-id` | `drive-virtio-disk0` | No | QEMU block device ID to migrate |
| `-shared-storage` | `false` | No | Skip NBD drive-mirror (use with shared storage, e.g., Ceph/NFS) |

---

## Why Sequential Pre-Copy?

A natural question: *why not mirror storage and RAM in parallel?*

The `drive-mirror` operation generates substantial network I/O — it copies the **entire block device** (often tens of GB) over the wire. Running RAM pre-copy simultaneously would cause two problems:

1. **Buffer overflow on the network path.** Both streams compete for bandwidth. RAM pre-copy is latency-sensitive (dirty pages must be re-sent each round). When storage mirroring saturates the link, RAM rounds take longer, more pages get re-dirtied, and convergence stalls — or the migration fails entirely.

2. **Wasted bandwidth from redundant RAM retransmission.** While storage is still syncing, the VM keeps running and dirtying RAM. Each pre-copy round re-sends those dirty pages. If storage sync takes 5 minutes, that's 5 minutes of RAM rounds that will largely be invalidated. By waiting for storage to reach `ready`, we start RAM pre-copy on a quiet network with the shortest possible convergence path.

The sequential approach — storage first, then RAM — minimizes total migration time and keeps the final downtime window (the `STOP`→`RESUME` gap) as short as possible.

---

## Kubernetes Integration

`katamaran` is designed as a low-level migration primitive. In a production Kubernetes cluster, it would be invoked by a higher-level controller (e.g., a CRD operator) that orchestrates the full lifecycle: selecting a target node, preparing the destination VM, invoking `katamaran` on both sides, and updating Kubernetes state afterward.

This section explores which storage and networking stacks are compatible, what the ideal setup looks like, and the open integration points.

### Storage: CSI Driver Compatibility

The storage strategy depends on whether the cluster uses **shared storage** (both nodes see the same block device) or **local storage** (each node has its own disk).

| CSI Driver | Storage Type | katamaran Mode | Notes |
|-----------|-------------|-------------------|-------|
| **Ceph RBD** (`rbd.csi.ceph.com`) | Shared block | `-shared-storage` | Ideal. Both nodes mount the same RBD image. No data transfer needed — only RAM + network state migrate. Requires `ReadWriteMany` or controlled handoff (unmap on source, map on dest). |
| **CephFS** (`cephfs.csi.ceph.com`) | Shared filesystem | `-shared-storage` | Works if the VM's rootfs is on a CephFS-backed virtio-fs or virtiofs mount. Less common for block-level VM disks. |
| **NFS** (`nfs.csi.k8s.io`) | Shared filesystem | `-shared-storage` | Simple but slower. NFS latency can affect VM disk I/O during and after migration. Acceptable for low-IOPS workloads. |
| **Longhorn** (`driver.longhorn.io`) | Replicated local | NBD drive-mirror | Longhorn volumes are node-local with network replication. `katamaran` mirrors the block device via NBD, then the Longhorn controller can adopt the replica on the destination. |
| **OpenEBS Mayastor** (`io.openebs.csi-mayastor`) | Replicated local | NBD drive-mirror or `-shared-storage` | Mayastor NVMe-oF targets can be re-exported to the destination node, potentially allowing shared-storage mode. Otherwise, NBD drive-mirror works. |
| **TopoLVM** (`topolvm.io`) | Strict local | NBD drive-mirror | Purely local LVM. The entire block device must be mirrored. Best for small disks or infrequent migrations. |
| **Local Path Provisioner** | Strict local | NBD drive-mirror | No replication. Full block copy required. Suitable for dev/test. |

#### Shared Storage: The Fast Path

With Ceph RBD, migration skips the most time-consuming phase entirely. The flow becomes:

```
1. Destination QEMU opens the same RBD image (read-only initially)
2. katamaran -mode dest -shared-storage   → installs qdisc, waits for RESUME
3. katamaran -mode source -shared-storage  → RAM pre-copy only
4. Source VM pauses (STOP), destination resumes (RESUME)
5. RBD image ownership transfers (unmap source, promote dest to read-write)
6. Network cutover via IPIP tunnel + sch_plug flush
```

Total migration time is dominated by RAM pre-copy convergence — typically **seconds** for a 4 GB VM with moderate dirty page rate.

#### Local Storage: The Full Pipeline

With Longhorn or local disks, all three phases run:

```
1. NBD drive-mirror copies the entire block device (minutes to hours for large disks)
2. RAM pre-copy runs after storage is synchronized
3. Network cutover as above
```

The NBD mirror runs in the background while the VM stays live, but total wall-clock time scales with disk size and write rate.

### Networking: CNI Compatibility

The network cutover (Phase 3) must work with the cluster's CNI plugin. The key requirement is that **the VM's pod IP must remain reachable** during the gap between source STOP and destination RESUME, plus the time for the CNI to update its routing/forwarding tables.

| CNI | Compatibility | How It Works | Convergence Time |
|-----|--------------|-------------|-----------------|
| **Kube-OVN** | ★★★ Excellent | OVN's southbound DB updates the port-chassis binding. The logical switch port moves to the destination node automatically. GARP + OVN's own MAC learning provide near-instant convergence. | < 1s |
| **Cilium** | ★★★ Excellent | eBPF datapath. After migration, the destination node's Cilium agent detects the new endpoint and installs eBPF maps. The IPIP tunnel covers the gap. Cilium's IPAM can be configured to preserve pod IPs across nodes with `cluster-pool` mode. | 1–3s |
| **Calico** | ★★☆ Good | BGP route propagation. The destination node advertises the pod IP via BGP. The IPIP tunnel bridges the gap until all peers converge. Calico IPAM must allow the pod IP to exist on the destination node (use `--ipam=host-local` with a shared pool, not per-node blocks). | 2–5s |
| **Flannel** | ★★☆ Good | VXLAN FDB entries. The destination node must update the VXLAN forwarding database. GARP handles L2, but Flannel's `flanneld` may take a few seconds to update FDB entries on all nodes. The IPIP tunnel covers the gap. | 2–5s |
| **Antrea** | ★★☆ Good | OVS-based. Similar to Kube-OVN but with its own controller. Port migration requires the Antrea agent to update OVS flows on the destination. GARP + IPIP tunnel cover the gap. | 1–3s |
| **Multus** (meta-CNI) | Depends | Multus delegates to underlying CNIs. Compatibility depends on the primary and secondary CNI plugins. Each interface may need its own migration strategy. | Varies |

#### IP Preservation

The most critical requirement: **the VM's pod IP must survive migration**. This means:

- The IPAM must allow the same IP to be assigned on the destination node
- Per-node IP blocks (Calico's default) are problematic — the pod IP belongs to the source node's CIDR
- Solutions: cluster-wide IPAM pools, static IP annotation, or a migration-aware IPAM plugin

### The Ideal Setup

For production live migration with minimal downtime and operational complexity:

```
┌─────────────────────────────────────────────────────┐
│                  Ideal Stack                         │
├──────────────┬──────────────────────────────────────┤
│ Runtime      │ Kata Containers 3.x + Cloud Hypervisor or QEMU 8+ │
│ Storage CSI  │ Ceph RBD (rbd.csi.ceph.com)          │
│ Storage Mode │ -shared-storage (skip NBD mirror)     │
│ CNI          │ Kube-OVN or Cilium                    │
│ IPAM         │ Cluster-wide pool (not per-node)      │
│ Kernel       │ 5.15+ (sch_plug, IPIP, KVM)           │
│ Network      │ 25 Gbps+ node-to-node (for RAM pre-copy) │
│ Orchestrator │ CRD operator (manages lifecycle)      │
└──────────────┴──────────────────────────────────────┘
```

**Why this stack:**

1. **Ceph RBD** eliminates the storage mirroring phase entirely. Migration becomes RAM-only, completing in seconds instead of minutes.
2. **Kube-OVN or Cilium** provide the fastest network convergence. OVN's centralized southbound DB updates port bindings atomically. Cilium's eBPF datapath reconverges without waiting for BGP propagation.
3. **Cluster-wide IPAM** ensures the pod IP is valid on any node, avoiding the per-node CIDR problem.
4. **25 Gbps+ networking** ensures the final dirty page flush (the actual downtime-causing transfer) completes well within the 50ms budget.

### Integration Architecture (Operator-Driven)

A Kubernetes operator would manage migration as a CRD:

```yaml
apiVersion: katamaran.io/v1alpha1
kind: VMMigration
metadata:
  name: migrate-nginx-pod
spec:
  podName: nginx-kata-7b4f8c
  targetNode: worker-02
  strategy: live
  sharedStorage: true
  timeout: 300s
```

The operator's reconciliation loop:

```
1. Validate: target node has capacity, Kata runtime, compatible kernel
2. Prepare destination: create placeholder pod, get QMP socket path
3. Start dest-side: invoke katamaran -mode dest on target node
4. Start source-side: invoke katamaran -mode source on source node
5. Wait for completion (or timeout/rollback)
6. Update: patch pod's nodeName, update endpoint slices, clean up source
7. If failed: cancel migration, resume source VM, clean up destination
```

### Open Questions for Production

- **Pod checkpoint/restore**: Should the operator snapshot the pod spec and container state for rollback?
- **Multi-disk VMs**: katamaran currently mirrors a single drive. Multi-disk setups would need parallel NBD mirrors or sequential mirroring.
- **Live migration scheduling**: Which node to pick? Factors: resource headroom, storage locality, network topology, anti-affinity rules.
- **Preemption**: Can a migration be preempted mid-flight if the destination node runs out of resources? This requires `migrate_cancel` QMP support (already available in QEMU).
- **Encryption**: NBD traffic and RAM migration traffic are currently unencrypted. For cross-rack or cross-AZ migration, WireGuard or IPsec tunnels should wrap the migration streams.
- **Observability**: Exposing migration progress (storage sync %, RAM dirty rate, downtime duration) as Prometheus metrics via the operator.

---

## Testing

A local two-node QEMU environment is provided for end-to-end testing. See [TESTING.md](TESTING.md) for step-by-step instructions.

Quick smoke test (no VMs required):

```bash
./testenv/test.sh
```

### Minikube Smoke Test

Validates katamaran against a real Kata Containers QMP socket inside a minikube cluster. Requires KVM with nested virtualization, minikube, and kubectl:

```bash
./testenv/minikube-test.sh          # auto-cleans up after
./testenv/minikube-test.sh --keep   # keep cluster for debugging
```

### k3s End-to-End Migration Test

Layers a two-node k3s cluster on top of the existing QEMU test VMs. Deploys a Kata pod on Node A and provides instructions for running a full live migration to Node B:

```bash
# Provision VMs first (if not already running):
./testenv/setup.sh
./testenv/start-node-a.sh   # Terminal 1
./testenv/start-node-b.sh   # Terminal 2

# Install k3s + deploy test pod:
./testenv/k3s-e2e.sh setup

# Check status:
./testenv/k3s-e2e.sh status

# Teardown k3s (VMs stay running):
./testenv/k3s-e2e.sh teardown
```

---
