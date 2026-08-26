#!/usr/bin/env python3
"""Hot-plug a virtio-blk data disk into a running QEMU over its QMP socket.

Usage: qmp-hotplug-disk.py <qmp-socket> <disk-image>

Pins the device to bus=pci-bridge-0,addr=0x8 so source and destination keep
matching PCI topology for live migration. Runs on the test node, where only
the stdlib is available.
"""

import json
import socket
import sys
import time

SETTLE = 0.2  # seconds to wait for QEMU's reply before the next command
DEVICE_SETTLE = 0.3  # device_add takes longer than blockdev-add
RECV_SIZE = 4096


def send(sock: socket.socket, command: dict[str, object], settle: float) -> None:
    sock.sendall(json.dumps(command).encode() + b"\n")
    time.sleep(settle)
    sock.recv(RECV_SIZE)


def main(qmp_socket: str, disk_image: str) -> None:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        sock.connect(qmp_socket)
        sock.recv(RECV_SIZE)  # greeting
        send(sock, {"execute": "qmp_capabilities"}, SETTLE)
        send(sock, {
            "execute": "blockdev-add",
            "arguments": {
                "driver": "raw",
                "node-name": "drive-virtio-disk0",
                "file": {"driver": "file", "filename": disk_image},
            },
        }, SETTLE)
        send(sock, {
            "execute": "device_add",
            "arguments": {
                "driver": "virtio-blk-pci",
                "drive": "drive-virtio-disk0",
                "id": "data-disk0",
                "bus": "pci-bridge-0",
                "addr": "0x8",
            },
        }, DEVICE_SETTLE)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(f"usage: {sys.argv[0]} <qmp-socket> <disk-image>")
    main(sys.argv[1], sys.argv[2])
