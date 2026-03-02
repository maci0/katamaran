package migration

import (
	"context"
	"strings"
	"testing"
)

func TestRunDestination_Failures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		tap           string
		sharedStorage bool
	}{
		{"BadQMPSocket", "", false},
		{"SharedStorage_BadQMPSocket", "", true},
		{"WithTap_BadQMPSocket", "nonexistent-tap0", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunDestination(
				context.Background(),
				"/nonexistent/qmp.sock",
				tt.tap,
				"drive-virtio-disk0",
				tt.sharedStorage,
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

func TestRunDestination_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := RunDestination(ctx, "/nonexistent/qmp.sock", "", "drive-virtio-disk0", false)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
