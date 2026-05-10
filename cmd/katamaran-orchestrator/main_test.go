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

func TestReadRequestRejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := readRequest(strings.NewReader(`{"SourceNode":"worker-a","TypoField":true}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadRequestRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	_, err := readRequest(strings.NewReader(`{"SourceNode":"worker-a"} {"DestNode":"worker-b"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "single object") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadRequestRejectsOversizedJSON(t *testing.T) {
	t.Parallel()
	_, err := readRequest(strings.NewReader(strings.Repeat(" ", maxRequestJSONBytes+1)))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Fatalf("unexpected error: %v", err)
	}
}
