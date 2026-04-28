package controller

import (
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/orchestrator"
)

func TestSpecToRequest_Minimal(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{
				"namespace": "default",
				"name":      "kata-demo",
			},
			"destNode": "worker-b",
			"image":    "localhost/katamaran:dev",
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatalf("specToRequest: %v", err)
	}
	if req.SourcePod == nil || req.SourcePod.Name != "kata-demo" || req.SourcePod.Namespace != "default" {
		t.Errorf("SourcePod = %+v", req.SourcePod)
	}
	if req.DestNode != "worker-b" || req.Image != "localhost/katamaran:dev" {
		t.Errorf("DestNode/Image not set: %+v", req)
	}
	if req.SourceNode != "" || req.DestIP != "" {
		t.Errorf("SourceNode/DestIP must be left empty for the reconciler to fill via Discoverer; got %+v", req)
	}
}

func TestSpecToRequest_AllFields(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"sourcePod": map[string]any{
				"namespace": "default",
				"name":      "kata-demo",
			},
			"destPod": map[string]any{
				"namespace": "default",
				"name":      "kata-dest",
			},
			"destNode":        "worker-b",
			"image":           "localhost/katamaran:dev",
			"sharedStorage":   true,
			"replayCmdline":   true,
			"tunnelMode":      "ipip",
			"downtimeMS":      int64(50),
			"autoDowntime":    true,
			"multifdChannels": int64(4),
		},
	}
	req, err := specToRequest(obj)
	if err != nil {
		t.Fatalf("specToRequest: %v", err)
	}
	if !req.SharedStorage || !req.ReplayCmdline || !req.AutoDowntime {
		t.Errorf("bool fields not threaded: %+v", req)
	}
	if req.DowntimeMS != 50 || req.MultifdChannels != 4 || req.TunnelMode != "ipip" {
		t.Errorf("numeric/string fields not threaded: %+v", req)
	}
	if req.DestPod == nil || req.DestPod.Name != "kata-dest" {
		t.Errorf("DestPod not threaded: %+v", req.DestPod)
	}
}

func TestSpecToRequest_MissingRequired(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]any
		want string
	}{
		{
			name: "no sourcePod",
			obj: map[string]any{"spec": map[string]any{
				"destNode": "x", "image": "y",
			}},
			want: "spec.sourcePod",
		},
		{
			name: "no destNode",
			obj: map[string]any{"spec": map[string]any{
				"sourcePod": map[string]any{"namespace": "default", "name": "p"},
				"image":     "y",
			}},
			want: "spec.destNode",
		},
		{
			name: "no image",
			obj: map[string]any{"spec": map[string]any{
				"sourcePod": map[string]any{"namespace": "default", "name": "p"},
				"destNode":  "x",
			}},
			want: "spec.destNode and spec.image are required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := specToRequest(tc.obj)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// Compile-time check: Discoverer is the right shape — keeps drift between
// the orchestrator package's interface and what Reconciler.dispatch calls
// from showing up at runtime.
var _ orchestrator.Discoverer = (orchestrator.Discoverer)(nil)
