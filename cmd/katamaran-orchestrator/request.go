package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/maci0/katamaran/internal/orchestrator"
)

const maxRequestJSONBytes = 1 << 20

func readRequest(r io.Reader) (orchestrator.Request, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxRequestJSONBytes+1))
	if err != nil {
		return orchestrator.Request{}, fmt.Errorf("read stdin: %w", err)
	}
	if len(body) > maxRequestJSONBytes {
		return orchestrator.Request{}, fmt.Errorf("request JSON must be at most %d bytes", maxRequestJSONBytes)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return orchestrator.Request{}, fmt.Errorf("request JSON is required on stdin")
	}
	var req orchestrator.Request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return orchestrator.Request{}, fmt.Errorf("decode request JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return orchestrator.Request{}, fmt.Errorf("request JSON must contain a single object")
	}
	return req, nil
}
