// Package logging provides shared logging helpers for katamaran binaries.
package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// parseLevel parses a log level string and returns the corresponding slog.Level.
// Returns false if the level string is not recognized.
func parseLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return 0, false
	}
}

// TransientLevel returns the slog level for a recurring transient error that
// has now failed `consecutive` times in a row: Debug until the 10th
// consecutive occurrence, then Warn on the 10th and every 30th after that.
// Persistent failures (a flapping socket, RBAC loss) surface in production
// logs without spamming every individual retry.
func TransientLevel(consecutive int) slog.Level {
	if consecutive < 1 {
		return slog.LevelDebug
	}
	if consecutive == 10 || consecutive%30 == 0 {
		return slog.LevelWarn
	}
	return slog.LevelDebug
}

// SetupLogger configures the default slog logger with the given format, level,
// and component name. The component is included as a default attribute on every
// log entry to distinguish services in aggregated log systems. Logs are written
// to w. Returns a non-nil error if format or level is invalid.
func SetupLogger(w io.Writer, format, level, component string) error {
	lvl, ok := parseLevel(level)
	if !ok {
		return fmt.Errorf("invalid log level %q (valid: debug, info, warn, error)", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return fmt.Errorf("invalid log format %q (valid: text, json)", format)
	}
	logger := slog.New(h)
	if component != "" {
		logger = logger.With("component", component)
	}
	slog.SetDefault(logger)
	return nil
}
