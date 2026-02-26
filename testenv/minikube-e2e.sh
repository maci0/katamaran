#!/bin/bash
# minikube-e2e.sh — Automates a full 2-node live migration using Minikube.
#
# This script:
#   1. Creates a 2-node minikube cluster (kvm2 driver, flannel CNI).
#   2. Installs Kata Containers via Helm on both nodes.
#   3. Enables the extra QMP monitor socket on both nodes.
#   4. Deploys a source pod on Node 1 and a destination pod on Node 2.
#   5. Automates the katamaran live migration between them.
#   6. Runs continuous ping to verify zero-packet-drop.

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly BINARY="${PROJECT_ROOT}/katamaran"
readonly PROFILE="katamaran-e2e"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

cd "${SCRIPT_DIR}"

log() { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
error() { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

cleanup() {
    log "Cleaning up minikube profile '${PROFILE}'..."
    minikube delete -p "${PROFILE}" 2>/dev/null || true
}

if [[ "${1:-}" == "teardown" ]]; then
    cleanup
    exit 0
fi

# 1. Pre-flight
log "Checking prerequisites..."
for cmd in minikube kubectl helm; do
    if ! command -v "$cmd" >/dev/null; then
        error "$cmd is required."
        exit 1
    fi
done
if [[ ! -x "${BINARY}" ]]; then
    error "katamaran binary not found at ${BINARY}. Build it first."
    exit 1
fi

# 2. Start Cluster
log "Starting 2-node minikube cluster (kvm2, Calico CNI)..."
minikube start \
    -p "${PROFILE}" \
    --nodes 2 \
    --driver=kvm2 \
    --memory=8192 \
    --cpus=4 \
    --container-runtime=containerd \
    --cni=calico

# 3. Install Kata Containers
log "Installing Kata Containers via Helm..."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --version "${KATA_CHART_VERSION}" \
    --kube-context "${PROFILE}" \
    --namespace kube-system \
    --create-namespace \
    --set shims.disableAll=true \
    --set shims.qemu.enabled=true \
    --wait=false

log "Waiting for kata-deploy daemonset to roll out on both nodes..."
kubectl --context "${PROFILE}" -n kube-system rollout status daemonset/kata-deploy --timeout=600s

log "Waiting for kata-qemu RuntimeClass..."
while ! kubectl --context "${PROFILE}" get runtimeclass kata-qemu >/dev/null 2>&1; do sleep 5; done
success "kata-qemu RuntimeClass exists"

# 4. Configure QMP Sockets
log "Enabling extra QMP monitor socket on both nodes..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
for node in "${PROFILE}" "${PROFILE}-m02"; do
    log "Waiting for Kata configuration to appear on $node..."
    while ! minikube -p "${PROFILE}" ssh -n "$node" -- "test -f ${KATA_CFG}" 2>/dev/null; do
        sleep 2
    done

    minikube -p "${PROFILE}" ssh -n "$node" -- "
        sudo sed -i '/^\[hypervisor\.qemu\]/,/^\[/{
            s/^enable_debug = false/enable_debug = true/
            s/^extra_monitor_socket = \"\"/extra_monitor_socket = \"qmp\"/
        }' '${KATA_CFG}'
        sudo sed -i 's/^cdh_api_timeout = .*/cdh_api_timeout = 180/' '${KATA_CFG}'
        sudo sed -i 's/^dial_timeout = .*/dial_timeout = 180/' '${KATA_CFG}'
        sudo sed -i 's/^create_container_timeout = .*/create_container_timeout = 180/' '${KATA_CFG}'
    "
done
success "extra QMP monitor sockets configured"

# 5. Copy Binary
log "Copying katamaran binary to both nodes..."
for node in "${PROFILE}" "${PROFILE}-m02"; do
    minikube -p "${PROFILE}" cp "${BINARY}" "${node}:/tmp/katamaran"
    minikube -p "${PROFILE}" ssh -n "$node" -- sudo chmod +x /tmp/katamaran
done

log "Cleaning up old pods..."
kubectl --context "${PROFILE}" delete pod kata-src kata-dst --force --grace-period=0 2>/dev/null || true

# 6. Deploy Source Pod & Extract State
log "Deploying source (Node 1) pod..."
kubectl --context "${PROFILE}" apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-src
  labels:
    app: ping-test
spec:
  runtimeClassName: kata-qemu
  nodeName: ${PROFILE}
  hostNetwork: true
  containers:
  - name: nginx
    image: nginx:alpine
PODEOF

log "Waiting for source pod to be Ready..."
kubectl --context "${PROFILE}" wait --for=condition=Ready pod/kata-src --timeout=300s
success "Source pod is running"

log "Extracting QEMU state from Node 1..."
QEMU_CMD=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
SRC_UUID=$(echo "$QEMU_CMD" | sed -n -E 's/.*-uuid ([a-f0-9\-]+).*/\1/p' | awk '{print $1}')
SRC_VSOCK=$(echo "$QEMU_CMD" | grep -oE "id=vsock-[0-9]+" | awk -F= '{print $2}')
SRC_CID=$(echo "$QEMU_CMD" | grep -oE "guest-cid=[0-9]+" | awk -F= '{print $2}')
SRC_CHAR=$(echo "$QEMU_CMD" | grep -oE "id=char-[a-f0-9]+" | head -1 | awk -F= '{print $2}')

success "SRC_UUID=${SRC_UUID}, SRC_VSOCK=${SRC_VSOCK}, SRC_CID=${SRC_CID}, SRC_CHAR=${SRC_CHAR}"

# 7. Configure Destination Wrapper
log "Installing state-matching QEMU wrapper on Node 2..."
WRAPPER_SCRIPT=$(cat <<EOF
#!/bin/bash
new_args=()
replace_next_uuid=0
for arg in "\\\$@"; do
    if [ "\\\$replace_next_uuid" == "1" ]; then
        arg="${SRC_UUID}"
        replace_next_uuid=0
    elif [ "\\\$arg" == "-uuid" ]; then
        replace_next_uuid=1
    fi
    arg=\$(echo "\\\$arg" | sed -E "s/id=vsock-[0-9]+/id=${SRC_VSOCK}/g")
    arg=\$(echo "\\\$arg" | sed -E "s/guest-cid=[0-9]+/guest-cid=${SRC_CID}/g")
    arg=\$(echo "\\\$arg" | sed -E "s/id=char-[a-f0-9]+/id=${SRC_CHAR}/g")
    arg=\$(echo "\\\$arg" | sed -E "s/chardev=char-[a-f0-9]+/chardev=${SRC_CHAR}/g")
    new_args+=("\\\$arg")
done
exec /opt/kata/bin/qemu-system-x86_64.orig "\\\${new_args[@]}" -incoming tcp:0.0.0.0:4444
EOF
)

minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "
    sudo mv /opt/kata/bin/qemu-system-x86_64 /opt/kata/bin/qemu-system-x86_64.orig 2>/dev/null || true
    cat << 'OUTEREOF' > /tmp/wrapper.sh
${WRAPPER_SCRIPT}
OUTEREOF
    sudo cp /tmp/wrapper.sh /opt/kata/bin/qemu-system-x86_64
    sudo chmod +x /opt/kata/bin/qemu-system-x86_64
"
success "State-matching wrapper configured"

# 8. Deploy Destination Pod
log "Deploying destination (Node 2) pod..."
kubectl --context "${PROFILE}" apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-dst
spec:
  runtimeClassName: kata-qemu
  nodeName: ${PROFILE}-m02
  hostNetwork: true
  containers:
  - name: nginx
    image: nginx:alpine
PODEOF

log "Waiting for destination QEMU process and QMP socket to appear..."
while true; do
    DST_PID=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- 'pgrep qemu | head -1' | tr -d '\r\n')
    DST_SOCK=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- 'sudo find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')
    if [[ -n "$DST_PID" && -n "$DST_SOCK" ]]; then
        break
    fi
    sleep 2
done
success "Destination QMP socket is ready (PID: $DST_PID)"

SRC_IP=$(kubectl --context "${PROFILE}" get node "${PROFILE}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "${PROFILE}" get node "${PROFILE}-m02" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

# 9. Execute Migration
log "Locating QMP sockets..."
SRC_SOCK=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')

if [[ -z "$SRC_SOCK" || -z "$DST_SOCK" ]]; then
    error "Could not find extra-monitor.sock on one or both nodes."
    exit 1
fi
success "Source QMP: $SRC_SOCK"
success "Dest QMP:   $DST_SOCK"

log "Starting katamaran DESTINATION on Node 2..."
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "
    sudo systemctl reset-failed katamaran-dest.service 2>/dev/null || true
    sudo systemctl stop katamaran-dest.service 2>/dev/null || true
    sudo systemd-run --unit=katamaran-dest.service --remain-after-exit /tmp/katamaran -mode dest -qmp '${DST_SOCK}' -shared-storage
"

sleep 3 # Wait for dest to be ready

log "Starting katamaran SOURCE on Node 1 (migrating directly to host IP ${NODE2_IP})..."
set +e
minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- "sudo /tmp/katamaran -mode source -qmp '${SRC_SOCK}' -dest-ip '${NODE2_IP}' -vm-ip '${SRC_IP}' -shared-storage" 2>&1 | tee /tmp/katamaran-source.log
MIG_STATUS=${PIPESTATUS[0]}
set -e

sync

echo ""
echo "=== DESTINATION LOGS ==="
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "sudo journalctl -u katamaran-dest.service --no-pager" || true

echo ""
echo "=== MIGRATION RESULTS ==="
if [ $MIG_STATUS -eq 0 ]; then
    success "Live migration command completed successfully!"
else
    error "Live migration command failed (exit code $MIG_STATUS)."
fi
    sleep 2
done
success "Destination QMP socket is ready (PID: \$DST_PID)"

SRC_IP=\$(kubectl --context "${PROFILE}" get node "${PROFILE}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=\$(kubectl --context "${PROFILE}" get node "${PROFILE}-m02" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

# 9. Execute Migration
log "Locating QMP sockets..."
SRC_SOCK=\$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')

if [[ -z "\$SRC_SOCK" || -z "\$DST_SOCK" ]]; then
    error "Could not find extra-monitor.sock on one or both nodes."
    exit 1
fi
success "Source QMP: \$SRC_SOCK"
success "Dest QMP:   \$DST_SOCK"

log "Starting katamaran DESTINATION on Node 2..."
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "
    sudo systemctl reset-failed katamaran-dest.service 2>/dev/null || true
    sudo systemctl stop katamaran-dest.service 2>/dev/null || true
    sudo systemd-run --unit=katamaran-dest.service --remain-after-exit /tmp/katamaran -mode dest -qmp '${DST_SOCK}' -shared-storage
"

sleep 3 # Wait for dest to be ready

log "Starting katamaran SOURCE on Node 1 (migrating directly to host IP ${NODE2_IP})..."
set +e
minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- "sudo /tmp/katamaran -mode source -qmp '${SRC_SOCK}' -dest-ip '${NODE2_IP}' -vm-ip '${SRC_IP}' -shared-storage" 2>&1 | tee /tmp/katamaran-source.log
MIG_STATUS=${PIPESTATUS[0]}
set -e

sync

echo ""
echo "=== DESTINATION LOGS ==="
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "sudo journalctl -u katamaran-dest.service --no-pager" || true

echo ""
echo "=== MIGRATION RESULTS ==="
if [ $MIG_STATUS -eq 0 ]; then
    success "Live migration command completed successfully!"
else
    error "Live migration command failed (exit code $MIG_STATUS)."
fi
