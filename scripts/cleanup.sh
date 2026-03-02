#!/bin/bash
# cleanup.sh — Mass cleanup of Katamaran test environments.
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
export PATH="${PROJECT_ROOT}/bin:${PATH}"

KEEP_LOGS=false

log() { echo ">>> $1"; }

usage() {
    cat <<USAGE
Usage: ./scripts/cleanup.sh [--keep-logs] [--help]

Options:
  --keep-logs   Keep /tmp/katamaran-*.log files
  --help        Show this help message
USAGE
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --keep-logs) KEEP_LOGS=true; shift ;;
        --help) usage; exit 0 ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 1 ;;
    esac
done

log "Starting mass cleanup of katamaran environments..."

if command -v minikube >/dev/null 2>&1; then
    log "Cleaning up minikube profiles..."
    # Parse table output: Name is in the second column (delimited by |)
    # Filter for profiles starting with katamaran-
    PROFILES=$(minikube profile list --light 2>/dev/null | awk -F"|" '{print $2}' | tr -d ' ' | grep '^katamaran-' || true)
    for p in $PROFILES; do
        log "Deleting minikube profile: $p"
        minikube delete -p "$p" >/dev/null 2>&1 || true
    done
else
    log "minikube not found, skipping."
fi

if command -v kind >/dev/null 2>&1; then
    log "Cleaning up Kind clusters..."
    CLUSTERS=$(kind get clusters 2>/dev/null | grep '^katamaran-' || true)
    for c in $CLUSTERS; do
        log "Deleting Kind cluster: $c"
        kind delete cluster --name "$c" >/dev/null 2>&1 || true
    done
else
    log "kind not found, skipping."
fi

log "Removing temporary files..."
if [[ "${KEEP_LOGS}" == "false" ]]; then
    rm -f /tmp/katamaran-*.log 2>/dev/null || true
fi
rm -f /tmp/kata-cfg-override.toml 2>/dev/null || true
rm -f katamaran.tar 2>/dev/null || true

log "Cleanup complete."
