#!/bin/bash
# build-minikube-modules.sh — Build missing kernel modules for minikube kvm2 VMs.
#
# Minikube's kvm2 kernel (Buildroot-based) ships without several modules needed
# by katamaran: sch_plug (network buffering) and NFS client (shared storage tests).
# This script reproduces the exact Buildroot 2025.02 cross-compiler toolchain,
# then builds the requested modules and loads them on all minikube nodes.
#
# How it works:
#   1. Extracts /proc/config.gz and kernel version from the running VM
#   2. Builds a Buildroot cross-compiler matching minikube's exact toolchain
#   3. Enables requested CONFIG_* options and builds modules in-tree
#   4. Copies the .ko files to each node and loads them with insmod
#
# Usage:
#   ./scripts/build-minikube-modules.sh <minikube-profile> [module-group...]
#
#   Module groups:
#     sch_plug   — tc sch_plug qdisc for zero-drop packet buffering (default)
#     nfs        — NFS client (sunrpc, lockd, nfs, nfsv3) for --storage nfs
#
#   If no module groups are specified, only sch_plug is built.
#
# Examples:
#   ./scripts/build-minikube-modules.sh my-profile              # sch_plug only
#   ./scripts/build-minikube-modules.sh my-profile nfs           # nfs only
#   ./scripts/build-minikube-modules.sh my-profile sch_plug nfs  # both
#
# Prerequisites: minikube (running), docker or podman
# First run takes ~15 min (Buildroot toolchain build). Subsequent runs are cached.

set -euo pipefail

usage() {
    cat <<USAGE
Usage: $0 <minikube-profile> [sch_plug|nfs ...]

Module groups:
  sch_plug   tc sch_plug qdisc for zero-drop packet buffering (default)
  nfs        NFS client (sunrpc, lockd, nfs, nfsv3) for --storage nfs

Examples:
  $0 my-profile              # sch_plug only
  $0 my-profile nfs          # nfs only
  $0 my-profile sch_plug nfs # both
USAGE
}

if [[ "${1:-}" == "--help" ]] || [[ "${1:-}" == "-h" ]]; then
    usage
    exit 0
fi

if [[ $# -lt 1 ]]; then
    usage >&2
    exit 2
fi

PROFILE="$1"
shift
BUILD_DIR=$(mktemp -d)
trap 'rm -rf "${BUILD_DIR}"' EXIT

if [[ -z "${CE:-}" ]]; then
    if command -v podman >/dev/null 2>&1; then
        CE="podman"
    elif command -v docker >/dev/null 2>&1; then
        CE="docker"
    else
        echo "ERROR: podman or docker is required" >&2
        exit 1
    fi
fi

# Default to sch_plug if no module groups specified.
MODULE_GROUPS=("${@:-sch_plug}")

echo ">>> Module groups to build: ${MODULE_GROUPS[*]}"

echo ">>> Extracting kernel info from minikube profile '${PROFILE}'..."
KVER=$(minikube -p "${PROFILE}" ssh -- "uname -r" | tr -d '\r')
echo "    Kernel version: ${KVER}"

# Extract the running kernel's config. Use base64 to avoid PTY binary corruption.
minikube -p "${PROFILE}" ssh -- "base64 /proc/config.gz" | tr -d '\r' | base64 -d > "${BUILD_DIR}/config.gz"
gunzip -f "${BUILD_DIR}/config.gz"

# Enable kernel config options for each requested module group.
enable_config() {
    local key="$1" val="$2"
    if grep -q "^${key}=" "${BUILD_DIR}/config" || grep -q "^# ${key} is not set" "${BUILD_DIR}/config"; then
        sed -i "s/.*${key}.*/${key}=${val}/" "${BUILD_DIR}/config"
    else
        echo "${key}=${val}" >> "${BUILD_DIR}/config"
    fi
}

# Build list of in-tree module directories and .ko files to extract.
MAKE_TARGETS=""
KO_FILES=()
INSMOD_ORDER=()

for group in "${MODULE_GROUPS[@]}"; do
    case "${group}" in
        sch_plug)
            enable_config CONFIG_NET_SCH_PLUG m
            MAKE_TARGETS="${MAKE_TARGETS} net/sched/sch_plug.ko"
            KO_FILES+=("net/sched/sch_plug.ko")
            INSMOD_ORDER+=("sch_plug.ko")
            ;;
        nfs)
            enable_config CONFIG_SUNRPC m
            enable_config CONFIG_SUNRPC_GSS m
            enable_config CONFIG_LOCKD m
            enable_config CONFIG_LOCKD_V4 y
            enable_config CONFIG_NFS_FS m
            enable_config CONFIG_NFS_V3 m
            enable_config CONFIG_NFS_V4 y
            enable_config CONFIG_GRACE_PERIOD m
            MAKE_TARGETS="${MAKE_TARGETS} lib/ net/sunrpc/ fs/lockd/ fs/nfs/"
            KO_FILES+=(
                "net/sunrpc/sunrpc.ko"
                "net/sunrpc/auth_gss/auth_rpcgss.ko"
                "fs/lockd/lockd.ko"
                "fs/nfs/nfs.ko"
                "fs/nfs/nfsv3.ko"
                "lib/grace.ko"
            )
            # Order matters: dependencies must be loaded first.
            INSMOD_ORDER+=("grace.ko" "sunrpc.ko" "auth_rpcgss.ko" "lockd.ko" "nfs.ko" "nfsv3.ko")
            ;;
        *)
            echo "ERROR: Unknown module group '${group}'. Valid: sch_plug, nfs" >&2
            exit 2
            ;;
    esac
done

echo ">>> Building kernel modules (Buildroot toolchain + kernel, first run ~15 min)..."

cat > "${BUILD_DIR}/Dockerfile" <<DOCKERFILE
FROM ubuntu:24.04 AS toolchain

RUN apt-get update && \\
    apt-get install -y --no-install-recommends \\
        build-essential gcc g++ automake libtool git wget rsync bc flex bison \\
        libelf-dev libssl-dev unzip cpio python3 file ca-certificates \\
        libncurses-dev texinfo && \\
    rm -rf /var/lib/apt/lists/*

# Build Buildroot 2025.02 cross-compiler (matches minikube's toolchain exactly).
ARG BR_VERSION=2025.02
RUN git clone --depth=1 --branch=\${BR_VERSION} \\
        https://git.buildroot.org/buildroot /buildroot

WORKDIR /buildroot

# Minimal defconfig: only build the x86_64 glibc cross-toolchain.
RUN make defconfig && \\
    ./utils/config --set-str BR2_DEFCONFIG "" && \\
    ./utils/config --enable  BR2_x86_64 && \\
    ./utils/config --set-str BR2_TOOLCHAIN_BUILDROOT_CXX "" && \\
    ./utils/config --enable  BR2_TOOLCHAIN_BUILDROOT_LOCALE && \\
    ./utils/config --set-str BR2_TARGET_GENERIC_HOSTNAME "minikube" && \\
    ./utils/config --set-str BR2_TARGET_GENERIC_ISSUE "" && \\
    ./utils/config --set-str BR2_LINUX_KERNEL "" && \\
    ./utils/config --set-str BR2_PACKAGE_BUSYBOX "" && \\
    make olddefconfig

RUN make toolchain -j\$(nproc) 2>&1 | tail -5 && \\
    ls output/host/bin/x86_64-buildroot-linux-gnu-gcc && \\
    output/host/bin/x86_64-buildroot-linux-gnu-gcc --version | head -1

# Stage 2: build kernel modules with the Buildroot cross-compiler.
FROM toolchain AS builder

ARG KVER
RUN KMAJOR=\$(echo "\${KVER}" | cut -d. -f1) && \\
    wget -qO- "https://cdn.kernel.org/pub/linux/kernel/v\${KMAJOR}.x/linux-\${KVER}.tar.xz" | tar xJ

WORKDIR /linux-\${KVER}
COPY config .config

ENV PATH="/buildroot/output/host/bin:\${PATH}"
ENV CROSS_COMPILE=x86_64-buildroot-linux-gnu-
ENV ARCH=x86_64

RUN make ARCH=x86_64 CROSS_COMPILE=\${CROSS_COMPILE} olddefconfig 2>/dev/null
RUN make ARCH=x86_64 CROSS_COMPILE=\${CROSS_COMPILE} -j\$(nproc) modules_prepare > /dev/null 2>&1

# Build requested modules in-tree.
RUN make ARCH=x86_64 CROSS_COMPILE=\${CROSS_COMPILE} KBUILD_MODPOST_WARN=1 \\
    -j\$(nproc) ${MAKE_TARGETS} 2>&1 | tail -20

# Collect built modules into /out for easy extraction.
RUN mkdir -p /out && \\
    for ko in ${KO_FILES[*]}; do \\
        if [ -f "\${ko}" ]; then \\
            cp "\${ko}" /out/; \\
            echo "  Built: \${ko}"; \\
        else \\
            echo "  WARNING: \${ko} not found (may be built-in or config dependency missing)"; \\
        fi; \\
    done && \\
    ls -la /out/
DOCKERFILE

"${CE}" build \
    --build-arg "KVER=${KVER}" \
    -t minikube-modules-builder \
    -f "${BUILD_DIR}/Dockerfile" \
    "${BUILD_DIR}" 2>&1 | grep -E "^(Step|Successfully|  LD |  CC |  Built|  WARNING|---)" | head -40

echo ">>> Extracting built modules..."
CONTAINER_ID=$("${CE}" create minikube-modules-builder)
"${CE}" cp "${CONTAINER_ID}:/out/." "${BUILD_DIR}/modules/"
"${CE}" rm "${CONTAINER_ID}" >/dev/null

echo "    Modules extracted:"
ls -la "${BUILD_DIR}/modules/"*.ko 2>/dev/null || { echo "ERROR: No modules built." >&2; exit 1; }

echo ">>> Deploying modules to minikube nodes..."
NODES=$(minikube -p "${PROFILE}" node list 2>/dev/null | awk '{print $1}' | tr -d '\r')

for node in ${NODES}; do
    echo "    Deploying to ${node}..."
    MOD_DIR="/lib/modules/${KVER}/extra"
    minikube -p "${PROFILE}" ssh -n "${node}" -- "sudo mkdir -p ${MOD_DIR}"

    # Copy all built modules to the node.
    for ko_file in "${BUILD_DIR}"/modules/*.ko; do
        ko_name=$(basename "${ko_file}")
        minikube -p "${PROFILE}" cp "${ko_file}" "${node}:${MOD_DIR}/${ko_name}"
    done

    # Load modules in dependency order. Skip already-loaded modules.
    for ko_name in "${INSMOD_ORDER[@]}"; do
        mod_name="${ko_name%.ko}"
        if minikube -p "${PROFILE}" ssh -n "${node}" -- "lsmod | grep -q ^${mod_name}" 2>/dev/null; then
            echo "      ${mod_name} already loaded"
            continue
        fi
        if [[ -f "${BUILD_DIR}/modules/${ko_name}" ]]; then
            if minikube -p "${PROFILE}" ssh -n "${node}" -- "sudo insmod ${MOD_DIR}/${ko_name}" 2>/dev/null; then
                echo "      ${mod_name} loaded"
            else
                echo "      WARNING: ${mod_name} failed to load (may have unmet dependencies)" >&2
            fi
        fi
    done
done

echo ">>> Done. Modules available on all minikube nodes: ${MODULE_GROUPS[*]}"
