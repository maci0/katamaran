package migration

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestCleanupCtx(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	ctx, cancel := cleanupCtx(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cleanupCtx should have a deadline")
	}

	remaining := time.Until(deadline)
	if remaining < 5*time.Second || remaining > cleanupTimeout+time.Second {
		t.Fatalf("expected deadline ~%v from now, got %v", cleanupTimeout, remaining)
	}

	select {
	case <-ctx.Done():
		t.Fatal("cleanupCtx should not be cancelled when parent is")
	default:
	}
}

func TestConstants_Reasonable(t *testing.T) {
	t.Parallel()

	for name, val := range map[string]string{
		"nbdPort":          nbdPort,
		"ramMigrationPort": ramMigrationPort,
		"plugQdiscLimit":   plugQdiscLimit,
		"tunnelPrefix":     tunnelPrefix,
	} {
		if val == "" {
			t.Errorf("%s must not be empty", name)
		}
	}

	// Check each numeric/duration constant individually so failures
	// identify exactly which constant is wrong.
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"maxBandwidth", maxBandwidth > 0},
		{"eventWaitTimeout", eventWaitTimeout > 0},
		{"storagePollInterval", storagePollInterval > 0},
		{"migrationPollInterval", migrationPollInterval > 0},
		{"postMigrationTunnelDelay", postMigrationTunnelDelay > 0},
		{"garpRounds", garpRounds > 0},
		{"migrationTimeout", migrationTimeout > 0},
		{"storageSyncTimeout", storageSyncTimeout > 0},
		{"jobAppearTimeout", jobAppearTimeout > 0},
		{"cleanupTimeout", cleanupTimeout > 0},
		{"DefaultMultifdChannels", DefaultMultifdChannels > 0},
		{"rttMultiplier", rttMultiplier > 0},
		{"rttMinOverheadMS", rttMinOverheadMS > 0},
		{"garpInitialMS", garpInitialMS > 0},
		{"garpMaxMS", garpMaxMS > 0},
		{"garpStepMS", garpStepMS > 0},
		{"rttDialTimeout", rttDialTimeout > 0},
	} {
		if !c.ok {
			t.Errorf("%s must be positive", c.name)
		}
	}

	// Verify relationships between related constants. These catch
	// misconfigurations where a timeout is accidentally swapped or
	// a poll interval exceeds its parent timeout.
	for _, rel := range []struct {
		name string
		ok   bool
	}{
		{"cleanupTimeout < migrationTimeout", cleanupTimeout < migrationTimeout},
		{"storagePollInterval < storageSyncTimeout", storagePollInterval < storageSyncTimeout},
		{"migrationPollInterval < migrationTimeout", migrationPollInterval < migrationTimeout},
		{"jobAppearTimeout < storageSyncTimeout", jobAppearTimeout < storageSyncTimeout},
		{"garpInitialMS < garpMaxMS", garpInitialMS < garpMaxMS},
	} {
		if !rel.ok {
			t.Errorf("constant relationship violated: %s", rel.name)
		}
	}
}

func TestIPFamily(t *testing.T) {
	t.Parallel()
	if got := IPFamily(netip.MustParseAddr("10.0.0.1")); got != "IPv4" {
		t.Fatalf("got %q, want IPv4", got)
	}
	if got := IPFamily(netip.MustParseAddr("fd00::1")); got != "IPv6" {
		t.Fatalf("got %q, want IPv6", got)
	}
}

func FuzzFormatQEMUHost(f *testing.F) {
	f.Add("10.0.0.1")
	f.Add("127.0.0.1")
	f.Add("fd00::1")
	f.Add("::1")
	f.Add("2001:db8::1")
	f.Add("::ffff:10.0.0.1")
	f.Add("0.0.0.0")
	f.Add("::")
	f.Add("255.255.255.255")
	f.Add("fe80::1%eth0")
	f.Add("::ffff:192.168.0.1")
	f.Add("1.1.1.1")

	f.Fuzz(func(t *testing.T, s string) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return
		}
		result := formatQEMUHost(addr)
		if result == "" {
			t.Fatal("formatQEMUHost returned empty string for valid address")
		}
		// IPv6 addresses must be bracketed; IPv4 (including unmapped) must not.
		unmapped := addr.Unmap()
		if unmapped.Is6() {
			if result[0] != '[' || result[len(result)-1] != ']' {
				t.Fatalf("IPv6 address %v not bracketed: %q", addr, result)
			}
		} else {
			if strings.Contains(result, "[") || strings.Contains(result, "]") {
				t.Fatalf("IPv4 address %v should not be bracketed: %q", addr, result)
			}
		}
	})
}

func TestValidateTapIface(t *testing.T) {
	t.Parallel()
	valid := []string{"eth0", "tap0_kata", "br-abc123", "lo", "veth1234abc", "ens3", "docker0", "a"}
	for _, name := range valid {
		if err := validateTapIface(name); err != nil {
			t.Errorf("expected %q to be valid, got: %v", name, err)
		}
	}
	invalid := []string{
		"",                 // empty
		".hidden",          // starts with dot
		"-flag",            // starts with hyphen
		"a234567890123456", // 16 chars, exceeds IFNAMSIZ-1
		"eth0;rm -rf /",    // shell injection
		"eth0\nlo",         // newline
		"tap$(id)",         // command substitution
		"/proc/1/ns/net",   // path, not interface name
		"eth0 lo",          // space
	}
	for _, name := range invalid {
		if err := validateTapIface(name); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestValidateTapNetns(t *testing.T) {
	t.Parallel()
	valid := []string{
		"/proc/1234/ns/net",
		"/proc/1/ns/net",
		"/var/run/netns/blue",
		"/run/netns/test-ns",
	}
	for _, path := range valid {
		if err := validateTapNetns(path); err != nil {
			t.Errorf("expected %q to be valid, got: %v", path, err)
		}
	}
	invalid := []string{
		"",                        // empty
		"relative/path",           // not absolute
		"/proc/../etc/passwd",     // path traversal
		"/proc/1/ns/net;id",       // shell injection
		"/proc/1/ns/net\x00evil",  // null byte
		"/proc/1/ns/net$(whoami)", // command substitution
		"/ space/in/path",         // space
	}
	for _, path := range invalid {
		if err := validateTapNetns(path); err == nil {
			t.Errorf("expected %q to be invalid", path)
		}
	}

	// Path length limit.
	long := "/" + strings.Repeat("a", 256)
	if err := validateTapNetns(long); err == nil {
		t.Error("expected overlong path to be rejected")
	}
}

func TestValidateDriveID(t *testing.T) {
	t.Parallel()
	valid := []string{"drive-virtio-disk0", "drive0", "virtio-blk-pci0", "a", "mirror-drive-virtio-disk0"}
	for _, id := range valid {
		if err := validateDriveID(id); err != nil {
			t.Errorf("expected %q to be valid, got: %v", id, err)
		}
	}
	invalid := []string{
		"",                       // empty
		"-leading-hyphen",        // starts with hyphen
		".leading-dot",           // starts with dot
		"drive;rm -rf /",         // shell injection
		"drive$(id)",             // command substitution
		"drive id",               // space
		"drive\nid",              // newline
		"drive:colon",            // colon (NBD URI delimiter)
		strings.Repeat("a", 257), // too long
	}
	for _, id := range invalid {
		if err := validateDriveID(id); err == nil {
			t.Errorf("expected %q to be invalid", id)
		}
	}
}

func TestFormatQEMUHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		addr netip.Addr
		want string
	}{
		{"ipv4", netip.MustParseAddr("10.0.0.1"), "10.0.0.1"},
		{"ipv4 loopback", netip.MustParseAddr("127.0.0.1"), "127.0.0.1"},
		{"ipv6 full", netip.MustParseAddr("fd00::1"), "[fd00::1]"},
		{"ipv6 loopback", netip.MustParseAddr("::1"), "[::1]"},
		{"ipv6 long", netip.MustParseAddr("2001:db8::1"), "[2001:db8::1]"},
		{"ipv4-mapped ipv6", netip.MustParseAddr("::ffff:10.0.0.1"), "10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatQEMUHost(tt.addr); got != tt.want {
				t.Errorf("formatQEMUHost(%v) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}
