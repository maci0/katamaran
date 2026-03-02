package main

import "testing"

func TestValidTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"empty", "", false},
		{"normal IPv4", "10.244.1.15", true},
		{"IPv4 with port", "10.244.1.15:80", true},
		{"hostname", "my-pod.default.svc.cluster.local", true},
		{"loopback", "127.0.0.1", false},
		{"loopback6", "::1", false},
		{"link-local", "169.254.1.1", false},
		{"metadata endpoint", "169.254.169.254", false},
		{"shell metachar semicolon", "10.0.0.1;rm -rf /", false},
		{"shell metachar pipe", "10.0.0.1|cat /etc/passwd", false},
		{"shell metachar backtick", "`whoami`", false},
		{"shell metachar dollar", "${HOME}", false},
		{"newline injection", "10.0.0.1\nmalicious", false},
		{"space injection", "10.0.0.1 -c 1 127.0.0.1", false},
		{"valid IPv6", "fd00::1", true},
		{"valid external", "8.8.8.8", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := validTarget(tt.target); got != tt.want {
				t.Errorf("validTarget(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestValidFormValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"normal node name", "worker-1", true},
		{"IP address", "10.0.0.1", true},
		{"socket path", "/run/vc/vm/extra-monitor.sock", true},
		{"image ref", "localhost/katamaran:dev", true},
		{"semicolon injection", "worker-1;rm -rf /", false},
		{"pipe injection", "worker-1|evil", false},
		{"backtick injection", "`whoami`", false},
		{"dollar injection", "${HOME}", false},
		{"newline", "worker-1\nevil", false},
		{"empty", "", true},
		{"spaces", "node 1", false},
		{"tab", "node\t1", false},
		{"ampersand", "a&b", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := validFormValue(tt.value); got != tt.want {
				t.Errorf("validFormValue(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
