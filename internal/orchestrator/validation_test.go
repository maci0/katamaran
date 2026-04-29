package orchestrator

import (
	"strings"
	"testing"
)

func TestValidateRejectsNegativeAutoDowntimeFloor(t *testing.T) {
	t.Parallel()
	req := Request{
		SourceNode:          "worker-a",
		DestNode:            "worker-b",
		DestIP:              "10.0.0.20",
		Image:               "localhost/katamaran:dev",
		SourceQMP:           "/run/vc/vm/source/extra-monitor.sock",
		VMIP:                "10.244.1.5",
		AutoDowntime:        true,
		AutoDowntimeFloorMS: -1,
		MultifdChannels:     4,
	}
	err := Validate(req)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "autoDowntimeFloorMS") {
		t.Fatalf("expected autoDowntimeFloorMS error, got: %v", err)
	}
}
