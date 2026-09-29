package migration

import (
	"errors"
	"strings"
	"testing"
)

// The CLI calls these before a migration starts so a bad flag exits 2
// instead of failing the run. Each rejection must name the flag it came
// from and carry ErrInvalidConfig, which is what Run picks on.
func TestValidateConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{"SourceClean", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"drive-virtio-disk0"}, TunnelMode: TunnelModeGRE})
		}, ""},
		{"SourceEmptyTunnelMode", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"drive-virtio-disk0"}})
		}, ""},
		{"SourceSharedStorageSkipsDrives", func() error {
			return ValidateSourceConfig(SourceConfig{SharedStorage: true, DriveIDs: []string{"bad;id"}})
		}, ""},
		{"SourceBadTunnelMode", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"drive-virtio-disk0"}, TunnelMode: "vxlan"})
		}, "invalid --tunnel-mode"},
		{"SourceNegativeMultifd", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"drive-virtio-disk0"}, MultifdChannels: -1})
		}, "--multifd-channels must be non-negative"},
		{"SourceBadDriveID", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"drive 0"}})
		}, "--drive-id"},
		{"SourceDuplicateDriveID", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"disk0", "disk0"}})
		}, "duplicate drive ID"},
		{"SourceCmdlineWithoutPod", func() error {
			return ValidateSourceConfig(SourceConfig{DriveIDs: []string{"drive-virtio-disk0"}, EmitCmdlineTo: "/tmp/cmdline"})
		}, "--emit-cmdline-to requires pod mode"},
		{"SourceCmdlineWithPod", func() error {
			return ValidateSourceConfig(SourceConfig{
				DriveIDs:      []string{"drive-virtio-disk0"},
				PodName:       "kata-demo",
				PodNamespace:  "default",
				EmitCmdlineTo: "/tmp/cmdline",
			})
		}, ""},
		{"DestClean", func() error {
			return ValidateDestConfig(DestConfig{DriveIDs: []string{"drive-virtio-disk0"}, TapIface: "tap0_kata"})
		}, ""},
		{"DestNoTap", func() error {
			return ValidateDestConfig(DestConfig{DriveIDs: []string{"drive-virtio-disk0"}})
		}, ""},
		{"DestBadTap", func() error {
			return ValidateDestConfig(DestConfig{DriveIDs: []string{"drive-virtio-disk0"}, TapIface: "tap0;tap1"})
		}, "--tap"},
		{"DestBadTapNetns", func() error {
			return ValidateDestConfig(DestConfig{DriveIDs: []string{"drive-virtio-disk0"}, TapNetns: "/proc/1/ns/../net"})
		}, "--tap-netns"},
		{"DestNegativeMultifd", func() error {
			return ValidateDestConfig(DestConfig{DriveIDs: []string{"drive-virtio-disk0"}, MultifdChannels: -3})
		}, "--multifd-channels must be non-negative"},
		{"DestBadDriveID", func() error {
			return ValidateDestConfig(DestConfig{DriveIDs: []string{"disk;0"}})
		}, "--drive-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.run()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want it to wrap ErrInvalidConfig", err)
			}
		})
	}
}
