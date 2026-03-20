#!/bin/bash
# deploy/migrate.sh — Orchestration wrapper for katamaran Job-based migration.
#
# Usage:
#   ./deploy/migrate.sh \
#     --source-node <name> \
#     --dest-node <name> \
#     --tap <iface> \
#     --qmp-source <path> \
#     --qmp-dest <path> \
#     --dest-ip <ip> \
#     --vm-ip <ip> \
#     --image <image:tag> \
#     [--tap-netns <path>] \
#     [--shared-storage] \
#     [--tunnel-mode ipip|gre|none] \
#     [--downtime <ms>] \
#     [--auto-downtime] \
#     [--multifd-channels <n>] \
#     [--context <kubectl-context>]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Default values
SHARED_STORAGE=false
TUNNEL_MODE="ipip"
DOWNTIME="25"
AUTO_DOWNTIME=false
MULTIFD_CHANNELS="4"
KUBECTL_CONTEXT=""
MIG_SUCCESS=false
SOURCE_NODE=""
DEST_NODE=""
TAP_IFACE=""
TAP_NETNS=""
QMP_SOURCE=""
QMP_DEST=""
DEST_IP=""
VM_IP=""
IMAGE_REF=""

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Required flags:"
    echo "  --source-node <name>    Name of the source K8s node"
    echo "  --dest-node <name>      Name of the destination K8s node"
    echo "  --tap <iface>           Destination tap interface for zero-drop buffering (use 'none' to skip)"
    echo "  --qmp-source <path>     Path to QMP socket on source node"
    echo "  --qmp-dest <path>       Path to QMP socket on destination node"
    echo "  --dest-ip <ip>          IP address of the destination node"
    echo "  --vm-ip <ip>            IP address of the VM (pod IP)"
    echo "  --image <tag>           Katamaran container image to use"
    echo ""
    echo "Optional flags:"
    echo "  --tap-netns <path>      Network namespace path for tap interface (e.g. /proc/PID/ns/net)"
    echo "  --shared-storage        Enable shared storage mode"
    echo "  --tunnel-mode <mode>    Tunnel encapsulation: ipip, gre, or none (default: ipip)"
    echo "  --downtime <ms>         Max allowed downtime in milliseconds (default: 25)"
    echo "  --auto-downtime         Auto-calculate downtime based on RTT (overrides --downtime)"
    echo "  --multifd-channels <n>  Parallel TCP channels for RAM migration (default: 4, 0 to disable)"
    echo "  --context <context>     Kubectl context to use"
    echo "  --help                  Show this help message"
    exit "${1:-1}"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --source-node) SOURCE_NODE="$2"; shift 2 ;;
        --dest-node) DEST_NODE="$2"; shift 2 ;;
        --tap) TAP_IFACE="$2"; shift 2 ;;
        --tap-netns) TAP_NETNS="$2"; shift 2 ;;
        --qmp-source) QMP_SOURCE="$2"; shift 2 ;;
        --qmp-dest) QMP_DEST="$2"; shift 2 ;;
        --dest-ip) DEST_IP="$2"; shift 2 ;;
        --vm-ip) VM_IP="$2"; shift 2 ;;
        --image) IMAGE_REF="$2"; shift 2 ;;
        --shared-storage) SHARED_STORAGE=true; shift ;;
        --auto-downtime) AUTO_DOWNTIME=true; shift ;;
        --tunnel-mode) TUNNEL_MODE="$2"; shift 2 ;;
        --downtime) DOWNTIME="$2"; shift 2 ;;
        --multifd-channels) MULTIFD_CHANNELS="$2"; shift 2 ;;
        --context) KUBECTL_CONTEXT="$2"; shift 2 ;;
        --help) usage 0 ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

missing_args=()
[[ -z "$SOURCE_NODE" ]] && missing_args+=(--source-node)
[[ -z "$DEST_NODE" ]] && missing_args+=(--dest-node)
[[ -z "$TAP_IFACE" ]] && missing_args+=(--tap)
[[ -z "$QMP_SOURCE" ]] && missing_args+=(--qmp-source)
[[ -z "$QMP_DEST" ]] && missing_args+=(--qmp-dest)
[[ -z "$DEST_IP" ]] && missing_args+=(--dest-ip)
[[ -z "$VM_IP" ]] && missing_args+=(--vm-ip)
[[ -z "$IMAGE_REF" ]] && missing_args+=(--image)
if [[ ${#missing_args[@]} -gt 0 ]]; then
    echo "Error: missing required flag(s): ${missing_args[*]}"
    usage
fi

if [[ "$TAP_IFACE" == "none" ]]; then
    TAP_IFACE=""
fi

if [[ "$TUNNEL_MODE" != "ipip" && "$TUNNEL_MODE" != "gre" && "$TUNNEL_MODE" != "none" ]]; then
    echo "Error: --tunnel-mode must be 'ipip', 'gre', or 'none'."
    exit 1
fi

if [[ "$TAP_IFACE" == *[[:space:]]* ]]; then
    echo "Error: --tap must be a single interface name without spaces."
    exit 1
fi

for cmd in kubectl envsubst; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "Error: required command not found: $cmd"
        exit 1
    fi
done

KUBECTL=(kubectl)
if [[ -n "$KUBECTL_CONTEXT" ]]; then
    KUBECTL+=(--context "$KUBECTL_CONTEXT")
fi

DEST_EXTRA_ARGS="--multifd-channels $MULTIFD_CHANNELS"
if [[ "$SHARED_STORAGE" == "true" ]]; then
    DEST_EXTRA_ARGS="$DEST_EXTRA_ARGS --shared-storage"
fi

SRC_EXTRA_ARGS="$DEST_EXTRA_ARGS --tunnel-mode $TUNNEL_MODE --downtime $DOWNTIME"
if [[ "$AUTO_DOWNTIME" == "true" ]]; then
    SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --auto-downtime"
fi

# Cleanup trap
cleanup() {
    if [[ "${KATAMARAN_KEEP_JOBS:-}" == "true" ]]; then
        echo ">>> KATAMARAN_KEEP_JOBS set, keeping migration jobs."
        return
    fi
    if [[ "$MIG_SUCCESS" == "true" ]]; then
        echo ">>> Cleaning up migration jobs..."
        "${KUBECTL[@]}" -n kube-system delete job katamaran-dest katamaran-source --ignore-not-found 2>/dev/null || true
    else
        echo ">>> Migration failed; keeping jobs for forensic debugging."
    fi
}
trap cleanup EXIT

dump_debug() {
    echo ""
    echo "=== DESTINATION LOGS ==="
    "${KUBECTL[@]}" -n kube-system logs job/katamaran-dest || true
    echo ""
    echo "=== SOURCE LOGS ==="
    "${KUBECTL[@]}" -n kube-system logs job/katamaran-source || true
    echo ""
    echo "=== DESTINATION DESCRIBE ==="
    "${KUBECTL[@]}" -n kube-system describe job/katamaran-dest || true
    echo ""
    echo "=== SOURCE DESCRIBE ==="
    "${KUBECTL[@]}" -n kube-system describe job/katamaran-source || true
}

echo ">>> Preparing migration..."
"${KUBECTL[@]}" -n kube-system delete job katamaran-dest katamaran-source --ignore-not-found

echo ">>> Deploying destination job on $DEST_NODE..."
export NODE_NAME="$DEST_NODE"
export QMP_SOCKET="$QMP_DEST"
export IMAGE="$IMAGE_REF"
if [[ -n "${TAP_IFACE}" ]]; then
    export EXTRA_ARGS="${DEST_EXTRA_ARGS} --tap ${TAP_IFACE}"
    if [[ -n "${TAP_NETNS}" ]]; then
        export EXTRA_ARGS="${EXTRA_ARGS} --tap-netns ${TAP_NETNS}"
    fi
else
    export EXTRA_ARGS="${DEST_EXTRA_ARGS}"
fi

envsubst '$NODE_NAME $QMP_SOCKET $IMAGE $EXTRA_ARGS' < "${SCRIPT_DIR}/job-dest.yaml" | "${KUBECTL[@]}" apply -f -

echo ">>> Waiting for destination pod to appear..."
for _ in $(seq 1 30); do
    if "${KUBECTL[@]}" -n kube-system get pod -l job-name=katamaran-dest --no-headers 2>/dev/null | grep -q .; then
        break
    fi
    sleep 2
done
echo ">>> Waiting for destination pod to be ready..."
"${KUBECTL[@]}" -n kube-system wait --for=condition=Ready pod -l job-name=katamaran-dest --timeout=60s

echo ">>> Waiting for destination service loop to become ready..."
ready=0
for _ in $(seq 1 20); do
    if "${KUBECTL[@]}" -n kube-system logs job/katamaran-dest 2>/dev/null | grep -q "Waiting for QEMU RESUME"; then
        ready=1
        break
    fi
    sleep 2
done
if [[ "$ready" -ne 1 ]]; then
    echo "Error: destination did not reach ready state in time."
    dump_debug
    exit 1
fi

echo ">>> Deploying source job on $SOURCE_NODE..."
export NODE_NAME="$SOURCE_NODE"
export QMP_SOCKET="$QMP_SOURCE"
export IMAGE="$IMAGE_REF"
export DEST_IP="$DEST_IP"
export VM_IP="$VM_IP"
export EXTRA_ARGS="$SRC_EXTRA_ARGS"

envsubst '$NODE_NAME $QMP_SOCKET $IMAGE $DEST_IP $VM_IP $EXTRA_ARGS' < "${SCRIPT_DIR}/job-source.yaml" | "${KUBECTL[@]}" apply -f -

echo ">>> Waiting for migration to complete..."
set +e
"${KUBECTL[@]}" -n kube-system wait --for=condition=complete job/katamaran-source --timeout=600s
wait_rc=$?
set -e

# Wait for dest job to complete too (it finishes shortly after source).
if [[ "$wait_rc" -eq 0 ]]; then
    "${KUBECTL[@]}" -n kube-system wait --for=condition=complete job/katamaran-dest --timeout=60s 2>/dev/null || true
fi

dump_debug

if [[ "$wait_rc" -ne 0 ]]; then
    echo "Error: source migration job did not complete successfully."
    exit "$wait_rc"
fi

MIG_SUCCESS=true

echo ""
echo ">>> Migration completed successfully!"
