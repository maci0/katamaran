#!/bin/bash
# scripts/sweep.sh — Parameter sweep tool for katamaran downtime limit
#
# Usage:
#   ./scripts/sweep.sh [--provider <name>] [--help] <downtime1> [downtime2 ... | auto]
#   Example: ./scripts/sweep.sh 10 25 50 auto
#   Example: ./scripts/sweep.sh --provider kind 10 25 auto

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
export PATH="${PROJECT_ROOT}/bin:${PATH}"
source "${SCRIPT_DIR}/lib.sh"

PROVIDER="minikube"

# Parse flags (before positional downtime args).
while [[ $# -gt 0 ]]; do
    case "$1" in
        --provider) PROVIDER="$2"; shift 2 ;;
        --help)
            echo "Usage: $0 [--provider minikube|kind] <downtime1> [downtime2 ... | auto]"
            echo "Example: $0 10 25 50 auto"
            echo "Example: $0 --provider kind 10 25 auto"
            echo ""
            echo "Use 'auto' to test RTT-based auto-downtime calculation."
            exit 0
            ;;
        --*) echo "Error: unknown option: $1" >&2; exit 1 ;;
        *) break ;;  # First non-flag arg starts the downtime list.
    esac
done

if [[ $# -eq 0 ]]; then
    echo "Error: at least one downtime value is required." >&2
    echo "Usage: $0 [--provider minikube|kind] <downtime1> [downtime2 ... | auto]" >&2
    exit 1
fi

if [[ "${PROVIDER}" != "minikube" && "${PROVIDER}" != "kind" ]]; then
    error "--provider must be 'minikube' or 'kind' (got '${PROVIDER}')"
    exit 1
fi

DOWNTIMES=("$@")

# Results directory for raw logs and TSV output
RESULTS_DIR="${PROJECT_ROOT}/sweep-results"
mkdir -p "$RESULTS_DIR"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
TSV_FILE="${RESULTS_DIR}/sweep-${TIMESTAMP}.tsv"

echo "============================================================"
echo " Katamaran Parameter Sweep: Downtime Limits"
echo " Values to test: ${DOWNTIMES[*]}"
echo " Results dir: ${RESULTS_DIR}"
echo "============================================================"

# First, run e2e.sh --env-only to set up the environment
log "Setting up environment..."
"${SCRIPT_DIR}/e2e.sh" --provider "${PROVIDER}" --env-only

# Set up provider-aware variables for node_exec (from lib.sh) and kubectl.
if [[ "${PROVIDER}" == "kind" ]]; then
    SUDO=""
    CE="${CE:-$(command -v podman 2>/dev/null || echo docker)}"
else
    SUDO="sudo"
    CE=""
fi
# Derive profile and kubectl context to match e2e.sh naming convention.
# e2e.sh defaults to kindnet for Kind and calico for minikube.
if [[ "${PROVIDER}" == "kind" ]]; then
    CNI_DEFAULT="kindnet"
else
    CNI_DEFAULT="calico"
fi
PROFILE="katamaran-e2e-${PROVIDER}-${CNI_DEFAULT}-none-job"
if [[ "${PROVIDER}" == "kind" ]]; then
    CTX="kind-${PROFILE}"
else
    CTX="${PROFILE}"
fi
KUBECTL=(kubectl --context "${CTX}")

get_pod_ip() {
    local pod="$1"
    "${KUBECTL[@]}" get pod "$pod" -o jsonpath='{.status.podIP}'
}

get_pod_node() {
    local pod="$1"
    "${KUBECTL[@]}" get pod "$pod" -o jsonpath='{.spec.nodeName}'
}

get_node_ip() {
    local node="$1"
    "${KUBECTL[@]}" get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'
}

get_qmp_socket() {
    local pod="$1"
    local node="$2"
    local pod_uid=$("${KUBECTL[@]}" get pod "$pod" -o jsonpath='{.metadata.uid}')
    node_exec "${node}" "${SUDO} crictl inspect -o go-template --template '{{.info.runtimeSpec.annotations.io\\.katacontainers\\.config\\.hypervisor\\.monitor_path}}' \$(${SUDO} crictl ps --label io.kubernetes.pod.uid=${pod_uid} -q) 2>/dev/null"
}

# Extract actual migration metrics from source job logs.
# Looks for the log line: "Migration completed: actual_downtime=Xms total_time=Yms setup_time=Zms"
extract_metrics() {
    local logfile="$1"
    local metric_line
    metric_line=$(grep -o 'actual_downtime=[0-9]*ms total_time=[0-9]*ms setup_time=[0-9]*ms' "$logfile" 2>/dev/null || true)
    if [[ -n "$metric_line" ]]; then
        local actual_dt total_t setup_t
        actual_dt=$(echo "$metric_line" | grep -o 'actual_downtime=[0-9]*' | cut -d= -f2)
        total_t=$(echo "$metric_line" | grep -o 'total_time=[0-9]*' | cut -d= -f2)
        setup_t=$(echo "$metric_line" | grep -o 'setup_time=[0-9]*' | cut -d= -f2)
        echo "${actual_dt}	${total_t}	${setup_t}"
    else
        echo "-	-	-"
    fi
}

# Write TSV header
echo -e "downtime_limit_ms\tactual_downtime_ms\ttotal_time_ms\tsetup_time_ms\tresult" > "$TSV_FILE"

log "Environment ready. Starting sweep..."

for DOWNTIME in "${DOWNTIMES[@]}"; do
    # Determine if this is an auto-downtime run
    IS_AUTO=false
    if [[ "$DOWNTIME" == "auto" ]]; then
        IS_AUTO=true
        echo "============================================================"
        echo " RUNNING MIGRATION WITH AUTO-DOWNTIME"
        echo "============================================================"
    else
        echo "============================================================"
        echo " RUNNING MIGRATION WITH DOWNTIME: ${DOWNTIME}ms"
        echo "============================================================"
    fi

    RUN_LOGDIR="${RESULTS_DIR}/${TIMESTAMP}-${DOWNTIME}"
    mkdir -p "$RUN_LOGDIR"

    # 1. Clean up from previous run if necessary
    log "Cleaning up previous pods..."
    "${KUBECTL[@]}" delete pod kata-src mig-helper --ignore-not-found --wait=true
    "${KUBECTL[@]}" -n kube-system delete job katamaran-dest katamaran-source --ignore-not-found --wait=true

    # 2. Start the source pod
    log "Deploying source Kata pod..."
    "${KUBECTL[@]}" apply -f "${SCRIPT_DIR}/manifests/pod-src.yaml"
    "${KUBECTL[@]}" wait --for=condition=Ready pod/kata-src --timeout=120s

    SRC_IP=$(get_pod_ip kata-src)
    SRC_NODE=$(get_pod_node kata-src)

    # 3. Start the destination helper pod
    log "Deploying destination helper pod..."
    "${KUBECTL[@]}" apply -f "${SCRIPT_DIR}/manifests/pod-dest.yaml"
    "${KUBECTL[@]}" wait --for=condition=Ready pod/mig-helper --timeout=120s

    DEST_NODE=$(get_pod_node mig-helper)
    DEST_NODE_IP=$(get_node_ip "$DEST_NODE")

    if [[ "$SRC_NODE" == "$DEST_NODE" ]]; then
        error "Source and destination are on the same node ($SRC_NODE). Migration requires different nodes."
        echo -e "${DOWNTIME}\t-\t-\t-\tERROR (same node)" >> "$TSV_FILE"
        continue
    fi

    # 4. Get QMP socket of source
    SRC_QMP=$(get_qmp_socket "kata-src" "$SRC_NODE")
    if [[ -z "$SRC_QMP" ]]; then
        error "Could not find QMP socket for kata-src"
        echo -e "${DOWNTIME}\t-\t-\t-\tERROR (no SRC_QMP)" >> "$TSV_FILE"
        continue
    fi

    echo "Source Node: $SRC_NODE, IP: $SRC_IP, QMP: $SRC_QMP"

    # 5. Start destination QEMU manually via mig-helper
    log "Starting destination QEMU..."

    node_exec "$DEST_NODE" "${SUDO} bash -c 'nohup /opt/kata/bin/qemu-system-x86_64 -name sandbox-mig-helper -machine q35,accel=kvm,kernel_irqchip=split -m 2048 -smp 2 -cpu host -no-user-config -nodefaults -nographic -vga none -daemonize -incoming defer -monitor none -qmp unix:/tmp/qmp-dest.sock,server=on,wait=off -object memory-backend-file,id=dimm1,size=2048M,mem-path=/dev/shm,share=on -numa node,memdev=dimm1 -pidfile /tmp/qemu-dest.pid >/tmp/qemu.log 2>&1 &'"
    DEST_QMP="/tmp/qmp-dest.sock"

    # Wait a bit for QEMU to start
    sleep 2

    # 6. Run the migration
    MIGRATE_ARGS=(
        --source-node "$SRC_NODE"
        --dest-node "$DEST_NODE"
        --qmp-source "$SRC_QMP"
        --qmp-dest "$DEST_QMP"
        --dest-ip "$DEST_NODE_IP"
        --vm-ip "$SRC_IP"
        --image "localhost/katamaran:latest"
        --tap "none"
    )

    if [[ "$IS_AUTO" == "true" ]]; then
        log "Running katamaran migration job with auto-downtime..."
        MIGRATE_ARGS+=(--auto-downtime)
    else
        log "Running katamaran migration job with downtime ${DOWNTIME}ms..."
        MIGRATE_ARGS+=(--downtime "$DOWNTIME")
    fi

    set +e
    "${PROJECT_ROOT}/deploy/migrate.sh" "${MIGRATE_ARGS[@]}" 2>&1 | tee "${RUN_LOGDIR}/migrate.log"
    MIG_EXIT=${PIPESTATUS[0]}
    set -e

    # Save individual job logs
    "${KUBECTL[@]}" -n kube-system logs job/katamaran-source > "${RUN_LOGDIR}/source.log" 2>/dev/null || true
    "${KUBECTL[@]}" -n kube-system logs job/katamaran-dest > "${RUN_LOGDIR}/dest.log" 2>/dev/null || true

    # Extract metrics from source logs
    METRICS=$(extract_metrics "${RUN_LOGDIR}/source.log")
    ACTUAL_DT=$(echo "$METRICS" | cut -f1)
    TOTAL_T=$(echo "$METRICS" | cut -f2)
    SETUP_T=$(echo "$METRICS" | cut -f3)

    if [[ $MIG_EXIT -eq 0 ]]; then
        RESULT="SUCCESS"
        success "Migration with downtime ${DOWNTIME} (actual_downtime=${ACTUAL_DT}ms total_time=${TOTAL_T}ms)"
    else
        RESULT="FAILED"
        error "Migration with downtime ${DOWNTIME} FAILED (exit $MIG_EXIT)"
    fi

    echo -e "${DOWNTIME}\t${ACTUAL_DT}\t${TOTAL_T}\t${SETUP_T}\t${RESULT}" >> "$TSV_FILE"

    # 7. Clean up QEMU on dest
    node_exec "$DEST_NODE" "${SUDO} kill -9 \$(cat /tmp/qemu-dest.pid) 2>/dev/null || true; ${SUDO} rm -f /tmp/qmp-dest.sock /tmp/qemu-dest.pid /tmp/qemu.log"
done

echo ""
echo "============================================================"
echo " SWEEP RESULTS (TSV: ${TSV_FILE})"
echo "============================================================"
# Print TSV with column alignment
column -t -s $'\t' < "$TSV_FILE"
echo "============================================================"
echo "Raw logs saved to: ${RESULTS_DIR}/"
