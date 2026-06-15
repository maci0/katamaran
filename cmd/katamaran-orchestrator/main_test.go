package main

import (
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/orchestrator"
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

// FuzzReadRequest exercises the orchestrator's untrusted stdin entry point:
// arbitrary bytes decoded into an orchestrator.Request and then validated.
// readRequest and Validate must never panic on malformed input, and any
// Request that readRequest accepts must be safe to hand straight to Validate
// (the production main() path) without crashing.
func FuzzReadRequest(f *testing.F) {
	seeds := []string{
		"",
		"   ",
		"{}",
		"[]",
		"null",
		"{\"SourceNode\":\"worker-a\"}",
		"{\"SourceNode\":\"a\",\"DestNode\":\"b\",\"DestIP\":\"10.0.0.1\",\"Image\":\"img\",\"SourceQMP\":\"/q\",\"VMIP\":\"10.0.0.2\"}",
		"{\"SourceNode\":\"a\",\"TypoField\":true}",
		"{\"SourceNode\":\"worker-a\"} {\"DestNode\":\"worker-b\"}",
		"{\"DowntimeMS\":-1}",
		"{\"DestIP\":\"$(whoami)\"}",
		"{\"VMIP\":\"not-an-ip\"}",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := readRequest(bytesReader(body))
		if err != nil {
			return
		}
		// Accepted by the parser: the production path calls Validate next, so
		// it must not panic regardless of the decoded field values.
		_ = orchestrator.Validate(req)
	})
}

// bytesReader avoids importing bytes in the non-fuzz test body just for one call.
func bytesReader(b []byte) *strings.Reader { return strings.NewReader(string(b)) }
