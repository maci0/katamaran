#!/bin/bash
# build-minikube-modules.sh — Build missing kernel modules for minikube kvm2 VMs.
#
# Minikube's kvm2 kernel (Buildroot-based) ships without CONFIG_NET_SCH_PLUG.
# This script reproduces the exact Buildroot 2025.02 cross-compiler toolchain,
# then builds sch_plug.ko and loads it on all minikube nodes.
#
# How it works:
#   1. Extracts /proc/config.gz and kernel version from the running VM
#   2. Builds a Buildroot cross-compiler matching minikube's exact toolchain
#   3. Compiles sch_plug.ko using that cross-compiler
#   4. Copies the .ko file to each node and loads it with insmod
#
# Usage:
#   ./scripts/build-minikube-modules.sh <minikube-profile>
#
# Prerequisites: minikube (running), docker or podman
# First run takes ~15 min (Buildroot toolchain build). Subsequent runs are cached.

set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <minikube-profile>" >&2
    exit 1
fi

PROFILE="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR=$(mktemp -d)
trap "rm -rf ${BUILD_DIR}" EXIT

CE="${CE:-$(command -v podman 2>/dev/null || command -v docker)}"

echo ">>> Extracting kernel info from minikube profile '${PROFILE}'..."
KVER=$(minikube -p "${PROFILE}" ssh -- "uname -r" | tr -d '\r')
echo "    Kernel version: ${KVER}"

# Extract the running kernel's config. Use base64 to avoid PTY binary corruption.
minikube -p "${PROFILE}" ssh -- "base64 /proc/config.gz" | tr -d '\r' | base64 -d > "${BUILD_DIR}/config.gz"
gunzip -f "${BUILD_DIR}/config.gz"

# Enable the modules we need.
sed -i 's/.*CONFIG_NET_SCH_PLUG.*/CONFIG_NET_SCH_PLUG=m/' "${BUILD_DIR}/config"
grep -q 'CONFIG_NET_SCH_PLUG' "${BUILD_DIR}/config" || echo 'CONFIG_NET_SCH_PLUG=m' >> "${BUILD_DIR}/config"

echo ">>> Building kernel modules (Buildroot toolchain + kernel, first run ~15 min)..."
cat > "${BUILD_DIR}/Dockerfile" <<'DOCKERFILE'
FROM ubuntu:24.04 AS toolchain

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        build-essential gcc g++ automake libtool git wget rsync bc flex bison \
        libelf-dev libssl-dev unzip cpio python3 file ca-certificates \
        libncurses-dev texinfo && \
    rm -rf /var/lib/apt/lists/*

# Build Buildroot 2025.02 cross-compiler (matches minikube's toolchain exactly).
ARG BR_VERSION=2025.02
RUN git clone --depth=1 --branch=${BR_VERSION} \
        https://git.buildroot.org/buildroot /buildroot

WORKDIR /buildroot

# Minimal defconfig: only build the x86_64 glibc cross-toolchain.
RUN make defconfig && \
    ./utils/config --set-str BR2_DEFCONFIG "" && \
    ./utils/config --enable  BR2_x86_64 && \
    ./utils/config --set-str BR2_TOOLCHAIN_BUILDROOT_CXX "" && \
    ./utils/config --enable  BR2_TOOLCHAIN_BUILDROOT_LOCALE && \
    ./utils/config --set-str BR2_TARGET_GENERIC_HOSTNAME "minikube" && \
    ./utils/config --set-str BR2_TARGET_GENERIC_ISSUE "" && \
    ./utils/config --set-str BR2_LINUX_KERNEL "" && \
    ./utils/config --set-str BR2_PACKAGE_BUSYBOX "" && \
    make olddefconfig

RUN make toolchain -j$(nproc) 2>&1 | tail -5 && \
    ls output/host/bin/x86_64-buildroot-linux-gnu-gcc && \
    output/host/bin/x86_64-buildroot-linux-gnu-gcc --version | head -1

# Stage 2: build the kernel module with the Buildroot cross-compiler.
FROM toolchain AS builder

ARG KVER
RUN KMAJOR=$(echo "${KVER}" | cut -d. -f1) && \
    wget -qO- "https://cdn.kernel.org/pub/linux/kernel/v${KMAJOR}.x/linux-${KVER}.tar.xz" | tar xJ

WORKDIR /linux-${KVER}
COPY config .config

ENV PATH="/buildroot/output/host/bin:${PATH}"
ENV CROSS_COMPILE=x86_64-buildroot-linux-gnu-
ENV ARCH=x86_64

RUN make ARCH=x86_64 CROSS_COMPILE=${CROSS_COMPILE} olddefconfig 2>/dev/null
RUN make ARCH=x86_64 CROSS_COMPILE=${CROSS_COMPILE} -j$(nproc) modules_prepare > /dev/null 2>&1

# Build sch_plug as out-of-tree module.
RUN mkdir -p /build && cp net/sched/sch_plug.c /build/ && \
    echo "obj-m := sch_plug.o" > /build/Kbuild && \
    make ARCH=x86_64 CROSS_COMPILE=${CROSS_COMPILE} \
         KBUILD_MODPOST_WARN=1 M=/build modules 2>&1 && \
    ls -la /build/sch_plug.ko
DOCKERFILE

${CE} build \
    --build-arg "KVER=${KVER}" \
    -t minikube-modules-builder \
    -f "${BUILD_DIR}/Dockerfile" \
    "${BUILD_DIR}" 2>&1 | grep -E "^(Step|Successfully|  LD |  CC |.*sch_plug|---)" | head -30

if ! ${CE} run --rm minikube-modules-builder ls /build/sch_plug.ko >/dev/null 2>&1; then
    echo "ERROR: Module build failed." >&2
    exit 1
fi

echo ">>> Extracting built modules..."
CONTAINER_ID=$(${CE} create minikube-modules-builder)
${CE} cp "${CONTAINER_ID}:/build/sch_plug.ko" "${BUILD_DIR}/sch_plug.ko"
${CE} rm "${CONTAINER_ID}" >/dev/null

echo "    Built: sch_plug.ko ($(stat -c%s "${BUILD_DIR}/sch_plug.ko") bytes)"

echo ">>> Deploying modules to minikube nodes..."
NODES=$(minikube -p "${PROFILE}" node list 2>/dev/null | awk '{print $1}' | tr -d '\r')

for node in ${NODES}; do
    echo "    Loading on ${node}..."
    MOD_DIR="/lib/modules/${KVER}/extra"
    minikube -p "${PROFILE}" ssh -n "${node}" -- "sudo mkdir -p ${MOD_DIR}"
    minikube -p "${PROFILE}" cp "${BUILD_DIR}/sch_plug.ko" "${node}:${MOD_DIR}/sch_plug.ko"
    minikube -p "${PROFILE}" ssh -n "${node}" -- "sudo insmod ${MOD_DIR}/sch_plug.ko"
    if minikube -p "${PROFILE}" ssh -n "${node}" -- "lsmod | grep -q sch_plug" 2>/dev/null; then
        echo "    sch_plug loaded successfully on ${node}"
    else
        echo "    WARNING: sch_plug failed to load on ${node}" >&2
    fi
done

echo ">>> Done. sch_plug module available on all minikube nodes."
