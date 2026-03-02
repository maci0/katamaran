#!/bin/bash
# e2e.sh — Unified E2E test harness for Katamaran live migration.

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

    # Kata QEMU backs VM memory with /dev/shm.  Podman defaults to 63 MB,
    # which is far too small.  Remount with enough room (4 GB).
    for n in "${NODE1}" "${NODE2}"; do
        podman exec "$n" mount -t tmpfs -o size=4g tmpfs /dev/shm
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

# Extract the source sandbox ID from the QMP socket path.
SRC_SANDBOX_DIR=$(dirname "${SRC_SOCK}")
SRC_SANDBOX_ID=$(basename "${SRC_SANDBOX_DIR}")

log "Extracting source QEMU configuration..."
# Parse the running source QEMU's command line to get device IDs that
# the destination must match exactly for live migration to succeed.
SRC_QEMU_PID=$(node_exec "${NODE1}" "${SUDO} cat ${SRC_SANDBOX_DIR}/pid")
SRC_CMDLINE=$(node_exec "${NODE1}" "${SUDO} cat /proc/${SRC_QEMU_PID}/cmdline | tr '\0' '\n'")
SRC_UUID=$(echo "${SRC_CMDLINE}" | grep -A1 '^-uuid$' | tail -1)
SRC_VSOCK_ID=$(echo "${SRC_CMDLINE}" | grep 'vhost-vsock-pci' | grep -oP 'id=\Kvsock-[0-9]+')
SRC_CHARDEV_FS=$(echo "${SRC_CMDLINE}" | grep 'vhost-user-fs-pci' | grep -oP 'chardev=\K[^,]+')
SRC_MAC=$(echo "${SRC_CMDLINE}" | grep 'virtio-net-pci' | grep -oP 'mac=\K[^,]+')
SRC_APPEND=$(echo "${SRC_CMDLINE}" | grep '^tsc=reliable')
success "Source UUID: ${SRC_UUID}, MAC: ${SRC_MAC}"

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
    MIG_HELPER_PID=$(node_exec "${NODE2}" "${SUDO} crictl ps -q --label io.kubernetes.pod.name=mig-helper 2>/dev/null | head -1 | xargs -I{} ${SUDO} crictl inspect {} 2>/dev/null | python3 -c \"import sys,json; print(json.load(sys.stdin)['info']['pid'])\"" 2>/dev/null || true)
    if [[ -n "${MIG_HELPER_PID}" && "${MIG_HELPER_PID}" =~ ^[0-9]+$ ]]; then break; fi
    sleep 1
done
if [[ -z "${MIG_HELPER_PID}" || ! "${MIG_HELPER_PID}" =~ ^[0-9]+$ ]]; then
    error "Could not determine mig-helper container PID."
    exit 1
fi
success "Helper container PID: ${MIG_HELPER_PID}"

log "Setting up destination QEMU (manual, with -incoming defer)..."
# Start a standalone QEMU on Node 2 that matches the source VM's device
# topology. This bypasses Kata's sandbox lifecycle, which kills VMs that
# don't boot within its vsock timeout (incompatible with -incoming mode).
#
# Key fixes for live migration:
#   1. nvdimm uses a writable copy (source is readonly=on, but QEMU sends
#      nvdimm pages during migration; writing to readonly mmap = SIGSEGV)
#   2. virtiofsd started with --migration-on-error=guest-error so that
#      inode state mismatches between source/dest don't abort migration
#   3. Device IDs, UUID, and MAC extracted from the source QEMU to ensure
#      the destination has an identical device topology
DST_SANDBOX="manual-dst-$$"
DST_VM_DIR="/run/vc/vm/${DST_SANDBOX}"
DST_SOCK="${DST_VM_DIR}/extra-monitor.sock"
node_exec "${NODE2}" "${SUDO} mkdir -p ${DST_VM_DIR}"
node_exec "${NODE2}" "${SUDO} mkdir -p /run/kata-containers/shared/sandboxes/${DST_SANDBOX}/shared"

# Create a writable copy of the nvdimm image.
node_exec "${NODE2}" "${SUDO} cp /opt/kata/share/kata-containers/kata-ubuntu-noble.image /tmp/kata-dst-nvdimm-$$.img"

# Start virtiofsd for the vhost-user-fs device.
node_exec "${NODE2}" "${SUDO} /opt/kata/libexec/virtiofsd \
    --socket-path=${DST_VM_DIR}/vhost-fs.sock \
    --shared-dir=/run/kata-containers/shared/sandboxes/${DST_SANDBOX}/shared \
    --cache=auto --thread-pool-size=1 --announce-submounts \
    --sandbox=none --migration-on-error=guest-error &"
sleep 2
if ! node_exec "${NODE2}" "[ -S ${DST_VM_DIR}/vhost-fs.sock ]" 2>/dev/null; then
    error "virtiofsd socket not found."
    exit 1
fi

# Create tap interface in the helper pod's network namespace.
node_exec "${NODE2}" "${SUDO} nsenter --net=/proc/${MIG_HELPER_PID}/ns/net ip tuntap add dev tap0_kata mode tap" 2>/dev/null || true
node_exec "${NODE2}" "${SUDO} nsenter --net=/proc/${MIG_HELPER_PID}/ns/net ip link set tap0_kata up"

# Start destination QEMU with matching device topology.
node_exec "${NODE2}" "${SUDO} nsenter --net=/proc/${MIG_HELPER_PID}/ns/net \
    /opt/kata/bin/qemu-system-x86_64 \
    -name sandbox-${DST_SANDBOX},debug-threads=on \
    -uuid ${SRC_UUID} \
    -machine q35,accel=kvm,nvdimm=on \
    -cpu host,pmu=off \
    -qmp unix:path=${DST_VM_DIR}/qmp-primary.sock,server=on,wait=off \
    -qmp unix:path=${DST_SOCK},server=on,wait=off \
    -m 2048M,slots=10,maxmem=127437M \
    -device pci-bridge,bus=pcie.0,id=pci-bridge-0,chassis_nr=1,shpc=off,addr=2,io-reserve=4k,mem-reserve=1m,pref64-reserve=1m \
    -device virtio-serial-pci,disable-modern=false,id=serial0 \
    -device virtconsole,chardev=charconsole0,id=console0 \
    -chardev socket,id=charconsole0,path=${DST_VM_DIR}/console.sock,server=on,wait=off \
    -device nvdimm,id=nv0,memdev=mem0,unarmed=on \
    -object memory-backend-file,id=mem0,mem-path=/tmp/kata-dst-nvdimm-$$.img,size=268435456 \
    -device virtio-scsi-pci,id=scsi0,disable-modern=false \
    -object rng-random,id=rng0,filename=/dev/urandom \
    -device virtio-rng-pci,rng=rng0 \
    -device vhost-vsock-pci,disable-modern=false,id=${SRC_VSOCK_ID},guest-cid=88888888 \
    -chardev socket,id=${SRC_CHARDEV_FS},path=${DST_VM_DIR}/vhost-fs.sock \
    -device vhost-user-fs-pci,chardev=${SRC_CHARDEV_FS},tag=kataShared,queue-size=1024 \
    -netdev tap,id=network-0,vhost=on,ifname=tap0_kata,script=no,downscript=no \
    -device driver=virtio-net-pci,netdev=network-0,mac=${SRC_MAC},disable-modern=false,mq=on,vectors=4 \
    -rtc base=utc,driftfix=slew,clock=host \
    -global kvm-pit.lost_tick_policy=discard \
    -vga none -no-user-config -nodefaults -nographic --no-reboot \
    -object memory-backend-file,id=dimm1,size=2048M,mem-path=/dev/shm,share=on \
    -numa node,memdev=dimm1 \
    -kernel /opt/kata/share/kata-containers/vmlinux-6.12.47-173 \
    -append '${SRC_APPEND}' \
    -pidfile ${DST_VM_DIR}/pid \
    -smp 1,cores=1,threads=1,sockets=32,maxcpus=32 \
    -incoming defer \
    -daemonize"

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

# The destination QEMU runs inside the helper pod's network namespace.
# The tap interface (tap0_kata) is in that namespace. Pass the netns path
# so katamaran can run tc commands via nsenter.
DST_TAP="tap0_kata"
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
    log "Executing Live Migration (job mode)..."
    MIG_LOG=$(mktemp)
    "${PROJECT_ROOT}/deploy/migrate.sh" \
        --context "${CTX}" --source-node "${NODE1}" --dest-node "${NODE2}" \
        --tap "${DST_TAP}" --tap-netns "${DST_TAP_NETNS}" \
        --qmp-source "${SRC_SOCK}" --qmp-dest "${DST_SOCK}" \
        --dest-ip "${DST_POD_IP}" --vm-ip "${SRC_POD_IP}" \
        --image "localhost/katamaran:dev" --shared-storage --downtime 25 2>&1 | tee "${MIG_LOG}" || {
            error "Migration failed!"
            exit 1
        }
else
    error "SSH method not implemented."
    exit 1
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
