#!/bin/bash
# minikube-test.sh — Smoke tests for katamaran on a minikube cluster with Kata Containers.
#
# Validates:
#   1. katamaran binary runs correctly inside a Kubernetes node with Kata
#   2. Can locate and connect to a real Kata Containers QMP socket
#   3. QMP handshake succeeds with a live Kata VM
#   4. Basic CLI behavior works inside the minikube node
#
# Prerequisites:
#   - Linux host with KVM and nested virtualization enabled
#   - minikube, kubectl, helm installed
#   - ~20 GB free disk space, ~16 GB free RAM (nested Kata VMs need headroom)
#
# Usage:
#   ./testenv/minikube-test.sh [--keep]
#
#   --keep    Don't delete the minikube cluster on exit (for debugging)

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly BINARY="${PROJECT_ROOT}/katamaran"
readonly PROFILE="katamaran-test"
readonly KATA_CHART="oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy"
readonly KATA_CHART_VERSION="3.27.0"

cd "${SCRIPT_DIR}"

PASS=0
FAIL=0
KEEP=false

for arg in "$@"; do
    case "${arg}" in
        --keep) KEEP=true ;;
        *) echo "Unknown argument: ${arg}"; exit 1 ;;
    esac
done
readonly KEEP

pass() {
    echo "  PASS: $1"
    PASS=$((PASS + 1))
}

fail() {
    echo "  FAIL: $1"
    FAIL=$((FAIL + 1))
}

cleanup() {
    if [[ "${KEEP}" == "true" ]]; then
        echo ">>> --keep specified, leaving minikube profile '${PROFILE}' intact"
        return
    fi
    echo ">>> Uninstalling kata-deploy Helm release..."
    helm uninstall kata-deploy --kube-context "${PROFILE}" -n kube-system 2>/dev/null || true
    echo ">>> Cleaning up minikube profile '${PROFILE}'..."
    minikube delete -p "${PROFILE}" 2>/dev/null || true
}

trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Prerequisites check
# ---------------------------------------------------------------------------
echo "=== Step 1: Prerequisites check ==="

if command -v minikube &>/dev/null; then
    pass "minikube is installed"
else
    fail "minikube is not installed"
    echo "FATAL: minikube is required. Aborting."
    exit 1
fi

if command -v kubectl &>/dev/null; then
    pass "kubectl is installed"
else
    fail "kubectl is not installed"
    echo "FATAL: kubectl is required. Aborting."
    exit 1
fi

if command -v helm &>/dev/null; then
    pass "helm is installed"
else
    fail "helm is not installed"
    echo "FATAL: helm is required (kata-deploy is distributed as a Helm chart). Aborting."
    exit 1
fi

NESTED_VIRT=false
if [[ -f /sys/module/kvm_intel/parameters/nested ]]; then
    if [[ "$(cat /sys/module/kvm_intel/parameters/nested)" == "Y" || "$(cat /sys/module/kvm_intel/parameters/nested)" == "1" ]]; then
        NESTED_VIRT=true
    fi
elif [[ -f /sys/module/kvm_amd/parameters/nested ]]; then
    if [[ "$(cat /sys/module/kvm_amd/parameters/nested)" == "1" ]]; then
        NESTED_VIRT=true
    fi
fi

if [[ "${NESTED_VIRT}" == "true" ]]; then
    pass "nested virtualization is enabled"
else
    fail "nested virtualization is not enabled"
    echo "WARNING: Kata Containers may not work without nested virtualization."
fi

if [[ -x "${BINARY}" ]]; then
    pass "katamaran binary exists at ${BINARY}"
else
    fail "katamaran binary not found at ${BINARY}"
    echo "FATAL: Build katamaran first. Aborting."
    exit 1
fi

# ---------------------------------------------------------------------------
# 2. Start minikube
# ---------------------------------------------------------------------------
echo ""
echo "=== Step 2: Start minikube cluster ==="

echo ">>> Starting minikube profile '${PROFILE}'..."
minikube start \
    -p "${PROFILE}" \
    --driver=kvm2 \
    --memory=16384 \
    --cpus=8 \
    --container-runtime=containerd

if minikube status -p "${PROFILE}" &>/dev/null; then
    pass "minikube cluster started"
else
    fail "minikube cluster failed to start"
    echo "FATAL: Cannot continue without a running cluster. Aborting."
    exit 1
fi

# ---------------------------------------------------------------------------
# 3. Install Kata Containers (via Helm chart)
# ---------------------------------------------------------------------------
echo ""
echo "=== Step 3: Install Kata Containers ==="

echo ">>> Installing kata-deploy Helm chart (version ${KATA_CHART_VERSION})..."
echo "    Only the qemu shim is enabled (others disabled to save time)."
helm upgrade --install kata-deploy "${KATA_CHART}" \
    --version "${KATA_CHART_VERSION}" \
    --kube-context "${PROFILE}" \
    --namespace kube-system \
    --create-namespace \
    --set shims.disableAll=true \
    --set shims.qemu.enabled=true \
    --wait=false

echo ">>> Waiting for kata-deploy pod to be ready (up to 10 minutes)..."
readonly KATA_TIMEOUT=600
KATA_ELAPSED=0
KATA_READY=false
while [[ ${KATA_ELAPSED} -lt ${KATA_TIMEOUT} ]]; do
    if kubectl --context "${PROFILE}" -n kube-system get pods -l name=kata-deploy -o jsonpath='{.items[0].status.phase}' 2>/dev/null | grep -q "Running"; then
        KATA_READY=true
        break
    fi
    sleep 10
    KATA_ELAPSED=$((KATA_ELAPSED + 10))
    echo "    ... waiting (${KATA_ELAPSED}s / ${KATA_TIMEOUT}s)"
done

if [[ "${KATA_READY}" == "true" ]]; then
    pass "kata-deploy pod is running"
else
    fail "kata-deploy pod did not become ready within ${KATA_TIMEOUT}s"
    echo "DEBUG: pod status:"
    kubectl --context "${PROFILE}" -n kube-system get pods -l name=kata-deploy -o wide 2>&1 || true
    kubectl --context "${PROFILE}" -n kube-system logs -l name=kata-deploy --tail=30 2>&1 || true
    echo "FATAL: Kata Containers installation failed. Aborting."
    exit 1
fi

echo ">>> Waiting for kata-qemu RuntimeClass (up to 120s)..."
KATA_RC_ELAPSED=0
KATA_RC_READY=false
while [[ ${KATA_RC_ELAPSED} -lt 120 ]]; do
    if kubectl --context "${PROFILE}" get runtimeclass kata-qemu &>/dev/null; then
        KATA_RC_READY=true
        break
    fi
    sleep 5
    KATA_RC_ELAPSED=$((KATA_RC_ELAPSED + 5))
done

if [[ "${KATA_RC_READY}" == "true" ]]; then
    pass "kata-qemu RuntimeClass exists"
else
    fail "kata-qemu RuntimeClass not found"
    echo "FATAL: Cannot deploy Kata pods without RuntimeClass. Aborting."
    exit 1
fi

# Enable the extra QMP monitor socket so katamaran can connect independently
# of the kata-shim (the primary qmp.sock is 1:1 and already owned by the shim).
# Requires enable_debug=true in the [hypervisor.qemu] section.
echo ">>> Enabling extra QMP monitor socket in Kata config..."
KATA_CFG="/opt/kata/share/defaults/kata-containers/runtimes/qemu/configuration-qemu.toml"
minikube -p "${PROFILE}" ssh -- "
    sudo sed -i '/^\[hypervisor\.qemu\]/,/^\[/{
        s/^enable_debug = false/enable_debug = true/
        s/^extra_monitor_socket = \"\"/extra_monitor_socket = \"qmp\"/
    }' '${KATA_CFG}'
"
# Verify the change took effect.
if minikube -p "${PROFILE}" ssh -- "sudo grep -q 'extra_monitor_socket = \"qmp\"' '${KATA_CFG}'"; then
    pass "extra QMP monitor socket configured"
else
    fail "failed to configure extra QMP monitor socket"
    echo "WARNING: QMP handshake tests may fail."
fi

# ---------------------------------------------------------------------------
# 4. Deploy test pod
# ---------------------------------------------------------------------------
echo ""
echo "=== Step 4: Deploy test pod ==="

echo ">>> Creating kata-test pod..."
kubectl --context "${PROFILE}" apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: kata-test
spec:
  runtimeClassName: kata-qemu
  containers:
  - name: nginx
    image: nginx:alpine
EOF

echo ">>> Waiting for kata-test pod to be ready (up to 300s)..."
if kubectl --context "${PROFILE}" wait --for=condition=Ready pod/kata-test --timeout=300s; then
    pass "kata-test pod is ready"
else
    fail "kata-test pod did not become ready"
    echo "FATAL: Cannot test without a running Kata pod. Aborting."
    exit 1
fi

# ---------------------------------------------------------------------------
# 5. Copy binary + validate QMP
# ---------------------------------------------------------------------------
echo ""
echo "=== Step 5: Copy binary and validate QMP ==="

echo ">>> Copying katamaran binary into minikube node..."
minikube -p "${PROFILE}" cp "${BINARY}" /tmp/katamaran
minikube -p "${PROFILE}" ssh -- sudo chmod +x /tmp/katamaran

echo ">>> Locating extra QMP monitor socket..."
# The primary qmp.sock is owned 1:1 by kata-shim. We use the extra-monitor.sock
# that we configured via extra_monitor_socket = "qmp" in the Kata config.
QMP_SOCK="$(minikube -p "${PROFILE}" ssh -- 'sudo find /run/vc -name "extra-monitor.sock" 2>/dev/null | head -1')"
QMP_SOCK="$(echo "${QMP_SOCK}" | tr -d '[:space:]')"

if [[ -n "${QMP_SOCK}" ]]; then
    pass "found extra QMP socket at ${QMP_SOCK}"
else
    fail "no extra QMP monitor socket found (is extra_monitor_socket configured?)"
    echo "WARNING: Skipping QMP handshake tests."
fi

if [[ -n "${QMP_SOCK}" ]]; then
    echo ">>> Running katamaran in dest mode against extra QMP socket..."
    # Use -shared-storage to skip NBD (single-node smoke test, no real migration target).
    # Run with output to a file inside the node so log lines survive the
    # SIGTERM from timeout (Go's buffers are lost on signal kill).
    minikube -p "${PROFILE}" ssh -- "sudo timeout 10 /tmp/katamaran -mode dest -qmp '${QMP_SOCK}' -shared-storage >/tmp/katamaran-dest.log 2>&1 || true"
    DEST_OUTPUT="$(minikube -p "${PROFILE}" ssh -- 'cat /tmp/katamaran-dest.log')"
    echo "${DEST_OUTPUT}"

    if echo "${DEST_OUTPUT}" | grep -q "Setting up destination node"; then
        pass "katamaran dest mode started successfully"
    else
        fail "katamaran dest mode did not produce expected startup message"
    fi

    if echo "${DEST_OUTPUT}" | grep -qiE "Waiting for QEMU RESUME|Shared storage mode"; then
        pass "QMP handshake succeeded (post-handshake output detected)"
    else
        fail "QMP handshake did not produce expected post-handshake output"
    fi
fi

# ---------------------------------------------------------------------------
# 6. In-node CLI tests
# ---------------------------------------------------------------------------
echo ""
echo "=== Step 6: In-node CLI tests ==="

echo ">>> Testing invalid mode..."
INVALID_OUTPUT="$(minikube -p "${PROFILE}" ssh -- '/tmp/katamaran -mode bogus 2>&1 || true')"
if echo "${INVALID_OUTPUT}" | grep -qi "invalid mode"; then
    pass "invalid mode produces error message"
else
    fail "invalid mode did not produce expected error message"
fi

echo ">>> Testing no flags (usage)..."
USAGE_OUTPUT="$(minikube -p "${PROFILE}" ssh -- '/tmp/katamaran 2>&1 || true')"
if echo "${USAGE_OUTPUT}" | grep -qi "Usage"; then
    pass "no flags produces usage message"
else
    fail "no flags did not produce usage message"
fi

# ---------------------------------------------------------------------------
# 7. Summary
# ---------------------------------------------------------------------------
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="

if [[ ${FAIL} -gt 0 ]]; then
    exit 1
fi

exit 0
