#!/bin/bash
# start-node-b.sh — Launches the destination node (Node B) VM.
#
# Network layout:
#   net0 — SLIRP user-mode NAT (internet access + SSH on host port 2223)
#   net1 — Private inter-node link (socket, connects to Node A on :12345)
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Pre-flight: verify required files exist before launching QEMU.
for f in node-b.qcow2 seed-b.iso; do
    if [[ ! -f "${f}" ]]; then
        echo "Error: ${f} not found. Run ./testenv/setup.sh first."
        exit 1
    fi
done

echo "Starting Node B (Destination) — SSH: localhost:2223"
exec qemu-system-x86_64 \
  -enable-kvm -cpu host -m 4096 -smp 4 \
  -drive file=node-b.qcow2,format=qcow2,if=virtio \
  -drive file=seed-b.iso,format=raw,if=virtio \
  -netdev user,id=net0,hostfwd=tcp::2223-:22 \
  -device virtio-net-pci,netdev=net0 \
  -netdev socket,id=net1,connect=127.0.0.1:12345 \
  -device virtio-net-pci,netdev=net1,mac=52:54:00:12:34:57 \
  -nographic
