package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// runCmdInNetns executes a command inside the given network namespace.
// If netnsPath is empty, it runs the command in the current namespace.
func runCmdInNetns(ctx context.Context, netnsPath string, name string, args ...string) error {
	if netnsPath == "" {
		return runCmd(ctx, name, args...)
	}
	nsArgs := append([]string{"--net=" + netnsPath, name}, args...)
	return runCmd(ctx, "nsenter", nsArgs...)
}

// runCmd executes an external command. It captures combined stdout/stderr and
// returns a wrapped error including the full command line and output on failure.
// If the context was cancelled or expired, the returned error wraps ctx.Err()
// (context.Canceled or context.DeadlineExceeded) for caller detection.
func runCmd(ctx context.Context, name string, args ...string) error {
	slog.Debug("Executing command", "command", name, "args", args)
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("command cancelled: %s %v: %w", name, args, ctx.Err())
		}
		errMsg := strings.TrimSpace(out.String())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if errMsg == "" {
				return fmt.Errorf("executing %s %v (exit %d): %w", name, args, exitErr.ExitCode(), err)
			}
			return fmt.Errorf("executing %s %v (exit %d): %s: %w", name, args, exitErr.ExitCode(), errMsg, err)
		}
		if errMsg == "" {
			return fmt.Errorf("executing %s %v: %w", name, args, err)
		}
		return fmt.Errorf("executing %s %v: %s: %w", name, args, errMsg, err)
	}
	slog.Debug("Command completed", "command", name, "elapsed", time.Since(start).Round(time.Millisecond))
	return nil
}
