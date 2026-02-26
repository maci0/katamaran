#!/bin/bash
# start-node-a.sh — Launches the source node (Node A) VM.
#
# Network layout:
#   net0 — SLIRP user-mode NAT (internet access + SSH on host port 2222)
#   net1 — Private inter-node link (socket, listens on :12345)
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Pre-flight: verify required files exist before launching QEMU.
for f in node-a.qcow2 seed-a.iso; do
    if [[ ! -f "${f}" ]]; then
        echo "Error: ${f} not found. Run ./testenv/setup.sh first."
        exit 1
    fi
done

echo "Starting Node A (Source) — SSH: localhost:2222"
exec qemu-system-x86_64 \
  -enable-kvm -cpu host -m 4096 -smp 4 \
  -drive file=node-a.qcow2,format=qcow2,if=virtio \
  -drive file=seed-a.iso,format=raw,if=virtio \
  -netdev user,id=net0,hostfwd=tcp::2222-:22 \
  -device virtio-net-pci,netdev=net0 \
  -netdev socket,id=net1,listen=:12345 \
  -device virtio-net-pci,netdev=net1,mac=52:54:00:12:34:56 \
  -nographic
