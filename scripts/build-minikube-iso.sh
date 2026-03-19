#!/bin/bash
# build-minikube-iso.sh — Build a custom minikube ISO with sch_plug kernel module.
#
# Clones the minikube repo, patches the kernel defconfig to enable
# CONFIG_NET_SCH_PLUG=m, and builds the ISO inside Docker.
#
# Usage:
#   ./scripts/build-minikube-iso.sh
#
# Output:
#   out/minikube-amd64.iso
#
# Then use it:
#   minikube start --iso-url=file://$(pwd)/out/minikube-amd64.iso --driver=kvm2
#
# Prerequisites: docker, git, ~10 GB disk, ~30 min build time (first run)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MINIKUBE_DIR="${PROJECT_ROOT}/.minikube-build"
MINIKUBE_BRANCH="v1.38.1"
ISO_OUTPUT="${PROJECT_ROOT}/out/minikube-amd64.iso"

CE="${CE:-$(command -v podman 2>/dev/null || command -v docker)}"

echo ">>> Preparing minikube source (${MINIKUBE_BRANCH})..."
if [[ -d "${MINIKUBE_DIR}/.git" ]]; then
    echo "    Using existing checkout at ${MINIKUBE_DIR}"
    cd "${MINIKUBE_DIR}"
    git checkout -f "${MINIKUBE_BRANCH}" 2>/dev/null || true
else
    git clone --depth=1 --branch="${MINIKUBE_BRANCH}" \
        https://github.com/kubernetes/minikube.git "${MINIKUBE_DIR}"
    cd "${MINIKUBE_DIR}"
fi

DEFCONFIG="deploy/iso/minikube-iso/board/minikube/x86_64/linux_x86_64_defconfig"

echo ">>> Patching kernel defconfig to enable sch_plug and nf_tables..."
if ! grep -q 'CONFIG_NET_SCH_PLUG' "${DEFCONFIG}"; then
    cat >> "${DEFCONFIG}" <<'EOF'

# Katamaran: enable sch_plug for zero-drop packet buffering during live migration
CONFIG_NET_SCH_PLUG=m
EOF
    echo "    Added CONFIG_NET_SCH_PLUG=m"
else
    echo "    CONFIG_NET_SCH_PLUG already present"
fi

if ! grep -q 'CONFIG_NF_TABLES=m' "${DEFCONFIG}"; then
    cat >> "${DEFCONFIG}" <<'EOF'

# Katamaran: enable nf_tables for CNI compatibility
CONFIG_NF_TABLES=m
CONFIG_NFT_COMPAT=m
CONFIG_NFT_NAT=m
CONFIG_NFT_CHAIN_NAT=m
EOF
    echo "    Added CONFIG_NF_TABLES=m (+ compat, nat)"
else
    echo "    CONFIG_NF_TABLES already present"
fi

echo ">>> Building ISO build Docker image..."
make buildroot-image 2>&1 | tail -5

echo ">>> Building auto-pause binary..."
make out/auto-pause-amd64 2>&1 | tail -3

echo ">>> Building custom minikube ISO (this takes ~30 min on first run)..."
echo "    Output: ${ISO_OUTPUT}"
make out/minikube-amd64.iso 2>&1 | tail -20

if [[ -f "${MINIKUBE_DIR}/out/minikube-amd64.iso" ]]; then
    mkdir -p "${PROJECT_ROOT}/out"
    cp "${MINIKUBE_DIR}/out/minikube-amd64.iso" "${ISO_OUTPUT}"
    echo ""
    echo ">>> Custom ISO built successfully!"
    echo "    ${ISO_OUTPUT} ($(du -h "${ISO_OUTPUT}" | cut -f1))"
    echo ""
    echo "    Use it with:"
    echo "    minikube start --iso-url=file://${ISO_OUTPUT} --driver=kvm2"
else
    echo "ERROR: ISO build failed. Check output above." >&2
    exit 1
fi
