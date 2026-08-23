package main

import (
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

func TestNewStatusOutputTimeUTC(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 3, 29, 1, 30, 5, int(250*time.Millisecond), time.UTC)
	out := newStatusOutput(orchestrator.StatusUpdate{When: when})
	want := "2026-03-29T01:30:05.250Z"
	if out.Time != want {
		t.Fatalf("got %q, want %q", out.Time, want)
	}
	if _, err := time.Parse(time.RFC3339Nano, out.Time); err != nil {
		t.Fatalf("output not RFC3339-parseable: %v", err)
	}
}

func TestNewStatusOutputTimeOffsetTruthful(t *testing.T) {
	t.Parallel()
	// Non-UTC instants must be normalized to UTC, not rendered in whatever
	// zone the producer happened to carry (host TZ must not leak into
	// status output). The Z07:00 token keeps the offset truthful if the
	// normalization is ever dropped.
	loc := time.FixedZone("test", 2*60*60)
	out := newStatusOutput(orchestrator.StatusUpdate{
		When: time.Date(2026, 3, 29, 3, 30, 5, int(250*time.Millisecond), loc),
	})
	want := "2026-03-29T01:30:05.250Z"
	if out.Time != want {
		t.Fatalf("got %q, want %q", out.Time, want)
	}
	if _, err := time.Parse(time.RFC3339Nano, out.Time); err != nil {
		t.Fatalf("output not RFC3339-parseable: %v", err)
	}
}
