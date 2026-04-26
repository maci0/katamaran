package migration

import "net/netip"

// formatQEMUHost returns the IP address formatted for use in QEMU's
// colon-delimited URIs (e.g., nbd:host:port, tcp:host:port). IPv6 addresses
// are wrapped in square brackets to avoid ambiguity with URI field separators.
// IPv4 addresses (including IPv4-mapped IPv6) are returned unchanged.
func formatQEMUHost(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return "[" + addr.String() + "]"
	}
	return addr.String()
}
