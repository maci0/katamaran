package migration

import (
	"fmt"
	"regexp"
	"strings"
)

// validIfaceName matches Linux network interface names. IFNAMSIZ is 16 (15 usable
// chars). Allows alphanumerics, dots, hyphens, underscores, colons, and @ (VLAN).
var validIfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@:\-]{0,14}$`)

// validNetnsPath matches safe network namespace paths such as /proc/<pid>/ns/net
// or /var/run/netns/<name>. Only allows alphanumerics and common path characters.
var validNetnsPath = regexp.MustCompile(`^/[a-zA-Z0-9][a-zA-Z0-9/_.\-]*$`)

// validateTapIface checks that name is a valid Linux network interface name.
func validateTapIface(name string) error {
	if !validIfaceName.MatchString(name) {
		return fmt.Errorf("invalid tap interface name: %q", name)
	}
	return nil
}

// validateTapNetns checks that path is a safe network namespace path.
func validateTapNetns(path string) error {
	if len(path) > 256 {
		return fmt.Errorf("netns path too long: %d chars", len(path))
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("netns path contains path traversal: %q", path)
	}
	if !validNetnsPath.MatchString(path) {
		return fmt.Errorf("invalid netns path: %q", path)
	}
	return nil
}

// validDriveID matches QEMU block device IDs (e.g., "drive-virtio-disk0").
// Only allows alphanumerics, hyphens, underscores, and dots.
var validDriveID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-]{0,255}$`)

// validateDriveID checks that id is a safe QEMU block device identifier.
func validateDriveID(id string) error {
	if !validDriveID.MatchString(id) {
		return fmt.Errorf("invalid drive ID: %q", id)
	}
	return nil
}
