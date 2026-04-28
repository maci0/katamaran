package logging

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func Test_parseLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  slog.Level
		ok    bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"", 0, false},
		{"DEBUG", 0, false},
		{"Info", 0, false},
		{"trace", 0, false},
		{"fatal", 0, false},
	}
	for _, tt := range tests {
		name := tt.input
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseLevel(tt.input)
			if ok != tt.ok {
				t.Fatalf("parseLevel(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestSetupLogger(t *testing.T) {
	// Successful SetupLogger calls mutate slog.Default. Save and restore so
	// subsequent tests in this package see the original logger.
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	tests := []struct {
		format  string
		level   string
		wantErr string
	}{
		{"text", "info", ""},
		{"json", "debug", ""},
		{"text", "warn", ""},
		{"json", "error", ""},
		{"yaml", "info", "invalid log format"},
		{"text", "trace", "invalid log level"},
		{"", "info", "invalid log format"},
		{"text", "", "invalid log level"},
	}
	for _, tt := range tests {
		t.Run(tt.format+"_"+tt.level, func(t *testing.T) {
			err := SetupLogger(io.Discard, tt.format, tt.level, "test")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
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
}
