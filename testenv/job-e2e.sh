#!/bin/bash
# job-e2e.sh — Job-based two-node live migration E2E test using Kind + Podman.
#
# Alternative to kind-e2e.sh that uses Kubernetes Jobs to run the migration.
#
# This script:
#   1. Creates a 2-node Kind cluster (Podman provider, /dev/kvm mount).
#   2. Installs Kata Containers via Helm on both nodes.
#   3. Enables the extra QMP monitor socket on both nodes.
#   4. Deploys a source pod on the control-plane and a destination pod on the worker.
#   5. Starts a continuous ping to the VM pod IP before migration.
#   6. Runs katamaran migration via deploy/migrate.sh (K8s Jobs).
#   7. Reports migration results and zero-drop proof (ping statistics).

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly CLUSTER="katamaran-job-e2e"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

# Kind node container names
readonly NODE1="${CLUSTER}-control-plane"
readonly NODE2="${CLUSTER}-worker"

# Ping configuration for zero-drop proof
readonly PING_INTERVAL="0.05"
readonly PING_LOG="/tmp/katamaran-job-ping.log"
readonly PING_DEADLINE=300

cd "${SCRIPT_DIR}"

log()     { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
warn()    { echo -e "\033[1;33m  WARN: $1\033[0m"; }
error()   { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

node_exec() {
    local node="$1"
    shift
    podman exec "$node" bash -c "$*"
}

PING_PID=""

cleanup() {
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

# ─── 2. Create Kind Cluster ─────────────────────────────────────────────────
log "Creating 2-node Kind cluster (Podman provider, /dev/kvm mount)..."
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
kubectl --context "kind-${CLUSTER}" wait --for=condition=Ready node --all --timeout=120s

NODE1_IP=$(kubectl --context "kind-${CLUSTER}" get node "${NODE1}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "kind-${CLUSTER}" get node "${NODE2}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

# ─── 3. Install Kata Containers ─────────────────────────────────────────────
log "Installing Kata Containers..."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --version "${KATA_CHART_VERSION}" \
    --kube-context "kind-${CLUSTER}" \
    --namespace kube-system \
    --create-namespace \
    --set shims.disableAll=true \
    --set shims.qemu.enabled=true \
    --wait=false

kubectl --context "kind-${CLUSTER}" -n kube-system rollout status daemonset/kata-deploy --timeout=600s
while ! kubectl --context "kind-${CLUSTER}" get runtimeclass kata-qemu >/dev/null 2>&1; do sleep 5; done

# ─── 4. Configure QMP Sockets & Modules ──────────────────────────────────────
log "Configuring nodes (QMP sockets, kernel modules)..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
for node in "${NODE1}" "${NODE2}"; do
    while ! node_exec "$node" "test -f ${KATA_CFG}" 2>/dev/null; do sleep 2; done
    node_exec "$node" "
        sed -i '/^\[hypervisor\.qemu\]/,/^\[/{
            s/^enable_debug = false/enable_debug = true/
            s/^extra_monitor_socket = \"\"/extra_monitor_socket = \"qmp\"/
        }' '${KATA_CFG}'
        modprobe ipip 2>/dev/null || true
        modprobe ip_gre 2>/dev/null || true
        modprobe sch_plug 2>/dev/null || true
    "
done

# ─── 5. Build and Load Container Image ──────────────────────────────────────
log "Building and loading katamaran container image..."
podman build -t katamaran:dev "${PROJECT_ROOT}"
kind load docker-image katamaran:dev --name "${CLUSTER}"

# ─── 6. Deploy Source Pod & Extract State ───────────────────────────────────
log "Deploying source pod..."
kubectl --context "kind-${CLUSTER}" apply -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: kata-src
spec:
  runtimeClassName: kata-qemu
  nodeName: ${NODE1}
  containers:
  - name: nginx
    image: nginx:alpine
PODEOF

kubectl --context "kind-${CLUSTER}" wait --for=condition=Ready pod/kata-src --timeout=300s
POD_IP=$(kubectl --context "kind-${CLUSTER}" get pod kata-src -o jsonpath='{.status.podIP}')

log "Extracting QEMU state..."
QEMU_CMD=$(node_exec "${NODE1}" 'cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
SRC_UUID=$(echo "$QEMU_CMD" | sed -n -E 's/.*-uuid ([a-f0-9\-]+).*/\1/p' | awk '{print $1}')
SRC_VSOCK=$(echo "$QEMU_CMD" | grep -oE "id=vsock-[0-9]+" | awk -F= '{print $2}')
SRC_CID=$(echo "$QEMU_CMD" | grep -oE "guest-cid=[0-9]+" | awk -F= '{print $2}')
SRC_CHAR=$(echo "$QEMU_CMD" | grep -oE "id=char-[a-f0-9]+" | head -1 | awk -F= '{print $2}')

# ─── 7. Configure Destination Wrapper ───────────────────────────────────────
log "Installing QEMU wrapper on worker..."
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
    echo \"${WRAPPER_SCRIPT}\" > /tmp/wrapper.sh
    cp /tmp/wrapper.sh /opt/kata/bin/qemu-system-x86_64
    chmod +x /opt/kata/bin/qemu-system-x86_64
"

# ─── 8. Deploy Destination Pod ──────────────────────────────────────────────
log "Deploying destination pod..."
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
PODEOF

while true; do
    DST_PID=$(node_exec "${NODE2}" 'pgrep qemu | head -1' | tr -d '\r\n')
    DST_SOCK=$(node_exec "${NODE2}" 'find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')
    [[ -n "$DST_PID" && -n "$DST_SOCK" ]] && break
    sleep 2
done

# ─── 9. Start Continuous Ping ───────────────────────────────────────────────
log "Starting continuous ping for zero-drop proof..."
SRC_SOCK=$(node_exec "${NODE1}" 'find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')

podman exec "${NODE1}" ping -i "${PING_INTERVAL}" -w "${PING_DEADLINE}" -D "${POD_IP}" > "${PING_LOG}" 2>&1 &
PING_PID=$!
sleep 2

# ─── 10. Execute Migration (Job-Based) ──────────────────────────────────────
log "Executing migration via K8s Jobs..."
set +e
"${PROJECT_ROOT}/deploy/migrate.sh" \
    --source-node "${NODE1}" \
    --dest-node "${NODE2}" \
    --qmp-source "${SRC_SOCK}" \
    --qmp-dest "${DST_SOCK}" \
    --dest-ip "${NODE2_IP}" \
    --vm-ip "${POD_IP}" \
    --image "katamaran:dev" \
    --shared-storage \
    --context "kind-${CLUSTER}"
MIG_STATUS=$?
set -e

# ─── 11. Report Results ─────────────────────────────────────────────────────
log "Stopping ping and analyzing results..."
[[ -n "${PING_PID}" ]] && kill "${PING_PID}" 2>/dev/null && wait "${PING_PID}" 2>/dev/null || true

echo ""
echo "=== KIND+PODMAN JOB-BASED E2E MIGRATION RESULTS ==="
if [ $MIG_STATUS -eq 0 ]; then success "Migration Job completed successfully!"; else error "Migration Job failed!"; fi

if [[ -f "${PING_LOG}" ]]; then
    PING_SUMMARY=$(grep -E "packets transmitted" "${PING_LOG}" || true)
    echo "  ${PING_SUMMARY}"
    LOSS=$(echo "${PING_SUMMARY}" | grep -oE "[0-9]+%" | head -1)
    if [[ "${LOSS}" == "0%" ]]; then success "ZERO PACKET LOSS VERIFIED"; else error "PACKET LOSS DETECTED (${LOSS})"; fi
fi
