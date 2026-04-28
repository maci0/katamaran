package qmp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// FuzzResponseUnmarshal targets JSON unmarshaling of QMP responses —
// the primary attack surface since clients parse arbitrary data from QEMU sockets.
func FuzzResponseUnmarshal(f *testing.F) {
	f.Add([]byte(`{"return":{}}`))
	f.Add([]byte(`{"return":{"status":"completed"}}`))
	f.Add([]byte(`{"error":{"class":"GenericError","desc":"device not found"}}`))
	f.Add([]byte(`{"event":"STOP"}`))
	f.Add([]byte(`{"event":"RESUME","timestamp":{"seconds":1234567890,"microseconds":0}}`))
	f.Add([]byte(`{"event":"BLOCK_JOB_READY","data":{"device":"mirror-drive-virtio-disk0"}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"return":null}`))
	f.Add([]byte(`{"error":null}`))
	f.Add([]byte(`{"return":[]}`))
	f.Add([]byte(`{"return":"string"}`))
	f.Add([]byte(`{"return":42}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp response
		_ = json.Unmarshal(data, &resp)
		if resp.Error != nil {
			_ = resp.Error.Error()
		}
	})
}

// FuzzBlockJobInfoUnmarshal targets query-block-jobs parsing, polled
// periodically during drive-mirror sync in waitForStorageSync.
func FuzzBlockJobInfoUnmarshal(f *testing.F) {
	f.Add([]byte(`[{"device":"mirror-virtio0","len":1073741824,"offset":536870912,"ready":false,"status":"running","type":"mirror"}]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"device":"","len":0,"offset":0,"ready":true,"status":"concluded","type":"mirror"}]`))
	f.Add([]byte(`[{"device":"mirror-virtio0","len":-1,"offset":-1,"ready":false,"status":"null","type":""}]`))
	f.Add([]byte(`[{}]`))
	f.Add([]byte(`[{"device":"a","len":9999999999999999,"offset":0,"ready":false,"status":"running","type":"mirror"},{"device":"b","len":0,"offset":0,"ready":true,"status":"ready","type":"commit"}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var jobs []BlockJobInfo
		if err := json.Unmarshal(data, &jobs); err != nil {
			return
		}
		for _, j := range jobs {
			_ = j.Device
			_ = j.Ready
			_ = j.Status
			if j.Len > 0 {
				_ = float64(j.Offset) / float64(j.Len) * 100
			}
		}
	})
}

// FuzzMigrateInfoUnmarshal exercises the parsing of query-migrate output.
// The source migration loop polls this periodically during RAM pre-copy.
func FuzzMigrateInfoUnmarshal(f *testing.F) {
	f.Add([]byte(`{"status":"completed"}`))
	f.Add([]byte(`{"status":"failed","error-desc":"out of memory"}`))
	f.Add([]byte(`{"status":"active"}`))
	f.Add([]byte(`{"status":"cancelled"}`))
	f.Add([]byte(`{"status":"setup"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"status":""}`))
	f.Add([]byte(`{"status":"failed"}`))
	f.Add([]byte(`{"status":"failed","error-desc":""}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var info MigrateInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return
		}
		switch info.Status {
		case "completed", "failed", "cancelled":
		}
		if info.Status == "failed" && info.ErrorDesc != "" {
			_ = fmt.Errorf("migration failed: %s", info.ErrorDesc)
		}
	})
}

// FuzzErrorFormat exercises the Error.Error() formatting with arbitrary strings.
func FuzzErrorFormat(f *testing.F) {
	f.Add("GenericError", "device not found")
	f.Add("", "")
	f.Add("CommandNotFound", "unknown command 'foo'")
	f.Add("DeviceNotActive", "No such device: 'drive-virtio-disk99'")
	f.Add("GenericError", "Timed out during operation: cannot acquire state change lock (held by monitor)")
	f.Add("a]b[c", "d\"e\\f\x00g")

	f.Fuzz(func(t *testing.T, class, desc string) {
		e := &Error{Class: class, Desc: desc}
		msg := e.Error()
		if msg == "" {
			t.Fatal("Error.Error() should never return empty string")
		}
	})
}

// FuzzClientProtocol exercises the full QMP client handshake and command
// execution with arbitrary wire data. This simulates a malicious or buggy
// QEMU instance sending unexpected bytes on the socket.
func FuzzClientProtocol(f *testing.F) {
	f.Add(
		[]byte(`{"QMP":{"version":{"qemu":{"micro":0,"minor":2,"major":6}}}}`+"\n"+`{"return":{}}`+"\n"),
		[]byte(`{"return":{"status":"completed"}}`+"\n"),
	)
	f.Add(
		[]byte(`{"QMP":{}}`+"\n"+`{"error":{"class":"GenericError","desc":"caps rejected"}}`+"\n"),
		[]byte{},
	)
	f.Add(
		[]byte(`{"return":{}}`+"\n"),
		[]byte(`{"event":"STOP"}`+"\n"+`{"return":{}}`+"\n"),
	)
	f.Add(
		[]byte(`{"QMP":{}}`+"\n"+`{"return":{}}`+"\n"),
		[]byte(`{"event":"BLOCK_JOB_READY"}`+"\n"+`{"event":"STOP"}`+"\n"+`{"return":{"status":"active"}}`+"\n"),
	)

	f.Fuzz(func(t *testing.T, handshakeData, executeData []byte) {
		if len(handshakeData) > 8192 || len(executeData) > 8192 {
			return
		}

		socketPath := filepath.Join(t.TempDir(), "qmp.sock")
		l, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Skip("cannot create socket")
		}
		defer l.Close()

		go func() {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer conn.Close()

			conn.Write(handshakeData)
			buf := make([]byte, 4096)
			conn.Read(buf)
			conn.Write(executeData)
			conn.Read(buf)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		client, err := NewClient(ctx, socketPath)
		if err != nil {
			return
		}
		defer client.Close()

		raw, err := client.Execute(ctx, "query-migrate", nil)
		if err != nil {
			return
		}

		var result map[string]any
		_ = json.Unmarshal(raw, &result)
	})
}

// FuzzArgsSerialization exercises JSON serialization of all QMP argument types
// to verify that marshaling never panics with any combination of field values.
func FuzzArgsSerialization(f *testing.F) {
	f.Add("drive-virtio-disk0", "nbd:10.0.0.1:10809:exportname=drive0", "full", "existing", "mirror-drive0", true)
	f.Add("", "", "", "", "", false)
	f.Add("virtio0", "nbd:[::1]:10809:exportname=virtio0", "top", "absolute-paths", "mirror-virtio0", true)
	f.Add("a\"b", "c\\d", "e\x00f", "g\nf", "h\tf", false)

	f.Fuzz(func(t *testing.T, device, target, sync, mode, jobID string, force bool) {
		args := []Args{
			DriveMirrorArgs{Device: device, Target: target, Sync: sync, Mode: mode, JobID: jobID},
			BlockJobCancelArgs{Device: device, Force: force},
			NBDServerAddArgs{Device: device, Writable: force},
			MigrateArgs{URI: target},
		}
		for _, a := range args {
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}
			if len(b) == 0 {
				t.Fatal("Marshal produced empty output")
			}

			req := request{Execute: "test", Arguments: a}
			rb, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("Marshal request failed: %v", err)
			}
			if len(rb) == 0 {
				t.Fatal("Marshal request produced empty output")
			}
		}
	})
}
