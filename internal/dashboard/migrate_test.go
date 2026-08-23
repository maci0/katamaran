package dashboard

import (
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
