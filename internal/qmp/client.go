// Package qmp implements a minimal synchronous client for the QEMU Machine Protocol.
//
// QMP is a JSON-based protocol for programmatic control of a QEMU instance.
// This client supports synchronous command execution and asynchronous event
// waiting. It is NOT safe for concurrent use — callers must serialize calls
// to Execute and WaitForEvent externally. The internal mutex only protects
// the connection state (nil check) and the buffered event queue.
package qmp

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

// Client timeouts.
const (
	// DialTimeout is the maximum time to wait for a QMP socket connection.
	DialTimeout = 10 * time.Second

	// GreetingTimeout is the maximum time to wait for the QMP greeting banner
	// during initial connection. If QEMU was started with -qmp wait=off, no
	// greeting is sent and we proceed after this timeout elapses.
	GreetingTimeout = 1 * time.Second

	// ExecuteTimeout is the maximum time to wait for a synchronous QMP
	// command response. If QEMU becomes unresponsive mid-command, Execute()
	// returns a timeout error instead of blocking forever.
	ExecuteTimeout = 2 * time.Minute
)

// Client is a minimal synchronous client for the QEMU Machine Protocol.
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	events []response // Buffered events received during synchronous command execution.
}

// NewClient connects to a QEMU QMP unix socket, performs the capability
// negotiation handshake, and returns a ready-to-use client.
func NewClient(ctx context.Context, socketPath string) (*Client, error) {
	var d net.Dialer
	d.Timeout = DialTimeout
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dialing QMP socket %s: %w", socketPath, err)
	}

	r := bufio.NewReader(conn)

	// Background context monitor: if the parent context is cancelled during
	// the handshake, close the connection to unblock any in-progress reads.
	// This is safe here because a handshake failure aborts the whole client.
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
	if err := conn.SetReadDeadline(time.Now().Add(GreetingTimeout)); err != nil {
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
	if err := conn.SetReadDeadline(time.Now().Add(DialTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting capabilities deadline: %w", err)
	}

	// Negotiate capabilities (required before any command can be issued).
	capReq := request{Execute: "qmp_capabilities"}
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
	var capResp response
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

	return &Client{conn: conn, r: r}, nil
}

// Close releases the underlying socket connection. It is safe to call
// multiple times; subsequent calls after the first return nil.
// It is thread-safe.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Execute sends a synchronous QMP command and returns the raw JSON response.
// Asynchronous events received while waiting for the reply are buffered so
// WaitForEvent can find them later.
//
// A read deadline of ExecuteTimeout (or the context deadline, whichever is
// sooner) is enforced. On context cancellation, the deadline is shortened to
// unblock reads without destroying the connection, preserving it for deferred
// cleanup commands.
//
// Returns an error if the connection has already been closed.
func (c *Client) Execute(ctx context.Context, cmd string, args Args) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return nil, fmt.Errorf("executing QMP command %q: connection is closed", cmd)
	}

	req := request{
		Execute:   cmd,
		Arguments: args,
	}

	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling QMP request %q: %w", cmd, err)
	}

	// Set a deadline so we don't block forever waiting for a response.
	deadline := time.Now().Add(ExecuteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	// Set both read and write deadlines to prevent getting stuck writing
	// to a full socket buffer.
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting IO deadline for %q: %w", cmd, err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	// Monitor context cancellation. Instead of closing the connection
	// (which would break deferred cleanup commands that run after cancel),
	// shorten the deadline to unblock any in-progress reads immediately.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	if _, err = conn.Write(append(b, '\n')); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("writing QMP command %q: %w", cmd, err)
	}

	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, fmt.Errorf("timed out waiting for QMP response to %q after %v", cmd, ExecuteTimeout)
			}
			return nil, fmt.Errorf("reading QMP response for %q: %w", cmd, err)
		}

		var resp response
		if err = json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("unmarshaling QMP response for %q: %w", cmd, err)
		}

		// Buffer asynchronous events received while waiting for the command response.
		// If we discard them here, WaitForEvent might hang forever waiting for an
		// event that already arrived.
		if resp.Event != "" {
			c.mu.Lock()
			c.events = append(c.events, resp)
			c.mu.Unlock()
			continue
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("QMP command %q failed: %w", cmd, resp.Error)
		}

		return resp.Return, nil
	}
}

// WaitForEvent blocks until the named QMP event is received or the timeout
// elapses. Non-matching events are silently discarded. The buffered event
// queue is checked first to find events that arrived during prior Execute calls.
//
// On context cancellation, the read deadline is shortened to unblock without
// destroying the connection.
//
// Returns an error if the connection has already been closed.
func (c *Client) WaitForEvent(ctx context.Context, eventName string, timeout time.Duration) error {
	c.mu.Lock()
	conn := c.conn
	// Check the buffered events first — an event might have arrived while we
	// were executing a synchronous command.
	for i, ev := range c.events {
		if ev.Event == eventName {
			// Remove the matched event from the buffer.
			// Copy elements and zero the last slot to prevent memory leaks.
			copy(c.events[i:], c.events[i+1:])
			c.events[len(c.events)-1] = response{}
			c.events = c.events[:len(c.events)-1]
			c.mu.Unlock()
			return nil
		}
	}
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("waiting for QMP event %q: connection is closed", eventName)
	}

	// Set a read deadline for the event wait. Use the shorter of the
	// explicit timeout or the parent context's deadline.
	eventDeadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(eventDeadline) {
		eventDeadline = d
	}
	if err := conn.SetReadDeadline(eventDeadline); err != nil {
		return fmt.Errorf("setting event read deadline: %w", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	// Monitor context cancellation: shorten the deadline to unblock reads
	// without closing the connection.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()

	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return fmt.Errorf("timed out waiting for QMP event %q after %v", eventName, timeout)
			}
			return fmt.Errorf("reading QMP event stream: %w", err)
		}

		var resp response
		if err = json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("unmarshaling QMP event: %w", err)
		}

		if resp.Event == eventName {
			return nil
		}
	}
}
