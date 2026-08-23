package migration

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/qmp"
	"github.com/maci0/katamaran/internal/qmptest"
)

// startFakeQMPAt starts a scripted QMP server listening on the exact
// unix socket path, mimicking qmptest.StartScriptedQMP for paths chosen
// by the code under test (the replayed QEMU monitor under sandboxRoot).
// Every command gets {"return":{}}; migrate-incoming additionally emits
// a RESUME event so RunDestination's event wait completes.
func startFakeQMPAt(t *testing.T, path string) error {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, readErr := conn.Read(buf)
			if readErr != nil {
				return
			}
			line := string(buf[:n])
			resps := []string{`{"return":{}}`}
			if strings.Contains(line, `"migrate-incoming"`) {
				resps = []string{`{"return":{}}`, `{"event":"RESUME"}`}
			}
			for _, resp := range resps {
				if _, writeErr := conn.Write([]byte(resp + "\n")); writeErr != nil {
					return
				}
			}
		}
	}()
	return nil
}

// TestRunDestination_ReplayFromPod_RemovesTempCmdlineFile pins the
// cleanup of the temporary cmdline file the dest binary materializes
// from the source pod log: it lives on a node-wide hostPath that outlives
// the pod, so leaving it behind would accumulate one stale file per
// migration on every node. The file must be gone once RunDestination is
// done with it.
func TestRunDestination_ReplayFromPod_RemovesTempCmdlineFile(t *testing.T) {
	// Not t.Parallel(): mutates package-level vars.
	//
	// Short temp root: the replayed QEMU monitor socket path must fit the
	// 108-byte unix-socket limit, which t.TempDir()'s long names exceed.
	tmp, err := os.MkdirTemp("", "kvmdst")
	if err != nil {
		t.Fatalf("mkdir temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })

	prevSandboxRoot, prevSharedRoot, prevCmdlineDir := sandboxRoot, kataSharedSandboxRoot, CmdlineHostDir
	sandboxRoot = filepath.Join(tmp, "vm")
	kataSharedSandboxRoot = filepath.Join(tmp, "sandboxes")
	CmdlineHostDir = filepath.Join(tmp, "cmdlines")
	if err := os.MkdirAll(CmdlineHostDir, 0o700); err != nil {
		t.Fatalf("mkdir cmdline dir: %v", err)
	}
	t.Cleanup(func() {
		sandboxRoot = prevSandboxRoot
		kataSharedSandboxRoot = prevSharedRoot
		CmdlineHostDir = prevCmdlineDir
	})

	prevSpawn, prevWait, prevTap := spawnDetachedProcess, waitForSocket, setupTapIface
	spawnDetachedProcess = func(_ context.Context, _ string, _ []string) error { return nil }
	setupTapIface = func(ctx context.Context, name string) error { return nil }
	waitForSocket = func(ctx context.Context, path string, total time.Duration) error {
		if !strings.HasSuffix(path, extraMonitorSocketName) {
			return nil // virtiofsd socket: no listener needed
		}
		return startFakeQMPAt(t, path)
	}
	t.Cleanup(func() {
		spawnDetachedProcess = prevSpawn
		waitForSocket = prevWait
		setupTapIface = prevTap
	})

	cmdlineBody := "qemu-system-x86_64\n-machine q35\n"
	marker := cmdlineMarker + base64.StdEncoding.EncodeToString([]byte(cmdlineBody))
	setupAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, marker+"\n")
	})

	err = RunDestination(context.Background(), DestConfig{
		ReplayCmdlineFromPod: "default/test-src-pod",
		SharedStorage:        true,
	})
	if err != nil {
		t.Fatalf("RunDestination replay-from-pod happy path: %v", err)
	}

	entries, err := os.ReadDir(CmdlineHostDir)
	if err != nil {
		t.Fatalf("read cmdline dir after run: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temporary cmdline files left behind in %s: %v", CmdlineHostDir, names)
	}
}

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
				DriveIDs:      []string{"drive-virtio-disk0"},
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

	err := RunDestination(ctx, DestConfig{QMPSocket: "/nonexistent/qmp.sock", DriveIDs: []string{"drive-virtio-disk0"}})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestRunDestination_NegativeMultifd(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket:       "/nonexistent/qmp.sock",
		DriveIDs:        []string{"drive-virtio-disk0"},
		MultifdChannels: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "multifd channels must be non-negative") {
		t.Fatalf("RunDestination error = %v, want multifd validation error", err)
	}
}

func TestRunDestination_SharedStorage_HappyPath(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-incoming": {`{"return":{}}`, `{"event":"RESUME"}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}, SharedStorage: true})
	if err != nil {
		t.Fatalf("RunDestination shared-storage happy path: %v", err)
	}
}

func TestRunDestination_NonShared_HappyPath(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"nbd-server-add": {`{"return":{}}`, `{"event":"RESUME"}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}})
	if err != nil {
		t.Fatalf("RunDestination non-shared happy path: %v", err)
	}
}

func TestRunDestination_SharedStorage_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-incoming": {`{"return":{}}`, `{"event":"RESUME"}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}, SharedStorage: true, MultifdChannels: 4})
	if err != nil {
		t.Fatalf("RunDestination with multifd: %v", err)
	}
}

func TestRunDestination_MigrateIncomingFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-incoming": {`{"error":{"class":"GenericError","desc":"incoming failed"}}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}, SharedStorage: true})
	if err == nil {
		t.Fatal("expected error for migrate-incoming failure")
	}
	if !strings.Contains(err.Error(), "incoming") {
		t.Fatalf("expected 'incoming' in error, got: %v", err)
	}
}

func TestRunDestination_NBDServerStartFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"nbd-server-start": {`{"error":{"class":"GenericError","desc":"bind failed"}}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}})
	if err == nil {
		t.Fatal("expected error for NBD server start failure")
	}
	if !strings.Contains(err.Error(), "NBD") {
		t.Fatalf("expected 'NBD' in error, got: %v", err)
	}
}

func TestRunDestination_NBDServerAddFailure(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"nbd-server-add": {`{"error":{"class":"GenericError","desc":"export failed"}}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}})
	if err == nil {
		t.Fatal("expected error for NBD server add failure")
	}
	if !strings.Contains(err.Error(), "NBD export") {
		t.Fatalf("expected 'NBD export' in error, got: %v", err)
	}
}

func TestRunDestination_SetCapabilitiesFailure_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-set-capabilities": {`{"error":{"class":"GenericError","desc":"caps error"}}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}, SharedStorage: true, MultifdChannels: 4})
	if err == nil {
		t.Fatal("expected error for capabilities failure")
	}
	if !strings.Contains(err.Error(), "capabilities") {
		t.Fatalf("expected 'capabilities' in error, got: %v", err)
	}
}

func TestRunDestination_SetParametersFailure_Multifd(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-set-parameters": {`{"error":{"class":"GenericError","desc":"params error"}}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}, SharedStorage: true, MultifdChannels: 4})
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
		DriveIDs:  []string{"drive-virtio-disk0"},
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
		DriveIDs:  []string{"drive-virtio-disk0"},
	})
	if err == nil || !strings.Contains(err.Error(), "path traversal") {
		t.Fatalf("expected netns validation error, got: %v", err)
	}
}

func TestRunDestination_InvalidDriveID(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket: "/nonexistent/qmp.sock",
		DriveIDs:  []string{";evil"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid drive ID") {
		t.Fatalf("expected drive ID validation error, got: %v", err)
	}
}

func TestRunDestination_SharedStorage_SkipsDriveIDValidation(t *testing.T) {
	t.Parallel()
	err := RunDestination(context.Background(), DestConfig{
		QMPSocket:     "/nonexistent/qmp.sock",
		DriveIDs:      []string{";evil"},
		SharedStorage: true,
	})
	if err == nil {
		t.Fatal("expected error (QMP connection should fail)")
	}
	if strings.Contains(err.Error(), "invalid drive ID") {
		t.Fatalf("shared storage should skip drive ID validation, got: %v", err)
	}
}

// TestRunDestination_GARPFailureIsNonFatal pins that a failed GARP
// announce-self does NOT fail the dest job: by that point RESUME has
// fired and the migration itself is complete, so an error would mark
// the migration Failed and skip writeMigrationMeta/surviveContainerExit,
// killing the migrated VM at container exit. The run must succeed and
// the post-GARP metadata write must still happen.
func TestRunDestination_GARPFailureIsNonFatal(t *testing.T) {
	t.Parallel()

	sock := qmptest.StartScriptedQMP(t, map[string][]string{
		"migrate-incoming": {`{"return":{}}`, `{"event":"RESUME"}`},
		"announce-self":    {`{"error":{"class":"GenericError","desc":"announce failed"}}`},
	})

	err := RunDestination(context.Background(), DestConfig{QMPSocket: sock, DriveIDs: []string{"drive-virtio-disk0"}, SharedStorage: true})
	if err != nil {
		t.Fatalf("GARP failure must not abort the completed migration, got: %v", err)
	}
	metaPath := filepath.Join(filepath.Dir(sock), MigrationMetaFile)
	if _, statErr := os.Stat(metaPath); statErr != nil {
		t.Fatalf("migration meta not written after GARP failure (%s): %v", metaPath, statErr)
	}
}

// TestRunDestination_NBDStopFailureStillRetried pins that a failed
// explicit nbd-server-stop does not leave the writable NBD export
// listening for the VM's lifetime: the deferred cleanup must re-attempt
// the stop because its guard only disarms once an explicit stop
// succeeds. Three stop attempts are expected in total: the pre-clear
// (expected failure), the explicit post-RESUME stop (fails here), and
// the deferred-cleanup retry.
func TestRunDestination_NBDStopFailureStillRetried(t *testing.T) {
	t.Parallel()

	sock, rec := startRecordingQMP(t, func(conn net.Conn, cmd recordedQMPCommand) string {
		switch cmd.Execute {
		case "nbd-server-add":
			return `{"return":{}}` + "\n" + `{"event":"RESUME"}`
		case "nbd-server-stop":
			return `{"error":{"class":"GenericError","desc":"stop failed"}}`
		default:
			return `{"return":{}}`
		}
	})

	err := RunDestination(context.Background(), DestConfig{
		QMPSocket: sock,
		DriveIDs:  []string{"drive-virtio-disk0"},
	})
	if err != nil {
		t.Fatalf("failed nbd-server-stop must be warn-only, got: %v", err)
	}

	stops := 0
	for _, cmd := range rec.Commands() {
		if cmd.Execute == "nbd-server-stop" {
			stops++
		}
	}
	if stops != 3 {
		t.Fatalf("nbd-server-stop attempts = %d, want 3 (pre-clear + explicit + deferred retry)", stops)
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
		DriveIDs:        []string{"drive-virtio-disk0"},
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

// TestSurviveContainerExit_HappyPath simulates the cgroup re-parent
// against a tmpdir-rooted fake cgroup tree. Locks the contract that
// surviveContainerExit reads <qmpDir>/pid, creates the per-sandbox
// cgroup dir under the configured root, and writes the pid into
// cgroup.procs.
func TestSurviveContainerExit_HappyPath(t *testing.T) {
	root := t.TempDir()
	qmpDir := t.TempDir()
	if err := os.WriteFile(qmpDir+"/pid", []byte("12345\n"), 0o644); err != nil {
		t.Fatalf("seed pid: %v", err)
	}

	prev := adoptedCgroupRoot
	adoptedCgroupRoot = root
	t.Cleanup(func() { adoptedCgroupRoot = prev })

	surviveContainerExit(qmpDir + "/qmp.sock")

	sandboxID := filepath.Base(qmpDir)
	procs, err := os.ReadFile(root + "/" + sandboxID + "/cgroup.procs")
	if err != nil {
		t.Fatalf("cgroup.procs read: %v", err)
	}
	if !strings.Contains(string(procs), "12345") {
		t.Fatalf("cgroup.procs = %q, want it to contain pid 12345", procs)
	}
}

// TestSurviveContainerExit_MissingPidFileIsBestEffort confirms
// surviveContainerExit is silent-and-safe when the pid file doesn't
// exist (e.g., QEMU was started via a non-default flag). Migration
// must still succeed; we just won't have a surviving QEMU.
func TestSurviveContainerExit_MissingPidFileIsBestEffort(t *testing.T) {
	prev := adoptedCgroupRoot
	adoptedCgroupRoot = t.TempDir() + "/no-such-tree"
	t.Cleanup(func() { adoptedCgroupRoot = prev })

	// No pid file and no cgroup tree — must not panic, must not error.
	surviveContainerExit("/no/such/qmp.sock")
}
