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
#     [--log-level debug|info|warn|error] \
#     [--log-format text|json] \
#     [--context <kubectl-context>]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Default values
SHARED_STORAGE=false
TUNNEL_MODE="ipip"
DOWNTIME="25"
AUTO_DOWNTIME=false
MULTIFD_CHANNELS="4"
DOWNTIME_SET=false
KUBECTL_CONTEXT=""
LOG_LEVEL=""
LOG_FORMAT=""
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
POD_NAME=""
POD_NAMESPACE=""
export KATAMARAN_MIGRATION_ID="${KATAMARAN_MIGRATION_ID:-}"

usage() {
    local code="${1:-1}"
    local fd=2
    [[ "$code" -eq 0 ]] && fd=1
    {
        echo "Usage: $0 [options]"
        echo ""
        echo "Required flags:"
        echo "  --source-node <name>    Name of the source K8s node"
        echo "  --dest-node <name>      Name of the destination K8s node"
        echo "  --tap <iface>           Destination tap interface for zero-drop buffering (use 'none' to skip)"
        echo "  --qmp-source <path>     Path to QMP socket on source node"
        echo "  --qmp-dest <path>       Path to QMP socket on destination node"
        echo "  --dest-ip <ip>          IP address of the destination (must be reachable from source QEMU)"
        echo "  --vm-ip <ip>            IP address of the VM (pod IP)"
        echo "  --image <image>         Katamaran container image to use"
        echo ""
        echo "Optional flags:"
        echo "  --tap-netns <path>      Network namespace path for tap interface (e.g. /proc/PID/ns/net)"
        echo "  --shared-storage        Enable shared storage mode"
        echo "  --tunnel-mode <mode>    Tunnel encapsulation: ipip, gre, or none (default: ipip)"
        echo "  --downtime <ms>         Max allowed downtime in milliseconds, 1-60000 (default: 25)"
        echo "  --auto-downtime         Auto-calculate downtime based on RTT (overrides --downtime)"
        echo "  --multifd-channels <n>  Parallel TCP channels for RAM migration (default: 4, 0 to disable)"
        echo "  --log-level <level>     Log level for katamaran: debug, info, warn, error"
        echo "  --log-format <fmt>      Log output format for katamaran: text or json"
        echo "  --context <context>     Kubectl context to use"
        echo ""
        echo "Other:"
        echo "  --help, -h              Show this help message"
        echo ""
        echo "Environment variables:"
        echo "  KATAMARAN_KEEP_JOBS=true   Keep migration jobs after completion (skip cleanup)"
        echo ""
        echo "Example:"
        echo "  $0 \\"
        echo "    --source-node node1 --dest-node node2 \\"
        echo "    --tap tap0_kata --qmp-source /run/vc/vm/ID/extra-monitor.sock \\"
        echo "    --qmp-dest /run/vc/vm/ID/extra-monitor.sock \\"
        echo "    --dest-ip 10.0.0.2 --vm-ip 10.244.1.5 \\"
        echo "    --image localhost/katamaran:dev"
    } >&"$fd"
    exit "$code"
}

need_arg() {
    if [[ $# -lt 2 || -z "$2" ]]; then
        echo "Error: $1 requires a value" >&2
        usage 2
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --source-node) need_arg "$1" "${2:-}"; SOURCE_NODE="$2"; shift 2 ;;
        --dest-node) need_arg "$1" "${2:-}"; DEST_NODE="$2"; shift 2 ;;
        --tap) need_arg "$1" "${2:-}"; TAP_IFACE="$2"; shift 2 ;;
        --tap-netns) need_arg "$1" "${2:-}"; TAP_NETNS="$2"; shift 2 ;;
        --qmp-source) need_arg "$1" "${2:-}"; QMP_SOURCE="$2"; shift 2 ;;
        --qmp-dest) need_arg "$1" "${2:-}"; QMP_DEST="$2"; shift 2 ;;
        --dest-ip) need_arg "$1" "${2:-}"; DEST_IP="$2"; shift 2 ;;
        --vm-ip) need_arg "$1" "${2:-}"; VM_IP="$2"; shift 2 ;;
        --image) need_arg "$1" "${2:-}"; IMAGE_REF="$2"; shift 2 ;;
        --pod-name) need_arg "$1" "${2:-}"; POD_NAME="$2"; shift 2 ;;
        --pod-namespace) need_arg "$1" "${2:-}"; POD_NAMESPACE="$2"; shift 2 ;;
        --shared-storage) SHARED_STORAGE=true; shift ;;
        --auto-downtime) AUTO_DOWNTIME=true; shift ;;
        --tunnel-mode) need_arg "$1" "${2:-}"; TUNNEL_MODE="$2"; shift 2 ;;
        --downtime) need_arg "$1" "${2:-}"; DOWNTIME="$2"; DOWNTIME_SET=true; shift 2 ;;
        --multifd-channels) need_arg "$1" "${2:-}"; MULTIFD_CHANNELS="$2"; shift 2 ;;
        --log-level) need_arg "$1" "${2:-}"; LOG_LEVEL="$2"; shift 2 ;;
        --log-format) need_arg "$1" "${2:-}"; LOG_FORMAT="$2"; shift 2 ;;
        --context) need_arg "$1" "${2:-}"; KUBECTL_CONTEXT="$2"; shift 2 ;;
        --help|-h) usage 0 ;;
        *) echo "Error: unknown option: $1" >&2; usage 2 ;;
    esac
done

# In pod mode, default --qmp-dest to a well-known path on the destination node;
# the dest container will create the parent directory for the QEMU socket.
if [[ -z "$QMP_DEST" && -n "$POD_NAME" ]]; then
    QMP_DEST="/run/vc/vm/katamaran-dest/qmp.sock"
fi

missing_args=()
[[ -z "$SOURCE_NODE" ]] && missing_args+=(--source-node)
[[ -z "$DEST_NODE" ]] && missing_args+=(--dest-node)
[[ -z "$QMP_DEST" ]] && missing_args+=(--qmp-dest)
[[ -z "$DEST_IP" ]] && missing_args+=(--dest-ip)
[[ -z "$IMAGE_REF" ]] && missing_args+=(--image)
if [[ -z "$POD_NAME" ]]; then
    # Legacy mode: explicit source values are required.
    [[ -z "$TAP_IFACE" ]] && missing_args+=(--tap)
    [[ -z "$QMP_SOURCE" ]] && missing_args+=(--qmp-source)
    [[ -z "$VM_IP" ]] && missing_args+=(--vm-ip)
fi
if [[ ${#missing_args[@]} -gt 0 ]]; then
    echo "Error: missing required flag(s): ${missing_args[*]}" >&2
    usage 2
fi

if [[ "$TAP_IFACE" == "none" ]]; then
    TAP_IFACE=""
fi

# Normalize enum flags for case-insensitive matching (aligned with katamaran binary).
TUNNEL_MODE="${TUNNEL_MODE,,}"
LOG_LEVEL="${LOG_LEVEL,,}"
LOG_FORMAT="${LOG_FORMAT,,}"

if [[ "$TUNNEL_MODE" != "ipip" && "$TUNNEL_MODE" != "gre" && "$TUNNEL_MODE" != "none" ]]; then
    echo "Error: invalid --tunnel-mode '$TUNNEL_MODE' (valid: ipip, gre, none)" >&2
    exit 1
fi

if [[ "$TAP_IFACE" == *[[:space:]]* ]]; then
    echo "Error: --tap must be a single interface name without spaces" >&2
    exit 1
fi

if [[ ! "$DOWNTIME" =~ ^[1-9][0-9]*$ ]]; then
    echo "Error: --downtime must be a positive integer, got '$DOWNTIME'" >&2
    exit 1
fi

if [[ "$DOWNTIME" -gt 60000 ]]; then
    echo "Error: --downtime must be between 1 and 60000, got '$DOWNTIME'" >&2
    exit 1
fi

if [[ ! "$MULTIFD_CHANNELS" =~ ^[0-9]+$ ]]; then
    echo "Error: --multifd-channels must be a non-negative integer, got '$MULTIFD_CHANNELS'" >&2
    exit 1
fi

if [[ -n "$LOG_LEVEL" && "$LOG_LEVEL" != "debug" && "$LOG_LEVEL" != "info" && "$LOG_LEVEL" != "warn" && "$LOG_LEVEL" != "error" ]]; then
    echo "Error: invalid --log-level '$LOG_LEVEL' (valid: debug, info, warn, error)" >&2
    exit 1
fi

if [[ -n "$LOG_FORMAT" && "$LOG_FORMAT" != "text" && "$LOG_FORMAT" != "json" ]]; then
    echo "Error: invalid --log-format '$LOG_FORMAT' (valid: text, json)" >&2
    exit 1
fi

# Reject shell metacharacters in values that will be interpolated into
# job YAML via envsubst → /bin/sh -c.  Defence-in-depth: the dashboard
# already validates these, but migrate.sh can also be called directly.
shell_safe_re='^[a-zA-Z0-9_./:@=-]+$'
for var_name in SOURCE_NODE DEST_NODE TAP_IFACE TAP_NETNS QMP_SOURCE QMP_DEST DEST_IP VM_IP IMAGE_REF KUBECTL_CONTEXT POD_NAME POD_NAMESPACE; do
    val="${!var_name}"
    if [[ -n "$val" && ! "$val" =~ $shell_safe_re ]]; then
        flag_name="${var_name,,}"
        flag_name="${flag_name//_/-}"
        echo "Error: --${flag_name} contains invalid characters" >&2
        exit 1
    fi
done

for cmd in kubectl envsubst; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "Error: required command not found: $cmd" >&2
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
if [[ -n "$LOG_LEVEL" ]]; then
    DEST_EXTRA_ARGS="$DEST_EXTRA_ARGS --log-level $LOG_LEVEL"
fi
if [[ -n "$LOG_FORMAT" ]]; then
    DEST_EXTRA_ARGS="$DEST_EXTRA_ARGS --log-format $LOG_FORMAT"
fi

SRC_EXTRA_ARGS="$DEST_EXTRA_ARGS --tunnel-mode $TUNNEL_MODE"
if [[ "$DOWNTIME_SET" == "true" ]]; then
    SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --downtime $DOWNTIME"
fi
if [[ "$AUTO_DOWNTIME" == "true" ]]; then
    SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --auto-downtime"
fi
if [[ -n "$POD_NAME" ]]; then
    # Pod mode: source job's resolver derives qmp/vm-ip/tap from the pod spec.
    SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --pod-name $POD_NAME --pod-namespace $POD_NAMESPACE"
else
    # Legacy mode: pass explicit qmp/vm-ip/tap through EXTRA_ARGS so the source
    # container command (which no longer hardcodes them) receives them.
    SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --qmp $QMP_SOURCE --vm-ip $VM_IP"
    if [[ -n "$TAP_IFACE" ]]; then
        SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --tap $TAP_IFACE"
        if [[ -n "$TAP_NETNS" ]]; then
            SRC_EXTRA_ARGS="$SRC_EXTRA_ARGS --tap-netns $TAP_NETNS"
        fi
    fi
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

envsubst '$NODE_NAME $QMP_SOCKET $IMAGE $EXTRA_ARGS $KATAMARAN_MIGRATION_ID' < "${SCRIPT_DIR}/job-dest.yaml" | "${KUBECTL[@]}" apply -f -

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
    echo "Error: destination did not reach ready state in time." >&2
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

envsubst '$NODE_NAME $QMP_SOCKET $IMAGE $DEST_IP $VM_IP $EXTRA_ARGS $KATAMARAN_MIGRATION_ID' < "${SCRIPT_DIR}/job-source.yaml" | "${KUBECTL[@]}" apply -f -

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
    echo "Error: source migration job did not complete successfully." >&2
    exit "$wait_rc"
fi

MIG_SUCCESS=true

echo ""
echo ">>> Migration completed successfully!"
