package migration

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestErrMigrationFailed_Exists(t *testing.T) {
	t.Parallel()
	if ErrMigrationFailed == nil || !errors.Is(ErrMigrationFailed, ErrMigrationFailed) {
		t.Fatal("ErrMigrationFailed should be valid and matchable")
	}
	if ErrMigrationCancelled == nil || !errors.Is(ErrMigrationCancelled, ErrMigrationCancelled) {
		t.Fatal("ErrMigrationCancelled should be valid and matchable")
	}
	if errors.Is(ErrMigrationFailed, ErrMigrationCancelled) {
		t.Fatal("ErrMigrationFailed and ErrMigrationCancelled should be distinct")
	}
}

func TestRunSource_Failures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		sharedStorage bool
		tunnelMode    string
	}{
		{"BadQMPSocket", false, "ipip"},
		{"SharedStorage_BadQMPSocket", true, "ipip"},
		{"NonShared_BadQMPSocket", false, "gre"},
	}

	destIP := netip.MustParseAddr("10.0.0.1")
	vmIP := netip.MustParseAddr("10.244.1.15")

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunSource(
				context.Background(),
				"/nonexistent/qmp.sock",
				destIP,
				vmIP,
				"drive-virtio-disk0",
				tt.sharedStorage,
				tt.tunnelMode,
				25,
			)
			if err == nil {
				t.Fatal("expected error for nonexistent QMP socket")
			}
			if !strings.Contains(err.Error(), "QMP") {
				t.Fatalf("expected QMP-related error, got: %v", err)
			}
		})
	}
}
