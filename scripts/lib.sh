#!/bin/bash
# scripts/lib.sh — Shared utility functions for katamaran scripts.
#
# Source this file after setting SCRIPT_DIR and PROJECT_ROOT.
# Functions that access cluster nodes (node_exec, node_cp_to) require:
#   PROVIDER  — 'minikube' or 'kind'
#   PROFILE   — minikube profile name or kind cluster name
#   SUDO      — 'sudo' or '' (empty for kind)
#   CE        — container engine: 'podman' or 'docker'

# --- Argument helpers ---

# need_arg checks that a flag has a non-empty value following it.
# Usage: need_arg "$1" "${2:-}"
need_arg() {
    if [[ $# -lt 2 || -z "$2" ]]; then
        echo "Error: $1 requires a value" >&2
        exit 2
    fi
}

# --- Output helpers ---

if [[ -z "${NO_COLOR:-}" && -t 1 ]]; then
    stdout_blue=$'\033[1;34m'
    stdout_green=$'\033[1;32m'
    stdout_yellow=$'\033[1;33m'
    stdout_reset=$'\033[0m'
else
    stdout_blue=""
    stdout_green=""
    stdout_yellow=""
    stdout_reset=""
fi

if [[ -z "${NO_COLOR:-}" && -t 2 ]]; then
    stderr_red=$'\033[1;31m'
    stderr_reset=$'\033[0m'
else
    stderr_red=""
    stderr_reset=""
fi

log()     { printf "\n%s>>> %s%s\n" "$stdout_blue" "$1" "$stdout_reset"; }
success() { printf "%s  PASS: %s%s\n" "$stdout_green" "$1" "$stdout_reset"; }
warn()    { printf "%s  WARN: %s%s\n" "$stdout_yellow" "$1" "$stdout_reset"; }
error()   { printf "%s  ERROR: %s%s\n" "$stderr_red" "$1" "$stderr_reset" >&2; }

# --- Provider-aware remote execution ---

# node_exec runs a command on a cluster node.
# Minikube: uses 'minikube ssh' with profile and strips carriage returns.
# Kind: uses container exec (podman/docker).
node_exec() {
    local node="$1"
    shift
    if [[ "${PROVIDER}" == "minikube" ]]; then
        # Pipe through tr to strip carriage returns added by minikube's PTY.
        minikube -p "${PROFILE}" ssh -n "$node" -- "$*" | tr -d '\r'
    else
        "${CE}" exec "$node" bash -c "$*"
    fi
}

# node_cp_to copies a local file to a cluster node.
node_cp_to() {
    local node="$1"
    local src="$2"
    local dst="$3"
    if [[ "${PROVIDER}" == "minikube" ]]; then
        minikube -p "${PROFILE}" cp "${src}" "${node}:${dst}"
    else
        "${CE}" cp "${src}" "${node}:${dst}"
    fi
}
