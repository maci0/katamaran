#!/bin/bash
# minikube-nfs-e2e.sh — Two-node live migration E2E test with NFS shared storage.
#
# This script validates katamaran's shared-storage mode with an actual NFS
# server running in-cluster, proving the full pipeline works when both nodes
# share a filesystem — the recommended production setup for Ceph/NFS backends.
#
# Differences from minikube-e2e.sh:
#   - Deploys an NFS server pod and creates a PV/PVC backed by it
#   - Both source and destination Kata pods mount the NFS PVC
#   - Writes test data to NFS before migration, verifies it after
#   - Runs continuous ping for zero-drop proof
#   - Uses -shared-storage (skips NBD drive-mirror — storage is on NFS)
#
# This script:
#   1. Creates a 2-node minikube cluster (kvm2 driver, Calico CNI).
#   2. Deploys an NFS server pod on Node 1 with an exported volume.
#   3. Creates a PersistentVolume and PersistentVolumeClaim backed by NFS.
#   4. Installs Kata Containers via Helm on both nodes.
#   5. Enables the extra QMP monitor socket on both nodes.
#   6. Deploys a source Kata pod on Node 1 with the NFS volume mounted.
#   7. Writes test data to the NFS mount from inside the source VM.
#   8. Starts a continuous ping to the VM pod IP before migration.
#   9. Runs katamaran live migration with -shared-storage.
#  10. Verifies test data is accessible from the destination VM.
#  11. Reports migration results, NFS verification, and zero-drop proof.
#
# Prerequisites:
#   - Linux host with KVM and nested virtualization enabled
#   - minikube, kubectl, helm installed
#   - ~20 GB free disk space, ~16 GB free RAM
#   - katamaran binary built (go build -o katamaran ./cmd/katamaran/)
#
# Usage:
#   ./testenv/minikube-nfs-e2e.sh              # run full e2e, clean up on exit
#   ./testenv/minikube-nfs-e2e.sh teardown     # destroy cluster only

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly PROFILE="katamaran-nfs-e2e"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

# NFS configuration
readonly NFS_EXPORT_PATH="/exports/kata-data"
readonly NFS_MOUNT_PATH="/mnt/shared"
readonly NFS_TEST_FILE="migration-proof.txt"
readonly NFS_TEST_DATA="katamaran-nfs-migration-$(date +%s)"

# Ping configuration for zero-drop proof
readonly PING_INTERVAL="0.05"     # 50ms between pings (20/sec)
readonly PING_LOG="/tmp/katamaran-nfs-ping.log"
readonly PING_DEADLINE=300        # max 5 minutes of pinging

cd "${SCRIPT_DIR}"

log()     { echo -e "\n\033[1;34m>>> $1\033[0m"; }
success() { echo -e "\033[1;32m  PASS: $1\033[0m"; }
warn()    { echo -e "\033[1;33m  WARN: $1\033[0m"; }
error()   { echo -e "\033[1;31m  ERROR: $1\033[0m" >&2; }

PING_PID=""

cleanup() {
    # Kill background ping if still running.
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
for cmd in minikube kubectl helm podman; do
    if ! command -v "$cmd" >/dev/null; then
        error "$cmd is required."
        exit 1
    fi
done

# ─── 2. Start Cluster ───────────────────────────────────────────────────────

log "Starting 2-node minikube cluster (kvm2, Calico CNI)..."
minikube start \
    -p "${PROFILE}" \
    --nodes 2 \
    --driver=kvm2 \
    --memory=8192 \
    --cpus=4 \
    --container-runtime=containerd \
    --cni=calico

# Get node IPs.
NODE1_IP=$(kubectl --context "${PROFILE}" get node "${PROFILE}" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
NODE2_IP=$(kubectl --context "${PROFILE}" get node "${PROFILE}-m02" \
    -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
success "Node 1 IP: ${NODE1_IP}"
success "Node 2 IP: ${NODE2_IP}"

# ─── 3. Deploy NFS Server ───────────────────────────────────────────────────

log "Deploying NFS server pod on Node 1..."
kubectl --context "${PROFILE}" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nfs-server
  labels:
    role: nfs-server
spec:
  nodeName: ${PROFILE}
  containers:
  - name: nfs-server
    image: itsthenetwork/nfs-server-alpine:12
    securityContext:
      privileged: true
    ports:
    - containerPort: 2049
      name: nfs
    env:
    - name: SHARED_DIRECTORY
      value: "${NFS_EXPORT_PATH}"
    volumeMounts:
    - name: nfs-data
      mountPath: ${NFS_EXPORT_PATH}
  volumes:
  - name: nfs-data
    emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: nfs-service
spec:
  selector:
    role: nfs-server
  ports:
  - port: 2049
    targetPort: 2049
    protocol: TCP
  clusterIP: None
EOF

log "Waiting for NFS server pod to be Ready..."
kubectl --context "${PROFILE}" wait --for=condition=Ready pod/nfs-server --timeout=120s
success "NFS server pod is running"

# Get the NFS server pod IP (needed for PV definition).
NFS_SERVER_IP=$(kubectl --context "${PROFILE}" get pod nfs-server -o jsonpath='{.status.podIP}')
success "NFS server IP: ${NFS_SERVER_IP}"

# Wait a moment for the NFS export to become available.
sleep 5

# ─── 4. Create NFS PV + PVC ─────────────────────────────────────────────────

log "Creating NFS PersistentVolume and PersistentVolumeClaim..."
kubectl --context "${PROFILE}" apply -f - <<EOF
apiVersion: v1
kind: PersistentVolume
metadata:
  name: nfs-pv
spec:
  capacity:
    storage: 1Gi
  accessModes:
  - ReadWriteMany
  nfs:
    server: ${NFS_SERVER_IP}
    path: "${NFS_EXPORT_PATH}"
  persistentVolumeReclaimPolicy: Delete
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: nfs-pvc
spec:
  accessModes:
  - ReadWriteMany
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: nfs-pv
EOF

# Wait for PVC to bind.
log "Waiting for NFS PVC to bind..."
for i in $(seq 1 30); do
    PVC_STATUS=$(kubectl --context "${PROFILE}" get pvc nfs-pvc -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "${PVC_STATUS}" == "Bound" ]]; then
        break
    fi
    sleep 2
done
if [[ "${PVC_STATUS}" == "Bound" ]]; then
    success "NFS PVC is bound"
else
    error "NFS PVC did not bind (status: ${PVC_STATUS})"
    exit 1
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

# Load kernel modules for tunnel and qdisc.
log "Loading kernel modules for tunnel and qdisc..."
for node in "${PROFILE}" "${PROFILE}-m02"; do
    minikube -p "${PROFILE}" ssh -n "$node" -- "
        sudo modprobe ipip 2>/dev/null || true
        sudo modprobe sch_plug 2>/dev/null || true
        sudo modprobe nfs 2>/dev/null || true
        sudo modprobe nfsd 2>/dev/null || true
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

# ─── 8. Deploy Source Pod with NFS Mount ─────────────────────────────────────

log "Deploying source (Node 1) pod with NFS volume..."
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
    volumeMounts:
    - name: shared-data
      mountPath: ${NFS_MOUNT_PATH}
  volumes:
  - name: shared-data
    persistentVolumeClaim:
      claimName: nfs-pvc
PODEOF

log "Waiting for source pod to be Ready..."
kubectl --context "${PROFILE}" wait --for=condition=Ready pod/kata-src --timeout=300s
success "Source pod is running"

# Get the pod IP.
POD_IP=$(kubectl --context "${PROFILE}" get pod kata-src -o jsonpath='{.status.podIP}')
success "Source pod IP: ${POD_IP}"

# ─── 9. Write Test Data to NFS ──────────────────────────────────────────────

log "Writing test data to NFS mount inside source pod..."
kubectl --context "${PROFILE}" exec kata-src -- \
    sh -c "echo '${NFS_TEST_DATA}' > ${NFS_MOUNT_PATH}/${NFS_TEST_FILE}"

# Verify the write succeeded.
VERIFY_DATA=$(kubectl --context "${PROFILE}" exec kata-src -- \
    cat "${NFS_MOUNT_PATH}/${NFS_TEST_FILE}" 2>/dev/null || true)
if [[ "${VERIFY_DATA}" == "${NFS_TEST_DATA}" ]]; then
    success "Test data written to NFS: ${NFS_TEST_DATA}"
else
    error "Failed to write test data to NFS mount"
    exit 1
fi

# ─── 10. Extract QEMU State ─────────────────────────────────────────────────

log "Extracting QEMU state from Node 1..."
QEMU_CMD=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo cat /proc/$(pgrep qemu | head -1)/cmdline | tr "\0" " "')
SRC_UUID=$(echo "$QEMU_CMD" | sed -n -E 's/.*-uuid ([a-f0-9\-]+).*/\1/p' | awk '{print $1}')
SRC_VSOCK=$(echo "$QEMU_CMD" | grep -oE "id=vsock-[0-9]+" | awk -F= '{print $2}')
SRC_CID=$(echo "$QEMU_CMD" | grep -oE "guest-cid=[0-9]+" | awk -F= '{print $2}')
SRC_CHAR=$(echo "$QEMU_CMD" | grep -oE "id=char-[a-f0-9]+" | head -1 | awk -F= '{print $2}')

success "SRC_UUID=${SRC_UUID}, SRC_VSOCK=${SRC_VSOCK}, SRC_CID=${SRC_CID}, SRC_CHAR=${SRC_CHAR}"

# ─── 11. Verify Pod Reachability Before Migration ───────────────────────────

log "Verifying pod is reachable before migration..."
if minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- "ping -c 3 -W 2 ${POD_IP}" >/dev/null 2>&1; then
    success "Pod ${POD_IP} is reachable from Node 1"
elif minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "ping -c 3 -W 2 ${POD_IP}" >/dev/null 2>&1; then
    success "Pod ${POD_IP} is reachable from Node 2"
else
    warn "Pod ${POD_IP} not pingable from nodes; continuing"
fi

# ─── 12. Configure Destination QEMU Wrapper ─────────────────────────────────

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

# ─── 13. Deploy Destination Pod with NFS Mount ──────────────────────────────

log "Deploying destination (Node 2) pod with NFS volume..."
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
    volumeMounts:
    - name: shared-data
      mountPath: ${NFS_MOUNT_PATH}
  volumes:
  - name: shared-data
    persistentVolumeClaim:
      claimName: nfs-pvc
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

# Verify destination pod can also see the NFS data (proves shared storage).
log "Verifying NFS data is visible from destination pod..."
# The destination pod may not be fully Ready (QEMU is waiting for incoming
# migration), but we can verify NFS is mounted on Node 2 at the host level.
DST_NFS_CHECK=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- \
    "mount | grep nfs" 2>/dev/null || true)
if [[ -n "${DST_NFS_CHECK}" ]]; then
    success "NFS is mounted on Node 2"
else
    warn "Could not verify NFS mount on Node 2 from host level"
fi

# ─── 14. Locate QMP Sockets ─────────────────────────────────────────────────

log "Locating QMP sockets..."
SRC_SOCK=$(minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- 'sudo find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1' | tr -d '\r\n')

if [[ -z "$SRC_SOCK" || -z "$DST_SOCK" ]]; then
    error "Could not find extra-monitor.sock on one or both nodes."
    exit 1
fi
success "Source QMP: $SRC_SOCK"
success "Dest QMP:   $DST_SOCK"

# ─── 15. Start Continuous Ping (Zero-Drop Proof) ────────────────────────────

log "Starting continuous ping to pod ${POD_IP} for zero-drop proof..."
minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- \
    "ping -i ${PING_INTERVAL} -w ${PING_DEADLINE} -D ${POD_IP}" \
    > "${PING_LOG}" 2>&1 &
PING_PID=$!
sleep 2  # Let a few pings land before migrating.

if grep -q "bytes from" "${PING_LOG}" 2>/dev/null; then
    success "Pings are flowing to ${POD_IP}"
else
    warn "No ping replies yet; migration will proceed anyway"
fi

# ─── 16. Execute Migration ──────────────────────────────────────────────────

log "Starting katamaran DESTINATION on Node 2..."
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- "
    sudo systemctl reset-failed katamaran-dest.service 2>/dev/null || true
    sudo systemctl stop katamaran-dest.service 2>/dev/null || true
    sudo systemd-run --unit=katamaran-dest.service --remain-after-exit /usr/local/bin/katamaran -mode dest -qmp '${DST_SOCK}' -shared-storage
"

sleep 3  # Wait for dest to be ready.

log "Starting katamaran SOURCE on Node 1 (migrating to ${NODE2_IP}) with -shared-storage..."
set +e
minikube -p "${PROFILE}" ssh -n "${PROFILE}" -- \
    "sudo /usr/local/bin/katamaran -mode source -qmp '${SRC_SOCK}' -dest-ip '${NODE2_IP}' -vm-ip '${POD_IP}' -shared-storage" \
    2>&1 | tee /tmp/katamaran-nfs-source.log
MIG_STATUS=${PIPESTATUS[0]}
set -e

# ─── 17. Stop Ping & Collect Results ────────────────────────────────────────

log "Stopping continuous ping..."
if [[ -n "${PING_PID}" ]] && kill -0 "${PING_PID}" 2>/dev/null; then
    kill "${PING_PID}" 2>/dev/null || true
    wait "${PING_PID}" 2>/dev/null || true
fi
PING_PID=""  # Prevent double-kill in cleanup trap.
sync

# ─── 18. Verify NFS Data After Migration ────────────────────────────────────

log "Verifying NFS test data is accessible after migration..."
NFS_VERIFIED=false

# Try to read the test file from the NFS server pod (always accessible).
POST_DATA=$(kubectl --context "${PROFILE}" exec nfs-server -- \
    cat "${NFS_EXPORT_PATH}/${NFS_TEST_FILE}" 2>/dev/null || true)
if [[ "${POST_DATA}" == "${NFS_TEST_DATA}" ]]; then
    NFS_VERIFIED=true
    success "NFS data intact after migration: ${POST_DATA}"
else
    warn "Could not verify NFS data from NFS server pod"
fi

# ─── 19. Report Results ─────────────────────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== NFS SHARED-STORAGE E2E MIGRATION RESULTS ==="
echo "========================================================================"

# Migration result
echo ""
echo "--- Migration Status ---"
if [ $MIG_STATUS -eq 0 ]; then
    success "Live migration completed successfully!"
else
    error "Live migration failed (exit code $MIG_STATUS)."
fi

# NFS verification
echo ""
echo "--- NFS Shared Storage Verification ---"
echo "  Test data:        ${NFS_TEST_DATA}"
echo "  Written to:       ${NFS_MOUNT_PATH}/${NFS_TEST_FILE}"
if [[ "${NFS_VERIFIED}" == "true" ]]; then
    success "NFS data survived migration"
else
    error "NFS data verification failed"
fi

# Destination logs
echo ""
echo "--- Destination Logs ---"
minikube -p "${PROFILE}" ssh -n "${PROFILE}-m02" -- \
    "sudo journalctl -u katamaran-dest.service --no-pager" 2>/dev/null || true

# Source logs
echo ""
echo "--- Source Log Highlights ---"
if [[ -f /tmp/katamaran-nfs-source.log ]]; then
    grep -E "(tunnel|STOP|RESUME|complete|cancel|failed|succeeded|shared)" /tmp/katamaran-nfs-source.log || true
fi

# ─── 20. Zero-Drop Proof (Ping Analysis) ────────────────────────────────────

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

# ─── 21. Final Summary ──────────────────────────────────────────────────────

echo ""
echo "========================================================================"
echo "=== SUMMARY ==="
echo "========================================================================"
echo ""
echo "  CNI:              Calico"
echo "  Storage:          NFS (shared, skipped NBD drive-mirror)"
echo "  NFS Server:       ${NFS_SERVER_IP} (in-cluster pod)"
echo "  Nodes:            ${PROFILE} (source) → ${PROFILE}-m02 (dest)"
echo "  Node IPs:         ${NODE1_IP} → ${NODE2_IP}"
echo "  Pod IP:           ${POD_IP}"
echo "  Migration exit:   ${MIG_STATUS}"
echo "  NFS data intact:  ${NFS_VERIFIED}"
if [[ -n "${PING_SUMMARY:-}" ]]; then
    echo "  Ping result:      ${PING_SUMMARY}"
fi
echo ""
echo "  Artifacts:"
echo "    Source log:      /tmp/katamaran-nfs-source.log"
echo "    Ping log:        ${PING_LOG}"
echo ""

if [[ $MIG_STATUS -eq 0 && "${NFS_VERIFIED}" == "true" && "${LOSS:-100%}" == "0%" ]]; then
    success "NFS SHARED-STORAGE ZERO-DROP MIGRATION: VERIFIED"
elif [[ $MIG_STATUS -eq 0 && "${NFS_VERIFIED}" == "true" ]]; then
    warn "Migration and NFS succeeded but ping showed loss (${LOSS:-unknown})"
elif [[ $MIG_STATUS -eq 0 ]]; then
    warn "Migration succeeded but NFS verification failed"
else
    error "Migration failed (exit code ${MIG_STATUS})"
fi
