package main

import (
	"strings"
	"testing"
)

func TestReadRequestRejectsEmptyStdin(t *testing.T) {
	t.Parallel()
	_, err := readRequest(strings.NewReader(" \n\t "))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "request JSON is required on stdin") {
		t.Fatalf("unexpected error: %v", err)
	}
}
