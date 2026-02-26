#!/bin/bash
# k3s-e2e.sh — Sets up a two-node k3s cluster with Kata Containers for
# end-to-end testing of katamaran live migration.
#
# This script layers k3s on top of the existing QEMU test VMs (provisioned
# by setup.sh + start-node-{a,b}.sh). It installs k3s server on Node A,
# k3s agent on Node B, configures the Kata runtime, and deploys a test pod.
#
# Prerequisites:
#   - Both VMs must be running (./testenv/start-node-{a,b}.sh)
#   - Cloud-init must have completed (wait ~3 min after VM boot)
#   - SSH key at testenv/kata_test_rsa
#
# Usage:
#   ./testenv/k3s-e2e.sh [setup|teardown|status]
#
#   setup      Install k3s + deploy test pod (default)
#   teardown   Uninstall k3s from both nodes
#   status     Show cluster and pod status

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SCRIPT_DIR}"

readonly SSH_KEY="${SCRIPT_DIR}/kata_test_rsa"
readonly SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10"
readonly NODE_A_PORT="2222"
readonly NODE_B_PORT="2223"
readonly NODE_A_IP="10.0.0.1"  # inter-node IP
readonly NODE_B_IP="10.0.0.2"  # inter-node IP
readonly K3S_VERSION="v1.29.2+k3s1"

# ---------------------------------------------------------------------------
# Helper functions
# ---------------------------------------------------------------------------

ssh_a() { ssh ${SSH_OPTS} -i "${SSH_KEY}" -p "${NODE_A_PORT}" ubuntu@localhost "$@"; }
ssh_b() { ssh ${SSH_OPTS} -i "${SSH_KEY}" -p "${NODE_B_PORT}" ubuntu@localhost "$@"; }
scp_a() { scp ${SSH_OPTS} -i "${SSH_KEY}" -P "${NODE_A_PORT}" "$@" ubuntu@localhost:~/; }
scp_b() { scp ${SSH_OPTS} -i "${SSH_KEY}" -P "${NODE_B_PORT}" "$@" ubuntu@localhost:~/; }

log() { echo ">>> $*"; }

wait_for() {
    local description="$1"
    local timeout="$2"
    shift 2
    local deadline=$(( $(date +%s) + timeout ))
    log "Waiting up to ${timeout}s for ${description}..."
    while true; do
        if "$@" >/dev/null 2>&1; then
            log "${description} — ready."
            return 0
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            echo "ERROR: Timed out waiting for ${description}" >&2
            return 1
        fi
        sleep 3
    done
}

# ---------------------------------------------------------------------------
# setup
# ---------------------------------------------------------------------------

do_setup() {
    # 0. Pre-flight: verify required files exist
    if [[ ! -f "${SSH_KEY}" ]]; then
        echo "ERROR: SSH key not found at ${SSH_KEY}. Run ./testenv/setup.sh first." >&2
        exit 1
    fi
    if [[ ! -x "${PROJECT_ROOT}/katamaran" ]]; then
        echo "ERROR: katamaran binary not found at ${PROJECT_ROOT}/katamaran. Build it first." >&2
        exit 1
    fi

    # 1. Verify VM connectivity
    log "Verifying VM connectivity..."
    if ! ssh_a "echo ok" >/dev/null 2>&1; then
        echo "ERROR: Cannot SSH into Node A (port ${NODE_A_PORT}). Is the VM running?" >&2
        exit 1
    fi
    if ! ssh_b "echo ok" >/dev/null 2>&1; then
        echo "ERROR: Cannot SSH into Node B (port ${NODE_B_PORT}). Is the VM running?" >&2
        exit 1
    fi
    log "Both VMs reachable."

    # 2. Copy katamaran binary
    log "Copying katamaran binary to both nodes..."
    scp_a "${PROJECT_ROOT}/katamaran"
    scp_b "${PROJECT_ROOT}/katamaran"
    ssh_a "chmod +x ~/katamaran"
    ssh_b "chmod +x ~/katamaran"

    # 3. Install k3s server on Node A
    log "Installing k3s server on Node A..."
    ssh_a "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='${K3S_VERSION}' \
        K3S_NODE_NAME=node-a \
        INSTALL_K3S_EXEC='server --node-ip ${NODE_A_IP} --flannel-iface enp0s4 --disable traefik --write-kubeconfig-mode 644' \
        sudo -E sh -"

    wait_for "k3s server on Node A" 60 \
        ssh_a "sudo k3s kubectl get nodes"

    # 4. Get join token
    log "Retrieving k3s join token..."
    local TOKEN
    TOKEN=$(ssh_a "sudo cat /var/lib/rancher/k3s/server/node-token")

    # 5. Install k3s agent on Node B
    log "Installing k3s agent on Node B..."
    ssh_b "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='${K3S_VERSION}' \
        K3S_URL='https://${NODE_A_IP}:6443' \
        K3S_TOKEN='${TOKEN}' \
        K3S_NODE_NAME=node-b \
        INSTALL_K3S_EXEC='agent --node-ip ${NODE_B_IP} --flannel-iface enp0s4' \
        sudo -E sh -"

    wait_for "node-b to be Ready" 120 \
        ssh_a "sudo k3s kubectl get nodes | grep -q 'node-b.*Ready'"

    # 6. Verify / create Kata RuntimeClass
    log "Checking for Kata RuntimeClass..."
    if ! ssh_a "sudo k3s kubectl get runtimeclass kata" >/dev/null 2>&1; then
        log "Creating kata RuntimeClass..."
        ssh_a "sudo k3s kubectl apply -f - <<'RTEOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: kata
handler: kata
RTEOF"
    else
        log "Kata RuntimeClass already exists."
    fi

    # 7. Deploy test pod on Node A
    log "Deploying test pod (kata-migrate-test) on Node A..."
    ssh_a "sudo k3s kubectl apply -f - <<'PODEOF'
apiVersion: v1
kind: Pod
metadata:
  name: kata-migrate-test
spec:
  runtimeClassName: kata
  nodeName: node-a
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80
PODEOF"

    wait_for "pod kata-migrate-test to be Running" 300 \
        ssh_a "sudo k3s kubectl get pod kata-migrate-test -o jsonpath='{.status.phase}' | grep -q Running"

    # 8. Get pod IP
    local POD_IP
    POD_IP=$(ssh_a "sudo k3s kubectl get pod kata-migrate-test -o jsonpath='{.status.podIP}'")

    # 9. Print next steps
    cat <<EOF

=== k3s E2E Environment Ready ===

Cluster:
  Node A (server): SSH port ${NODE_A_PORT}, IP ${NODE_A_IP}
  Node B (agent):  SSH port ${NODE_B_PORT}, IP ${NODE_B_IP}

Test pod:
  Name: kata-migrate-test
  Node: node-a
  IP:   ${POD_IP}

To find the QMP socket on Node A:
  ssh -o StrictHostKeyChecking=no -i testenv/kata_test_rsa -p 2222 ubuntu@localhost \\
    "sudo find /run/vc -name 'qmp.sock' 2>/dev/null"

To run katamaran migration:
  # On Node B (destination) — run first:
  ssh ... ubuntu@localhost "sudo ./katamaran -mode dest -qmp <SOCKET> -tap <TAP>"

  # On Node A (source) — run second:
  ssh ... ubuntu@localhost "sudo ./katamaran -mode source -qmp <SOCKET> -dest-ip 10.0.0.2 -vm-ip ${POD_IP}"

To verify zero-drop migration (run from host before starting migration):
  ssh -o StrictHostKeyChecking=no -i testenv/kata_test_rsa -p 2222 ubuntu@localhost \\
    "ping -i 0.1 ${POD_IP}"

To check cluster status:
  ./testenv/k3s-e2e.sh status

To teardown:
  ./testenv/k3s-e2e.sh teardown
EOF
}

# ---------------------------------------------------------------------------
# teardown
# ---------------------------------------------------------------------------

do_teardown() {
    echo "Uninstalling k3s from Node B..."
    ssh_b "sudo /usr/local/bin/k3s-agent-uninstall.sh" || true
    echo "Uninstalling k3s from Node A..."
    ssh_a "sudo /usr/local/bin/k3s-uninstall.sh" || true
    echo "k3s uninstalled from both nodes."
}

# ---------------------------------------------------------------------------
# status
# ---------------------------------------------------------------------------

do_status() {
    echo "=== Cluster Status ==="
    ssh_a "sudo k3s kubectl get nodes -o wide" || echo "(k3s not running on Node A)"
    echo ""
    echo "=== Pod Status ==="
    ssh_a "sudo k3s kubectl get pods -o wide" || echo "(no pods)"
    echo ""
    echo "=== Kata QEMU Processes ==="
    echo "Node A:"
    ssh_a "sudo pgrep -a qemu-system" || echo "  (none)"
    echo "Node B:"
    ssh_b "sudo pgrep -a qemu-system" || echo "  (none)"
}

# ---------------------------------------------------------------------------
# Main dispatch
# ---------------------------------------------------------------------------

case "${1:-setup}" in
    setup)    do_setup ;;
    teardown) do_teardown ;;
    status)   do_status ;;
    *)
        echo "Usage: $0 [setup|teardown|status]"
        exit 1
        ;;
esac
