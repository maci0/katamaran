package orchestrator

import (
	"strings"
	"testing"
)

func TestValidateAcceptsAutoSelectedDestForSourcePod(t *testing.T) {
	t.Parallel()
	req := validRequestForValidation()
	req.SourceQMP = ""
	req.VMIP = ""
	req.SourcePod = &PodRef{Namespace: "default", Name: "kata-vm"}
	req.DestNode = ""
	req.DestIP = ""

	if err := Validate(req); err != nil {
		t.Fatalf("Validate auto-selected destination: %v", err)
	}
}

func TestValidateRejectsInvalidIPFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr string
	}{
		{
			name: "dest ip hostname",
			mutate: func(req *Request) {
				req.DestIP = "node-a"
			},
			wantErr: "destIP",
		},
		{
			name: "dest ip command injection",
			mutate: func(req *Request) {
				req.DestIP = "10.0.0.20;id"
			},
			wantErr: "DestIP contains invalid characters",
		},
		{
			name: "vm ip hostname",
			mutate: func(req *Request) {
				req.VMIP = "vm-a"
			},
			wantErr: "vmIP",
		},
		{
			name: "vm ip command injection",
			mutate: func(req *Request) {
				req.VMIP = "10.244.1.5$(id)"
			},
			wantErr: "VMIP contains invalid characters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := validRequestForValidation()
			tt.mutate(&req)

			err := Validate(req)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateRejectsUnsafeRequestArgValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Request)
		wantErr string
	}{
		{
			name: "source node shell metacharacter",
			mutate: func(req *Request) {
				req.SourceNode = "worker-a;id"
			},
			wantErr: "SourceNode contains invalid characters",
		},
		{
			name: "image path traversal",
			mutate: func(req *Request) {
				req.Image = "../katamaran:dev"
			},
			wantErr: "Image contains invalid path traversal",
		},
		{
			name: "source pod namespace shell metacharacter",
			mutate: func(req *Request) {
				req.SourceQMP = ""
				req.VMIP = ""
				req.SourcePod = &PodRef{Namespace: "default;id", Name: "kata-vm"}
			},
			wantErr: "SourcePod.Namespace contains invalid characters",
		},
		{
			name: "dest pod name whitespace",
			mutate: func(req *Request) {
				req.DestPod = &PodRef{Namespace: "default", Name: "dest vm"}
			},
			wantErr: "DestPod.Name contains invalid characters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := validRequestForValidation()
			tt.mutate(&req)

			err := Validate(req)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateSafeArgValueBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "empty"},
		{name: "safe chars", value: "registry.example.com/katamaran:dev@sha256=abc-123_ABC"},
		{name: "at length limit", value: strings.Repeat("a", MaxSafeArgValueLen)},
		{name: "over length limit", value: strings.Repeat("a", MaxSafeArgValueLen+1), wantErr: "too long"},
		{name: "path traversal", value: "/run/vc/../qmp.sock", wantErr: "path traversal"},
		{name: "space", value: "worker a", wantErr: "invalid characters"},
		{name: "newline", value: "worker-a\nid", wantErr: "invalid characters"},
		{name: "null byte", value: "worker-a\x00id", wantErr: "invalid characters"},
		{name: "command substitution", value: "worker-a$(id)", wantErr: "invalid characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSafeArgValue("field", tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSafeArgValue(%q): %v", tt.value, err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

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

func TestValidateRejectsNegativeCNIConvergenceDelay(t *testing.T) {
	t.Parallel()
	req := validRequestForValidation()
	req.CNIConvergenceDelaySeconds = -1

	err := Validate(req)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "cniConvergenceDelaySeconds") {
		t.Fatalf("expected cniConvergenceDelaySeconds error, got: %v", err)
	}
}

func TestValidateRejectsNegativePodWaitTimeout(t *testing.T) {
	t.Parallel()
	req := validRequestForValidation()
	req.PodWaitTimeoutSeconds = -1

	err := Validate(req)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "podWaitTimeoutSeconds") {
		t.Fatalf("expected podWaitTimeoutSeconds error, got: %v", err)
	}
}

func TestValidateRejectsInvalidSourceCleanup(t *testing.T) {
	t.Parallel()
	req := validRequestForValidation()
	req.SourceCleanup = "remove"

	err := Validate(req)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "sourceCleanup") {
		t.Fatalf("expected sourceCleanup error, got: %v", err)
	}
}

func validRequestForValidation() Request {
	return Request{
		SourceNode:      "worker-a",
		DestNode:        "worker-b",
		DestIP:          "10.0.0.20",
		Image:           "localhost/katamaran:dev",
		SourceQMP:       "/run/vc/vm/source/extra-monitor.sock",
		VMIP:            "10.244.1.5",
		MultifdChannels: 4,
	}
}
