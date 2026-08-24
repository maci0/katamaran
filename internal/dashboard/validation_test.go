package dashboard

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestBlockedTargetIP pins the SSRF blocklist class-by-class. The guard is
// the last line of defense for both load generators (safeTargetIPs resolves
// through lookupSafeTargetIPs, and safeDialContext re-resolves at connect
// time), so every address class that could reach an internal service or a
// cloud metadata endpoint must stay blocked.
func TestBlockedTargetIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},       // loopback
		{"::1", true},             // IPv6 loopback
		{"0.0.0.0", true},         // unspecified
		{"::", true},              // IPv6 unspecified
		{"169.254.169.254", true}, // link-local (cloud metadata v4)
		{"fe80::1", true},         // IPv6 link-local unicast
		{"224.0.0.1", true},       // multicast
		{"ff02::1", true},         // IPv6 link-local multicast
		{"ff01::1", true},         // IPv6 interface-local multicast
		{"169.254.1.1", true},     // any link-local, not just metadata
		{"fd00:ec2::254", true},   // AWS IMDS IPv6 alias
		{"100.100.100.200", true}, // Alibaba Cloud metadata
		{"10.244.1.5", false},     // routable unicast must stay allowed
		{"192.0.2.1", false},      // documentation range, allowed
		{"2001:db8::1", false},    // documentation range, allowed
	}
	for _, tt := range tests {
		if got := blockedTargetIP(net.ParseIP(tt.ip)); got != tt.blocked {
			t.Errorf("blockedTargetIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
		}
	}

	// net.ParseIP never returns a non-nil IP for bracketed input, so the
	// dialer's SplitHostPort path (which strips brackets) is what feeds this
	// function; pin that assumption so a future refactor passing raw URL
	// hosts fails loudly here instead of silently unblocking everything.
	if net.ParseIP("[::1]") != nil {
		t.Fatal("precondition violated: ParseIP now accepts bracketed literals; blocklist comparisons need updating")
	}
}

// TestLookupSafeTargetIPs_FailClosedOnAnyBlockedIP locks the fail-closed
// semantics of DNS revalidation: when a hostname resolves to several
// addresses and ANY one of them is blocked, the whole lookup must fail.
// Returning the safe subset instead would let the HTTP load generator dial
// the blocked address on a later attempt (DNS round-robin / rebinding).
func TestLookupSafeTargetIPs_FailClosedOnAnyBlockedIP(t *testing.T) {
	t.Parallel()
	// IP literals bypass the network resolver (LookupIPAddr parses them in
	// process), so these cases are deterministic without DNS stubbing.
	for _, host := range []string{
		"127.0.0.1", // only blocked address
		"169.254.169.254",
		"0.0.0.0",
	} {
		if ips, err := lookupSafeTargetIPs(context.Background(), host); !errors.Is(err, errUnsafeTargetIP) {
			t.Errorf("lookupSafeTargetIPs(%q) = (%v, %v), want errUnsafeTargetIP", host, ips, err)
		}
	}

	ips, err := lookupSafeTargetIPs(context.Background(), "192.0.2.1")
	if err != nil {
		t.Fatalf("lookupSafeTargetIPs(192.0.2.1): %v", err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("192.0.2.1")) {
		t.Fatalf("ips = %v, want [192.0.2.1]", ips)
	}
}
