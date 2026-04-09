package migration

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunCmd(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}

	ctx := context.Background()

	tests := []struct {
		name    string
		cmd     string
		args    []string
		wantErr string
	}{
		{"Success", "true", nil, ""},
		{"Failure", "false", nil, "executing false"},
		{"WithOutput", "sh", []string{"-c", "echo 'failure output' >&2; exit 1"}, "failure output"},
		{"NotFound", "nonexistent-binary-xyz-123", nil, "executing nonexistent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := runCmd(ctx, tt.cmd, tt.args...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}

	// Context cancellation tested separately so the 200ms timeout starts
	// counting from when the parallel subtest actually runs, not from
	// when it's registered.
	t.Run("ContextCancelled", func(t *testing.T) {
		t.Parallel()
		cancelCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		err := runCmd(cancelCtx, "sleep", "30")
		if err == nil {
			t.Fatal("expected error for cancelled context")
		}
		if !strings.Contains(err.Error(), "cancel") {
			t.Fatalf("expected error containing %q, got: %v", "cancel", err)
		}
	})
}

func TestRunCmdInNetns_EmptyNetns(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	// With empty netnsPath, should delegate directly to runCmd.
	if err := runCmdInNetns(context.Background(), "", "true"); err != nil {
		t.Fatalf("expected success with empty netns, got: %v", err)
	}
}

func TestRunCmdInNetns_WithNetns(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	// Use a nonexistent PID so nsenter always fails — even as root.
	// This ensures the test detects regressions where runCmdInNetns
	// silently skips the nsenter code path.
	err := runCmdInNetns(context.Background(), "/proc/999999999/ns/net", "true")
	if err == nil {
		t.Fatal("expected error for nonexistent netns")
	}
	if !strings.Contains(err.Error(), "nsenter") {
		t.Fatalf("expected nsenter-related error, got: %v", err)
	}
}
