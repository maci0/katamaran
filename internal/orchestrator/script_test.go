package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		req     Request
		wantErr string
	}{
		{
			name:    "missing nodes",
			req:     Request{DestIP: "1.2.3.4", Image: "x"},
			wantErr: "SourceNode and DestNode",
		},
		{
			name:    "same node",
			req:     Request{SourceNode: "n", DestNode: "n", DestIP: "1.2.3.4", Image: "x"},
			wantErr: "must differ",
		},
		{
			name:    "no source spec",
			req:     Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x"},
			wantErr: "either SourcePod or",
		},
		{
			name:    "partial source pod",
			req:     Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourcePod: &PodRef{Name: "p"}},
			wantErr: "Name and Namespace",
		},
		{
			name: "pod mode with advanced overrides",
			req:  Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourcePod: &PodRef{Namespace: "ns", Name: "p"}, SourceQMP: "/q-override", VMIP: "10.0.0.99"},
		},
		{
			name: "valid pod mode",
			req:  Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourcePod: &PodRef{Namespace: "ns", Name: "p"}},
		},
		{
			name: "valid legacy mode",
			req:  Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourceQMP: "/q", VMIP: "10.0.0.1"},
		},
		{
			name:    "unsafe source pod name",
			req:     Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourcePod: &PodRef{Namespace: "ns", Name: "p;sh"}},
			wantErr: "SourcePod.Name contains invalid characters",
		},
		{
			name:    "unsafe path traversal",
			req:     Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourceQMP: "/run/../qmp.sock", VMIP: "10.0.0.1"},
			wantErr: "SourceQMP contains invalid path traversal",
		},
		{
			name:    "invalid tunnel mode",
			req:     Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourcePod: &PodRef{Namespace: "ns", Name: "p"}, TunnelMode: "vxlan"},
			wantErr: "TunnelMode must be one of",
		},
		{
			name:    "invalid multifd",
			req:     Request{SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x", SourcePod: &PodRef{Namespace: "ns", Name: "p"}, MultifdChannels: -1},
			wantErr: "MultifdChannels must be non-negative",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestBuildArgs_PodMode(t *testing.T) {
	t.Parallel()
	s := NewScript("/usr/local/bin/migrate.sh")
	args, err := s.buildArgs(Request{
		SourceNode:    "n1",
		DestNode:      "n2",
		DestIP:        "10.0.0.10",
		Image:         "katamaran:dev",
		SourcePod:     &PodRef{Namespace: "default", Name: "vm-a"},
		DestPod:       &PodRef{Namespace: "default", Name: "shell-b"},
		SharedStorage: true,
		ReplayCmdline: true,
		DowntimeMS:    25,
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	want := []string{
		"/usr/local/bin/migrate.sh",
		"--source-node", "n1",
		"--dest-node", "n2",
		"--dest-ip", "10.0.0.10",
		"--image", "katamaran:dev",
		"--pod-name", "vm-a",
		"--pod-namespace", "default",
		"--dest-pod-name", "shell-b",
		"--dest-pod-namespace", "default",
		"--shared-storage",
		"--replay-cmdline",
		"--downtime", "25",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("unexpected args:\n got: %v\nwant: %v", args, want)
	}
}

func TestBuildArgs_LegacyMode(t *testing.T) {
	t.Parallel()
	s := NewScript("")
	args, err := s.buildArgs(Request{
		SourceNode: "n1",
		DestNode:   "n2",
		DestIP:     "10.0.0.10",
		Image:      "katamaran:dev",
		SourceQMP:  "/run/vc/vm/abc/extra-monitor.sock",
		VMIP:       "10.244.1.5",
		DestQMP:    "/run/vc/vm/def/extra-monitor.sock",
		TapIface:   "tap0_kata",
		TunnelMode: "gre",
	})
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	if !slices.Contains(args, "--qmp-source") || !slices.Contains(args, "--qmp-dest") {
		t.Fatalf("legacy args missing qmp flags: %v", args)
	}
	if !slices.Contains(args, "--tunnel-mode") {
		t.Fatalf("legacy args missing tunnel-mode: %v", args)
	}
}

func TestApplyAndWatch_StubScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "migrate.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewScript(stub)
	id, err := s.Apply(context.Background(), Request{
		SourceNode: "a", DestNode: "b", DestIP: "1.2.3.4", Image: "x",
		SourceQMP: "/q", VMIP: "10.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected non-empty migration id")
	}
	updates, err := s.Watch(context.Background(), id)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	got := drain(updates, 2*time.Second)
	if len(got) < 2 {
		t.Fatalf("want >=2 updates, got %d", len(got))
	}
	if got[0].Phase != PhaseSubmitted {
		t.Errorf("first update phase = %s, want %s", got[0].Phase, PhaseSubmitted)
	}
	last := got[len(got)-1]
	if !last.Phase.IsTerminal() {
		t.Errorf("last update is not terminal: %+v", last)
	}
	if last.Phase != PhaseSucceeded {
		t.Errorf("expected PhaseSucceeded, got %s (err=%v)", last.Phase, last.Error)
	}
}

func TestStop_UnknownID(t *testing.T) {
	t.Parallel()
	s := NewScript("")
	if err := s.Stop(context.Background(), "nope"); !errors.Is(err, ErrUnknownID) {
		t.Fatalf("want ErrUnknownID, got %v", err)
	}
}

func drain(ch <-chan StatusUpdate, deadline time.Duration) []StatusUpdate {
	var out []StatusUpdate
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, u)
			if u.Phase.IsTerminal() {
				return out
			}
		case <-timer.C:
			return out
		}
	}
}
