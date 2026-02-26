#!/bin/bash
# setup.sh — Provisions a two-node QEMU test environment for
# katamaran. Creates cloud-init ISOs, VM disks, and builds the tool.
#
# Idempotent: safe to re-run. Existing SSH keys and base images are reused.
# Disk images are always recreated (they are cheap COW overlays).
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SCRIPT_DIR}"

readonly BASE_IMAGE="jammy-server-cloudimg-amd64.img"
readonly BASE_IMAGE_URL="https://cloud-images.ubuntu.com/jammy/current/${BASE_IMAGE}"
readonly SSH_KEY="kata_test_rsa"
readonly DISK_SIZE="20G"

echo "=== Setting up Katamaran Test Environment ==="

# --- 1. Check host dependencies ---
MISSING=()
for cmd in qemu-system-x86_64 qemu-img cloud-localds wget; do
    if ! command -v "${cmd}" &>/dev/null; then
        MISSING+=("${cmd}")
    fi
done
if [[ ${#MISSING[@]} -gt 0 ]]; then
    echo "Error: missing required commands: ${MISSING[*]}"
    echo "Ubuntu: sudo apt install qemu-system-x86 qemu-utils cloud-image-utils wget"
    exit 1
fi

# --- 2. Build katamaran for Linux/amd64 ---
if command -v go &>/dev/null; then
    GO_CMD="go"
else
    echo "Error: Go not found. Install Go 1.22+ system-wide."
    exit 1
fi
readonly GO_CMD
echo "Building katamaran for Linux/amd64 (using ${GO_CMD})..."
GOOS=linux GOARCH=amd64 "${GO_CMD}" build -o "${PROJECT_ROOT}/katamaran" "${PROJECT_ROOT}"

# --- 3. Download Ubuntu 22.04 cloud image (if not cached) ---
if [[ ! -f "${BASE_IMAGE}" ]]; then
    echo "Downloading Ubuntu 22.04 cloud image..."
    TMPFILE="$(mktemp "${BASE_IMAGE}.XXXXXX")"
    if wget -q --timeout=60 -O "${TMPFILE}" "${BASE_IMAGE_URL}"; then
        mv "${TMPFILE}" "${BASE_IMAGE}"
    else
        rm -f "${TMPFILE}"
        echo "Error: failed to download ${BASE_IMAGE_URL}"
        exit 1
    fi
else
    echo "Using cached base image: ${BASE_IMAGE}"
fi

# --- 4. Create COW overlay disks for Node A and Node B ---
# Always recreated — they are lightweight copy-on-write overlays.
echo "Creating VM disks..."
qemu-img create -f qcow2 -b "${BASE_IMAGE}" -F qcow2 node-a.qcow2 "${DISK_SIZE}"
qemu-img create -f qcow2 -b "${BASE_IMAGE}" -F qcow2 node-b.qcow2 "${DISK_SIZE}"

# --- 5. Generate SSH keypair (if not present) ---
if [[ ! -f "${SSH_KEY}" ]]; then
    echo "Generating SSH keypair..."
    ssh-keygen -t rsa -b 4096 -f "${SSH_KEY}" -N "" -q
else
    echo "Using existing SSH key: ${SSH_KEY}"
fi
PUB_KEY="$(cat "${SSH_KEY}.pub")"

# --- 6. Generate cloud-init configs ---
cat > cloud-init/network-config.yaml <<'EOF'
version: 2
ethernets:
  enp0s3:
    dhcp4: true
  enp0s4:
    dhcp4: false
EOF

# Base user-data template (shared by both nodes).
# Node-specific runcmd entries are appended below.
cat > cloud-init/user-data.yaml <<EOF
#cloud-config
users:
  - name: ubuntu
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: false
    plain_text_passwd: ubuntu
    ssh_authorized_keys:
      - ${PUB_KEY}

packages:
  - containerd
  - qemu-kvm
  - iproute2

bootcmd:
  - systemctl stop systemd-resolved || true
  - systemctl disable systemd-resolved || true
  - rm -f /etc/resolv.conf
  - echo "nameserver 10.0.2.3" > /etc/resolv.conf
  - chattr +i /etc/resolv.conf

runcmd:
  - chattr -i /etc/resolv.conf

  # Load kernel modules required for nested KVM
  - modprobe kvm || true
  - modprobe kvm_intel || true
  - modprobe kvm_amd || true
  - modprobe vhost_net || true
  - modprobe vhost_vsock || true
  - modprobe sch_plug || true
  - modprobe ipip || true

  # Install Kata Containers 3.x
  - curl -sL --connect-timeout 30 --max-time 300 https://raw.githubusercontent.com/kata-containers/kata-containers/main/utils/kata-manager.sh -o /tmp/kata-manager.sh
  - chmod +x /tmp/kata-manager.sh
  - /tmp/kata-manager.sh

  # Configure containerd for Kata runtime
  - mkdir -p /etc/containerd
  - containerd config default > /etc/containerd/config.toml
  - sed -i '/\[plugins."io.containerd.grpc.v1.cri".containerd.runtimes\]/a \        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata]\n          runtime_type = "io.containerd.kata.v2"\n          privileged_without_host_devices = true\n          pod_annotations = ["io.katacontainers.*"]' /etc/containerd/config.toml
  - systemctl restart containerd

  # Bring up the private inter-node network (enp0s4)
  - ip link set enp0s4 up
EOF

# Node A: source node at 10.0.0.1
cp cloud-init/user-data.yaml cloud-init/user-data-a.yaml
echo "  - ip addr add 10.0.0.1/24 dev enp0s4" >> cloud-init/user-data-a.yaml

# Node B: destination node at 10.0.0.2
cp cloud-init/user-data.yaml cloud-init/user-data-b.yaml
echo "  - ip addr add 10.0.0.2/24 dev enp0s4" >> cloud-init/user-data-b.yaml

# --- 7. Generate cloud-init seed ISOs ---
echo "Generating cloud-init ISOs..."
cloud-localds -N cloud-init/network-config.yaml seed-a.iso cloud-init/user-data-a.yaml
cloud-localds -N cloud-init/network-config.yaml seed-b.iso cloud-init/user-data-b.yaml

echo ""
echo "=== Environment Ready ==="
echo "Start Node A (source):      ./testenv/start-node-a.sh"
echo "Start Node B (destination): ./testenv/start-node-b.sh"
echo "SSH into Node A:            ssh -o StrictHostKeyChecking=no -i testenv/${SSH_KEY} -p 2222 ubuntu@localhost"
echo "SSH into Node B:            ssh -o StrictHostKeyChecking=no -i testenv/${SSH_KEY} -p 2223 ubuntu@localhost"
