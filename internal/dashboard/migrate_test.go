package dashboard

import (
	"math"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/orchestrator"
)

// TestHumanBytes pins the IEC-unit formatting of the final migration log
// line, including the KiB/MiB/GiB boundaries where an off-by-one divisor
// switch silently mislabels transfer sizes.
func TestHumanBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{-5, "-5 B"},
		{1023, "1023 B"},
		{1 << 10, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1<<20 - 1, "1024.0 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 * 1 << 20, "3.0 MiB"},
		{1<<30 - 1, "1024.0 MiB"},
		{1 << 30, "1.00 GiB"},
		{int64(2.5 * float64(1<<30)), "2.50 GiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// TestTransferPercent pins the RAM share rendered into the migration log line.
// The value must be rounded rather than truncated, so a transfer at 99.9% does
// not read "99%" in the log while the progress bar beside it reads 100%, and
// clamped into [0,100] so a negative or over-complete count scraped from the
// source pod log cannot print a negative or >100% percentage.
func TestTransferPercent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		transferred, total int64
		want               int
	}{
		{"zero total", 100, 0, 0},
		{"empty", 0, 1024, 0},
		{"half", 512, 1024, 50},
		{"complete", 1024, 1024, 100},
		{"rounds up at 99.9", 999, 1000, 100},
		{"rounds up at 49.6", 496, 1000, 50},
		{"rounds down at 49.4", 494, 1000, 49},
		{"clamps above total", 2000, 1000, 100},
		{"clamps negative", -5, 1000, 0},
		{"negative total", 100, -1, 0},
		// Byte counts near int64 max must not overflow the intermediate
		// product the way transferred*100 would.
		{"huge counts", math.MaxInt64 / 2, math.MaxInt64, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := transferPercent(tt.transferred, tt.total); got != tt.want {
				t.Errorf("transferPercent(%d, %d) = %d, want %d", tt.transferred, tt.total, got, tt.want)
			}
		})
	}
}

// TestPhaseBreakdown pins the wall-clock split formatting: empty when the
// run was too short to round to a second or no transferring timestamp was
// observed (fast paths and test fakes), setup+xfer split otherwise.
func TestPhaseBreakdown(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		end     time.Time
		phaseAt map[orchestrator.StatusPhase]time.Time
		want    string
	}{
		{
			name: "sub-second run reports nothing",
			end:  start.Add(400 * time.Millisecond), // rounds to 0s
			want: "",
		},
		{
			name: "no transferring phase reports bare wall clock",
			end:  start.Add(35 * time.Second),
			want: "35s wall",
		},
		{
			name: "setup and transfer split",
			end:  start.Add(35 * time.Second),
			phaseAt: map[orchestrator.StatusPhase]time.Time{
				orchestrator.PhaseTransferring: start.Add(4 * time.Second),
			},
			want: "35s wall (4s setup + 31s xfer)",
		},
		{
			name: "phases rounded to seconds",
			end:  start.Add(35800 * time.Millisecond),
			phaseAt: map[orchestrator.StatusPhase]time.Time{
				orchestrator.PhaseTransferring: start.Add(3500 * time.Millisecond),
			},
			want: "36s wall (4s setup + 32s xfer)",
		},
		{
			name: "unstamped transferring phase reports bare wall clock",
			end:  start.Add(35 * time.Second),
			phaseAt: map[orchestrator.StatusPhase]time.Time{
				orchestrator.PhaseTransferring: {},
			},
			want: "35s wall",
		},
		{
			// A producer whose clock trails the dashboard's, e.g. a
			// cross-node orchestrator reporting an instant from before
			// this process started watching.
			name: "transferring stamp before start reports bare wall clock",
			end:  start.Add(35 * time.Second),
			phaseAt: map[orchestrator.StatusPhase]time.Time{
				orchestrator.PhaseTransferring: start.Add(-2 * time.Second),
			},
			want: "35s wall",
		},
		{
			name: "transferring stamp after end reports bare wall clock",
			end:  start.Add(35 * time.Second),
			phaseAt: map[orchestrator.StatusPhase]time.Time{
				orchestrator.PhaseTransferring: start.Add(40 * time.Second),
			},
			want: "35s wall",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := phaseBreakdown(start, tt.phaseAt, tt.end); got != tt.want {
				t.Errorf("phaseBreakdown() = %q, want %q", got, tt.want)
			}
		})
	}
}
