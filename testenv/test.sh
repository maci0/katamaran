#!/bin/bash
# test.sh — Smoke tests for the katamaran project.
#
# Validates:
#   1. Go source compiles cleanly (go vet + gofmt + go build)
#   2. Binary prints usage and exits non-zero when invoked without flags
#   3. Binary rejects source mode when required flags are missing (with specific error)
#   4. Binary rejects dest mode with nonexistent QMP socket (with socket path in error)
#   5. Binary validates -shared-storage flag combinations
#   6. Binary rejects unexpected positional arguments
#   7. Binary rejects invalid -mode values with specific error message
#   8. Empty mode prints "Usage" message
#   9. -help flag prints flag descriptions (all eight flags)
#  10. Binary rejects invalid IP addresses for -dest-ip and -vm-ip
#  11. Binary rejects single-missing-flag combinations (-dest-ip only, -vm-ip only)
#  12. Binary validates IPv6 addresses (valid and invalid)
#  13. Binary validates -tunnel-mode flag (valid values and rejection of invalid)
#  14. Binary rejects cross-family IP combinations (IPv4 dest + IPv6 vm and vice versa)
#  15. Binary normalizes IPv4-mapped IPv6 addresses (::ffff:10.0.0.1 treated as IPv4)
#  16. Shell scripts have valid syntax (bash -n)
#  17. Required project files exist
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${SCRIPT_DIR}"

if command -v go &>/dev/null; then
    GO_CMD="go"
else
    echo "Error: Go not found. Install Go 1.22+ system-wide."
    exit 1
fi
readonly GO_CMD

PASS=0
FAIL=0
readonly BINARY="${PROJECT_ROOT}/katamaran"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

echo "=== katamaran smoke tests ==="
echo ""

# --- 1. Go source quality ---
# Go commands must run from the module root (PROJECT_ROOT).
echo "--- Go source ---"

if (cd "${PROJECT_ROOT}" && "${GO_CMD}" vet ./...) 2>/dev/null; then
    pass "go vet reports no issues"
else
    fail "go vet found issues"
fi

# gofmt check — all Go source files should already be formatted.
# Use gofmt directly (from same dir as Go SDK, or system PATH).
GOFMT_CMD="$(dirname "${GO_CMD}")/gofmt"
if [[ ! -x "${GOFMT_CMD}" ]]; then
    GOFMT_CMD="gofmt"
fi
readonly GOFMT_CMD
GOFMT_DIFF=$(cd "${PROJECT_ROOT}" && find cmd internal -name '*.go' -exec "${GOFMT_CMD}" -l {} + 2>/dev/null)
if [[ -z "${GOFMT_DIFF}" ]]; then
    pass "gofmt reports no formatting issues"
else
    fail "gofmt found formatting issues in: ${GOFMT_DIFF}"
fi

if (cd "${PROJECT_ROOT}" && GOOS=linux GOARCH=amd64 "${GO_CMD}" build -o "${BINARY}" ./cmd/katamaran/) 2>/dev/null; then
    pass "go build succeeds"
else
    fail "go build failed"
fi

# --- 2. Binary behavior ---
echo "--- Binary behavior ---"

if [[ -x "${BINARY}" ]]; then
    # No flags → should print usage and exit 1
    if "${BINARY}" 2>/dev/null; then
        fail "binary should exit non-zero without flags"
    else
        pass "binary exits non-zero without flags"
    fi

    # Source mode without required flags → should exit non-zero
    if "${BINARY}" -mode source 2>/dev/null; then
        fail "source mode should require -dest-ip and -vm-ip"
    else
        pass "source mode rejects missing required flags"
    fi

    # Source mode missing flags → error should mention -dest-ip and -vm-ip
    SOURCE_ERR=$("${BINARY}" -mode source 2>&1 || true)
    if echo "${SOURCE_ERR}" | grep -q "\-dest-ip" && echo "${SOURCE_ERR}" | grep -q "\-vm-ip"; then
        pass "source mode error mentions -dest-ip and -vm-ip"
    else
        fail "source mode error should mention -dest-ip and -vm-ip"
    fi

    # Dest mode with bad QMP path → should exit non-zero
    if "${BINARY}" -mode dest -qmp /nonexistent/qmp.sock 2>/dev/null; then
        fail "dest mode should fail with bad QMP socket"
    else
        pass "dest mode fails with nonexistent QMP socket"
    fi

    # Dest mode QMP error → should mention the socket path in stderr
    DEST_ERR=$("${BINARY}" -mode dest -qmp /nonexistent/qmp.sock 2>&1 || true)
    if echo "${DEST_ERR}" | grep -q "/nonexistent/qmp.sock"; then
        pass "dest mode QMP error mentions socket path"
    else
        fail "dest mode QMP error should mention socket path"
    fi

    # Source mode with -shared-storage but missing required flags → should exit non-zero
    if "${BINARY}" -mode source -shared-storage 2>/dev/null; then
        fail "source -shared-storage should still require -dest-ip and -vm-ip"
    else
        pass "source -shared-storage rejects missing required flags"
    fi

    # Dest mode with -shared-storage and bad QMP → should exit non-zero (but not crash)
    if "${BINARY}" -mode dest -shared-storage -qmp /nonexistent/qmp.sock 2>/dev/null; then
        fail "dest -shared-storage should fail with bad QMP socket"
    else
        pass "dest -shared-storage fails with nonexistent QMP socket"
    fi

    # Unexpected positional arguments → should exit non-zero
    if "${BINARY}" -mode dest foo bar 2>/dev/null; then
        fail "binary should reject unexpected positional arguments"
    else
        pass "binary rejects unexpected positional arguments"
    fi

    # Invalid mode value → should exit non-zero
    if "${BINARY}" -mode invalid 2>/dev/null; then
        fail "binary should reject invalid -mode value"
    else
        pass "binary rejects invalid -mode value"
    fi

    # Invalid mode → should mention the invalid value in stderr
    INVALID_ERR=$("${BINARY}" -mode bogus 2>&1 || true)
    if echo "${INVALID_ERR}" | grep -q "invalid mode"; then
        pass "invalid mode error message includes 'invalid mode'"
    else
        fail "invalid mode error message should include 'invalid mode'"
    fi

    # Empty mode → should print "Usage" in stderr
    EMPTY_ERR=$("${BINARY}" 2>&1 || true)
    if echo "${EMPTY_ERR}" | grep -q "Usage"; then
        pass "empty mode prints Usage message"
    else
        fail "empty mode should print Usage message"
    fi

    # -help flag → should exit 0 and print flag descriptions for all seven flags
    HELP_OUT=$("${BINARY}" -help 2>&1 || true)
    if echo "${HELP_OUT}" | grep -q "\-mode"; then
        pass "-help output includes -mode flag description"
    else
        fail "-help output should include -mode flag description"
    fi

    for flag_name in dest-ip vm-ip qmp tap drive-id shared-storage tunnel-mode; do
        if echo "${HELP_OUT}" | grep -q "\-${flag_name}"; then
            pass "-help output includes -${flag_name} flag"
        else
            fail "-help output should include -${flag_name} flag"
        fi
    done

    # Invalid -dest-ip → should exit non-zero with specific error
    if "${BINARY}" -mode source -dest-ip "not-an-ip" -vm-ip 10.0.0.1 2>/dev/null; then
        fail "source mode should reject invalid -dest-ip"
    else
        pass "source mode rejects invalid -dest-ip"
    fi

    DESTIP_ERR=$("${BINARY}" -mode source -dest-ip "not-an-ip" -vm-ip 10.0.0.1 2>&1 || true)
    if echo "${DESTIP_ERR}" | grep -q "invalid -dest-ip"; then
        pass "invalid -dest-ip error mentions the flag name"
    else
        fail "invalid -dest-ip error should mention the flag name"
    fi

    # Invalid -vm-ip → should exit non-zero with specific error
    if "${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip "bogus" 2>/dev/null; then
        fail "source mode should reject invalid -vm-ip"
    else
        pass "source mode rejects invalid -vm-ip"
    fi

    VMIP_ERR=$("${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip "bogus" 2>&1 || true)
    if echo "${VMIP_ERR}" | grep -q "invalid -vm-ip"; then
        pass "invalid -vm-ip error mentions the flag name"
    else
        fail "invalid -vm-ip error should mention the flag name"
    fi

    # Valid IPs should pass validation (fail later at QMP connect, not at validation)
    VALID_ERR=$("${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip 10.244.1.15 2>&1 || true)
    if echo "${VALID_ERR}" | grep -q "invalid"; then
        fail "valid IPs should not trigger validation errors"
    else
        pass "valid IPs pass validation (fails at QMP connect as expected)"
    fi

    # Single missing flag: only -dest-ip provided → should exit non-zero
    if "${BINARY}" -mode source -dest-ip 10.0.0.1 2>/dev/null; then
        fail "source mode should reject when only -dest-ip is provided"
    else
        pass "source mode rejects missing -vm-ip when -dest-ip is set"
    fi

    # Single missing flag: only -vm-ip provided → should exit non-zero
    if "${BINARY}" -mode source -vm-ip 10.244.1.15 2>/dev/null; then
        fail "source mode should reject when only -vm-ip is provided"
    else
        pass "source mode rejects missing -dest-ip when -vm-ip is set"
    fi

    # Valid IPv6 addresses should pass validation (fail later at QMP connect)
    VALID6_ERR=$("${BINARY}" -mode source -dest-ip fd00::1 -vm-ip fd00::2 2>&1 || true)
    if echo "${VALID6_ERR}" | grep -q "invalid"; then
        fail "valid IPv6 addresses should not trigger validation errors"
    else
        pass "valid IPv6 addresses pass validation"
    fi

    # Invalid IPv6 -dest-ip → should exit non-zero
    if "${BINARY}" -mode source -dest-ip "::gg" -vm-ip fd00::1 2>/dev/null; then
        fail "source mode should reject invalid IPv6 -dest-ip"
    else
        pass "source mode rejects invalid IPv6 -dest-ip"
    fi

    # Invalid IPv6 -vm-ip → should exit non-zero
    if "${BINARY}" -mode source -dest-ip fd00::1 -vm-ip "::gg" 2>/dev/null; then
        fail "source mode should reject invalid IPv6 -vm-ip"
    else
        pass "source mode rejects invalid IPv6 -vm-ip"
    fi

    # -tunnel-mode ipip → should pass validation (fail later at QMP connect)
    TUNIPIP_ERR=$("${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip 10.244.1.15 -tunnel-mode ipip 2>&1 || true)
    if echo "${TUNIPIP_ERR}" | grep -q "invalid -tunnel-mode"; then
        fail "-tunnel-mode ipip should be accepted"
    else
        pass "-tunnel-mode ipip passes validation"
    fi

    # -tunnel-mode gre → should pass validation (fail later at QMP connect)
    TUNGRE_ERR=$("${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip 10.244.1.15 -tunnel-mode gre 2>&1 || true)
    if echo "${TUNGRE_ERR}" | grep -q "invalid -tunnel-mode"; then
        fail "-tunnel-mode gre should be accepted"
    else
        pass "-tunnel-mode gre passes validation"
    fi

    # Invalid -tunnel-mode → should exit non-zero
    if "${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip 10.244.1.15 -tunnel-mode vxlan 2>/dev/null; then
        fail "source mode should reject invalid -tunnel-mode"
    else
        pass "source mode rejects invalid -tunnel-mode"
    fi

    # Invalid -tunnel-mode → error should mention the flag name
    TUNBAD_ERR=$("${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip 10.244.1.15 -tunnel-mode bogus 2>&1 || true)
    if echo "${TUNBAD_ERR}" | grep -q "invalid -tunnel-mode"; then
        pass "invalid -tunnel-mode error mentions the flag name"
    else
        fail "invalid -tunnel-mode error should mention the flag name"
    fi

    # Cross-family: IPv4 dest + IPv6 vm → should exit non-zero
    if "${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip fd00::1 2>/dev/null; then
        fail "source mode should reject cross-family IPs (IPv4 dest + IPv6 vm)"
    else
        pass "source mode rejects cross-family IPs (IPv4 dest + IPv6 vm)"
    fi

    # Cross-family: IPv6 dest + IPv4 vm → should exit non-zero
    if "${BINARY}" -mode source -dest-ip fd00::1 -vm-ip 10.0.0.1 2>/dev/null; then
        fail "source mode should reject cross-family IPs (IPv6 dest + IPv4 vm)"
    else
        pass "source mode rejects cross-family IPs (IPv6 dest + IPv4 vm)"
    fi

    # Cross-family error → should mention "address family"
    XFAM_ERR=$("${BINARY}" -mode source -dest-ip 10.0.0.1 -vm-ip fd00::1 2>&1 || true)
    if echo "${XFAM_ERR}" | grep -q "address family"; then
        pass "cross-family error mentions 'address family'"
    else
        fail "cross-family error should mention 'address family'"
    fi

    # IPv4-mapped IPv6 address (::ffff:10.0.0.1) should be treated as IPv4
    # and pass validation when paired with a plain IPv4 address.
    V4MAP_ERR=$("${BINARY}" -mode source -dest-ip "::ffff:10.0.0.1" -vm-ip 10.244.1.15 2>&1 || true)
    if echo "${V4MAP_ERR}" | grep -q "address family"; then
        fail "IPv4-mapped ::ffff:10.0.0.1 should be treated as IPv4 (not rejected as cross-family)"
    else
        pass "IPv4-mapped ::ffff:10.0.0.1 is normalized to IPv4"
    fi
else
    fail "binary not found or not executable"
fi

# --- 3. Shell script syntax ---
echo "--- Shell scripts ---"

for script in test.sh minikube-test.sh minikube-e2e.sh minikube-ovn-e2e.sh minikube-nfs-e2e.sh kind-e2e.sh; do
    if [[ -f "${script}" ]]; then
        if bash -n "${script}" 2>/dev/null; then
            pass "${script} has valid syntax"
        else
            fail "${script} has syntax errors"
        fi
    else
        fail "${script} not found"
    fi
done

# --- 4. Required files ---
echo "--- Required files ---"

for file in "${PROJECT_ROOT}/go.mod" "${PROJECT_ROOT}/cmd/katamaran/main.go" "${PROJECT_ROOT}/README.md" "${PROJECT_ROOT}/docs/TESTING.md" "${PROJECT_ROOT}/docs/STORIES.md"; do
    basename="$(basename "${file}")"
    if [[ -f "${file}" ]]; then
        pass "${basename} exists"
    else
        fail "${basename} missing"
    fi
done

# --- Summary ---
echo ""
echo "=== Results: ${PASS} passed, ${FAIL} failed ==="

if [[ ${FAIL} -gt 0 ]]; then
    exit 1
fi
exit 0
