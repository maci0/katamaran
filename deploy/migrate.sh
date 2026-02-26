#!/bin/bash
# deploy/migrate.sh — Orchestration wrapper for katamaran Job-based migration.
#
# Usage:
#   ./deploy/migrate.sh \
#     --source-node <name> \
#     --dest-node <name> \
#     --qmp-source <path> \
#     --qmp-dest <path> \
#     --dest-ip <ip> \
#     --vm-ip <ip> \
#     --image <image:tag> \
#     [--shared-storage] \
#     [--tunnel-mode ipip|gre] \
#     [--context <kubectl-context>]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Default values
SHARED_STORAGE=false
TUNNEL_MODE="ipip"
KUBECTL_CONTEXT=""
SOURCE_NODE=""
DEST_NODE=""
QMP_SOURCE=""
QMP_DEST=""
DEST_IP=""
VM_IP=""
IMAGE=""

usage() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Required options:"
    echo "  --source-node <name>    Name of the source K8s node"
    echo "  --dest-node <name>      Name of the destination K8s node"
    echo "  --qmp-source <path>     Path to QMP socket on source node"
    echo "  --qmp-dest <path>       Path to QMP socket on destination node"
    echo "  --dest-ip <ip>          IP address of the destination node"
    echo "  --vm-ip <ip>            IP address of the VM (pod IP)"
    echo "  --image <tag>           Katamaran container image to use"
    echo ""
    echo "Optional options:"
    echo "  --shared-storage        Enable shared storage mode"
    echo "  --tunnel-mode <mode>    Tunnel encapsulation (ipip or gre, default: ipip)"
    echo "  --context <context>     Kubectl context to use"
    echo "  --help                  Show this help message"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --source-node) SOURCE_NODE="$2"; shift 2 ;;
        --dest-node) DEST_NODE="$2"; shift 2 ;;
        --qmp-source) QMP_SOURCE="$2"; shift 2 ;;
        --qmp-dest) QMP_DEST="$2"; shift 2 ;;
        --dest-ip) DEST_IP="$2"; shift 2 ;;
        --vm-ip) VM_IP="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --shared-storage) SHARED_STORAGE=true; shift ;;
        --tunnel-mode) TUNNEL_MODE="$2"; shift 2 ;;
        --context) KUBECTL_CONTEXT="$2"; shift 2 ;;
        --help) usage ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

if [[ -z "$SOURCE_NODE" || -z "$DEST_NODE" || -z "$QMP_SOURCE" || -z "$QMP_DEST" || -z "$DEST_IP" || -z "$VM_IP" || -z "$IMAGE" ]]; then
    echo "Error: Missing required arguments."
    usage
fi

KUBECTL="kubectl"
if [[ -n "$KUBECTL_CONTEXT" ]]; then
    KUBECTL="kubectl --context $KUBECTL_CONTEXT"
fi

DEST_EXTRA_ARGS=""
if [[ "$SHARED_STORAGE" == "true" ]]; then
    DEST_EXTRA_ARGS="-shared-storage"
fi

SRC_EXTRA_ARGS="$DEST_EXTRA_ARGS -tunnel-mode $TUNNEL_MODE"

# Cleanup trap
cleanup() {
    echo ">>> Cleaning up migration jobs..."
    $KUBECTL -n kube-system delete job katamaran-dest katamaran-source --ignore-not-found 2>/dev/null || true
}
trap cleanup EXIT

echo ">>> Preparing migration..."
$KUBECTL -n kube-system delete job katamaran-dest katamaran-source --ignore-not-found

echo ">>> Deploying destination job on $DEST_NODE..."
export NODE_NAME="$DEST_NODE"
export QMP_SOCKET="$QMP_DEST"
export IMAGE="$IMAGE"
export EXTRA_ARGS="$DEST_EXTRA_ARGS"

envsubst '$NODE_NAME $QMP_SOCKET $IMAGE $EXTRA_ARGS' < "${SCRIPT_DIR}/job-dest.yaml" | $KUBECTL apply -f -

echo ">>> Waiting for destination pod to be ready..."
$KUBECTL -n kube-system wait --for=condition=Ready pod -l job-name=katamaran-dest --timeout=60s

sleep 3

echo ">>> Deploying source job on $SOURCE_NODE..."
export NODE_NAME="$SOURCE_NODE"
export QMP_SOCKET="$QMP_SOURCE"
export IMAGE="$IMAGE"
export DEST_IP="$DEST_IP"
export VM_IP="$VM_IP"
export EXTRA_ARGS="$SRC_EXTRA_ARGS"

envsubst '$NODE_NAME $QMP_SOCKET $IMAGE $DEST_IP $VM_IP $EXTRA_ARGS' < "${SCRIPT_DIR}/job-source.yaml" | $KUBECTL apply -f -

echo ">>> Waiting for migration to complete..."
$KUBECTL -n kube-system wait --for=condition=complete job/katamaran-source --timeout=600s

echo ""
echo "=== DESTINATION LOGS ==="
$KUBECTL -n kube-system logs job/katamaran-dest || true

echo ""
echo "=== SOURCE LOGS ==="
$KUBECTL -n kube-system logs job/katamaran-source || true

echo ""
echo ">>> Migration completed successfully!"
