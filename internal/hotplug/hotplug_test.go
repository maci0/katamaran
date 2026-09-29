package hotplug

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/qmp"
)

func TestRunHelpGoesToStdout(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{flag}, &stdout, &stderr); code != 0 {
			t.Fatalf("Run(%s) code = %d, want 0", flag, code)
		}
		if stderr.Len() != 0 {
			t.Errorf("Run(%s) wrote to stderr: %q", flag, stderr.String())
		}
		if !strings.Contains(stdout.String(), "<qmp-socket> <disk-image>") {
			t.Errorf("Run(%s) stdout = %q, want the usage line", flag, stdout.String())
		}
	}
}

func TestRunRejectsWrongArgCount(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		args := make([]string, n)
		for i := range args {
			args[i] = "unused"
		}
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Errorf("Run with %d args code = %d, want 2", n, code)
		}
		if stdout.Len() != 0 {
			t.Errorf("Run with %d args wrote to stdout: %q", n, stdout.String())
		}
		if !strings.Contains(stderr.String(), "<qmp-socket> <disk-image>") {
			t.Errorf("Run with %d args stderr = %q, want the usage line", n, stderr.String())
		}
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("Run with an unknown flag code = %d, want 2", code)
	}
}

// TestRunSendsBlockdevThenDeviceAdd drives the real QMP wire protocol
// against a stub server and asserts the two commands land in the order QEMU
// requires, with the arguments the migration topology depends on.
func TestRunSendsBlockdevThenDeviceAdd(t *testing.T) {
	sock := serveStubQMP(t, false, func(cmd string, args json.RawMessage) {
		switch cmd {
		case "blockdev-add":
			var got qmp.BlockdevAddArgs
			decode(t, args, &got)
			if got.Driver != "raw" || got.NodeName != "drive-virtio-disk0" {
				t.Errorf("blockdev-add = %+v, want raw block node drive-virtio-disk0", got)
			}
			if got.File.Driver != "file" || got.File.Filename != "/disks/data.img" {
				t.Errorf("blockdev-add file = %+v, want file backend on /disks/data.img", got.File)
			}
		case "device_add":
			var got qmp.DeviceAddArgs
			decode(t, args, &got)
			if got.Driver != "virtio-blk-pci" || got.Drive != "drive-virtio-disk0" {
				t.Errorf("device_add = %+v, want virtio-blk-pci on drive-virtio-disk0", got)
			}
			// These two are the reason the tool exists: a different bus or
			// address on the destination breaks live migration.
			if got.Bus != "pci-bridge-0" || got.Addr != "0x8" {
				t.Errorf("device_add = %+v, want bus pci-bridge-0 addr 0x8", got)
			}
			if got.ID != "data-disk0" {
				t.Errorf("device_add id = %q, want data-disk0", got.ID)
			}
		default:
			t.Errorf("unexpected QMP command %q", cmd)
		}
	})

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{sock, "/disks/data.img"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("Run wrote to stdout: %q", stdout.String())
	}
}

func TestRunReportsQEMPErrors(t *testing.T) {
	sock := serveStubQMP(t, true, func(cmd string, _ json.RawMessage) {
		t.Errorf("unexpected command %q after blockdev-add failed", cmd)
	})

	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{sock, "/disks/data.img"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("Run code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "blockdev-add") {
		t.Errorf("stderr = %q, want it to name the failing command", stderr.String())
	}
}

func decode(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}

// serveStubQMP runs a minimal QMP server: it sends the greeting, answers
// qmp_capabilities, then records the commands handle sees. With failBlockdev
// set, blockdev-add is answered with a QMP error so the client's error path
// is exercised over a real socket rather than a fake transport.
func serveStubQMP(t *testing.T, failBlockdev bool, handle func(cmd string, args json.RawMessage)) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// The client reads the greeting before sending qmp_capabilities.
		if _, err := conn.Write([]byte(`{"QMP":{"version":{"qemu":{"major":9,"minor":0}}}}` + "\n")); err != nil {
			return
		}
		dec := json.NewDecoder(conn)
		for {
			var req struct {
				Execute   string          `json:"execute"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := dec.Decode(&req); err != nil {
				return
			}
			if req.Execute == "qmp_capabilities" {
				if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
					return
				}
				continue
			}

			// blockdev-add fails once, so the client stops before
			// device_add and the ordering assertion still holds.
			if req.Execute == "blockdev-add" && failBlockdev {
				if _, err := conn.Write([]byte(`{"error":{"class":"GenericError","desc":"stubbed failure"}}` + "\n")); err != nil {
					return
				}
				continue
			}

			handle(req.Execute, req.Arguments)
			if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
				return
			}
		}
	}()

	return path
}
