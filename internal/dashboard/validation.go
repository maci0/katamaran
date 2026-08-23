package dashboard

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// maxTargetLen caps target length to the maximum DNS hostname length (253).
const maxTargetLen = 253

// targetDNSTimeout bounds the DNS resolution performed during target
// validation. Without an explicit timeout, a slow or unreachable resolver
// can stall the HTTP handler beyond the server's request timeouts.
const targetDNSTimeout = 5 * time.Second

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
// also exposes an IPv6 alias; Alibaba Cloud uses 100.100.100.200 (carrier
// CGNAT range, not link-local). Accessing these from the dashboard pod
// could disclose node IAM credentials, so we hard-block them.
var blockedMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),
	net.ParseIP("fd00:ec2::254"),
	net.ParseIP("100.100.100.200"),
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

// safeTargetIPs runs every static and SSRF check that validTarget performs
// and additionally resolves the host, returning the screened IPs so callers
// can pin the target to the exact address that was validated instead of
// paying for (and racing) a second lookup. ok=false means the target must be
// rejected: unresolvable hostnames fail closed here to prevent SSRF bypass
// via names that the Go resolver cannot resolve but the target process
// (ping, HTTP client) might resolve differently.
func safeTargetIPs(target string) ([]net.IP, bool) {
	if len(target) > maxTargetLen+len(":65535") {
		return nil, false
	}
	if strings.HasPrefix(target, "-") {
		return nil, false
	}
	// Reject path separators: valid targets are host or host:port only.
	// Without this, "service:8080/admin/action" would be constructed into
	// "http://service:8080/admin/action", enabling path-controlled SSRF.
	if strings.Contains(target, "/") {
		return nil, false
	}
	host, port, hasPort, ok := splitTarget(target)
	if !ok || host == "" || len(host) > maxTargetLen {
		return nil, false
	}
	if hasPort && !validTargetPort(port) {
		return nil, false
	}
	// Reject shell metacharacters and null bytes that could escape into
	// arguments. Null bytes are rejected explicitly because C-based system
	// calls (ping, DNS resolver with cgo) truncate at \x00, which could
	// cause the validated hostname to differ from what the subprocess sees.
	if strings.ContainsAny(host, "\x00;|&$`\\\"'<>(){}!\n\r\t @#%") {
		return nil, false
	}
	// Reject ".." sequences in the host: prevents abuse of resolver quirks
	// or downstream URL-construction edge cases where ".." could traverse
	// or confuse host parsing.
	if strings.Contains(host, "..") {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), targetDNSTimeout)
	defer cancel()
	ips, err := lookupSafeTargetIPs(ctx, host)
	if err != nil {
		return nil, false
	}
	return ips, true
}

// validTarget checks that the target is a plausible IP or hostname for
// ping/HTTP probing. Rejects loopback, link-local, cloud metadata
// addresses, and unresolvable hostnames to prevent SSRF.
//
// Callers that probe by IP literal should use safeTargetIPs instead so the
// validated addresses are reused rather than resolved twice.
func validTarget(target string) bool {
	_, ok := safeTargetIPs(target)
	return ok
}

// validFormValue wraps orchestrator.ValidateSafeArgValue to check that a form
// value contains only shell-safe characters and does not exceed max len.
func validFormValue(v string) bool {
	return orchestrator.ValidateSafeArgValue("form field", v) == nil
}
