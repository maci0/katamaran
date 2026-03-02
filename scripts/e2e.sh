#!/bin/bash
# e2e.sh — Unified E2E test harness for Katamaran live migration.

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
export PATH="${PROJECT_ROOT}/bin:${PATH}"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

log() { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
warn() { echo -e "\033[1;33m  WARN: $1\033[0m"; }
error() { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

PROVIDER="minikube"
CNI="auto"
STORAGE="none"
METHOD="job"
SUDO="sudo"
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
        --help) grep "^# " "$0" | sed "s/^#//"; exit 0 ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

if [[ "${STORAGE}" == "nfs" ]]; then
    error "--storage nfs is not yet implemented. All E2E tests currently use shared storage mode."
    error "NFS manifests exist in scripts/manifests/ but the e2e.sh harness does not deploy them."
    exit 1
fi

if [[ "${PROVIDER}" == "kind" ]]; then SUDO=""; fi
export SUDO

if [[ "${CNI}" == "auto" ]]; then
    if [[ "${PROVIDER}" == "minikube" ]]; then
        CNI="calico"
        log "Minikube detected: defaulting to Calico (OVN requires nftables support missing in minikube kernel)."
    else
        CNI="kindnet"
    fi
fi

PROFILE="katamaran-e2e-${PROVIDER}-${CNI}-${STORAGE}-${METHOD}"

node_exec() {
    local node="$1"
    shift
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube -p "${PROFILE}" ssh -n "$node" -- "$*"
    else
        podman exec "$node" bash -c "$*"
    fi
}

node_cp_to() {
    local node="$1"
    local src="$2"
    local dst="$3"
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube -p "${PROFILE}" cp "${src}" "${node}:${dst}"
    else
        podman cp "${src}" "${node}:${dst}"
    fi
}

cleanup() {
    if [[ -n "${PING_PID:-}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
        kill "${PING_PID}" 2>/dev/null || true
        wait "${PING_PID}" 2>/dev/null || true
    fi
    if [[ "${ENV_ONLY}" == "true" ]]; then
        log "--env-only set, keeping cluster '${PROFILE}'"
        return
    fi
    if [[ "${E2E_NO_CLEANUP:-}" == "true" ]]; then
        log "E2E_NO_CLEANUP set, skipping cluster deletion for '${PROFILE}'"
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
    cleanup
    exit 0
fi

trap cleanup EXIT

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

log "Starting 2-node ${PROVIDER} cluster (CNI: ${CNI})..."

if [[ "${PROVIDER}" == "minikube" ]]; then
    MINIKUBE_ARGS=("--nodes" "2" "--driver=kvm2" "--memory=12288" "--cpus=6" "--container-runtime=containerd" "--extra-config=kubelet.runtime-request-timeout=10m")
    if [[ "${CNI}" == "ovn" || "${CNI}" == "cilium" || "${CNI}" == "flannel" ]]; then
        MINIKUBE_ARGS+=("--cni=bridge")
    else
        MINIKUBE_ARGS+=("--cni=${CNI}")
    fi
    minikube start -p "${PROFILE}" "${MINIKUBE_ARGS[@]}"

    NODE1="${PROFILE}"
    NODE2="${PROFILE}-m02"
    CTX="${PROFILE}"
else
    if [[ "${CNI}" == "cilium" || "${CNI}" == "flannel" ]]; then
        KIND_CONFIG="${SCRIPT_DIR}/manifests/kind-config-nocni.yaml"
    else
        KIND_CONFIG="${SCRIPT_DIR}/manifests/kind-config.yaml"
    fi
    KIND_EXPERIMENTAL_PROVIDER=podman kind create cluster --name "${PROFILE}" --config "${KIND_CONFIG}" --wait 120s
    NODE1="${PROFILE}-control-plane"
    NODE2="${PROFILE}-worker"
    CTX="kind-${PROFILE}"
fi

log "Untainting nodes..."
kubectl --context "${CTX}" taint nodes --all node-role.kubernetes.io/control-plane- || true
log "Waiting for nodes to register..."
for i in $(seq 1 60); do
    NODE_COUNT=$(kubectl --context "${CTX}" get nodes --no-headers 2>/dev/null | wc -l || echo 0)
    if [[ "${NODE_COUNT}" -ge 2 ]]; then break; fi
    sleep 5
done

if [[ "${NODE_COUNT}" -lt 2 ]]; then
    error "Nodes failed to register in time."
    exit 1
fi

NODE1_IP=$(kubectl --context "${CTX}" get node "${NODE1}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "${CTX}" get node "${NODE2}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
success "Node 1 (${NODE1}): ${NODE1_IP}"
success "Node 2 (${NODE2}): ${NODE2_IP}"

if [[ "${CNI}" == "ovn" ]]; then
    log "Deploying OVN-Kubernetes..."
    API_SERVER=$(kubectl --context "${CTX}" config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    API_HOST=$(echo "${API_SERVER}" | cut -d/ -f3 | cut -d: -f1)
    API_PORT=$(echo "${API_SERVER}" | cut -d/ -f3 | cut -d: -f2)

    OVN_K_DIR="/tmp/ovn-kubernetes-${PROFILE}"
    rm -rf "${OVN_K_DIR}"
    git clone --depth 1 --branch "master" "https://github.com/ovn-org/ovn-kubernetes.git" "${OVN_K_DIR}"
    helm upgrade --install ovn-kubernetes "${OVN_K_DIR}/helm/ovn-kubernetes" \
        --kube-context "${CTX}" --namespace ovn-kubernetes --create-namespace \
        -f "${OVN_K_DIR}/helm/ovn-kubernetes/values-no-ic.yaml" \
        --set k8sAPIServer="https://${API_HOST}:${API_PORT}" \
        --set global.k8sServiceHost="${API_HOST}" --set global.k8sServicePort="${API_PORT}" \
        --set podNetwork="10.244.0.0/16/24" --set serviceNetwork="10.96.0.0/16" \
        --set tags.ovnkube-db=true --set tags.ovnkube-master=true --set tags.ovnkube-node=true --wait=false

    log "Waiting for OVN-Kubernetes..."
    kubectl --context "${CTX}" -n ovn-kubernetes rollout status daemonset/ovnkube-node --timeout=600s 2>/dev/null || true
elif [[ "${CNI}" == "cilium" ]]; then
    log "Deploying Cilium..."
    helm upgrade --install cilium oci://quay.io/cilium/charts/cilium \
        --kube-context "${CTX}" --namespace kube-system \
        --set k8sServiceHost="${NODE1_IP}" --set k8sServicePort="6443" \
        --set operator.replicas=1 --wait
elif [[ "${CNI}" == "flannel" ]]; then
    log "Deploying Flannel..."
    kubectl --context "${CTX}" apply -f https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
    kubectl --context "${CTX}" -n kube-flannel rollout status daemonset/kube-flannel-ds --timeout=300s
fi

log "Installing Kata Containers via Helm..."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --kube-context "${CTX}" --namespace kube-system \
    --version "${KATA_CHART_VERSION}" \
    --set shims.disableAll=true --set shims.qemu.enabled=true --wait=false
kubectl --context "${CTX}" -n kube-system rollout status daemonset/kata-deploy --timeout=300s

log "Applying Kata configuration overrides (QMP socket + high timeout)..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
for node in "${NODE1}" "${NODE2}"; do
    log "Waiting for Kata configuration on node ${node}..."
    for i in $(seq 1 60); do
        if node_exec "${node}" "[ -f ${KATA_CFG} ]"; then break; fi
        sleep 2
    done
    node_exec "$node" "${SUDO} sed -i 's|^#*enable_debug = .*|enable_debug = true|' '${KATA_CFG}'"
    node_exec "$node" "${SUDO} sed -i 's|^#*extra_monitor_socket = .*|extra_monitor_socket = \"qmp\"|' '${KATA_CFG}'"
    node_exec "$node" "${SUDO} sed -i 's|^#*create_container_timeout = .*|create_container_timeout = 600|' '${KATA_CFG}'"
done

kubectl --context "${CTX}" label node "${NODE1}" katamaran-role=source --overwrite
kubectl --context "${CTX}" label node "${NODE2}" katamaran-role=dest --overwrite

log "Building and deploying katamaran..."
podman build -t localhost/katamaran:dev .
rm -f katamaran.tar
podman save localhost/katamaran:dev -o katamaran.tar

if [[ "${PROVIDER}" == "minikube" ]]; then
    minikube -p "${PROFILE}" image load katamaran.tar
else
    kind load image-archive katamaran.tar --name "${PROFILE}"
fi

envsubst < "${PROJECT_ROOT}/deploy/daemonset.yaml" | kubectl --context "${CTX}" apply -f -
kubectl --context "${CTX}" -n kube-system rollout status daemonset/katamaran-deploy --timeout=300s

log "Deploying source pod on Node 1..."
kubectl --context "${CTX}" delete pod kata-src --ignore-not-found --force --grace-period=0
cat <<EOFPOD | kubectl --context "${CTX}" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: kata-src
  labels:
    app: ping-target
spec:
  runtimeClassName: kata-qemu
  nodeSelector:
    katamaran-role: source
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOFPOD
if ! kubectl --context "${CTX}" wait --for=condition=Ready pod/kata-src --timeout=300s; then
    error "Source pod failed to become Ready. Collecting diagnostics..."
    kubectl --context "${CTX}" --request-timeout=20s describe pod kata-src 2>&1 | tail -30
    node_exec "${NODE1}" "${SUDO} journalctl --no-pager -u containerd --since '5 minutes ago' 2>&1 | grep -iE 'kata|qemu|kvm|error|fail' | tail -20" || true
    exit 1
fi
SRC_POD_IP=$(kubectl --context "${CTX}" get pod kata-src -o jsonpath='{.status.podIP}')
success "Source pod IP: ${SRC_POD_IP}"

log "Finding source QMP socket..."
SRC_SOCK=""
for i in $(seq 1 30); do
    SRC_SOCK=$(node_exec "${NODE1}" "${SUDO} find /run/vc 2>/dev/null -name extra-monitor.sock | head -n 1")
    if [[ -n "${SRC_SOCK}" ]]; then break; fi
    sleep 2
done

if [[ -z "${SRC_SOCK}" ]]; then
    error "Source QMP socket not found."
    exit 1
fi

log "Installing state-matching QEMU wrapper on Node 2..."
WRAPPER_TMP="/tmp/qemu-wrapper-${PROFILE}.sh"
printf '#!/bin/bash\nexec /opt/kata/bin/qemu-system-x86_64 "$@" -incoming tcp:0.0.0.0:4444\n' > "${WRAPPER_TMP}"
node_cp_to "${NODE2}" "${WRAPPER_TMP}" "/tmp/qemu-wrapper.sh"
node_exec "${NODE2}" "${SUDO} mv /tmp/qemu-wrapper.sh /usr/local/bin/qemu-wrapper.sh && ${SUDO} chmod +x /usr/local/bin/qemu-wrapper.sh"
rm -f "${WRAPPER_TMP}"

# Point Kata to the wrapper on Node 2
node_exec "${NODE2}" "${SUDO} sed -i 's|^#*path = .*|path = \"/usr/local/bin/qemu-wrapper.sh\"|' '${KATA_CFG}'"

log "Deploying destination pod on Node 2..."
kubectl --context "${CTX}" delete pod kata-dst --ignore-not-found --force --grace-period=0
cat <<EOFPOD2 | kubectl --context "${CTX}" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: kata-dst
spec:
  runtimeClassName: kata-qemu
  nodeSelector:
    katamaran-role: dest
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOFPOD2

log "Waiting for destination QEMU QMP socket to appear..."
DST_SOCK=""
for i in $(seq 1 60); do
    DST_SOCK=$(node_exec "${NODE2}" "${SUDO} find /run/vc 2>/dev/null -name extra-monitor.sock | head -n 1")
    if [[ -n "${DST_SOCK}" ]]; then break; fi
    sleep 2
done

if [[ -z "${DST_SOCK}" ]]; then
    error "Destination QMP socket never appeared. Collecting diagnostics..."
    kubectl --context "${CTX}" --request-timeout=20s describe pod kata-dst 2>&1 | tail -30
    node_exec "${NODE2}" "${SUDO} journalctl --no-pager -u containerd --since '5 minutes ago' 2>&1 | grep -iE 'kata|qemu|kvm|error|fail' | tail -20" || true
    exit 1
fi

log "Detecting destination tap interface..."
DST_TAP=""
for i in $(seq 1 15); do
    DST_TAP=$(node_exec "${NODE2}" "${SUDO} ip -br link 2>/dev/null | grep -oE 'tap[^ ]+' | head -n 1" || true)
    if [[ -n "${DST_TAP}" ]]; then break; fi
    sleep 2
done
if [[ -n "${DST_TAP}" ]]; then
    success "Destination tap interface: ${DST_TAP}"
else
    warn "No tap interface found on destination; zero-drop qdisc buffering will be skipped."
    DST_TAP="none"
fi

PING_PID=""
if [[ "${PING_PROOF}" == "true" ]]; then
    log "Starting continuous ping..."
    ping -i 0.1 "${SRC_POD_IP}" > /tmp/katamaran-ping.log 2>&1 &
    PING_PID=$!
fi

if [[ "${ENV_ONLY}" == "true" ]]; then
    log "Environment ready. Skipping migration."
    exit 0
fi

if [[ "${METHOD}" == "job" ]]; then
    log "Executing Live Migration (job mode)..."
    "${PROJECT_ROOT}/deploy/migrate.sh" \
        --context "${CTX}" --source-node "${NODE1}" --dest-node "${NODE2}" \
        --tap "${DST_TAP}" --qmp-source "${SRC_SOCK}" --qmp-dest "${DST_SOCK}" \
        --dest-ip "${NODE2_IP}" --vm-ip "${SRC_POD_IP}" \
        --image "localhost/katamaran:dev" --shared-storage --downtime 25 || {
            error "Migration failed!"
            exit 1
        }
else
    error "SSH method not implemented."
    exit 1
fi

if [[ "${PING_PROOF}" == "true" ]]; then
    log "Checking packet loss..."
    kill "${PING_PID}" || true
    wait "${PING_PID}" 2>/dev/null || true
    LOSS=$(grep "packet loss" /tmp/katamaran-ping.log | awk '{print $6}')
    log "Ping results: ${LOSS}"
    if [[ "${LOSS}" != "0%" ]]; then
        error "Packet loss detected!"
        exit 1
    fi
fi

success "E2E Test Passed!"
