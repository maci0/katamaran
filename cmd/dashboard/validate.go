package main

import (
	"net"
	"regexp"
	"strings"
)

// maxTargetLen caps target length to the maximum DNS hostname length (253).
const maxTargetLen = 253

// validTarget checks that the target is a plausible IP or hostname for
// ping/HTTP probing. Rejects loopback, link-local, cloud metadata
// addresses, and unresolvable hostnames to prevent SSRF.
//
// Limitation: DNS rebinding could bypass this check — the hostname may
// resolve to a safe IP here but rebind to an internal IP at connect time.
// Accepted risk: this dashboard is a cluster-internal monitoring tool,
// not a public-facing API.
func validTarget(target string) bool {
	if len(target) > maxTargetLen {
		return false
	}
	if strings.HasPrefix(target, "-") {
		return false
	}
	// Reject path separators: valid targets are host or host:port only.
	// Without this, "service:8080/admin/action" would be constructed into
	// "http://service:8080/admin/action", enabling path-controlled SSRF.
	if strings.Contains(target, "/") {
		return false
	}
	host := target
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	if host == "" {
		return false
	}
	// Reject shell metacharacters and null bytes that could escape into
	// arguments. Null bytes are rejected explicitly because C-based system
	// calls (ping, DNS resolver with cgo) truncate at \x00, which could
	// cause the validated hostname to differ from what the subprocess sees.
	if strings.ContainsAny(host, "\x00;|&$`\\\"'<>(){}!\n\r\t @#%") {
		return false
	}
	ip, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		// Fail closed: reject unresolvable hostnames to prevent SSRF bypass
		// via names that the Go resolver cannot resolve but the target process
		// (ping, HTTP client) might resolve differently.
		return false
	}
	if ip.IP.IsLoopback() || ip.IP.IsUnspecified() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsMulticast() {
		return false
	}
	// Block cloud metadata endpoint (169.254.169.254).
	if ip.IP.Equal(net.ParseIP("169.254.169.254")) {
		return false
	}
	return true
}

// maxFormValueLen caps the length of form values passed to migrate.sh.
// Prevents extremely long arguments from reaching subprocess argv.
const maxFormValueLen = 512

// formValueRe is the allowlist for form values passed to migrate.sh.
// Aligned with migrate.sh's shell_safe_re to ensure defence-in-depth
// rejects the same characters at both layers.
var formValueRe = regexp.MustCompile(`^[a-zA-Z0-9_./:@=\-]+$`)

// validFormValue checks that a form value contains only shell-safe characters
// and does not exceed maxFormValueLen. Uses a whitelist aligned with
// migrate.sh's shell_safe_re regex, rejecting any characters that could be
// misinterpreted by envsubst or /bin/sh -c.
func validFormValue(v string) bool {
	return len(v) <= maxFormValueLen && formValueRe.MatchString(v)
}
