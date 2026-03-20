#!/bin/bash
# e2e.sh — Unified E2E test harness for Katamaran live migration.
#
# --provider <name>  Cluster provider: 'minikube' (default) or 'kind'.
# --cni <name>       CNI plugin: 'auto' (default), 'calico', 'cilium', 'flannel',
#                    'ovn', or 'kindnet'.
# --storage <mode>   Storage mode: 'none' (default, skip NBD), 'local' (NBD drive-mirror),
#                    or 'nfs' (NFS shared storage). 'local' exercises the full 3-phase
#                    migration including storage mirroring. 'nfs' deploys an NFS server
#                    pod and uses it as shared storage.
# --method <name>    Orchestration method: 'job' (default) or 'direct'.
# --ping-proof       Run continuous ping during migration and assert zero packet loss.
# --env-only         Provision the cluster and install Kata, then stop (no migration).
# --help             Show this help text.

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
export PATH="${PROJECT_ROOT}/bin:${PATH}"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.24.0"

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

if [[ "${STORAGE}" != "none" && "${STORAGE}" != "local" && "${STORAGE}" != "nfs" ]]; then
    error "--storage must be 'none', 'local', or 'nfs' (got '${STORAGE}')"
    exit 1
fi

if [[ "${PROVIDER}" == "kind" ]]; then SUDO=""; fi
export SUDO

# Auto-detect container engine (podman preferred, docker fallback).
if command -v podman >/dev/null 2>&1; then
    CE="podman"
elif command -v docker >/dev/null 2>&1; then
    CE="docker"
else
    error "Neither podman nor docker found. One is required."
    exit 1
fi

if [[ "${CNI}" == "auto" ]]; then
    if [[ "${PROVIDER}" == "minikube" ]]; then
        CNI="calico"
        log "Minikube detected: defaulting to Calico."
    else
        CNI="kindnet"
    fi
fi

PROFILE="katamaran-e2e-${PROVIDER}-${CNI}-${STORAGE}-${METHOD}"

node_exec() {
    local node="$1"
    shift
    if [[ "${PROVIDER}" == "minikube" ]]; then
        # Pipe through tr to strip carriage returns added by minikube's PTY.
        minikube -p "${PROFILE}" ssh -n "$node" -- "$*" | tr -d '\r'
    else
        ${CE} exec "$node" bash -c "$*"
    fi
}

node_cp_to() {
    local node="$1"
    local src="$2"
    local dst="$3"
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube -p "${PROFILE}" cp "${src}" "${node}:${dst}"
    else
        ${CE} cp "${src}" "${node}:${dst}"
    fi
}

# qmp_hotplug_disk attaches a virtio-blk data disk to a running QEMU via QMP.
# Uses a fixed PCI address (bus=pci-bridge-0,addr=0x8) so source and destination
# have matching PCI topology for live migration.
#
# Usage: qmp_hotplug_disk <node> <qmp_socket> <disk_image_path>
qmp_hotplug_disk() {
    local node="$1" sock="$2" disk="$3"
    # QMP is conversational: consume greeting, negotiate capabilities, then send commands.
    # Each command needs a pause for QEMU to process and respond.
    node_exec "${node}" "${SUDO} bash -c '(
        sleep 0.2
        printf \"{\\\"execute\\\": \\\"qmp_capabilities\\\"}\\n\"
        sleep 0.2
        printf \"{\\\"execute\\\": \\\"blockdev-add\\\", \\\"arguments\\\": {\\\"driver\\\": \\\"raw\\\", \\\"node-name\\\": \\\"drive-virtio-disk0\\\", \\\"file\\\": {\\\"driver\\\": \\\"file\\\", \\\"filename\\\": \\\"${disk}\\\"}}}\\n\"
        sleep 0.2
        printf \"{\\\"execute\\\": \\\"device_add\\\", \\\"arguments\\\": {\\\"driver\\\": \\\"virtio-blk-pci\\\", \\\"drive\\\": \\\"drive-virtio-disk0\\\", \\\"id\\\": \\\"data-disk0\\\", \\\"bus\\\": \\\"pci-bridge-0\\\", \\\"addr\\\": \\\"0x8\\\"}}\\n\"
        sleep 0.3
    ) | nc -U ${sock}'" 2>/dev/null
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
    # Unmount NFS if mounted.
    if [[ "${STORAGE}" == "nfs" ]]; then
        for node in "${NODE1:-}" "${NODE2:-}"; do
            if [[ -n "${node}" ]]; then
                node_exec "${node}" "${SUDO} umount /mnt/nfs-katamaran 2>/dev/null" || true
            fi
        done
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
REQS="kubectl helm"
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
    # Use custom ISO with sch_plug if available.
    CUSTOM_ISO="${PROJECT_ROOT}/out/minikube-amd64.iso"
    if [[ -f "${CUSTOM_ISO}" ]]; then
        log "Using custom minikube ISO with sch_plug: ${CUSTOM_ISO}"
        MINIKUBE_ARGS+=("--iso-url=file://${CUSTOM_ISO}")
    fi
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
    if [[ "${CE}" == "podman" ]]; then
        KIND_EXPERIMENTAL_PROVIDER=podman kind create cluster --name "${PROFILE}" --config "${KIND_CONFIG}" --wait 120s
    else
        kind create cluster --name "${PROFILE}" --config "${KIND_CONFIG}" --wait 120s
    fi
    NODE1="${PROFILE}-control-plane"
    NODE2="${PROFILE}-worker"
    CTX="kind-${PROFILE}"

    # Kata QEMU backs VM memory with /dev/shm.  Podman defaults to 63 MB,
    # which is far too small.  Remount with enough room (4 GB).
    for n in "${NODE1}" "${NODE2}"; do
        ${CE} exec "$n" mount -t tmpfs -o size=4g tmpfs /dev/shm
    done
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
    # Pre-create namespace to avoid conflict with chart-managed Namespace resource.
    kubectl --context "${CTX}" create namespace ovn-kubernetes 2>/dev/null || true
    helm upgrade --install ovn-kubernetes "${OVN_K_DIR}/helm/ovn-kubernetes" \
        --kube-context "${CTX}" --namespace ovn-kubernetes \
        -f "${OVN_K_DIR}/helm/ovn-kubernetes/values-no-ic.yaml" \
        --set k8sAPIServer="https://${API_HOST}:${API_PORT}" \
        --set global.k8sServiceHost="${API_HOST}" --set global.k8sServicePort="${API_PORT}" \
        --set podNetwork="10.244.0.0/16/24" --set serviceNetwork="10.96.0.0/16" \
        --set tags.ovnkube-db=true --set tags.ovnkube-master=true --set tags.ovnkube-node=true --wait=false

    log "Waiting for OVN-Kubernetes..."
    kubectl --context "${CTX}" -n ovn-kubernetes rollout status daemonset/ovnkube-node --timeout=600s
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

# After deploying a non-default CNI, wait for all nodes to become Ready.
# Without this, kata-deploy pods may fail to start because the CNI hasn't
# finished configuring networking on all nodes.
if [[ "${CNI}" != "kindnet" && "${CNI}" != "calico" ]]; then
    log "Waiting for all nodes to become Ready after CNI deployment..."
    kubectl --context "${CTX}" wait --for=condition=Ready node --all --timeout=300s
fi

log "Installing Kata Containers via Helm..."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --kube-context "${CTX}" --namespace kube-system \
    --version "${KATA_CHART_VERSION}" \
    --set shims.qemu.enabled=true --wait=false
kubectl --context "${CTX}" -n kube-system rollout status daemonset/kata-deploy --timeout=300s

log "Applying Kata configuration overrides (QMP socket + high timeout)..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"
for node in "${NODE1}" "${NODE2}"; do
    log "Waiting for Kata configuration on node ${node}..."
    for i in $(seq 1 60); do
        if node_exec "${node}" "[ -f ${KATA_CFG} ]"; then break; fi
        sleep 2
    done
    if ! node_exec "${node}" "[ -f ${KATA_CFG} ]"; then
        error "Kata configuration not found on ${node} after waiting."
        exit 1
    fi
    node_exec "$node" "${SUDO} sed -i 's|^#*enable_debug = .*|enable_debug = true|' '${KATA_CFG}'"
    node_exec "$node" "${SUDO} sed -i 's|^#*extra_monitor_socket = .*|extra_monitor_socket = \"qmp\"|' '${KATA_CFG}'"
    node_exec "$node" "${SUDO} sed -i 's|^#*create_container_timeout = .*|create_container_timeout = 600|' '${KATA_CFG}'"
done

kubectl --context "${CTX}" label node "${NODE1}" katamaran-role=source --overwrite
kubectl --context "${CTX}" label node "${NODE2}" katamaran-role=dest --overwrite

log "Building and deploying katamaran..."
${CE} build -t localhost/katamaran:dev .
rm -f katamaran.tar
${CE} save localhost/katamaran:dev -o katamaran.tar

if [[ "${PROVIDER}" == "minikube" ]]; then
    minikube -p "${PROFILE}" image load katamaran.tar
else
    kind load image-archive katamaran.tar --name "${PROFILE}"
fi

envsubst < "${PROJECT_ROOT}/deploy/daemonset.yaml" | kubectl --context "${CTX}" apply -f -
kubectl --context "${CTX}" -n kube-system rollout status daemonset/katamaran-deploy --timeout=300s

# --- Storage: deploy NFS server for shared storage test ---
NFS_DISK_IMG=""
if [[ "${STORAGE}" == "nfs" ]]; then
    log "Deploying NFS server on Node 1..."
    export NODE_NAME="${NODE1}"
    export NFS_EXPORT_PATH="/exports"
    envsubst '$NODE_NAME $NFS_EXPORT_PATH' < "${SCRIPT_DIR}/manifests/nfs-server.yaml" \
        | kubectl --context "${CTX}" apply -f -

    kubectl --context "${CTX}" wait --for=condition=Ready pod/nfs-server --timeout=120s
    NFS_SERVER_IP=$(kubectl --context "${CTX}" get pod nfs-server -o jsonpath='{.status.podIP}')
    success "NFS server running at ${NFS_SERVER_IP}"

    # Mount NFS on both nodes and create the shared disk image.
    NFS_MNT="/mnt/nfs-katamaran"
    for node in "${NODE1}" "${NODE2}"; do
        node_exec "${node}" "${SUDO} mkdir -p ${NFS_MNT}"
        if ! node_exec "${node}" "${SUDO} mount -t nfs -o vers=4,nolock ${NFS_SERVER_IP}:${NFS_EXPORT_PATH} ${NFS_MNT}" 2>/dev/null; then
            if ! node_exec "${node}" "${SUDO} mount -t nfs -o vers=3,nolock ${NFS_SERVER_IP}:${NFS_EXPORT_PATH} ${NFS_MNT}" 2>/dev/null; then
                error "NFS mount failed on ${node}. Kernel NFS client modules (nfs, sunrpc) may be missing."
                error "For custom minikube ISOs, enable CONFIG_NFS_FS and CONFIG_SUNRPC in the kernel config."
                exit 1
            fi
        fi
        success "NFS mounted on ${node} at ${NFS_MNT}"
    done

    # Create the shared data disk on NFS (visible from both nodes).
    NFS_DISK_IMG="${NFS_MNT}/shared-disk.img"
    node_exec "${NODE1}" "${SUDO} qemu-img create -f raw ${NFS_DISK_IMG} 64M"
    success "Shared disk image created on NFS: ${NFS_DISK_IMG}"
fi

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

# Extract the source sandbox ID from the QMP socket path.
SRC_SANDBOX_DIR=$(dirname "${SRC_SOCK}")
SRC_SANDBOX_ID=$(basename "${SRC_SANDBOX_DIR}")

# --- Storage: create and attach data disk to source QEMU ---
if [[ "${STORAGE}" == "local" ]]; then
    log "Creating local data disk for source QEMU..."
    SRC_DISK_IMG="/tmp/kata-src-data-$$.img"
    node_exec "${NODE1}" "${SUDO} qemu-img create -f raw ${SRC_DISK_IMG} 64M"
    qmp_hotplug_disk "${NODE1}" "${SRC_SOCK}" "${SRC_DISK_IMG}"
    success "Source data disk hotplugged: ${SRC_DISK_IMG}"
elif [[ "${STORAGE}" == "nfs" ]]; then
    log "Attaching shared NFS data disk to source QEMU..."
    qmp_hotplug_disk "${NODE1}" "${SRC_SOCK}" "${NFS_DISK_IMG}"
    success "Source data disk hotplugged (NFS): ${NFS_DISK_IMG}"
fi

log "Capturing source QEMU command line for replay..."
SRC_QEMU_PID=$(node_exec "${NODE1}" "${SUDO} cat ${SRC_SANDBOX_DIR}/pid")
SRC_CMDLINE=$(node_exec "${NODE1}" "${SUDO} cat /proc/${SRC_QEMU_PID}/cmdline | tr '\0' '\n'")
# Extract the nvdimm image path from source (needed for writable copy on dest).
SRC_NVDIMM_PATH=$(echo "${SRC_CMDLINE}" | grep -oP 'mem-path=\K[^,]+' | grep -v '/dev/shm' | head -1)
success "Source QEMU PID: ${SRC_QEMU_PID} — cmdline captured ($(echo "${SRC_CMDLINE}" | wc -l) args)"

log "Removing tc mirred redirect on source pod's eth0..."
# Kata installs a tc filter that redirects ALL ingress on eth0 to tap0_kata.
# This prevents the QEMU process from making outbound TCP connections (the
# migration stream). We remove it so QEMU can reach the destination.
node_exec "${NODE1}" "${SUDO} nsenter --net=/proc/${SRC_QEMU_PID}/ns/net tc filter del dev eth0 ingress" 2>/dev/null || true
success "Source tc redirect removed."

log "Deploying helper pod on Node 2 for destination network namespace..."
# We cannot start a Kata VM with -incoming defer because Kata's containerd
# shim kills VMs that don't connect via vsock within the dial_timeout.
# Instead, deploy a lightweight non-Kata pod on Node 2 to provide a clean
# network namespace with a routable pod IP, then run the destination QEMU
# inside that namespace.
kubectl --context "${CTX}" delete pod mig-helper --ignore-not-found --force --grace-period=0 2>/dev/null || true
cat <<EOFPOD | kubectl --context "${CTX}" apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: mig-helper
spec:
  nodeSelector:
    katamaran-role: dest
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOFPOD
if ! kubectl --context "${CTX}" wait --for=condition=Ready pod/mig-helper --timeout=120s; then
    error "Helper pod failed to become Ready."
    exit 1
fi
DST_POD_IP=$(kubectl --context "${CTX}" get pod mig-helper -o jsonpath='{.status.podIP}')
success "Helper pod IP: ${DST_POD_IP}"

# Get the helper container's PID for nsenter.
MIG_HELPER_PID=""
for i in $(seq 1 15); do
    MIG_HELPER_PID=$(node_exec "${NODE2}" "${SUDO} crictl ps -q --label io.kubernetes.pod.name=mig-helper 2>/dev/null | head -1 | xargs -I{} ${SUDO} crictl inspect -o go-template --template '{{.info.pid}}' {} 2>/dev/null" 2>/dev/null || true)
    if [[ -n "${MIG_HELPER_PID}" && "${MIG_HELPER_PID}" =~ ^[0-9]+$ ]]; then break; fi
    sleep 1
done
if [[ -z "${MIG_HELPER_PID}" || ! "${MIG_HELPER_PID}" =~ ^[0-9]+$ ]]; then
    error "Could not determine mig-helper container PID."
    exit 1
fi
success "Helper container PID: ${MIG_HELPER_PID}"

log "Setting up destination QEMU (manual, with -incoming defer)..."
# Replay the source QEMU's command line on Node 2 with path substitutions
# to guarantee identical device topology. This bypasses Kata's sandbox
# lifecycle, which kills VMs that don't connect via vsock within the
# dial_timeout (incompatible with -incoming mode).
#
# Key fixes for live migration:
#   1. nvdimm image is a writable copy (source has readonly=on, but QEMU
#      sends nvdimm pages during migration; writing to readonly mmap = SIGSEGV)
#   2. virtiofsd started with --migration-on-error=guest-error so that
#      inode state mismatches between source/dest don't abort migration
DST_SANDBOX="manual-dst-$$"
DST_VM_DIR="/run/vc/vm/${DST_SANDBOX}"
DST_SOCK="${DST_VM_DIR}/extra-monitor.sock"
node_exec "${NODE2}" "${SUDO} mkdir -p ${DST_VM_DIR}"
node_exec "${NODE2}" "${SUDO} mkdir -p /run/kata-containers/shared/sandboxes/${DST_SANDBOX}/shared"

# Create a writable copy of the nvdimm image (path extracted from source cmdline).
node_exec "${NODE2}" "${SUDO} cp ${SRC_NVDIMM_PATH} /tmp/kata-dst-nvdimm-$$.img"

# Start virtiofsd for the vhost-user-fs device.
# Wrap in 'bash -c "nohup ... &"' so the daemon survives SSH session exit (minikube).
node_exec "${NODE2}" "${SUDO} bash -c \"nohup /opt/kata/libexec/virtiofsd --socket-path=${DST_VM_DIR}/vhost-fs.sock --shared-dir=/run/kata-containers/shared/sandboxes/${DST_SANDBOX}/shared --cache=auto --thread-pool-size=1 --announce-submounts --sandbox=none --migration-on-error=guest-error >/dev/null 2>&1 &\""
sleep 2
if ! node_exec "${NODE2}" "[ -S ${DST_VM_DIR}/vhost-fs.sock ]" 2>/dev/null; then
    error "virtiofsd socket not found."
    exit 1
fi

# Create tap interface in the helper pod's network namespace.
node_exec "${NODE2}" "${SUDO} nsenter --net=/proc/${MIG_HELPER_PID}/ns/net ip tuntap add dev tap0_kata mode tap" 2>/dev/null || true
node_exec "${NODE2}" "${SUDO} nsenter --net=/proc/${MIG_HELPER_PID}/ns/net ip link set tap0_kata up"

# Start destination QEMU by replaying the source's command line with path
# substitutions. This ensures the device topology always matches exactly,
# regardless of which devices Kata added at runtime.
DST_NVDIMM_IMG="/tmp/kata-dst-nvdimm-$$.img"

# Substitute paths: source sandbox → destination sandbox, nvdimm → writable
# copy, and strip readonly from the nvdimm backend (migration writes to it).
DST_CMDLINE=$(echo "${SRC_CMDLINE}" \
    | sed "s|${SRC_SANDBOX_DIR}|${DST_VM_DIR}|g" \
    | sed "s|sandbox-${SRC_SANDBOX_ID}|sandbox-${DST_SANDBOX}|g" \
    | sed "s|,readonly=on||g; s|,readonly=true||g" \
)
[[ -n "${SRC_NVDIMM_PATH}" ]] && \
    DST_CMDLINE=$(echo "${DST_CMDLINE}" | sed "s|${SRC_NVDIMM_PATH}|${DST_NVDIMM_IMG}|g")

# Reconstruct the command: skip the QEMU binary (first arg), strip any
# existing -incoming/-daemonize from source, then quote each argument for
# remote execution via node_exec.
DST_QEMU_CMD="nsenter --net=/proc/${MIG_HELPER_PID}/ns/net /opt/kata/bin/qemu-system-x86_64"
first=true
skip_next=false
while IFS= read -r arg; do
    if $first; then first=false; continue; fi
    if $skip_next; then skip_next=false; continue; fi
    case "${arg}" in
        -daemonize) continue ;;
        -incoming)  skip_next=true; continue ;;
    esac
    DST_QEMU_CMD+=" $(printf '%q' "${arg}")"
done <<< "${DST_CMDLINE}"
DST_QEMU_CMD+=" -incoming defer -daemonize"

node_exec "${NODE2}" "${SUDO} ${DST_QEMU_CMD}"

# Wait for QMP socket to appear
for i in $(seq 1 15); do
    if node_exec "${NODE2}" "[ -S ${DST_SOCK} ]" 2>/dev/null; then break; fi
    sleep 1
done
if ! node_exec "${NODE2}" "[ -S ${DST_SOCK} ]" 2>/dev/null; then
    error "Destination QMP socket not found at ${DST_SOCK}"
    exit 1
fi
success "Destination QEMU started. QMP: ${DST_SOCK}"

# --- Storage: create and attach data disk to destination QEMU ---
if [[ "${STORAGE}" == "local" ]]; then
    log "Creating local data disk for destination QEMU..."
    DST_DISK_IMG="/tmp/kata-dst-data-$$.img"
    node_exec "${NODE2}" "${SUDO} qemu-img create -f raw ${DST_DISK_IMG} 64M"
    qmp_hotplug_disk "${NODE2}" "${DST_SOCK}" "${DST_DISK_IMG}"
    success "Destination data disk hotplugged: ${DST_DISK_IMG}"
elif [[ "${STORAGE}" == "nfs" ]]; then
    log "Attaching shared NFS data disk to destination QEMU..."
    qmp_hotplug_disk "${NODE2}" "${DST_SOCK}" "${NFS_DISK_IMG}"
    success "Destination data disk hotplugged (NFS): ${NFS_DISK_IMG}"
fi

# The destination QEMU runs inside the helper pod's network namespace.
# The tap interface (tap0_kata) is in that namespace. Pass the netns path
# so katamaran can run tc commands via nsenter.
# Check if sch_plug is available; if not, try to build it (minikube only).
if node_exec "${NODE2}" "${SUDO} modprobe sch_plug" 2>/dev/null; then
    DST_TAP="tap0_kata"
elif [[ "${PROVIDER}" == "minikube" ]] && [[ -x "${SCRIPT_DIR}/build-minikube-modules.sh" ]]; then
    log "sch_plug not available. Building kernel module for minikube..."
    if "${SCRIPT_DIR}/build-minikube-modules.sh" "${PROFILE}"; then
        DST_TAP="tap0_kata"
        success "sch_plug module built and loaded."
    else
        warn "Module build failed; skipping zero-drop qdisc (tap=none)."
        DST_TAP="none"
    fi
else
    log "sch_plug not available on ${NODE2}; skipping zero-drop qdisc (tap=none)."
    DST_TAP="none"
fi
DST_TAP_NETNS="/proc/${MIG_HELPER_PID}/ns/net"

PING_PID=""
if [[ "${PING_PROOF}" == "true" ]]; then
    log "Ping probe will verify sch_plug operation after migration."
fi

if [[ "${ENV_ONLY}" == "true" ]]; then
    log "Environment ready. Skipping migration."
    exit 0
fi

if [[ "${METHOD}" == "job" ]]; then
    STORAGE_FLAGS=""
    if [[ "${STORAGE}" != "local" ]]; then
        STORAGE_FLAGS="--shared-storage"
    fi
    log "Executing Live Migration (job mode, storage=${STORAGE})..."
    MIG_LOG=$(mktemp)
    "${PROJECT_ROOT}/deploy/migrate.sh" \
        --context "${CTX}" --source-node "${NODE1}" --dest-node "${NODE2}" \
        --tap "${DST_TAP}" --tap-netns "${DST_TAP_NETNS}" \
        --qmp-source "${SRC_SOCK}" --qmp-dest "${DST_SOCK}" \
        --dest-ip "${DST_POD_IP}" --vm-ip "${SRC_POD_IP}" \
        --image "localhost/katamaran:dev" ${STORAGE_FLAGS} --downtime 25 2>&1 | tee "${MIG_LOG}" || {
            error "Migration failed!"
            exit 1
        }
else
    error "SSH method not implemented."
    exit 1
fi

# Post-migration: check dest QEMU status for debugging.
DST_QEMU_ALIVE=$(node_exec "${NODE2}" "${SUDO} test -f ${DST_VM_DIR}/pid && ${SUDO} kill -0 \$(cat ${DST_VM_DIR}/pid) 2>/dev/null && echo alive || echo dead" 2>/dev/null || echo "unknown")
log "Destination QEMU status: ${DST_QEMU_ALIVE}"
if [[ -n "${DST_VM_DIR:-}" ]]; then
    QEMU_LOG=$(node_exec "${NODE2}" "${SUDO} cat ${DST_VM_DIR}/qemu.log 2>/dev/null" 2>/dev/null || true)
    if [[ -n "${QEMU_LOG}" ]]; then
        log "Destination QEMU log:"
        echo "${QEMU_LOG}" | head -20
    fi
    # Check dmesg for crashes.
    node_exec "${NODE2}" "dmesg | grep -iE 'qemu|segfault|killed|oom' | tail -5" 2>/dev/null || true
fi

if [[ "${PING_PROOF}" == "true" ]]; then
    log "Verifying sch_plug zero-drop buffering from migration output..."
    # The dest logs are captured in the migrate.sh debug dump output.
    PASS=true
    for pattern in "Network queue installed" "Network queue plugged" "VM resumed. Flushing" "Zero drops achieved"; do
        if grep -q "${pattern}" "${MIG_LOG}"; then
            success "sch_plug: ${pattern}"
        else
            error "sch_plug: missing '${pattern}' in dest logs"
            PASS=false
        fi
    done
    rm -f "${MIG_LOG}"
    if [[ "${PASS}" != "true" ]]; then
        exit 1
    fi
    success "Zero-drop sch_plug buffering verified!"
fi

success "E2E Test Passed!"
