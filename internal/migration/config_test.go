package migration

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestCleanupCtx_HasTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := CleanupCtx()
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("CleanupCtx should have a deadline")
	}

	remaining := time.Until(deadline)
	if remaining < 5*time.Second || remaining > CleanupTimeout+time.Second {
		t.Fatalf("expected deadline ~%v from now, got %v", CleanupTimeout, remaining)
	}
}

func TestCleanupCtx_IndependentOfParent(t *testing.T) {
	t.Parallel()

	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	ctx, cancel := CleanupCtx()
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("CleanupCtx should not be cancelled when parent is")
	default:
	}

	_ = parent
}

func TestRunCmd_Success(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}

	ctx := context.Background()
	err := RunCmd(ctx, "true")
	if err != nil {
		t.Fatalf("RunCmd(true): %v", err)
	}
}

func TestRunCmd_Failure(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}

	ctx := context.Background()
	err := RunCmd(ctx, "false")
	if err == nil {
		t.Fatal("expected error from 'false' command")
	}
}

func TestRunCmd_WithOutput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	err := RunCmd(ctx, "sh", "-c", "echo 'failure output' >&2; exit 1")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !contains(got, "failure output") {
		t.Fatalf("expected error to contain command output, got: %v", err)
	}
}

func TestRunCmd_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := RunCmd(ctx, "sleep", "30")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if !contains(err.Error(), "cancel") {
		t.Fatalf("expected cancel-related error, got: %v", err)
	}
}

func TestRunCmd_NotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	err := RunCmd(ctx, "nonexistent-binary-xyz-123")
	if err == nil {
		t.Fatal("expected error for nonexistent binary")
	}
}

func TestConstants_Reasonable(t *testing.T) {
	t.Parallel()

	if NBDPort == "" {
		t.Fatal("NBDPort should not be empty")
	}
	if RAMMigrationPort == "" {
		t.Fatal("RAMMigrationPort should not be empty")
	}
	if MaxDowntimeMS <= 0 {
		t.Fatal("MaxDowntimeMS should be positive")
	}
	if MaxBandwidth <= 0 {
		t.Fatal("MaxBandwidth should be positive")
	}
	if EventWaitTimeout <= 0 {
		t.Fatal("EventWaitTimeout should be positive")
	}
	if StoragePollInterval <= 0 {
		t.Fatal("StoragePollInterval should be positive")
	}
	if MigrationPollInterval <= 0 {
		t.Fatal("MigrationPollInterval should be positive")
	}
	if PostMigrationTunnelDelay <= 0 {
		t.Fatal("PostMigrationTunnelDelay should be positive")
	}
	if PlugQdiscLimit == "" {
		t.Fatal("PlugQdiscLimit should not be empty")
	}
	if GARPRounds <= 0 {
		t.Fatal("GARPRounds should be positive")
	}
	if TunnelName == "" {
		t.Fatal("TunnelName should not be empty")
	}
	if MigrationTimeout <= 0 {
		t.Fatal("MigrationTimeout should be positive")
	}
	if StorageSyncTimeout <= 0 {
		t.Fatal("StorageSyncTimeout should be positive")
	}
	if JobAppearTimeout <= 0 {
		t.Fatal("JobAppearTimeout should be positive")
	}
	if CleanupTimeout <= 0 {
		t.Fatal("CleanupTimeout should be positive")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
