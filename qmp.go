package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// QMPClient is a minimal synchronous client for the QEMU Machine Protocol.
//
// QMP is a JSON-based protocol for programmatic control of a QEMU instance.
// This client supports synchronous command execution and asynchronous event
// waiting. It is NOT safe for concurrent use.
type QMPClient struct {
	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	events []qmpResponse // Buffered events received during synchronous command execution
}

// qmpRequest represents a QMP command envelope.
type qmpRequest struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

// qmpResponse represents a QMP command response or asynchronous event.
type qmpResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *qmpError       `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
}

// qmpError represents a QMP protocol-level error.
type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qmpError) Error() string {
	return fmt.Sprintf("QMP error [%s]: %s", e.Class, e.Desc)
}

// blockJobInfo represents a single entry returned by query-block-jobs.
type blockJobInfo struct {
	Device string `json:"device"`
	Len    int64  `json:"len"`
	Offset int64  `json:"offset"`
	Ready  bool   `json:"ready"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// migrateInfo represents the response from query-migrate.
type migrateInfo struct {
	Status    string `json:"status"`
	ErrorDesc string `json:"error-desc,omitempty"`
}

// NewQMPClient connects to a QEMU QMP unix socket, performs the capability
// negotiation handshake, and returns a ready-to-use client.
func NewQMPClient(ctx context.Context, socketPath string) (*QMPClient, error) {
	// A simple way to respect the context during dial.
	var d net.Dialer
	d.Timeout = qmpDialTimeout
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dialing QMP socket %s: %w", socketPath, err)
	}

	r := bufio.NewReader(conn)

	// Background context monitor to close the connection if context is cancelled.
	// This immediately interrupts any blocked reads or writes.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	// Set a short read deadline specifically for the greeting.
	// This prevents hanging indefinitely if QEMU doesn't send a greeting (wait=off).
	if err := conn.SetReadDeadline(time.Now().Add(greetingTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting greeting deadline: %w", err)
	}

	// Attempt to read the QMP greeting banner.
	if _, err := r.ReadString('\n'); err != nil {
		var netErr net.Error
		// If we hit a timeout, assume QEMU didn't send a greeting and proceed.
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Ignore the timeout; QEMU likely skipped the greeting.
		} else {
			conn.Close()
			return nil, fmt.Errorf("reading QMP greeting: %w", err)
		}
	}

	// Reset the deadline for the rest of the handshake (capabilities negotiation).
	if err := conn.SetReadDeadline(time.Now().Add(qmpDialTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting capabilities deadline: %w", err)
	}

	// Negotiate capabilities (required before any command can be issued).
	capReq := qmpRequest{Execute: "qmp_capabilities"}
	capBytes, err := json.Marshal(capReq)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("marshaling qmp_capabilities: %w", err)
	}
	if _, err = conn.Write(append(capBytes, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending qmp_capabilities: %w", err)
	}

	// Read and validate the capabilities response.
	capLine, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading qmp_capabilities response: %w", err)
	}
	var capResp qmpResponse
	if err = json.Unmarshal([]byte(capLine), &capResp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("unmarshaling qmp_capabilities response: %w", err)
	}
	if capResp.Error != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp_capabilities rejected: %w", capResp.Error)
	}

	// Clear the handshake deadline — subsequent reads use per-call deadlines.
	if err = conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clearing handshake deadline: %w", err)
	}

	return &QMPClient{conn: conn, r: r}, nil
}

// Close releases the underlying socket connection. It is safe to call
// multiple times; subsequent calls after the first return nil.
// It is thread-safe.
func (q *QMPClient) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.conn == nil {
		return nil
	}
	err := q.conn.Close()
	q.conn = nil
	return err
}

// Execute sends a synchronous QMP command and returns the raw JSON response.
// Asynchronous events received while waiting for the reply are discarded.
// A read deadline of qmpExecuteTimeout is enforced to prevent indefinite
// hangs if QEMU becomes unresponsive mid-command.
// Returns an error if the connection has already been closed.
func (q *QMPClient) Execute(ctx context.Context, cmd string, args any) (json.RawMessage, error) {
	q.mu.Lock()
	conn := q.conn
	q.mu.Unlock()

	if conn == nil {
		return nil, fmt.Errorf("executing QMP command %q: connection is closed", cmd)
	}

	req := qmpRequest{
		Execute:   cmd,
		Arguments: args,
	}

	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling QMP request %q: %w", cmd, err)
	}

	// Set a deadline so we don't block forever waiting for a response.
	deadline := time.Now().Add(qmpExecuteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	// Set both read and write deadlines to prevent getting stuck writing
	// to a full socket buffer.
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting IO deadline for %q: %w", cmd, err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	// Background monitor for context cancellation
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	if _, err = conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("writing QMP command %q: %w", cmd, err)
	}

	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, fmt.Errorf("timed out waiting for QMP response to %q after %v", cmd, qmpExecuteTimeout)
			}
			return nil, fmt.Errorf("reading QMP response for %q: %w", cmd, err)
		}

		var resp qmpResponse
		if err = json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("unmarshaling QMP response for %q: %w", cmd, err)
		}

		// Buffer asynchronous events received while waiting for the command response.
		// If we discard them here, WaitForEvent might hang forever waiting for an
		// event that already arrived.
		if resp.Event != "" {
			q.mu.Lock()
			q.events = append(q.events, resp)
			q.mu.Unlock()
			continue
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("QMP command %q failed: %w", cmd, resp.Error)
		}

		return resp.Return, nil
	}
}

// WaitForEvent blocks until the named QMP event is received or the timeout
// elapses. Non-matching events are silently discarded.
// Returns an error if the connection has already been closed.
func (q *QMPClient) WaitForEvent(ctx context.Context, eventName string, timeout time.Duration) error {
	q.mu.Lock()
	conn := q.conn
	// Check the buffered events first! An event might have arrived while we
	// were executing a synchronous command.
	for i, ev := range q.events {
		if ev.Event == eventName {
			// Remove the matched event from the buffer
			// Copy elements and zero the last one to prevent memory leaks
			copy(q.events[i:], q.events[i+1:])
			q.events[len(q.events)-1] = qmpResponse{}
			q.events = q.events[:len(q.events)-1]
			q.mu.Unlock()
			return nil
		}
	}
	q.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("waiting for QMP event %q: connection is closed", eventName)
	}

	// Create a derived context with the timeout
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Background monitor to close connection on context cancellation
	// This immediately unblocks the ReadBytes below
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-waitCtx.Done():
			conn.Close()
		case <-done:
		}
	}()

	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			if waitCtx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out waiting for QMP event %q after %v", eventName, timeout)
			}
			if waitCtx.Err() == context.Canceled {
				return waitCtx.Err()
			}
			return fmt.Errorf("reading QMP event stream: %w", err)
		}

		var resp qmpResponse
		if err = json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("unmarshaling QMP event: %w", err)
		}

		if resp.Event == eventName {
			return nil
		}
	}
}

// QMP command argument types for strict typing

type nbdServerStartArgs struct {
	Addr nbdServerAddr `json:"addr"`
}

type nbdServerAddr struct {
	Type string            `json:"type"`
	Data nbdServerAddrData `json:"data"`
}

type nbdServerAddrData struct {
	Host string `json:"host"`
	Port string `json:"port"`
}

type nbdServerAddArgs struct {
	Device   string `json:"device"`
	Writable bool   `json:"writable"`
}

type driveMirrorArgs struct {
	Device string `json:"device"`
	Target string `json:"target"`
	Sync   string `json:"sync"`
	Mode   string `json:"mode"`
	JobID  string `json:"job-id"`
}

type blockJobCancelArgs struct {
	Device string `json:"device"`
	Force  bool   `json:"force"`
}

type migrateSetCapabilitiesArgs struct {
	Capabilities []migrationCapability `json:"capabilities"`
}

type migrationCapability struct {
	Capability string `json:"capability"`
	State      bool   `json:"state"`
}

type migrateSetParametersArgs struct {
	DowntimeLimit int64 `json:"downtime-limit"`
	MaxBandwidth  int64 `json:"max-bandwidth"`
}

type migrateArgs struct {
	URI string `json:"uri"`
}

type announceSelfArgs struct {
	Initial int `json:"initial"`
	Max     int `json:"max"`
	Rounds  int `json:"rounds"`
	Step    int `json:"step"`
}
