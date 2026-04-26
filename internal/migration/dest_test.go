package migration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
	"github.com/maci0/katamaran/internal/qmptest"
)

func TestRunDestination_Failures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		tap           string
		sharedStorage bool
	}{
		{"BadQMPSocket", "", false},
		{"SharedStorage_BadQMPSocket", "", true},
		{"WithTap_BadQMPSocket", "noexist-tap0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := RunDestination(context.Background(), DestConfig{
				QMPSocket:     "/nonexistent/qmp.sock",
				TapIface:      tt.tap,
				DriveID:       "drive-virtio-disk0",
				SharedStorage: tt.sharedStorage,
			})
			if err == nil {
				t.Fatal("expected error for nonexistent QMP socket")
			}
			if !strings.Contains(err.Error(), "QMP") {
				t.Fatalf("expected QMP-related error, got: %v", err)
			}
		})
	}
}

func TestRunDestination_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := RunDestination(ctx, DestConfig{QMPSocket: "/nonexistent/qmp.sock", DriveID: "drive-virtio-disk0"})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestRunDestination_NegativeMultifd(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket:       "/nonexistent/qmp.sock",
		DriveID:         "drive-virtio-disk0",
		MultifdChannels: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "multifd channels must be non-negative") {
		t.Fatalf("RunDestination error = %v, want multifd validation error", err)
	}
}

func TestRunDestination_SharedStorage_HappyPath(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// After accepting migrate-incoming, send RESUME event.
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			// Respond to all other commands with success.
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0", SharedStorage: true})
	if err != nil {
		t.Fatalf("RunDestination shared-storage happy path: %v", err)
	}
}

func TestRunDestination_NonShared_HappyPath(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// After migrate-incoming, the NBD setup commands come next,
				// then RESUME should fire.
				continue
			}
			if strings.Contains(line, "nbd-server-stop") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			if strings.Contains(line, "nbd-server-start") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				continue
			}
			if strings.Contains(line, "nbd-server-add") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				// After NBD setup is complete, send RESUME event.
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0"})
	if err != nil {
		t.Fatalf("RunDestination non-shared happy path: %v", err)
	}
}

func TestRunDestination_SharedStorage_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			// migrate-set-capabilities, migrate-set-parameters, etc.
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0", SharedStorage: true, MultifdChannels: 4})
	if err != nil {
		t.Fatalf("RunDestination with multifd: %v", err)
	}
}

func TestRunDestination_MigrateIncomingFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"incoming failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0", SharedStorage: true})
	if err == nil {
		t.Fatal("expected error for migrate-incoming failure")
	}
	if !strings.Contains(err.Error(), "incoming") {
		t.Fatalf("expected 'incoming' in error, got: %v", err)
	}
}

func TestRunDestination_NBDServerStartFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "nbd-server-start") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"bind failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0"})
	if err == nil {
		t.Fatal("expected error for NBD server start failure")
	}
	if !strings.Contains(err.Error(), "NBD") {
		t.Fatalf("expected 'NBD' in error, got: %v", err)
	}
}

func TestRunDestination_NBDServerAddFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "nbd-server-add") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"export failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0"})
	if err == nil {
		t.Fatal("expected error for NBD server add failure")
	}
	if !strings.Contains(err.Error(), "NBD export") {
		t.Fatalf("expected 'NBD export' in error, got: %v", err)
	}
}

func TestRunDestination_SetCapabilitiesFailure_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-set-capabilities") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"caps error"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0", SharedStorage: true, MultifdChannels: 4})
	if err == nil {
		t.Fatal("expected error for capabilities failure")
	}
	if !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("expected 'capabilities' in error, got: %v", err)
	}
}

func TestRunDestination_SetParametersFailure_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-set-parameters") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"params error"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0", SharedStorage: true, MultifdChannels: 4})
	if err == nil {
		t.Fatal("expected error for parameters failure")
	}
	if !strings.Contains(err.Error(), "parameters") {
		t.Fatalf("expected 'parameters' in error, got: %v", err)
	}
}

func TestRunDestination_InvalidTapIface(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket: "/nonexistent/qmp.sock",
		TapIface:  ";evil",
		DriveID:   "drive-virtio-disk0",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid tap interface") {
		t.Fatalf("expected tap interface validation error, got: %v", err)
	}
}

func TestRunDestination_InvalidTapNetns(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket: "/nonexistent/qmp.sock",
		TapNetns:  "/proc/../etc/passwd",
		DriveID:   "drive-virtio-disk0",
	})
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("expected netns validation error, got: %v", err)
	}
}

func TestRunDestination_InvalidDriveID(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket: "/nonexistent/qmp.sock",
		DriveID:   ";evil",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid drive ID") {
		t.Fatalf("expected drive ID validation error, got: %v", err)
	}
}

func TestRunDestination_SharedStorage_SkipsDriveIDValidation(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket:     "/nonexistent/qmp.sock",
		DriveID:       ";evil",
		SharedStorage: true,
	})
	if err == nil {
		t.Fatal("expected error (QMP connection should fail)")
	}
	if strings.Contains(err.Error(), "invalid drive ID") {
		t.Fatalf("shared storage should skip drive ID validation, got: %v", err)
	}
}

func TestRunDestination_GARPFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartFakeQMP(t, func(conn net.Conn) {
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			line := string(buf[:n])

			if strings.Contains(line, "migrate-incoming") {
				conn.Write([]byte(`{"return":{}}` + "\n"))
				time.Sleep(10 * time.Millisecond)
				conn.Write([]byte(`{"event":"RESUME"}` + "\n"))
				continue
			}
			if strings.Contains(line, "announce-self") {
				conn.Write([]byte(`{"error":{"class":"GenericError","desc":"announce failed"}}` + "\n"))
				continue
			}
			conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveID: "drive-virtio-disk0", SharedStorage: true})
	if err == nil {
		t.Fatal("expected error for GARP failure")
	}
	if !strings.Contains(err.Error(), "GARP") {
		t.Fatalf("expected 'GARP' in error, got: %v", err)
	}
}

func TestRunDestination_NonShared_CommandArguments(t *testing.T) {
	t.Parallel()

	sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "nbd-server-add":
			return `{"return":{}}` + "\n" + `{"event":"RESUME"}`
		default:
			return `{"return":{}}`
		}
	})

	err := RunDestination(context.Background(), DestConfig{
		QMPSocket:       sock,
		DriveID:         "drive-virtio-disk0",
		MultifdChannels: 4,
	})
	if err != nil {
		t.Fatalf("RunDestination non-shared command arguments: %v", err)
	}

	commands := rec.Commands()
	assertRecordedSubsequence(t, commands, []string{
		"migrate-set-capabilities",
		"migrate-set-parameters",
		"migrate-incoming",
		"nbd-server-stop",
		"nbd-server-start",
		"nbd-server-add",
		"nbd-server-stop",
		"announce-self",
	})

	var caps qmp.MigrateSetCapabilitiesArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "migrate-set-capabilities"), &caps)
	if len(caps.Capabilities) != 2 ||
		caps.Capabilities[0] != (qmp.MigrationCapability{Capability: "auto-converge", State: true}) ||
		caps.Capabilities[1] != (qmp.MigrationCapability{Capability: "multifd", State: true}) {
		t.Fatalf("unexpected destination capabilities: %+v", caps.Capabilities)
	}

	var params qmp.MigrateSetParametersArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "migrate-set-parameters"), &params)
	if params.MultifdChannels != 4 {
		t.Fatalf("destination multifd channels = %d, want 4", params.MultifdChannels)
	}

	var incoming qmp.MigrateArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "migrate-incoming"), &incoming)
	if incoming.URI != "tcp:[::]:4444" {
		t.Fatalf("migrate-incoming URI = %q, want tcp:[::]:4444", incoming.URI)
	}

	var start qmp.NBDServerStartArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "nbd-server-start"), &start)
	if start.Addr.Type != "inet" || start.Addr.Data.Host != "::" || start.Addr.Data.Port != nbdPort {
		t.Fatalf("unexpected nbd-server-start args: %+v", start)
	}

	var add qmp.NBDServerAddArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "nbd-server-add"), &add)
	if add.Device != "drive-virtio-disk0" || !add.Writable {
		t.Fatalf("unexpected nbd-server-add args: %+v", add)
	}

	var announce qmp.AnnounceSelfArgs
	decodeRecordedArgs(t, findRecordedCommand(t, commands, "announce-self"), &announce)
	if announce.Initial != garpInitialMS || announce.Max != garpMaxMS || announce.Rounds != garpRounds || announce.Step != garpStepMS {
		t.Fatalf("unexpected announce-self args: %+v", announce)
	}
}
