#!/bin/bash
# kind-e2e.sh — Two-node live migration E2E test using Kind + Podman.
#
# Alternative to minikube-e2e.sh for environments without KVM-based minikube.
# Kind uses container "nodes" (via Podman) instead of VMs, with /dev/kvm
# passed through for nested Kata Containers virtualization.
#
# This script:
#   1. Creates a 2-node Kind cluster (Podman provider, /dev/kvm mount).
#   2. Installs Kata Containers via Helm on both nodes.
#   3. Enables the extra QMP monitor socket on both nodes.
#   4. Deploys a source pod on the control-plane and a destination pod on the worker.
#   5. Starts a continuous ping to the VM pod IP before migration.
#   6. Runs katamaran live migration and captures ping results.
#   7. Reports migration results and zero-drop proof (ping statistics).
#
# Differences from minikube-e2e.sh:
#   - Uses Kind (Podman provider) instead of minikube (kvm2 driver)
#   - Node access via "podman exec" instead of "minikube ssh"
#   - Binary copy via "podman cp" instead of "minikube cp"
#   - Kind config mounts /dev/kvm into each node container
#   - Uses kindnet CNI (default) instead of Calico
#
# Prerequisites:
#   - Linux host with KVM and nested virtualization enabled
#   - kind, kubectl, helm, podman installed
#   - /dev/kvm accessible
#   - ~20 GB free disk space, ~16 GB free RAM
#   - katamaran binary built (go build -o katamaran ./cmd/katamaran/)
#
# Usage:
#   ./testenv/kind-e2e.sh              # run full e2e, clean up on exit
#   ./testenv/kind-e2e.sh teardown     # destroy cluster only

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly CLUSTER="katamaran-e2e"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

# Kind node container names follow the pattern: <cluster>-control-plane, <cluster>-worker
readonly NODE1="${CLUSTER}-control-plane"
readonly NODE2="${CLUSTER}-worker"

# Ping configuration for zero-drop proof
readonly PING_INTERVAL="0.05"     # 50ms between pings (20/sec)
readonly PING_LOG="/tmp/katamaran-kind-ping.log"
readonly PING_DEADLINE=300        # max 5 minutes of pinging

cd "${SCRIPT_DIR}"

log()     { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
warn()    { echo -e "\033[1;33m  WARN: $1\033[0m"; }
error()   { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

# node_exec runs a command inside a Kind node container via podman.
node_exec() {
    local node="$1"
    shift
    podman exec "$node" bash -c "$*"
}

PING_PID=""

cleanup() {
    # Kill background ping if still running.
    if [[ -n "${PING_PID}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
        kill "${PING_PID}" 2>/dev/null || true
        wait "${PING_PID}" 2>/dev/null || true
    fi
    log "Cleaning up Kind cluster '${CLUSTER}'..."
    kind delete cluster --name "${CLUSTER}" 2>/dev/null || true
}

if [[ "${1:-}" == "teardown" ]]; then
    cleanup
    exit 0
fi

trap cleanup EXIT

# ─── 1. Pre-flight ──────────────────────────────────────────────────────────

log "Checking prerequisites..."
for cmd in kind kubectl helm podman; do
    if ! command -v "$cmd" >/dev/null; then
        error "$cmd is required."
        exit 1
    fi
done
if [[ ! -c /dev/kvm ]]; then
    error "/dev/kvm not found. KVM is required for Kata Containers."
    exit 1
fi

# Kind with Podman requires rootful Podman (systemd socket).
# Check if we can reach the Podman socket.
if ! podman info >/dev/null 2>&1; then
    error "podman is not functional. Ensure rootful Podman is available."
    exit 1
fi

# ─── 2. Create Kind Cluster ─────────────────────────────────────────────────

log "Creating 2-node Kind cluster (Podman provider, /dev/kvm mount)..."

# Generate Kind config with /dev/kvm mount on both nodes.
KIND_CONFIG=$(mktemp /tmp/kind-config-XXXXXX.yaml)
cat > "${KIND_CONFIG}" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
- role: worker
  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
EOF

KIND_EXPERIMENTAL_PROVIDER=podman kind create cluster \
    --name "${CLUSTER}" \
    --config "${KIND_CONFIG}" \
    --wait 120s

rm -f "${KIND_CONFIG}"

# Verify both nodes are Ready.
kubectl --context "kind-${CLUSTER}" wait --for=condition=Ready node --all --timeout=120s
success "Kind cluster is ready with 2 nodes"

# Get node IPs (internal IPs assigned by Kind's container network).
NODE1_IP=$(kubectl --context "kind-${CLUSTER}" get node "${NODE1}" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "kind-${CLUSTER}" get node "${NODE2}" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
success "Node 1 (control-plane) IP: ${NODE1_IP}"
success "Node 2 (worker) IP: ${NODE2_IP}"

# ─── 3. Verify /dev/kvm Inside Nodes ────────────────────────────────────────

log "Verifying /dev/kvm is accessible inside Kind nodes..."
for node in "${NODE1}" "${NODE2}"; do
    if node_exec "$node" "test -c /dev/kvm"; then
        success "/dev/kvm accessible in $node"
    else
        error "/dev/kvm not accessible in $node. Cannot run Kata Containers."
        exit 1
    fi
done

# ─── 4. Install Kata Containers ─────────────────────────────────────────────

log "Installing Kata Containers via Helm..."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --version "${KATA_CHART_VERSION}" \
    --kube-context "kind-${CLUSTER}" \
    --namespace kube-system \
    --create-namespace \
    --set shims.disableAll=true \
    --set shims.qemu.enabled=true \
    --wait=false

log "Waiting for kata-deploy daemonset to roll out on both nodes..."
kubectl --context "kind-${CLUSTER}" -n kube-system rollout status daemonset/kata-deploy --timeout=600s

log "Waiting for kata-qemu RuntimeClass..."
while ! kubectl --context "kind-${CLUSTER}" get runtimeclass kata-qemu >/dev/null 2>&1; do sleep 5; done
success "kata-qemu RuntimeClass exists"

# ─── 5. Configure QMP Sockets ───────────────────────────────────────────────

log "Enabling extra QMP monitor socket on both nodes..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
for node in "${NODE1}" "${NODE2}"; do
    log "Waiting for Kata configuration to appear on $node..."
    while ! node_exec "$node" "test -f ${KATA_CFG}" 2>/dev/null; do
        sleep 2
    done

    node_exec "$node" "
        sed -i '/^\[hypervisor\.qemu\]/,/^\[/{
            s/^enable_debug = false/enable_debug = true/
            s/^extra_monitor_socket = \"\"/extra_monitor_socket = \"qmp\"/
        }' '${KATA_CFG}'
        sed -i 's/^cdh_api_timeout = .*/cdh_api_timeout = 180/' '${KATA_CFG}'
        sed -i 's/^dial_timeout = .*/dial_timeout = 180/' '${KATA_CFG}'
        sed -i 's/^create_container_timeout = .*/create_container_timeout = 180/' '${KATA_CFG}'
    "
done
success "extra QMP monitor sockets configured"

# Load kernel modules for tunnel and qdisc inside each node.
log "Loading kernel modules for tunnel and qdisc..."
for node in "${NODE1}" "${NODE2}"; do
    node_exec "$node" "
        modprobe ipip 2>/dev/null || true
        modprobe ip6_tunnel 2>/dev/null || true
        modprobe ip_gre 2>/dev/null || true
        modprobe sch_plug 2>/dev/null || true
    "
done
success "Kernel modules loaded"

# ─── 6. Deploy katamaran Binary ──────────────────────────────────
log "Building and deploying katamaran container image..."
podman build -t katamaran:dev "${PROJECT_ROOT}"
kind load docker-image katamaran:dev --name "${CLUSTER}"
kubectl --context "kind-${CLUSTER}" apply -f "${PROJECT_ROOT}/deploy/daemonset.yaml"
kubectl --context "kind-${CLUSTER}" -n kube-system rollout status daemonset/katamaran-deploy --timeout=120s

log "Cleaning up old pods..."
kubectl --context "kind-${CLUSTER}" delete pod kata-src kata-dst --force --grace-period=0 2>/dev/null || true

# ─── 7. Deploy Source Pod & Extract QEMU State ──────────────────────────────

log "Deploying source (control-plane) pod..."
kubectl --context "kind-${CLUSTER}" apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-src
  labels:
    app: ping-test
spec:
  runtimeClassName: kata-qemu
  nodeName: ${NODE1}
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80
PODEOF

log "Waiting for source pod to be Ready..."
kubectl --context "kind-${CLUSTER}" wait --for=condition=Ready pod/kata-src --timeout=300s
success "Source pod is running"

# Get the pod IP.
POD_IP=$(kubectl --context "kind-${CLUSTER}" get pod kata-src -o jsonpath='{.status.podIP}')
success "Source pod IP: ${POD_IP}"

log "Extracting QEMU state from control-plane node..."
QEMU_CMD=$(node_exec "${NODE1}" 'cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
SRC_UUID=$(echo "$QEMU_CMD" | sed -n -E 's/.*-uuid ([a-f0-9\-]+).*/\1/p' | awk '{print $1}')
SRC_VSOCK=$(echo "$QEMU_CMD" | grep -oE "id=vsock-[0-9]+" | awk -F= '{print $2}')
SRC_CID=$(echo "$QEMU_CMD" | grep -oE "guest-cid=[0-9]+" | awk -F= '{print $2}')
SRC_CHAR=$(echo "$QEMU_CMD" | grep -oE "id=char-[a-f0-9]+" | head -1 | awk -F= '{print $2}')

success "SRC_UUID=${SRC_UUID}, SRC_VSOCK=${SRC_VSOCK}, SRC_CID=${SRC_CID}, SRC_CHAR=${SRC_CHAR}"

# ─── 8. Verify Pod Reachability Before Migration ────────────────────────────

log "Verifying pod is reachable before migration..."
if node_exec "${NODE1}" "ping -c 3 -W 2 ${POD_IP}" >/dev/null 2>&1; then
    success "Pod ${POD_IP} is reachable from control-plane"
elif node_exec "${NODE2}" "ping -c 3 -W 2 ${POD_IP}" >/dev/null 2>&1; then
    success "Pod ${POD_IP} is reachable from worker"
else
    warn "Pod ${POD_IP} not pingable from nodes; continuing"
fi

# ─── 9. Configure Destination QEMU Wrapper ──────────────────────────────────

log "Installing state-matching QEMU wrapper on worker node..."
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

node_exec "${NODE2}" "
    mv /opt/kata/bin/qemu-system-x86_64 /opt/kata/bin/qemu-system-x86_64.orig 2>/dev/null || true
    cat << 'OUTEREOF' > /tmp/wrapper.sh
${WRAPPER_SCRIPT}
OUTEREOF
    cp /tmp/wrapper.sh /opt/kata/bin/qemu-system-x86_64
    chmod +x /opt/kata/bin/qemu-system-x86_64
"
success "State-matching wrapper configured"

# ─── 10. Deploy Destination Pod ─────────────────────────────────────────────

log "Deploying destination (worker) pod..."
kubectl --context "kind-${CLUSTER}" apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-dst
spec:
  runtimeClassName: kata-qemu
  nodeName: ${NODE2}
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80
PODEOF

log "Waiting for destination QEMU process and QMP socket to appear..."
while true; do
    DST_PID=$(node_exec "${NODE2}" 'pgrep qemu | head -1' | tr -d '\r\n')
    DST_SOCK=$(node_exec "${NODE2}" 'find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')
    if [[ -n "$DST_PID" && -n "$DST_SOCK" ]]; then
        break
    fi
    sleep 2
done
success "Destination QMP socket is ready (PID: $DST_PID)"

# ─── 11. Locate QMP Sockets ─────────────────────────────────────────────────

log "Locating QMP sockets..."
SRC_SOCK=$(node_exec "${NODE1}" 'find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')

if [[ -z "$SRC_SOCK" || -z "$DST_SOCK" ]]; then
    error "Could not find extra-monitor.sock on one or both nodes."
    exit 1
fi
success "Source QMP: $SRC_SOCK"
success "Dest QMP:   $DST_SOCK"

# ─── 12. Start Continuous Ping (Zero-Drop Proof) ────────────────────────────

log "Starting continuous ping to pod ${POD_IP} for zero-drop proof..."
# Run ping from the control-plane node container.
podman exec "${NODE1}" \
    ping -i "${PING_INTERVAL}" -w "${PING_DEADLINE}" -D "${POD_IP}" \
    > "${PING_LOG}" 2>&1 &
PING_PID=$!
sleep 2  # Let a few pings land before migrating.

# Verify pings are actually working.
if grep -q "bytes from" "${PING_LOG}" 2>/dev/null; then
    success "Pings are flowing to ${POD_IP}"
else
    warn "No ping replies yet; migration will proceed anyway"
fi

# ─── 13. Execute Migration ──────────────────────────────────────────────────

log "Starting katamaran DESTINATION on worker node..."
# Use systemd inside the Kind node container (Kind nodes run systemd).
node_exec "${NODE2}" "
    systemctl reset-failed katamaran-dest.service 2>/dev/null || true
    systemctl stop katamaran-dest.service 2>/dev/null || true
    systemd-run --unit=katamaran-dest.service --remain-after-exit /usr/local/bin/katamaran -mode dest -qmp '${DST_SOCK}' -shared-storage
"

sleep 3  # Wait for dest to be ready.

log "Starting katamaran SOURCE on control-plane (migrating to ${NODE2_IP})..."
set +e
node_exec "${NODE1}" \
    "/usr/local/bin/katamaran -mode source -qmp '${SRC_SOCK}' -dest-ip '${NODE2_IP}' -vm-ip '${POD_IP}' -shared-storage" \
    2>&1 | tee /tmp/katamaran-kind-source.log
MIG_STATUS=${PIPESTATUS[0]}
set -e

# ─── 14. Stop Ping & Collect Results ────────────────────────────────────────

log "Stopping continuous ping..."
if [[ -n "${PING_PID}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
    kill "${PING_PID}" 2>/dev/null || true
    wait "${PING_PID}" 2>/dev/null || true
fi
PING_PID=""  # Prevent double-kill in cleanup trap.
sync

# ─── 15. Report Results ─────────────────────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== KIND+PODMAN E2E MIGRATION RESULTS ==="
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
node_exec "${NODE2}" "journalctl -u katamaran-dest.service --no-pager" 2>/dev/null || true

# Source logs
echo ""
echo "--- Source Log Highlights ---"
if [[ -f /tmp/katamaran-kind-source.log ]]; then
    grep -E "(tunnel|STOP|RESUME|complete|cancel|failed|succeeded)" /tmp/katamaran-kind-source.log || true
fi

# ─── 16. Zero-Drop Proof (Ping Analysis) ────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== ZERO-DROP PING PROOF ==="
echo "========================================================================"

if [[ -f "${PING_LOG}" ]]; then
    PING_SUMMARY=$(grep -E "packets transmitted" "${PING_LOG}" || true)

    if [[ -n "${PING_SUMMARY}" ]]; then
        echo ""
        echo "  ${PING_SUMMARY}"
        echo ""

        TX=$(echo "${PING_SUMMARY}" | grep -oE "^[0-9]+" | head -1)
        RX=$(echo "${PING_SUMMARY}" | grep -oE "[0-9]+ received" | grep -oE "[0-9]+")
        LOSS=$(echo "${PING_SUMMARY}" | grep -oE "[0-9]+%" | head -1)

        if [[ "${LOSS}" == "0%" ]]; then
            success "ZERO PACKET LOSS: ${TX} transmitted, ${RX} received, ${LOSS} loss"
        else
            DROPPED=$((TX - RX))
            error "PACKET LOSS DETECTED: ${TX} transmitted, ${RX} received, ${DROPPED} dropped (${LOSS})"
        fi

        RTT_LINE=$(grep -E "rtt min/avg/max" "${PING_LOG}" || true)
        if [[ -n "${RTT_LINE}" ]]; then
            echo "  ${RTT_LINE}"
        fi

        echo ""
        echo "--- Packets with elevated RTT (>10ms, likely buffered during cutover) ---"
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

# ─── 17. Final Summary ──────────────────────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== SUMMARY ==="
echo "========================================================================"
echo ""
echo "  Provider:         Kind + Podman"
echo "  CNI:              kindnet (default)"
echo "  Nodes:            ${NODE1} (source) → ${NODE2} (dest)"
echo "  Node IPs:         ${NODE1_IP} → ${NODE2_IP}"
echo "  Pod IP:           ${POD_IP}"
echo "  Storage:          shared (skipped NBD)"
echo "  Migration exit:   ${MIG_STATUS}"
if [[ -n "${PING_SUMMARY:-}" ]]; then
    echo "  Ping result:      ${PING_SUMMARY}"
fi
echo ""
echo "  Artifacts:"
echo "    Source log:      /tmp/katamaran-kind-source.log"
echo "    Ping log:        ${PING_LOG}"
echo ""

if [[ $MIG_STATUS -eq 0 && "${LOSS:-100%}" == "0%" ]]; then
    success "KIND+PODMAN ZERO-DROP MIGRATION: VERIFIED"
elif [[ $MIG_STATUS -eq 0 ]]; then
    warn "Migration succeeded but ping showed loss (${LOSS:-unknown})"
else
    error "Migration failed (exit code ${MIG_STATUS})"
fi
