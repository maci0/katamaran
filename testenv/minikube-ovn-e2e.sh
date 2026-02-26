#!/bin/bash
# minikube-ovn-e2e.sh — Two-node live migration E2E test with OVN-Kubernetes CNI.
#
# This script validates katamaran's zero-packet-drop migration against
# OVN-Kubernetes, which provides the best CNI integration via its centralized
# OVN southbound DB for near-instant port-chassis rebinding.
#
# Differences from minikube-e2e.sh (Calico):
#   - Uses OVN-Kubernetes as the CNI instead of Calico
#   - Runs a continuous ping during migration to prove zero packet loss
#   - Loads the openvswitch kernel module on both nodes
#   - Reports detailed ping statistics as the zero-drop proof
#
# This script:
#   1. Creates a 2-node minikube cluster (kvm2 driver, no built-in CNI).
#   2. Loads the openvswitch kernel module and deploys OVN-Kubernetes via Helm.
#   3. Installs Kata Containers via Helm on both nodes.
#   4. Enables the extra QMP monitor socket on both nodes.
#   5. Deploys a source pod on Node 1 and a destination pod on Node 2.
#   6. Starts a continuous ping to the VM pod IP before migration.
#   7. Runs katamaran live migration and captures ping results.
#   8. Reports migration results and zero-drop proof (ping statistics).
#
# Prerequisites:
#   - Linux host with KVM and nested virtualization enabled
#   - minikube, kubectl, helm installed
#   - ~30 GB free disk space, ~20 GB free RAM
#   - katamaran binary built (go build -o katamaran ./cmd/katamaran/)
#
# Usage:
#   ./testenv/minikube-ovn-e2e.sh              # run full e2e, clean up on exit
#   ./testenv/minikube-ovn-e2e.sh teardown     # destroy cluster only

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly PROFILE="katamaran-ovn-e2e"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

# OVN-Kubernetes configuration
readonly OVN_K_REPO="https://github.com/ovn-org/ovn-kubernetes.git"
readonly OVN_K_BRANCH="master"
readonly POD_NETWORK="10.244.0.0/16/24"
readonly SERVICE_NETWORK="10.96.0.0/16"

# Ping configuration for zero-drop proof
readonly PING_INTERVAL="0.05"     # 50ms between pings (20/sec)
readonly PING_LOG="/tmp/katamaran-ovn-ping.log"
readonly PING_DEADLINE=300        # max 5 minutes of pinging

cd "${SCRIPT_DIR}"

log()     { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
warn()    { echo -e "\033[1;33m  WARN: $1\033[0m"; }
error()   { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

PING_PID=""

cleanup() {
    # Kill background ping if still running
    if [[ -n "${PING_PID}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
        kill "${PING_PID}" 2>/dev/null || true
        wait "${PING_PID}" 2>/dev/null || true
    fi
    log "Cleaning up minikube profile '${PROFILE}'..."
    minikube delete -p "${PROFILE}" 2>/dev/null || true
}

if [[ "${1:-}" == "teardown" ]]; then
    cleanup
    exit 0
fi

trap cleanup EXIT

# ─── 1. Pre-flight ──────────────────────────────────────────────────────────

log "Checking prerequisites..."
for cmd in minikube kubectl helm git podman; do
    if ! command -v "$cmd" >/dev/null; then
        error "$cmd is required."
        exit 1
    fi
done

# ─── 2. Start Cluster (no built-in CNI) ─────────────────────────────────────

log "Starting 2-node minikube cluster (kvm2, no built-in CNI)..."
minikube start \
    -p "${PROFILE}" \
    --nodes 2 \
    --driver=kvm2 \
    --memory=8192 \
    --cpus=4 \
    --container-runtime=containerd \
    --network-plugin=cni \
    --cni=false

# Get node IPs early — needed for OVN-K and later for migration.
NODE1_IP=$(kubectl --context "${PROFILE}" get node "${PROFILE}" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "${PROFILE}" get node "${PROFILE}-m02" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
success "Node 1 IP: ${NODE1_IP}"
success "Node 2 IP: ${NODE2_IP}"

# Get the API server address that OVN-K needs to connect to.
API_SERVER=$(kubectl --context "${PROFILE}" config view --minify \
    -o jsonpath='{.clusters[0].cluster.server}')
API_HOST=$(echo "${API_SERVER}" | sed -E 's|https?://([^:]+):.*|\1|')
API_PORT=$(echo "${API_SERVER}" | sed -E 's|.*:([0-9]+)$|\1|')
success "API server: ${API_HOST}:${API_PORT}"

# ─── 3. Load OVS kernel module ──────────────────────────────────────────────

log "Loading openvswitch kernel module on both nodes..."
for node in "${PROFILE}" "${PROFILE}-m02"; do
    minikube -p "${PROFILE}" ssh -n "$node" -- "sudo modprobe openvswitch 2>/dev/null || true"
    if minikube -p "${PROFILE}" ssh -n "$node" -- "lsmod | grep -q openvswitch"; then
        success "openvswitch loaded on $node"
    else
        warn "openvswitch module not found on $node (OVN-K may still work with its own OVS)"
    fi
done

# ─── 4. Deploy OVN-Kubernetes via Helm ───────────────────────────────────────

log "Cloning OVN-Kubernetes for Helm chart..."
OVN_K_DIR="/tmp/ovn-kubernetes-${PROFILE}"
rm -rf "${OVN_K_DIR}"
git clone --depth 1 --branch "${OVN_K_BRANCH}" "${OVN_K_REPO}" "${OVN_K_DIR}"

log "Installing OVN-Kubernetes via Helm..."
helm upgrade --install ovn-kubernetes "${OVN_K_DIR}/helm/ovn-kubernetes" \
    --kube-context "${PROFILE}" \
    --namespace ovn-kubernetes \
    --create-namespace \
    --set global.k8sServiceHost="${API_HOST}" \
    --set global.k8sServicePort="${API_PORT}" \
    --set global.podNetwork="${POD_NETWORK}" \
    --set global.serviceNetwork="${SERVICE_NETWORK}" \
    --set global.enableMulticast=true \
    --set tags.ovnkube-db=true \
    --set tags.ovnkube-master=true \
    --set tags.ovnkube-node=true \
    --wait=false

log "Waiting for OVN-Kubernetes pods to be ready..."
# Wait for the DaemonSets to roll out (ovnkube-node must be running on both nodes).
kubectl --context "${PROFILE}" -n ovn-kubernetes rollout status daemonset/ovnkube-node --timeout=600s 2>/dev/null || {
    # Fallback: wait for at least the key pods to be running.
    log "Waiting for OVN-K pods (fallback)..."
    for i in $(seq 1 120); do
        READY=$(kubectl --context "${PROFILE}" -n ovn-kubernetes get pods --no-headers 2>/dev/null | grep -c "Running" || true)
        TOTAL=$(kubectl --context "${PROFILE}" -n ovn-kubernetes get pods --no-headers 2>/dev/null | wc -l || true)
        if [[ "${READY}" -ge 3 && "${TOTAL}" -gt 0 ]]; then
            break
        fi
        sleep 5
    done
}
success "OVN-Kubernetes is deployed"

# Verify nodes are Ready (CNI is functional).
log "Waiting for all nodes to be Ready..."
kubectl --context "${PROFILE}" wait --for=condition=Ready node --all --timeout=300s
success "All nodes are Ready with OVN-Kubernetes CNI"

# Quick connectivity check: ensure CoreDNS pods can start (proves CNI is working).
log "Verifying CoreDNS is running (CNI health check)..."
for i in $(seq 1 60); do
    COREDNS_READY=$(kubectl --context "${PROFILE}" -n kube-system get pods -l k8s-app=kube-dns --no-headers 2>/dev/null | grep -c "Running" || true)
    if [[ "${COREDNS_READY}" -ge 1 ]]; then
        break
    fi
    sleep 5
done
if [[ "${COREDNS_READY}" -ge 1 ]]; then
    success "CoreDNS is running (CNI is functional)"
else
    warn "CoreDNS not fully ready yet; continuing anyway"
fi

# ─── 5. Install Kata Containers ─────────────────────────────────────────────

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

# ─── 6. Configure QMP Sockets ───────────────────────────────────────────────

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

# Also load ipip and sch_plug modules for the tunnel and qdisc.
log "Loading kernel modules for tunnel and qdisc..."
for node in "${PROFILE}" "${PROFILE}-m02"; do
    minikube -p "${PROFILE}" ssh -n "$node" -- "
        sudo modprobe ipip 2>/dev/null || true
        sudo modprobe sch_plug 2>/dev/null || true
    "
done
success "Kernel modules loaded"

# ─── 7. Deploy katamaran Binary ──────────────────────────────────
log "Building and deploying katamaran container image..."
podman build -t katamaran:dev "${PROJECT_ROOT}"
minikube -p "${PROFILE}" image load katamaran:dev
kubectl --context "${PROFILE}" apply -f "${PROJECT_ROOT}/deploy/daemonset.yaml"
kubectl --context "${PROFILE}" -n kube-system rollout status daemonset/katamaran-deploy --timeout=120s

log "Cleaning up old pods..."
kubectl --context "${PROFILE}" delete pod kata-src kata-dst --force --grace-period=0 2>/dev/null || true

# ─── 8. Deploy Source Pod & Extract QEMU State ──────────────────────────────

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
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80
PODEOF

log "Waiting for source pod to be Ready..."
kubectl --context "${PROFILE}" wait --for=condition=Ready pod/kata-src --timeout=300s
success "Source pod is running"

# Get the pod IP assigned by OVN-Kubernetes.
POD_IP=$(kubectl --context "${PROFILE}" get pod kata-src -o jsonpath='{.status.podIP}')
success "Source pod IP (OVN-K assigned): ${POD_IP}"

log "Extracting QEMU state from Node 1..."
QEMU_CMD=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
SRC_UUID=$(echo "$QEMU_CMD" | sed -n -E 's/.*-uuid ([a-f0-9\-]+).*/\1/p' | awk '{print $1}')
SRC_VSOCK=$(echo "$QEMU_CMD" | grep -oE "id=vsock-[0-9]+" | awk -F= '{print $2}')
SRC_CID=$(echo "$QEMU_CMD" | grep -oE "guest-cid=[0-9]+" | awk -F= '{print $2}')
SRC_CHAR=$(echo "$QEMU_CMD" | grep -oE "id=char-[a-f0-9]+" | head -1 | awk -F= '{print $2}')

success "SRC_UUID=${SRC_UUID}, SRC_VSOCK=${SRC_VSOCK}, SRC_CID=${SRC_CID}, SRC_CHAR=${SRC_CHAR}"

# ─── 9. Verify Pod Reachability Before Migration ────────────────────────────

log "Verifying pod is reachable before migration..."
if minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- "ping -c 3 -W 2 ${POD_IP}" >/dev/null 2>&1; then
    success "Pod ${POD_IP} is reachable from Node 1"
else
    # Pod networking may route differently; try from Node 2.
    if minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "ping -c 3 -W 2 ${POD_IP}" >/dev/null 2>&1; then
        success "Pod ${POD_IP} is reachable from Node 2"
    else
        warn "Pod ${POD_IP} not pingable from nodes (OVN-K routing may differ); continuing"
    fi
fi

# ─── 10. Configure Destination QEMU Wrapper ─────────────────────────────────

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

# ─── 11. Deploy Destination Pod ─────────────────────────────────────────────

log "Deploying destination (Node 2) pod..."
kubectl --context "${PROFILE}" apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-dst
spec:
  runtimeClassName: kata-qemu
  nodeName: ${PROFILE}-m02
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80
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

# ─── 12. Locate QMP Sockets ─────────────────────────────────────────────────

log "Locating QMP sockets..."
SRC_SOCK=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')

if [[ -z "$SRC_SOCK" || -z "$DST_SOCK" ]]; then
    error "Could not find extra-monitor.sock on one or both nodes."
    exit 1
fi
success "Source QMP: $SRC_SOCK"
success "Dest QMP:   $DST_SOCK"

# ─── 13. Start Continuous Ping (Zero-Drop Proof) ────────────────────────────

log "Starting continuous ping to pod ${POD_IP} for zero-drop proof..."
# Run ping from Node 1. During migration the IPIP tunnel on Node 1 will
# forward these pings to Node 2, where sch_plug buffers them.
minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- \
    "ping -i ${PING_INTERVAL} -w ${PING_DEADLINE} -D ${POD_IP}" \
    > "${PING_LOG}" 2>&1 &
PING_PID=$!
sleep 2  # Let a few pings land before migrating.

# Verify pings are actually working before we migrate.
if grep -q "bytes from" "${PING_LOG}" 2>/dev/null; then
    success "Pings are flowing to ${POD_IP}"
else
    warn "No ping replies yet; migration will proceed anyway"
fi

# ─── 14. Execute Migration ──────────────────────────────────────────────────

log "Starting katamaran DESTINATION on Node 2..."
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "
    sudo systemctl reset-failed katamaran-dest.service 2>/dev/null || true
    sudo systemctl stop katamaran-dest.service 2>/dev/null || true
    sudo systemd-run --unit=katamaran-dest.service --remain-after-exit /usr/local/bin/katamaran -mode dest -qmp '${DST_SOCK}' -shared-storage
"

sleep 3  # Wait for dest to be ready.

log "Starting katamaran SOURCE on Node 1 (migrating to ${NODE2_IP})..."
set +e
minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- \
    "sudo /usr/local/bin/katamaran -mode source -qmp '${SRC_SOCK}' -dest-ip '${NODE2_IP}' -vm-ip '${POD_IP}' -shared-storage" \
    2>&1 | tee /tmp/katamaran-ovn-source.log
MIG_STATUS=${PIPESTATUS[0]}
set -e

# ─── 15. Stop Ping & Collect Results ────────────────────────────────────────

log "Stopping continuous ping..."
if [[ -n "${PING_PID}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
    kill "${PING_PID}" 2>/dev/null || true
    wait "${PING_PID}" 2>/dev/null || true
fi
PING_PID=""  # Prevent double-kill in cleanup trap.
sync

# ─── 16. Report Results ─────────────────────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== OVN-KUBERNETES E2E MIGRATION RESULTS ==="
echo "========================================================================"

# Migration result
echo ""
echo "--- Migration Status ---"
if [ $MIG_STATUS -eq 0 ]; then
    success "Live migration completed successfully!"
else
    error "Live migration failed (exit code $MIG_STATUS)."
fi

# Destination logs
echo ""
echo "--- Destination Logs ---"
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- \
    "sudo journalctl -u katamaran-dest.service --no-pager" 2>/dev/null || true

# Source logs (already in tee output above, but repeat the key lines)
echo ""
echo "--- Source Log Highlights ---"
if [[ -f /tmp/katamaran-ovn-source.log ]]; then
    grep -E "(tunnel|STOP|RESUME|complete|cancel|failed|succeeded)" /tmp/katamaran-ovn-source.log || true
fi

# ─── 17. Zero-Drop Proof (Ping Analysis) ────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== ZERO-DROP PING PROOF ==="
echo "========================================================================"

if [[ -f "${PING_LOG}" ]]; then
    # Extract the summary line from ping output.
    PING_SUMMARY=$(grep -E "packets transmitted" "${PING_LOG}" || true)

    if [[ -n "${PING_SUMMARY}" ]]; then
        echo ""
        echo "  ${PING_SUMMARY}"
        echo ""

        # Parse transmitted, received, and loss.
        TX=$(echo "${PING_SUMMARY}" | grep -oE "^[0-9]+" | head -1)
        RX=$(echo "${PING_SUMMARY}" | grep -oE "[0-9]+ received" | grep -oE "[0-9]+")
        LOSS=$(echo "${PING_SUMMARY}" | grep -oE "[0-9]+%" | head -1)

        if [[ "${LOSS}" == "0%" ]]; then
            success "ZERO PACKET LOSS: ${TX} transmitted, ${RX} received, ${LOSS} loss"
        else
            DROPPED=$((TX - RX))
            error "PACKET LOSS DETECTED: ${TX} transmitted, ${RX} received, ${DROPPED} dropped (${LOSS})"
        fi

        # Show RTT statistics (the spike during cutover is expected).
        RTT_LINE=$(grep -E "rtt min/avg/max" "${PING_LOG}" || true)
        if [[ -n "${RTT_LINE}" ]]; then
            echo "  ${RTT_LINE}"
        fi

        # Show packets with elevated RTT (buffered during cutover).
        echo ""
        echo "--- Packets with elevated RTT (>10ms, likely buffered during cutover) ---"
        # Find lines with time= and extract those with high RTT.
        grep "bytes from" "${PING_LOG}" | awk -F'time=' '{
            if (NF > 1) {
                split($2, a, " ");
                rtt = a[1] + 0;
                if (rtt > 10.0) print "  " $0
            }
        }' | head -20
        echo ""
    else
        warn "No ping summary found (ping may not have completed)"
        echo "  Last 10 lines of ping log:"
        tail -10 "${PING_LOG}" | sed 's/^/  /'
    fi
else
    warn "Ping log not found at ${PING_LOG}"
fi

# ─── 18. OVN-K Specific Checks ──────────────────────────────────────────────

echo ""
echo "--- OVN-Kubernetes State ---"

# Show OVN-K pods status.
echo "  OVN-K pods:"
kubectl --context "${PROFILE}" -n ovn-kubernetes get pods --no-headers 2>/dev/null | sed 's/^/    /' || true

# Show OVN logical switch ports (if ovn-nbctl is available inside the pod).
echo ""
echo "  OVN logical switch ports:"
OVNKUBE_MASTER=$(kubectl --context "${PROFILE}" -n ovn-kubernetes get pods -l name=ovnkube-master --no-headers 2>/dev/null | awk '{print $1}' | head -1)
if [[ -n "${OVNKUBE_MASTER}" ]]; then
    kubectl --context "${PROFILE}" -n ovn-kubernetes exec "${OVNKUBE_MASTER}" -c ovnkube-master -- \
        ovn-nbctl --no-leader-only lsp-list "$(kubectl --context "${PROFILE}" -n ovn-kubernetes exec "${OVNKUBE_MASTER}" -c ovnkube-master -- ovn-nbctl --no-leader-only ls-list 2>/dev/null | head -1 | awk '{print $1}')" 2>/dev/null | head -10 | sed 's/^/    /' || true
fi

# ─── 19. Final Summary ──────────────────────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== SUMMARY ==="
echo "========================================================================"
echo ""
echo "  CNI:              OVN-Kubernetes"
echo "  Nodes:            ${PROFILE} (source) → ${PROFILE}-m02 (dest)"
echo "  Node IPs:         ${NODE1_IP} → ${NODE2_IP}"
echo "  Pod IP:           ${POD_IP}"
echo "  Storage:          shared (skipped NBD)"
echo "  Migration exit:   ${MIG_STATUS}"
if [[ -n "${PING_SUMMARY:-}" ]]; then
    echo "  Ping result:      ${PING_SUMMARY}"
fi
echo ""
echo "  Artifacts:"
echo "    Source log:      /tmp/katamaran-ovn-source.log"
echo "    Ping log:        ${PING_LOG}"
echo ""

if [[ $MIG_STATUS -eq 0 && "${LOSS:-100%}" == "0%" ]]; then
    success "OVN-KUBERNETES ZERO-DROP MIGRATION: VERIFIED"
elif [[ $MIG_STATUS -eq 0 ]]; then
    warn "Migration succeeded but ping showed loss (${LOSS:-unknown})"
else
    error "Migration failed (exit code ${MIG_STATUS})"
fi
