#!/bin/bash
# e2e.sh — Unified E2E test harness for Katamaran live migration.
#
# Usage:
#   ./scripts/e2e.sh [options]
#   ./scripts/e2e.sh teardown [--provider minikube|kind]
#
# Options:
#   --provider <name>   minikube or kind (default: minikube)
#   --cni <name>        calico, ovn, or none (default: calico for minikube, none for kind)
#   --storage <type>    none or nfs (default: none)
#   --method <type>     ssh or job (default: ssh)
#   --ping-proof        Run continuous ping to prove zero packet loss
#   --env-only          Stop after environment setup
#   --help              Show help message

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

# Defaults
PROVIDER="minikube"
CNI="auto"
STORAGE="none"
METHOD="ssh"
PING_PROOF=false
ENV_ONLY=false
TEARDOWN=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        teardown) TEARDOWN=true; shift ;;
        --provider) PROVIDER="$2"; shift 2 ;;
        --cni) CNI="$2"; shift 2 ;;
        --storage) STORAGE="$2"; shift 2 ;;
        --method) METHOD="$2"; shift 2 ;;
        --ping-proof) PING_PROOF=true; shift ;;
        --env-only) ENV_ONLY=true; shift ;;
        --help)
            grep "^# " "$0" | sed 's/^#//'
            exit 0
            ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

if [[ "${CNI}" == "auto" ]]; then
    if [[ "${PROVIDER}" == "minikube" ]]; then
        CNI="calico"
    else
        CNI="none" # Kind uses kindnet by default
    fi
fi

# Determine cluster name based on parameters
PROFILE="katamaran-e2e-${PROVIDER}-${CNI}-${STORAGE}-${METHOD}"

log() { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
warn() { echo -e "\033[1;33m  WARN: $1\033[0m"; }
error() { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

# Helper functions
node_exec() {
    local node="$1"
    shift
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube -p "${PROFILE}" ssh -n "$node" -- "$*"
    else
        podman exec "$node" bash -c "$*"
    fi
}

cleanup() {
    # Kill background ping if still running
    if [[ -n "${PING_PID:-}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
        kill "${PING_PID}" 2>/dev/null || true
        wait "${PING_PID}" 2>/dev/null || true
    fi
    if [[ "${ENV_ONLY}" == "true" ]]; then
        log "--env-only set, keeping cluster '${PROFILE}'"
        return
    fi
    log "Cleaning up cluster '${PROFILE}'..."
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube delete -p "${PROFILE}" 2>/dev/null || true
    else
        kind delete cluster --name "${PROFILE}" 2>/dev/null || true
    fi
}

if [[ "${TEARDOWN}" == "true" ]]; then
    ENV_ONLY=false
    cleanup
    exit 0
fi

trap cleanup EXIT

# ─── 1. Pre-flight ──────────────────────────────────────────────────────────
log "Checking prerequisites..."
REQS="kubectl helm podman"
if [[ "${PROVIDER}" == "minikube" ]]; then REQS="${REQS} minikube"; else REQS="${REQS} kind"; fi
if [[ "${CNI}" == "ovn" ]]; then REQS="${REQS} git"; fi

for cmd in $REQS; do
    if ! command -v "$cmd" >/dev/null; then
        error "$cmd is required."
        exit 1
    fi
done

if [[ "${PROVIDER}" == "kind" ]] && [[ ! -c /dev/kvm ]]; then
    error "/dev/kvm not found. KVM is required for Kata Containers."
    exit 1
fi

# ─── 2. Start Cluster ───────────────────────────────────────────────────────
log "Starting 2-node ${PROVIDER} cluster (CNI: ${CNI})..."

if [[ "${PROVIDER}" == "minikube" ]]; then
    MINIKUBE_ARGS=("--nodes" "2" "--driver=kvm2" "--memory=8192" "--cpus=4" "--container-runtime=containerd")
    if [[ "${CNI}" == "ovn" ]]; then
        MINIKUBE_ARGS+=("--network-plugin=cni" "--cni=false")
    else
        MINIKUBE_ARGS+=("--cni=${CNI}")
    fi
    minikube start -p "${PROFILE}" "${MINIKUBE_ARGS[@]}"

    NODE1="${PROFILE}"
    NODE2="${PROFILE}-m02"
    CTX="${PROFILE}"
else
    # Kind
    KIND_CONFIG="${SCRIPT_DIR}/manifests/kind-config.yaml"
    KIND_EXPERIMENTAL_PROVIDER=podman kind create cluster --name "${PROFILE}" --config "${KIND_CONFIG}" --wait 120s
    NODE1="${PROFILE}-control-plane"
    NODE2="${PROFILE}-worker"
    CTX="kind-${PROFILE}"
fi

# Wait for nodes
kubectl --context "${CTX}" wait --for=condition=Ready node --all --timeout=120s

# Get IPs
NODE1_IP=$(kubectl --context "${CTX}" get node "${NODE1}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "${CTX}" get node "${NODE2}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
success "Node 1 (${NODE1}): ${NODE1_IP}"
success "Node 2 (${NODE2}): ${NODE2_IP}"

# ─── 3. CNI Specifics (OVN) ─────────────────────────────────────────────────
if [[ "${CNI}" == "ovn" ]]; then
    log "Deploying OVN-Kubernetes..."
    # OVS
    for node in "${NODE1}" "${NODE2}"; do
        node_exec "$node" "sudo modprobe openvswitch 2>/dev/null || true"
    done
    API_SERVER=$(kubectl --context "${CTX}" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    API_HOST=$(echo "${API_SERVER}" | sed -E 's|https?://([^:]+):.*|\1|')
    API_PORT=$(echo "${API_SERVER}" | sed -E 's|.*:([0-9]+)$|\1|')

    OVN_K_DIR="/tmp/ovn-kubernetes-${PROFILE}"
    rm -rf "${OVN_K_DIR}"
    git clone --depth 1 --branch "master" "https://github.com/ovn-org/ovn-kubernetes.git" "${OVN_K_DIR}"
    helm upgrade --install ovn-kubernetes "${OVN_K_DIR}/helm/ovn-kubernetes" \
        --kube-context "${CTX}" --namespace ovn-kubernetes --create-namespace \
        --set global.k8sServiceHost="${API_HOST}" --set global.k8sServicePort="${API_PORT}" \
        --set global.podNetwork="10.244.0.0/16/24" --set global.serviceNetwork="10.96.0.0/16" \
        --set global.enableMulticast=true \
        --set tags.ovnkube-db=true --set tags.ovnkube-master=true --set tags.ovnkube-node=true --wait=false

    log "Waiting for OVN-Kubernetes..."
    kubectl --context "${CTX}" -n ovn-kubernetes rollout status daemonset/ovnkube-node --timeout=600s 2>/dev/null || true
    kubectl --context "${CTX}" wait --for=condition=Ready node --all --timeout=300s
fi

# ─── 4. Install Kata Containers ─────────────────────────────────────────────
log "Installing Kata Containers via Helm..."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --version "${KATA_CHART_VERSION}" \
    --kube-context "${CTX}" \
    --namespace kube-system \
    --create-namespace \
    --set shims.disableAll=true \
    --set shims.qemu.enabled=true \
    --wait=false

kubectl --context "${CTX}" -n kube-system rollout status daemonset/kata-deploy --timeout=600s
while ! kubectl --context "${CTX}" get runtimeclass kata-qemu >/dev/null 2>&1; do sleep 5; done

# ─── 5. Configure QMP & Modules ─────────────────────────────────────────────
log "Enabling extra QMP monitor socket and loading modules..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
for node in "${NODE1}" "${NODE2}"; do
    while ! node_exec "$node" "test -f ${KATA_CFG}" 2>/dev/null; do sleep 2; done
    node_exec "$node" "
        sudo sed -i '/^\[hypervisor\.qemu\]/,/^\[/{
            s/^enable_debug = false/enable_debug = true/
            s/^extra_monitor_socket = \"\"/extra_monitor_socket = \"qmp\"/
        }' '${KATA_CFG}'
        sudo sed -i 's/^cdh_api_timeout = .*/cdh_api_timeout = 180/' '${KATA_CFG}'
        sudo sed -i 's/^dial_timeout = .*/dial_timeout = 180/' '${KATA_CFG}'
        sudo sed -i 's/^create_container_timeout = .*/create_container_timeout = 180/' '${KATA_CFG}'
        sudo modprobe ipip 2>/dev/null || true
        sudo modprobe ip6_tunnel 2>/dev/null || true
        sudo modprobe ip_gre 2>/dev/null || true
        sudo modprobe sch_plug 2>/dev/null || true
    "
    if [[ "${STORAGE}" == "nfs" ]]; then
        node_exec "$node" "sudo modprobe nfs 2>/dev/null || true; sudo modprobe nfsd 2>/dev/null || true"
    fi
done

# ─── 6. Build & Deploy Katamaran ────────────────────────────────────────────
log "Building and deploying katamaran container image..."
podman build -t localhost/katamaran:dev "${PROJECT_ROOT}"
if [[ "${PROVIDER}" == "minikube" ]]; then
    IMAGE_ARCHIVE="$(mktemp /tmp/katamaran-image-XXXXXX.tar)"
    podman save localhost/katamaran:dev -o "${IMAGE_ARCHIVE}"
    minikube -p "${PROFILE}" image load "${IMAGE_ARCHIVE}"
    rm -f "${IMAGE_ARCHIVE}"
else
    kind load docker-image localhost/katamaran:dev --name "${PROFILE}"
fi
kubectl --context "${CTX}" apply -f "${PROJECT_ROOT}/deploy/daemonset.yaml"
kubectl --context "${CTX}" -n kube-system rollout status daemonset/katamaran-deploy --timeout=120s

# ─── 7. NFS Storage ─────────────────────────────────────────────────────────
if [[ "${STORAGE}" == "nfs" ]]; then
    log "Deploying NFS server and PV..."
    export NODE_NAME="${NODE1}"
    export NFS_EXPORT_PATH="/exports/kata-data"
    envsubst '${NODE_NAME} ${NFS_EXPORT_PATH}' < "${SCRIPT_DIR}/manifests/nfs-server.yaml" | kubectl --context "${CTX}" apply -f -
    kubectl --context "${CTX}" wait --for=condition=Ready pod/nfs-server --timeout=120s
    export NFS_SERVER_IP=$(kubectl --context "${CTX}" get pod nfs-server -o jsonpath='{.status.podIP}')
    envsubst '${NFS_SERVER_IP} ${NFS_EXPORT_PATH}' < "${SCRIPT_DIR}/manifests/nfs-pv.yaml" | kubectl --context "${CTX}" apply -f -
    # Wait for PVC
    for i in $(seq 1 30); do
        PVC_STATUS=$(kubectl --context "${CTX}" get pvc nfs-pvc -o jsonpath='{.status.phase}' 2>/dev/null || true)
        if [[ "${PVC_STATUS}" == "Bound" ]]; then break; fi
        sleep 2
    done
fi

if [[ "${ENV_ONLY}" == "true" ]]; then
    success "Environment setup complete (--env-only)."
    exit 0
fi

kubectl --context "${CTX}" delete pod kata-src kata-dst --force --grace-period=0 2>/dev/null || true

# ─── 8. Deploy Source Pod ───────────────────────────────────────────────────
log "Deploying source pod on Node 1..."
export POD_NAME="kata-src"
export NODE_NAME="${NODE1}"
if [[ "${CNI}" == "calico" && "${STORAGE}" != "nfs" ]]; then
    export HOST_NETWORK="hostNetwork: true"
else
    export HOST_NETWORK=""
fi
export VOLUME_MOUNTS_SECTION=""
export VOLUMES_SECTION=""
if [[ "${STORAGE}" == "nfs" ]]; then
    export VOLUME_MOUNTS_SECTION="    volumeMounts:\n    - name: shared-data\n      mountPath: /mnt/shared"
    export VOLUMES_SECTION="  volumes:\n  - name: shared-data\n    persistentVolumeClaim:\n      claimName: nfs-pvc"
fi
envsubst '${POD_NAME} ${NODE_NAME} ${HOST_NETWORK} ${VOLUME_MOUNTS_SECTION} ${VOLUMES_SECTION}' < "${SCRIPT_DIR}/manifests/kata-pod.yaml" | echo -e "$(cat)" | kubectl --context "${CTX}" apply -f -

kubectl --context "${CTX}" wait --for=condition=Ready pod/kata-src --timeout=300s
POD_IP=$(kubectl --context "${CTX}" get pod kata-src -o jsonpath='{.status.podIP}')
success "Source pod IP: ${POD_IP}"

if [[ "${STORAGE}" == "nfs" ]]; then
    kubectl --context "${CTX}" exec kata-src -- sh -c "echo 'katamaran-nfs-test' > /mnt/shared/proof.txt"
fi

log "Extracting QEMU state from Node 1..."
QEMU_CMD=$(node_exec "${NODE1}" 'sudo cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
export SRC_UUID=$(echo "$QEMU_CMD" | sed -n -E 's/.*-uuid ([a-f0-9\-]+).*/\1/p' | awk '{print $1}')
export SRC_VSOCK=$(echo "$QEMU_CMD" | grep -oE "id=vsock-[0-9]+" | awk -F= '{print $2}')
export SRC_CID=$(echo "$QEMU_CMD" | grep -oE "guest-cid=[0-9]+" | awk -F= '{print $2}')
export SRC_CHAR=$(echo "$QEMU_CMD" | grep -oE "id=char-[a-f0-9]+" | head -1 | awk -F= '{print $2}')
export NODE2_IP="${NODE2_IP}"

# ─── 9. Deploy Destination Pod & Wrapper ────────────────────────────────────
log "Installing state-matching QEMU wrapper on Node 2..."
envsubst '${SRC_UUID} ${SRC_VSOCK} ${SRC_CID} ${SRC_CHAR} ${NODE2_IP}' < "${SCRIPT_DIR}/manifests/qemu-wrapper.sh" > /tmp/wrapper.sh
if [[ "${PROVIDER}" == "minikube" ]]; then
    minikube -p "${PROFILE}" cp /tmp/wrapper.sh "${NODE2}:/tmp/wrapper.sh"
else
    podman cp /tmp/wrapper.sh "${NODE2}:/tmp/wrapper.sh"
fi
node_exec "${NODE2}" "
    sudo mv /opt/kata/bin/qemu-system-x86_64 /opt/kata/bin/qemu-system-x86_64.orig 2>/dev/null || true
    sudo cp /tmp/wrapper.sh /opt/kata/bin/qemu-system-x86_64
    sudo chmod +x /opt/kata/bin/qemu-system-x86_64
"

log "Deploying destination pod on Node 2..."
export POD_NAME="kata-dst"
export NODE_NAME="${NODE2}"
envsubst '${POD_NAME} ${NODE_NAME} ${HOST_NETWORK} ${VOLUME_MOUNTS_SECTION} ${VOLUMES_SECTION}' < "${SCRIPT_DIR}/manifests/kata-pod.yaml" | echo -e "$(cat)" | kubectl --context "${CTX}" apply -f -

# The destination pod will get stuck waiting for QEMU if it's expecting migration, but we wait for QMP
log "Waiting for destination QEMU process and QMP socket to appear..."
QMP_WAIT_DEADLINE=$((SECONDS + 180))
while (( SECONDS < QMP_WAIT_DEADLINE )); do
    DST_PID=$(node_exec "${NODE2}" 'pgrep qemu | head -1' | tr -d '\r\n')
    DST_SOCK=$(node_exec "${NODE2}" 'sudo find /run/vc \( -name "extra-monitor.sock" -o -name "qmp.sock" \) 2>/dev/null | head -1' | tr -d '\r\n')
    if [[ -n "$DST_PID" && -n "$DST_SOCK" ]]; then break; fi
    sleep 2
done
if [[ -z "${DST_PID:-}" || -z "${DST_SOCK:-}" ]]; then
    error "Timed out waiting for destination QEMU/QMP."
    exit 1
fi

SRC_SOCK=$(node_exec "${NODE1}" 'sudo find /run/vc \( -name "extra-monitor.sock" -o -name "qmp.sock" \) 2>/dev/null | head -1' | tr -d '\r\n')

# ─── 10. Ping Proof ─────────────────────────────────────────────────────────
if [[ "${PING_PROOF}" == "true" ]]; then
    log "Starting continuous ping for zero-drop proof..."
    PING_LOG="/tmp/katamaran-ping.log"
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube -p "${PROFILE}" ssh -n "${NODE1}" -- "ping -i 0.05 -w 300 -D ${POD_IP}" > "${PING_LOG}" 2>&1 &
    else
        podman exec "${NODE1}" ping -i 0.05 -w 300 -D "${POD_IP}" > "${PING_LOG}" 2>&1 &
    fi
    PING_PID=$!
    sleep 2
fi

# ─── 11. Execute Migration ──────────────────────────────────────────────────
log "Executing Live Migration (${METHOD} mode)..."
if [[ "${METHOD}" == "job" ]]; then
    DST_QEMU_CMD=$(node_exec "${NODE2}" 'cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
    DST_TAP=$(echo "${DST_QEMU_CMD}" | grep -oE "ifname=[^, ]+" | head -1 | cut -d= -f2)

    set +e
    "${PROJECT_ROOT}/deploy/migrate.sh" \
        --source-node "${NODE1}" --dest-node "${NODE2}" \
        --qmp-source "${SRC_SOCK}" --qmp-dest "${DST_SOCK}" \
        --tap "${DST_TAP}" --dest-ip "${NODE2_IP}" --vm-ip "${POD_IP}" \
        --image "localhost/katamaran:dev" --shared-storage --context "${CTX}"
    MIG_STATUS=$?
    set -e
else
    # SSH method
    node_exec "${NODE2}" "
        sudo systemctl reset-failed katamaran-dest.service 2>/dev/null || true
        sudo systemctl stop katamaran-dest.service 2>/dev/null || true
        sudo systemd-run --unit=katamaran-dest.service --remain-after-exit /usr/local/bin/katamaran -mode dest -qmp '${DST_SOCK}' -shared-storage
    "
    sleep 3
    set +e
    node_exec "${NODE1}" "sudo /usr/local/bin/katamaran -mode source -qmp '${SRC_SOCK}' -dest-ip '${NODE2_IP}' -vm-ip '${POD_IP}' -shared-storage" \
        2>&1 | tee /tmp/katamaran-source.log
    MIG_STATUS=${PIPESTATUS[0]}
    set -e
fi

# ─── 12. Verification & Results ─────────────────────────────────────────────
if [[ -n "${PING_PID:-}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
    kill "${PING_PID}" 2>/dev/null || true; wait "${PING_PID}" 2>/dev/null || true
fi
PING_PID=""

echo ""
echo "=== E2E MIGRATION RESULTS ==="
if [[ $MIG_STATUS -eq 0 ]]; then success "Migration completed successfully!"; else error "Migration failed!"; fi

if [[ "${STORAGE}" == "nfs" ]]; then
    POST_DATA=$(kubectl --context "${CTX}" exec nfs-server -- cat "/exports/kata-data/proof.txt" 2>/dev/null || true)
    if [[ "${POST_DATA}" == "katamaran-nfs-test" ]]; then success "NFS data intact after migration"; else error "NFS data missing"; fi
fi

if [[ "${PING_PROOF}" == "true" && -f "${PING_LOG:-}" ]]; then
    PING_SUMMARY=$(grep -E "packets transmitted" "${PING_LOG}" || true)
    echo "  ${PING_SUMMARY}"
    LOSS=$(echo "${PING_SUMMARY}" | grep -oE "[0-9]+%" | head -1)
    if [[ "${LOSS}" == "0%" ]]; then success "ZERO PACKET LOSS VERIFIED"; else error "PACKET LOSS DETECTED (${LOSS})"; fi
fi
exit $MIG_STATUS
