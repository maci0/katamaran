# Testing Guide

Step-by-step instructions for testing `katamaran` in a local two-node QEMU environment.

## Prerequisites

- Linux host with KVM support (`/dev/kvm` must exist)
- ~10 GB free disk space (base image + COW overlays)
- Packages: `qemu-system-x86`, `qemu-utils`, `cloud-image-utils`, `wget`
- Go 1.22+ (install system-wide)
- Kernel module `sch_plug` (loaded automatically by cloud-init; needed for zero-drop packet buffering)

## 1. Provision the Environment

```bash
./testenv/setup.sh
```

This will:
- Check host dependencies
- Build `katamaran` for Linux/amd64
- Download the Ubuntu 22.04 cloud image (cached on subsequent runs)
- Create COW overlay disks for Node A and Node B
- Generate an SSH keypair (if not present)
- Generate cloud-init seed ISOs with Kata Containers pre-configured

## 2. Start Both Nodes

Open **two terminals** in the project directory:

```bash
# Terminal 1 — Source node
./testenv/start-node-a.sh

# Terminal 2 — Destination node (start after Node A is listening)
./testenv/start-node-b.sh
```

Wait for both VMs to boot. Cloud-init takes approximately 2–3 minutes to install Kata Containers and configure networking.

## 3. SSH Into the Nodes

From a third terminal on the host:

```bash
# Node A (source) — port 2222
ssh -o StrictHostKeyChecking=no -i testenv/kata_test_rsa -p 2222 ubuntu@localhost

# Node B (destination) — port 2223
ssh -o StrictHostKeyChecking=no -i testenv/kata_test_rsa -p 2223 ubuntu@localhost
```

Default password (if SSH key fails): `ubuntu`

## 4. Copy the Migration Tool

```bash
# To Node A
scp -o StrictHostKeyChecking=no -i testenv/kata_test_rsa -P 2222 katamaran ubuntu@localhost:~/

# To Node B
scp -o StrictHostKeyChecking=no -i testenv/kata_test_rsa -P 2223 katamaran ubuntu@localhost:~/
```

## 5. Start a Kata Container on Node A

SSH into Node A and run:

```bash
sudo ctr image pull docker.io/library/nginx:alpine
sudo ctr run --runtime io.containerd.kata.v2 -d docker.io/library/nginx:alpine test-kata
```

Find the QMP socket path:

```bash
sudo ls /run/vc/vm/*/qmp.sock
```

Note the sandbox ID (the directory name under `/run/vc/vm/`).

## 6. Prepare the Destination (Node B)

SSH into Node B and start the destination listener:

```bash
sudo ./katamaran \
  -mode dest \
  -qmp /run/vc/vm/<sandbox-id>/qmp.sock \
  -tap tap0_kata \
  -drive-id drive-virtio-disk0
```

This prepares the destination for the incoming migration (starts an NBD server unless `-shared-storage` is used, and waits for the VM to resume).

## 7. Start the Migration (Node A)

SSH into Node A and initiate the migration:

```bash
sudo ./katamaran \
  -mode source \
  -qmp /run/vc/vm/<sandbox-id>/qmp.sock \
  -dest-ip 10.0.0.2 \
  -vm-ip <kata-pod-ip> \
  -drive-id drive-virtio-disk0
```

Replace `<kata-pod-ip>` with the IP assigned to the Kata container's veth interface.

## 8. Verify

After migration completes, the container should be running on Node B with its network connectivity intact. Verify:

```bash
# On Node B
sudo ctr task list
```

## Shared Storage Mode

If both nodes share a storage backend (e.g., Ceph RBD, NFS), skip the NBD drive-mirror phase:

```bash
# Destination
sudo ./katamaran -mode dest -shared-storage -qmp ... -tap ...

# Source
sudo ./katamaran -mode source -shared-storage -qmp ... -dest-ip ... -vm-ip ...
```

## Running Smoke Tests

The project includes a smoke test script:

```bash
./testenv/test.sh
```

This validates:
- Go source compiles cleanly (`go vet` + `gofmt` + `go build`)
- Binary rejects invalid invocations (missing flags, bad socket, unexpected args, invalid mode)
- Invalid mode produces a specific error message (not just generic usage)
- Source mode missing-flags error mentions `-dest-ip` and `-vm-ip` specifically
- Dest mode QMP error mentions the socket path for debuggability
- Empty mode prints a "Usage" message
- `-help` flag prints descriptions for all seven flags
- `-shared-storage` flag combinations work correctly
- Invalid IP addresses for `-dest-ip` and `-vm-ip` are rejected with specific error messages
- Valid IP addresses pass validation (fail later at QMP connect, not at validation)
- Shell scripts have valid syntax (including `minikube-test.sh` and `k3s-e2e.sh`)
- Required project files exist
- Start scripts fail early when required VM files are missing

## Minikube Smoke Test

For testing katamaran against a real Kata Containers QMP socket inside a single-node Kubernetes cluster:

### Prerequisites

- Linux host with KVM and **nested virtualization** enabled
- `minikube` and `kubectl` installed
- ~10 GB free disk space, ~6 GB free RAM
- katamaran binary built (`./testenv/setup.sh` or manual `go build`)

### Verify Nested Virtualization

```bash
# Intel
cat /sys/module/kvm_intel/parameters/nested   # should print Y or 1

# AMD
cat /sys/module/kvm_amd/parameters/nested     # should print 1
```

### Run

```bash
./testenv/minikube-test.sh          # auto-cleans up after
./testenv/minikube-test.sh --keep   # keep cluster for debugging
```

The script:
1. Starts a minikube cluster (KVM2 driver, containerd)
2. Installs Kata Containers via `kata-deploy` DaemonSet
3. Deploys an `nginx:alpine` pod with `runtimeClassName: kata-qemu`
4. Copies katamaran into the minikube node
5. Locates the Kata QMP socket and runs katamaran in dest mode to validate QMP handshake
6. Tests CLI behavior (invalid mode, usage output)

### What This Validates

- katamaran binary runs correctly on a real Kubernetes node
- QMP socket discovery works with Kata's runtime directory layout
- QMP handshake succeeds with a live Kata VM (proves `NewQMPClient` + `qmp_capabilities` work end-to-end)
- CLI error handling works in-situ

### What This Does NOT Test

- Actual live migration (requires two nodes)
- Network cutover (IPIP tunnel, sch_plug qdisc)
- Storage mirroring (NBD drive-mirror)

## k3s End-to-End Migration Test

For testing actual live migration with a real two-node Kubernetes cluster running on the existing QEMU test VMs.

### Prerequisites

- Both QEMU VMs running (`./testenv/start-node-{a,b}.sh`)
- Cloud-init completed (~3 minutes after boot)
- SSH key at `testenv/kata_test_rsa` (created by `./testenv/setup.sh`)

### Setup

```bash
./testenv/k3s-e2e.sh setup
```

This installs k3s server on Node A, k3s agent on Node B, creates a `kata` RuntimeClass, and deploys an `nginx:alpine` pod with `runtimeClassName: kata` pinned to Node A.

### Running the Migration

After setup completes, follow the printed instructions to:

1. Find the QMP socket on Node A
2. Start katamaran in dest mode on Node B
3. Start katamaran in source mode on Node A
4. Verify zero-drop migration with continuous ping

**Important**: Kubernetes will not be aware of the migration. The kubelet on Node A will see the QEMU process exit and may mark the pod as failed. This is expected — the test validates the **data plane** (packets survive the cutover), not the control plane.

### Status and Teardown

```bash
./testenv/k3s-e2e.sh status     # show cluster, pods, QEMU processes
./testenv/k3s-e2e.sh teardown   # uninstall k3s (VMs stay running)
```

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `Failed to connect to QMP` | Wrong socket path or VM not running | Verify `ls /run/vc/vm/*/qmp.sock` |
| `Failed to add plug qdisc` | `sch_plug` module not loaded | `modprobe sch_plug` |
| `NBD server start failed` | Port 10809 already in use | Check `ss -tlnp \| grep 10809` |
| `drive-mirror failed` | Destination NBD not ready | Ensure dest mode is running first |
| `QEMU reported migration failed` | Insufficient resources or network issue | Check QEMU logs; verify dest is reachable on port 4444 |
| `migration did not complete within` | Migration never converged (dirty page churn) | Reduce VM workload or increase `migrationTimeout` constant |
| `storage sync for job.*did not complete` | Drive-mirror never converged (VM write rate too high) | Reduce VM disk I/O or increase `storageSyncTimeout` constant |
| `timed out waiting for QMP response` | QEMU unresponsive mid-command | Check QEMU process health; may need restart |
| `connection is closed` | QMP command issued after socket was closed | Indicates a bug or QEMU crashed mid-operation; check QEMU logs |
| Cloud-init hangs | No internet in VM | Verify host has internet; VMs use SLIRP user-mode NAT |
| SSH connection refused | VM still booting | Wait 2–3 minutes after QEMU starts |

### NIC Naming Convention

The test VMs use predictable interface names assigned by systemd:

- `enp0s3` — first virtio NIC (SLIRP NAT, internet + SSH)
- `enp0s4` — second virtio NIC (private inter-node socket link)

The `network-config.yaml` and cloud-init `runcmd` both reference these names. If your kernel or distro uses a different naming scheme (e.g., `ens3`/`ens4` or `eth0`/`eth1`), update both `network-config.yaml` and the `runcmd` entries in `testenv/setup.sh` accordingly.
