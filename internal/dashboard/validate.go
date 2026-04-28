package dashboard

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// maxTargetLen caps target length to the maximum DNS hostname length (253).
const maxTargetLen = 253

var errUnsafeTargetIP = errors.New("target resolves to a blocked IP address")

func splitTarget(target string) (host, port string, hasPort bool, ok bool) {
	if h, p, err := net.SplitHostPort(target); err == nil {
		return h, p, true, true
	}
	if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
		host = target[1 : len(target)-1]
		return host, "", false, host != ""
	}
	return target, "", false, true
}

func targetHost(target string) string {
	host, _, _, ok := splitTarget(target)
	if !ok {
		return ""
	}
	return host
}

func validTargetPort(port string) bool {
	if port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	p, err := strconv.Atoi(port)
	return err == nil && p >= 1 && p <= 65535
}

// blockedMetadataIPs are well-known cloud-provider instance metadata
// endpoints. AWS/GCP/Azure share 169.254.169.254 (already covered by the
// link-local check on most platforms but pinned here defensively); AWS IMDS
// also exposes an IPv6 alias. Accessing these from the dashboard pod could
// disclose node IAM credentials, so we hard-block them.
var blockedMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),
	net.ParseIP("fd00:ec2::254"),
}

func blockedTargetIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}
	for _, blocked := range blockedMetadataIPs {
		if ip.Equal(blocked) {
			return true
		}
	}
	return false
}

func lookupSafeTargetIPs(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errUnsafeTargetIP
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if blockedTargetIP(addr.IP) {
			return nil, errUnsafeTargetIP
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

func resolvedTargetIP(target string) (string, bool) {
	ips, err := lookupSafeTargetIPs(context.Background(), targetHost(target))
	if err != nil || len(ips) == 0 {
		return "", false
	}
	return ips[0].String(), true
}

// validTarget checks that the target is a plausible IP or hostname for
// ping/HTTP probing. Rejects loopback, link-local, cloud metadata
// addresses, and unresolvable hostnames to prevent SSRF.
//
// HTTP probes revalidate DNS in the custom dialer, and ping probes use the
// resolved IP literal so a hostname cannot rebind after this check.
func validTarget(target string) bool {
	if len(target) > maxTargetLen+len(":65535") {
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
	host, port, hasPort, ok := splitTarget(target)
	if !ok || host == "" || len(host) > maxTargetLen {
		return false
	}
	if hasPort && !validTargetPort(port) {
		return false
	}
	// Reject shell metacharacters and null bytes that could escape into
	// arguments. Null bytes are rejected explicitly because C-based system
	// calls (ping, DNS resolver with cgo) truncate at \x00, which could
	// cause the validated hostname to differ from what the subprocess sees.
	if strings.ContainsAny(host, "\x00;|&$`\\\"'<>(){}!\n\r\t @#%") {
		return false
	}
	// Reject ".." sequences in the host: prevents abuse of resolver quirks
	// or downstream URL-construction edge cases where ".." could traverse
	// or confuse host parsing.
	if strings.Contains(host, "..") {
		return false
	}
	if _, err := lookupSafeTargetIPs(context.Background(), host); err != nil {
		// Fail closed: reject unresolvable hostnames to prevent SSRF bypass
		// via names that the Go resolver cannot resolve but the target process
		// (ping, HTTP client) might resolve differently.
		return false
	}
	return true
}

// maxFormValueLen caps migration form values before they are rendered into
// Job command arguments.
const maxFormValueLen = 512

// formValueRe is the allowlist for values that may be rendered into Job
// command arguments. Keep it aligned with deploy/migrate.sh's shell_safe_re
// so direct script and dashboard submissions accept the same character set.
var formValueRe = regexp.MustCompile(`^[a-zA-Z0-9_./:@=\-]+$`)

// validFormValue checks that a form value contains only shell-safe characters
// and does not exceed maxFormValueLen. The Job templates still run the
// katamaran command through /bin/sh -c, so reject characters with shell
// meaning before values reach the orchestrator.
//
// Also rejects ".." path-traversal sequences. The allowlist regex includes
// "." for legitimate path/IP/version components, but ".." enables path
// traversal in fields passed as filesystem paths (qmp_source, qmp_dest,
// tap_netns), escaping intended directories to access
// arbitrary unix sockets or namespaces.
func validFormValue(v string) bool {
	if len(v) > maxFormValueLen || !formValueRe.MatchString(v) {
		return false
	}
	if strings.Contains(v, "..") {
		return false
	}
	return true
}
