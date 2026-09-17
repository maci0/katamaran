package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/controller"
)

func TestValidListenAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		want bool
	}{
		{":8081", true},
		{"0.0.0.0:8081", true},
		{"[::1]:8081", true},
		{"localhost:8081", true},
		{":0", true},
		{":http", true},
		{"localhost", false},
		{":65536", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			if got := validListenAddr(tt.addr); got != tt.want {
				t.Fatalf("validListenAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestWriteProgressMetricsOrder(t *testing.T) {
	t.Parallel()
	snap := map[string]controller.MigrationProgressEntry{
		"mig-b": {Phase: "Transferring", RAMTransferred: 2, RAMTotal: 10, DowntimeMS: 20, AppliedDowntimeMS: 200, RTTMS: 2},
		"mig-c": {Phase: "Succeeded"},
		"mig-a": {Phase: "Failed", RAMTransferred: 1, RAMTotal: 10, DowntimeMS: 10, AppliedDowntimeMS: 100, RTTMS: 1},
	}
	var first, second bytes.Buffer
	writeProgressMetrics(&first, snap)
	writeProgressMetrics(&second, snap)
	if first.String() != second.String() {
		t.Fatalf("repeated scrapes of the same snapshot differ:\n%s\n---\n%s", first.String(), second.String())
	}
	want := []string{
		"katamaran_migration_ram_transferred_bytes{migration_id=\"mig-a\"} 1\n",
		"katamaran_migration_ram_transferred_bytes{migration_id=\"mig-b\"} 2\n",
		"katamaran_migration_ram_transferred_bytes{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_ram_total_bytes{migration_id=\"mig-a\"} 10\n",
		"katamaran_migration_ram_total_bytes{migration_id=\"mig-b\"} 10\n",
		"katamaran_migration_ram_total_bytes{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_phase{migration_id=\"mig-a\",phase=\"Failed\"} 1\n",
		"katamaran_migration_phase{migration_id=\"mig-b\",phase=\"Transferring\"} 1\n",
		"katamaran_migration_phase{migration_id=\"mig-c\",phase=\"Succeeded\"} 1\n",
		"katamaran_migration_downtime_ms{migration_id=\"mig-a\"} 10\n",
		"katamaran_migration_downtime_ms{migration_id=\"mig-b\"} 20\n",
		"katamaran_migration_downtime_ms{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_applied_downtime_ms{migration_id=\"mig-a\"} 100\n",
		"katamaran_migration_applied_downtime_ms{migration_id=\"mig-b\"} 200\n",
		"katamaran_migration_applied_downtime_ms{migration_id=\"mig-c\"} 0\n",
		"katamaran_migration_rtt_ms{migration_id=\"mig-a\"} 1\n",
		"katamaran_migration_rtt_ms{migration_id=\"mig-b\"} 2\n",
		"katamaran_migration_rtt_ms{migration_id=\"mig-c\"} 0\n",
	}
	var samples []string
	for line := range strings.SplitSeq(first.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			samples = append(samples, line+"\n")
		}
	}
	if got := strings.Join(samples, ""); got != strings.Join(want, "") {
		t.Fatalf("metric samples differ:\ngot:\n%swant:\n%s", got, strings.Join(want, ""))
	}
}
