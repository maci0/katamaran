package qmp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startFakeQMP creates a Unix listener that accepts one connection and runs handler.
func startFakeQMP(t *testing.T, handler func(conn net.Conn)) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return socketPath
}

// qmpHandshake performs the server side of the QMP greeting + capabilities handshake.
func qmpHandshake(conn net.Conn) {
	greeting := `{"QMP":{"version":{"qemu":{"micro":0,"minor":2,"major":6}}}}`
	conn.Write([]byte(greeting + "\n"))

	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	_ = n // consume qmp_capabilities

	conn.Write([]byte(`{"return":{}}` + "\n"))
}

func TestNewClient_FullHandshake(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(100 * time.Millisecond)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	if c.conn == nil {
		t.Fatal("expected non-nil connection")
	}
}

func TestNewClient_NoGreeting(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		// No greeting sent — client should time out on greeting read and proceed.
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"return":{}}` + "\n"))
		time.Sleep(100 * time.Millisecond)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient with no greeting: %v", err)
	}
	defer c.Close()
}

func TestNewClient_CapabilityRejected(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		conn.Write([]byte(`{"QMP":{}}` + "\n"))
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"error":{"class":"GenericError","desc":"caps rejected"}}` + "\n"))
	})

	ctx := context.Background()
	_, err := NewClient(ctx, sock)
	if err == nil {
		t.Fatal("expected error for rejected capabilities")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected 'rejected' in error, got: %v", err)
	}
}

func TestNewClient_BadSocket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := NewClient(ctx, "/nonexistent/qmp.sock")
	if err == nil {
		t.Fatal("expected error for nonexistent socket")
	}
}

func TestNewClient_ContextCancelled(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		// Hang forever — never send greeting.
		time.Sleep(30 * time.Second)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := NewClient(ctx, sock)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestExecute_Success(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"return":{"status":"completed"}}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	raw, err := c.Execute(ctx, "query-migrate", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result["status"] != "completed" {
		t.Fatalf("expected status=completed, got %s", result["status"])
	}
}

func TestExecute_WithArgs(t *testing.T) {
	t.Parallel()
	var received []byte

	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		received = make([]byte, n)
		copy(received, buf[:n])
		conn.Write([]byte(`{"return":{}}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	_, err = c.Execute(ctx, "migrate", MigrateArgs{URI: "tcp:10.0.0.1:4444"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if !strings.Contains(string(received), `"uri":"tcp:10.0.0.1:4444"`) {
		t.Fatalf("expected URI in request, got: %s", string(received))
	}
}

func TestExecute_QMPError(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"error":{"class":"GenericError","desc":"device not found"}}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	_, err = c.Execute(ctx, "drive-mirror", nil)
	if err == nil {
		t.Fatal("expected QMP error")
	}
	if !strings.Contains(err.Error(), "device not found") {
		t.Fatalf("expected 'device not found' in error, got: %v", err)
	}
}

func TestExecute_BuffersEvents(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		// Send an event before the response.
		conn.Write([]byte(`{"event":"STOP"}` + "\n"))
		conn.Write([]byte(`{"return":{}}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	_, err = c.Execute(ctx, "query-migrate", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	c.mu.Lock()
	eventCount := len(c.events)
	c.mu.Unlock()

	if eventCount != 1 {
		t.Fatalf("expected 1 buffered event, got %d", eventCount)
	}
}

func TestExecute_ClosedConnection(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(100 * time.Millisecond)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.Close()

	_, err = c.Execute(ctx, "query-migrate", nil)
	if err == nil {
		t.Fatal("expected error on closed connection")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected 'closed' in error, got: %v", err)
	}
}

func TestExecute_ContextCancelled(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		// Hang — never respond.
		time.Sleep(30 * time.Second)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	execCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	_, err = c.Execute(execCtx, "query-migrate", nil)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestWaitForEvent_FromBuffer(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"event":"STOP"}` + "\n"))
		conn.Write([]byte(`{"return":{}}` + "\n"))
		time.Sleep(time.Second)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	// Execute buffers the STOP event.
	_, err = c.Execute(ctx, "query-migrate", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// WaitForEvent should find it in the buffer immediately.
	err = c.WaitForEvent(ctx, "STOP", time.Second)
	if err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}

	// Buffer should now be empty.
	c.mu.Lock()
	count := len(c.events)
	c.mu.Unlock()
	if count != 0 {
		t.Fatalf("expected 0 buffered events after consumption, got %d", count)
	}
}

func TestWaitForEvent_FromWire(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(100 * time.Millisecond)
		conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	err = c.WaitForEvent(ctx, "RESUME", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}
}

func TestWaitForEvent_DiscardsNonMatching(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(50 * time.Millisecond)
		conn.Write([]byte(`{"event":"BLOCK_JOB_READY"}` + "\n"))
		conn.Write([]byte(`{"event":"STOP"}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	err = c.WaitForEvent(ctx, "STOP", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForEvent: %v", err)
	}
}

func TestWaitForEvent_Timeout(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(5 * time.Second)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	err = c.WaitForEvent(ctx, "RESUME", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected 'timed out' in error, got: %v", err)
	}
}

func TestWaitForEvent_ContextCancelled(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(30 * time.Second)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err = c.WaitForEvent(waitCtx, "RESUME", 30*time.Second)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestWaitForEvent_ClosedConnection(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(100 * time.Millisecond)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.Close()

	err = c.WaitForEvent(ctx, "RESUME", time.Second)
	if err == nil {
		t.Fatal("expected error on closed connection")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected 'closed' in error, got: %v", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(100 * time.Millisecond)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close should return nil, got: %v", err)
	}
}

func TestClose_ThreadSafe(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(time.Second)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Close()
		}()
	}
	wg.Wait()
}

func TestError_Format(t *testing.T) {
	t.Parallel()
	e := &Error{Class: "GenericError", Desc: "something broke"}
	want := "QMP error [GenericError]: something broke"
	if got := e.Error(); got != want {
		t.Fatalf("Error.Error() = %q, want %q", got, want)
	}
}

func TestArgs_JSONSerialization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args Args
		want map[string]interface{}
	}{
		{
			name: "NBDServerStartArgs",
			args: NBDServerStartArgs{
				Addr: NBDServerAddr{
					Type: "inet",
					Data: NBDServerAddrData{Host: "0.0.0.0", Port: "10809"},
				},
			},
			want: map[string]interface{}{
				"addr": map[string]interface{}{
					"type": "inet",
					"data": map[string]interface{}{
						"host": "0.0.0.0",
						"port": "10809",
					},
				},
			},
		},
		{
			name: "NBDServerAddArgs",
			args: NBDServerAddArgs{Device: "virtio0", Writable: true},
			want: map[string]interface{}{
				"device":   "virtio0",
				"writable": true,
			},
		},
		{
			name: "DriveMirrorArgs",
			args: DriveMirrorArgs{
				Device: "virtio0",
				Target: "nbd:10.0.0.1:10809:exportname=virtio0",
				Sync:   "full",
				Mode:   "existing",
				JobID:  "mirror-virtio0",
			},
			want: map[string]interface{}{
				"device": "virtio0",
				"target": "nbd:10.0.0.1:10809:exportname=virtio0",
				"sync":   "full",
				"mode":   "existing",
				"job-id": "mirror-virtio0",
			},
		},
		{
			name: "BlockJobCancelArgs",
			args: BlockJobCancelArgs{Device: "mirror-virtio0", Force: true},
			want: map[string]interface{}{
				"device": "mirror-virtio0",
				"force":  true,
			},
		},
		{
			name: "MigrateSetCapabilitiesArgs",
			args: MigrateSetCapabilitiesArgs{
				Capabilities: []MigrationCapability{
					{Capability: "auto-converge", State: true},
				},
			},
			want: map[string]interface{}{
				"capabilities": []interface{}{
					map[string]interface{}{
						"capability": "auto-converge",
						"state":      true,
					},
				},
			},
		},
		{
			name: "MigrateSetParametersArgs",
			args: MigrateSetParametersArgs{DowntimeLimit: 50, MaxBandwidth: 10_000_000_000},
			want: map[string]interface{}{
				"downtime-limit": float64(50),
				"max-bandwidth":  float64(10_000_000_000),
			},
		},
		{
			name: "MigrateArgs",
			args: MigrateArgs{URI: "tcp:10.0.0.1:4444"},
			want: map[string]interface{}{
				"uri": "tcp:10.0.0.1:4444",
			},
		},
		{
			name: "AnnounceSelfArgs",
			args: AnnounceSelfArgs{Initial: 50, Max: 550, Rounds: 5, Step: 100},
			want: map[string]interface{}{
				"initial": float64(50),
				"max":     float64(550),
				"rounds":  float64(5),
				"step":    float64(100),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var got map[string]interface{}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			wantJSON, _ := json.Marshal(tc.want)
			gotJSON, _ := json.Marshal(got)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("JSON mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestRequest_Serialization(t *testing.T) {
	t.Parallel()

	req := request{Execute: "migrate", Arguments: MigrateArgs{URI: "tcp:10.0.0.1:4444"}}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got := string(b)
	if !strings.Contains(got, `"execute":"migrate"`) {
		t.Fatalf("expected execute field, got: %s", got)
	}
	if !strings.Contains(got, `"uri":"tcp:10.0.0.1:4444"`) {
		t.Fatalf("expected arguments.uri, got: %s", got)
	}
}

func TestRequest_NoArgs(t *testing.T) {
	t.Parallel()

	req := request{Execute: "query-migrate"}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got := string(b)
	if strings.Contains(got, "arguments") {
		t.Fatalf("expected no arguments field with omitempty, got: %s", got)
	}
}

func TestExecute_MultipleEventsBeforeResponse(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		conn.Write([]byte(`{"event":"BLOCK_JOB_READY"}` + "\n"))
		conn.Write([]byte(`{"event":"STOP"}` + "\n"))
		conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
		conn.Write([]byte(`{"return":{"status":"completed"}}` + "\n"))
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	_, err = c.Execute(ctx, "query-migrate", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	c.mu.Lock()
	count := len(c.events)
	c.mu.Unlock()
	if count != 3 {
		t.Fatalf("expected 3 buffered events, got %d", count)
	}

	// Consume them in order.
	for _, name := range []string{"BLOCK_JOB_READY", "STOP", "RESUME"} {
		if err := c.WaitForEvent(ctx, name, time.Second); err != nil {
			t.Fatalf("WaitForEvent(%s): %v", name, err)
		}
	}

	c.mu.Lock()
	count = len(c.events)
	c.mu.Unlock()
	if count != 0 {
		t.Fatalf("expected 0 buffered events after consuming all, got %d", count)
	}
}

func TestBlockJobInfo_Unmarshal(t *testing.T) {
	t.Parallel()
	raw := `[{"device":"mirror-virtio0","len":1073741824,"offset":536870912,"ready":false,"status":"running","type":"mirror"}]`
	var jobs []BlockJobInfo
	if err := json.Unmarshal([]byte(raw), &jobs); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.Device != "mirror-virtio0" || j.Len != 1073741824 || j.Offset != 536870912 || j.Ready || j.Status != "running" || j.Type != "mirror" {
		t.Fatalf("unexpected job: %+v", j)
	}
}

func TestMigrateInfo_Unmarshal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw    string
		status string
		errMsg string
	}{
		{`{"status":"completed"}`, "completed", ""},
		{`{"status":"failed","error-desc":"out of memory"}`, "failed", "out of memory"},
		{`{"status":"active"}`, "active", ""},
	}
	for _, tc := range tests {
		var info MigrateInfo
		if err := json.Unmarshal([]byte(tc.raw), &info); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.raw, err)
		}
		if info.Status != tc.status {
			t.Fatalf("status: got %s, want %s", info.Status, tc.status)
		}
		if info.ErrorDesc != tc.errMsg {
			t.Fatalf("error-desc: got %s, want %s", info.ErrorDesc, tc.errMsg)
		}
	}
}

func TestNewClient_ReadGreetingError(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		// Close immediately after accept — read will fail.
		conn.Close()
	})

	ctx := context.Background()
	_, err := NewClient(ctx, sock)
	if err == nil {
		t.Fatal("expected error when connection closes during greeting")
	}
}

func TestExecute_ConnectionClosedMidRead(t *testing.T) {
	t.Parallel()
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		buf := make([]byte, 4096)
		conn.Read(buf)
		// Close without sending response.
		conn.Close()
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	_, err = c.Execute(ctx, "query-migrate", nil)
	if err == nil {
		t.Fatal("expected error when connection closes mid-read")
	}
}

func TestResponse_Unmarshal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw       string
		hasReturn bool
		hasError  bool
		hasEvent  bool
	}{
		{`{"return":{"status":"ok"}}`, true, false, false},
		{`{"error":{"class":"GenericError","desc":"fail"}}`, false, true, false},
		{`{"event":"STOP"}`, false, false, true},
	}

	for _, tc := range tests {
		var resp response
		if err := json.Unmarshal([]byte(tc.raw), &resp); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tc.raw, err)
		}
		if (resp.Return != nil) != tc.hasReturn {
			t.Fatalf("Return: got %v, want hasReturn=%v", resp.Return, tc.hasReturn)
		}
		if (resp.Error != nil) != tc.hasError {
			t.Fatalf("Error: got %v, want hasError=%v", resp.Error, tc.hasError)
		}
		if (resp.Event != "") != tc.hasEvent {
			t.Fatalf("Event: got %q, want hasEvent=%v", resp.Event, tc.hasEvent)
		}
	}
}

func TestArgs_SealedInterface(t *testing.T) {
	t.Parallel()
	// Verify all Args types implement the sealed qmpArgs() method.
	// This is a compile-time check — if any type doesn't implement Args,
	// this file won't compile.
	var _ Args = NBDServerStartArgs{}
	var _ Args = NBDServerAddArgs{}
	var _ Args = DriveMirrorArgs{}
	var _ Args = BlockJobCancelArgs{}
	var _ Args = MigrateSetCapabilitiesArgs{}
	var _ Args = MigrateSetParametersArgs{}
	var _ Args = MigrateArgs{}
	var _ Args = AnnounceSelfArgs{}
}

func TestWaitForEvent_BufferEventRemoval(t *testing.T) {
	t.Parallel()
	// Manually seed the event buffer and verify correct removal.
	sock := startFakeQMP(t, func(conn net.Conn) {
		qmpHandshake(conn)
		time.Sleep(time.Second)
	})

	ctx := context.Background()
	c, err := NewClient(ctx, sock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	// Seed 3 events manually.
	c.mu.Lock()
	c.events = []response{
		{Event: "BLOCK_JOB_READY"},
		{Event: "STOP"},
		{Event: "RESUME"},
	}
	c.mu.Unlock()

	// Consume the middle one.
	if err := c.WaitForEvent(ctx, "STOP", time.Second); err != nil {
		t.Fatalf("WaitForEvent(STOP): %v", err)
	}

	c.mu.Lock()
	remaining := make([]string, len(c.events))
	for i, e := range c.events {
		remaining[i] = e.Event
	}
	c.mu.Unlock()

	if len(remaining) != 2 || remaining[0] != "BLOCK_JOB_READY" || remaining[1] != "RESUME" {
		t.Fatalf("expected [BLOCK_JOB_READY, RESUME], got %v", remaining)
	}
}

func TestError_Implements_error(t *testing.T) {
	t.Parallel()
	var _ error = (*Error)(nil)

	e := &Error{Class: "TestClass", Desc: "test desc"}
	msg := fmt.Sprintf("wrap: %v", e)
	if !strings.Contains(msg, "TestClass") || !strings.Contains(msg, "test desc") {
		t.Fatalf("error formatting lost fields: %s", msg)
	}
}
