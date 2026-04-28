package main

import "testing"

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
