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
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"
)

// Client timeouts.
const (
	// dialTimeout is the maximum time to wait for a QMP socket connection.
	dialTimeout = 10 * time.Second

	// greetingTimeout is the maximum time to wait for the QMP greeting banner
	// during initial connection. If no greeting arrives (e.g. the greeting was
	// already consumed by a prior connection), we proceed after this timeout.
	greetingTimeout = 1 * time.Second

	// executeTimeout is the maximum time to wait for a synchronous QMP
	// command response. If QEMU becomes unresponsive mid-command, Execute()
	// returns a timeout error instead of blocking forever.
	executeTimeout = 2 * time.Minute

	// maxBufferedEvents caps the in-memory event queue size. Normal operation
	// buffers 1-3 events between Execute and WaitForEvent calls. This limit
	// prevents unbounded memory growth if a misbehaving QEMU floods the
	// socket with asynchronous events.
	maxBufferedEvents = 1000

	// maxLineSize caps the partial-line accumulation buffer. Prevents
	// unbounded memory growth if QEMU sends continuous data without
	// newlines (malicious or buggy). 4 MiB is far above any legitimate
	// QMP message (~10 KiB typical).
	maxLineSize = 4 * 1024 * 1024
)

// Client is a minimal synchronous client for the QEMU Machine Protocol.
type Client struct {
	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	events []response // Buffered events received during synchronous command execution.
	buf    []byte     // Unprocessed partial line data from timeouts.
	socket string     // Socket path for diagnostic logging.
}

// bufferEvent adds an asynchronous event to the internal queue.
// The queue is capped at maxBufferedEvents; oldest events are dropped.
func (c *Client) bufferEvent(ev response) {
	c.mu.Lock()
	if len(c.events) >= maxBufferedEvents {
		slog.Error("QMP event buffer full, dropping oldest event", "dropped", c.events[0].Event, "incoming", ev.Event, "queued", len(c.events), "socket", c.socket)
		c.events = slices.Delete(c.events, 0, 1)
	}
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

// readLine reads a complete newline-terminated JSON message.
// It safely accumulates partial reads across errors (e.g., timeouts), preventing data loss.
// The accumulation buffer is capped at maxLineSize to prevent unbounded memory growth
// from a misbehaving QEMU that sends data without newlines.
func (c *Client) readLine() ([]byte, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if len(line) > 0 {
			c.buf = append(c.buf, line...)
		}
		if len(c.buf) > maxLineSize {
			c.buf = nil
			return nil, fmt.Errorf("QMP line exceeds %d bytes, discarding", maxLineSize)
		}
		return nil, fmt.Errorf("reading QMP line: %w", err)
	}
	// Fast path: no prior partial data from a timeout — return the line
	// directly from ReadBytes without copying into the accumulation buffer.
	if len(c.buf) == 0 {
		return line, nil
	}
	// Slow path: had partial data from a previous timeout — assemble
	// the complete line from accumulated fragments.
	c.buf = append(c.buf, line...)
	fullLine := c.buf
	c.buf = nil
	return fullLine, nil
}

// NewClient connects to a QEMU QMP unix socket, performs the capability
// negotiation handshake, and returns a ready-to-use client.
func NewClient(ctx context.Context, socketPath string) (*Client, error) {
	var d net.Dialer
	d.Timeout = dialTimeout
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dialing QMP socket %s: %w", socketPath, err)
	}

	c := &Client{
		conn:   conn,
		r:      bufio.NewReader(conn),
		socket: socketPath,
	}

	// If the context is cancelled during the handshake, close the connection
	// to unblock any in-progress reads. context.AfterFunc provides an atomic
	// stop() that guarantees the callback will NOT fire if stop() returns
	// true, preventing a race where ctx cancellation could close a
	// successfully-returned connection.
	stop := context.AfterFunc(ctx, func() {
		conn.Close()
	})
	fail := func(err error) (*Client, error) {
		stop()
		conn.Close()
		return nil, err
	}

	if err := conn.SetReadDeadline(time.Now().Add(greetingTimeout)); err != nil {
		return fail(fmt.Errorf("setting greeting deadline: %w", err))
	}

	if _, err := c.readLine(); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Ignore timeout — QEMU likely skipped the greeting (wait=off).
		} else {
			return fail(fmt.Errorf("reading QMP greeting: %w", err))
		}
	}

	if err := conn.SetReadDeadline(time.Now().Add(dialTimeout)); err != nil {
		return fail(fmt.Errorf("setting capabilities deadline: %w", err))
	}

	capReq := request{Execute: "qmp_capabilities"}
	capBytes, err := json.Marshal(capReq)
	if err != nil {
		return fail(fmt.Errorf("marshaling qmp_capabilities: %w", err))
	}
	if _, err = conn.Write(append(capBytes, '\n')); err != nil {
		return fail(fmt.Errorf("sending qmp_capabilities: %w", err))
	}

	capLine, err := c.readLine()
	if err != nil {
		return fail(fmt.Errorf("reading qmp_capabilities response: %w", err))
	}
	var capResp response
	if err = json.Unmarshal(capLine, &capResp); err != nil {
		return fail(fmt.Errorf("unmarshaling qmp_capabilities response: %w", err))
	}
	if capResp.Error != nil {
		return fail(fmt.Errorf("qmp_capabilities rejected: %w", capResp.Error))
	}

	if err = conn.SetReadDeadline(time.Time{}); err != nil {
		return fail(fmt.Errorf("clearing handshake deadline: %w", err))
	}

	// Atomically prevent the AfterFunc from closing the connection.
	// If stop() returns false, the callback already fired — conn is closed.
	if !stop() {
		return nil, fmt.Errorf("QMP handshake interrupted for %s: %w", socketPath, ctx.Err())
	}

	slog.Debug("QMP client connected", "socket", socketPath)
	return c, nil
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
	slog.Debug("QMP client disconnected", "socket", c.socket)
	if err != nil {
		return fmt.Errorf("closing QMP connection: %w", err)
	}
	return nil
}

// Execute sends a synchronous QMP command and returns the raw JSON response.
// Asynchronous events received while waiting for the reply are buffered so
// WaitForEvent can find them later.
//
// A read deadline of executeTimeout (or the context deadline, whichever is
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
	deadline := time.Now().Add(executeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	// Set both read and write deadlines to prevent getting stuck writing
	// to a full socket buffer.
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting IO deadline for %q: %w", cmd, err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	// On context cancellation, shorten the deadline to unblock in-progress reads
	// rather than closing the connection. This preserves the socket for deferred
	// cleanup commands (migrate-cancel, block-job-cancel) that run after the main
	// context is cancelled.
	//
	// callbackDone synchronizes the context.AfterFunc callback with our deferred
	// SetDeadline(time.Time{}) clear — without it, the defer could race and undo
	// the deadline we just set to interrupt the read.
	callbackDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(callbackDone)
	})
	defer func() {
		if !stopCancel() {
			<-callbackDone
		}
	}()

	cmdStart := time.Now()
	slog.Debug("QMP execute", "cmd", cmd)
	if _, err = conn.Write(append(b, '\n')); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("QMP command %q interrupted: %w", cmd, ctx.Err())
		}
		return nil, fmt.Errorf("writing QMP command %q: %w", cmd, err)
	}

	for {
		line, err := c.readLine()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("QMP command %q interrupted: %w", cmd, ctx.Err())
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return nil, fmt.Errorf("timed out waiting for QMP response to %q after %v: %w", cmd, executeTimeout, err)
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("QEMU closed the QMP connection unexpectedly during %q (did QEMU crash?): %w", cmd, err)
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
			c.bufferEvent(resp)
			continue
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("QMP command %q failed: %w", cmd, resp.Error)
		}

		elapsed := time.Since(cmdStart)
		if elapsed >= 1*time.Second {
			slog.Warn("Slow QMP command", "cmd", cmd, "elapsed", elapsed.Round(time.Millisecond))
		} else {
			slog.Debug("QMP command completed", "cmd", cmd, "elapsed", elapsed.Round(time.Millisecond))
		}
		return resp.Return, nil
	}
}

// WaitForEvent blocks until the named QMP event is received or the timeout
// elapses. Non-matching events are buffered for later retrieval. The buffered
// event queue is checked first to find events that arrived during prior
// Execute or WaitForEvent calls.
//
// On context cancellation, the read deadline is shortened to unblock without
// destroying the connection.
//
// Returns an error if the connection has already been closed and no matching
// event is found in the buffer.
func (c *Client) WaitForEvent(ctx context.Context, eventName string, timeout time.Duration) error {
	c.mu.Lock()
	conn := c.conn
	// Check the buffered events first — an event might have arrived while we
	// were executing a synchronous command.
	for i, ev := range c.events {
		if ev.Event == eventName {
			// Remove the matched event from the buffer.
			c.events = slices.Delete(c.events, i, i+1)
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

	// On context cancellation, shorten the read deadline instead of closing the
	// connection — same strategy as Execute(). callbackDone prevents the deferred
	// SetReadDeadline(time.Time{}) from racing with the cancellation callback.
	callbackDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.SetReadDeadline(time.Now())
		close(callbackDone)
	})
	defer func() {
		if !stopCancel() {
			<-callbackDone
		}
	}()

	for {
		line, err := c.readLine()
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("waiting for QMP event %q interrupted: %w", eventName, ctx.Err())
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return fmt.Errorf("timed out waiting for QMP event %q after %v: %w", eventName, timeout, err)
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("QEMU closed the QMP connection unexpectedly while waiting for %q (did QEMU crash?): %w", eventName, err)
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

		// Buffer non-matching events so they aren't lost. Without this,
		// events arriving between WaitForEvent calls would be silently
		// dropped, causing subsequent WaitForEvent calls to hang.
		if resp.Event != "" {
			slog.Debug("Buffered non-matching QMP event", "received", resp.Event, "waiting_for", eventName)
			c.bufferEvent(resp)
		}
	}
}
