package migration

import (
	"context"
	"strings"
	"testing"
)

func TestRunDestination_BadQMPSocket(t *testing.T) {
	t.Parallel()
	err := RunDestination(
		context.Background(),
		"/nonexistent/qmp.sock",
		"", // no tap — skip qdisc
		"drive-virtio-disk0",
		false,
	)
	if err == nil {
		t.Fatal("expected error for nonexistent QMP socket")
	}
	if !strings.Contains(err.Error(), "QMP") {
		t.Fatalf("expected QMP-related error, got: %v", err)
	}
}

func TestRunDestination_SharedStorage_BadQMPSocket(t *testing.T) {
	t.Parallel()
	err := RunDestination(
		context.Background(),
		"/nonexistent/qmp.sock",
		"",
		"drive-virtio-disk0",
		true,
	)
	if err == nil {
		t.Fatal("expected error for nonexistent QMP socket")
	}
}

func TestRunDestination_WithTap_BadQMPSocket(t *testing.T) {
	t.Parallel()
	err := RunDestination(
		context.Background(),
		"/nonexistent/qmp.sock",
		"nonexistent-tap0",
		"drive-virtio-disk0",
		false,
	)
	if err == nil {
		t.Fatal("expected error for nonexistent QMP socket")
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
