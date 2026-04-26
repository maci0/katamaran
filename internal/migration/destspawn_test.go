package migration

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseCmdlineBytes_NULDelimited(t *testing.T) {
	t.Parallel()
	raw := []byte("/opt/kata/bin/qemu-system-x86_64\x00-name\x00sandbox-abc\x00-nodefaults\x00")
	got := parseCmdlineBytes(raw)
	want := []string{"/opt/kata/bin/qemu-system-x86_64", "-name", "sandbox-abc", "-nodefaults"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCmdlineBytes NUL got %v, want %v", got, want)
	}
}

func TestParseCmdlineBytes_NewlineDelimited(t *testing.T) {
	t.Parallel()
	raw := []byte("/opt/kata/bin/qemu-system-x86_64\n-name\nsandbox-abc\n-nodefaults\n")
	got := parseCmdlineBytes(raw)
	want := []string{"/opt/kata/bin/qemu-system-x86_64", "-name", "sandbox-abc", "-nodefaults"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCmdlineBytes newline got %v, want %v", got, want)
	}
}

func TestParseCmdlineBytes_DropsEmptyFields(t *testing.T) {
	t.Parallel()
	raw := []byte("\n/bin/true\n\n--flag\n\n")
	got := parseCmdlineBytes(raw)
	want := []string{"/bin/true", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCmdlineBytes drop-empty got %v, want %v", got, want)
	}
}

func TestExtractNvdimmPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "found",
			args: []string{
				"-object",
				"memory-backend-file,id=mem-nvdimm,mem-path=/opt/kata/share/kata-containers/kata-ubuntu-noble.image,size=512M,share=on,readonly=on",
			},
			want: "/opt/kata/share/kata-containers/kata-ubuntu-noble.image",
		},
		{
			name: "skips_dev_shm",
			args: []string{
				"-object",
				"memory-backend-file,id=ram,mem-path=/dev/shm/kata-ram,size=2G,share=on",
				"-object",
				"memory-backend-file,id=nvdimm,mem-path=/var/lib/kata/img,size=512M,readonly=on",
			},
			want: "/var/lib/kata/img",
		},
		{
			name: "absent",
			args: []string{"-name", "sandbox-abc"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := extractNvdimmPath(tt.args); got != tt.want {
				t.Fatalf("extractNvdimmPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFindSrcSandboxDir(t *testing.T) {
	t.Parallel()
	args := []string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-qmp",
		"unix:/run/vc/vm/abcd-1234/qmp.sock,server=on,wait=off",
		"-monitor",
		"unix:/run/vc/vm/abcd-1234/extra-monitor.sock,server=on,wait=off",
	}
	dir, id := findSrcSandboxDir(args, "/run/vc/vm")
	if dir != "/run/vc/vm/abcd-1234" || id != "abcd-1234" {
		t.Fatalf("findSrcSandboxDir = (%q, %q), want (/run/vc/vm/abcd-1234, abcd-1234)", dir, id)
	}
}

func TestFindSrcSandboxDir_NotFound(t *testing.T) {
	t.Parallel()
	args := []string{"/opt/kata/bin/qemu-system-x86_64", "-name", "vm0"}
	dir, id := findSrcSandboxDir(args, "/run/vc/vm")
	if dir != "" || id != "" {
		t.Fatalf("findSrcSandboxDir = (%q, %q), want empty", dir, id)
	}
}

func TestTransformCmdline_StripsAndAppendsIncomingDefer(t *testing.T) {
	t.Parallel()
	args := []string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-name", "sandbox-abcd",
		"-incoming", "tcp:[::]:4444",
		"-daemonize",
		"-nodefaults",
	}
	binary, out, err := transformCmdline(args, "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("transformCmdline: %v", err)
	}
	if binary != "/opt/kata/bin/qemu-system-x86_64" {
		t.Fatalf("binary = %q, want /opt/kata/bin/qemu-system-x86_64", binary)
	}

	// -incoming and -daemonize from the source must be gone (the original
	// -incoming carries a positional URI that must also be stripped).
	for _, a := range out {
		if a == "-daemonize" {
			t.Fatalf("-daemonize must NOT appear in transformed args (we run QEMU foreground): %v", out)
		}
		if a == "tcp:[::]:4444" {
			t.Fatalf("source -incoming URI %q leaked into output: %v", a, out)
		}
	}

	// And we expect -incoming defer at the tail.
	if len(out) < 2 ||
		out[len(out)-2] != "-incoming" ||
		out[len(out)-1] != "defer" {
		t.Fatalf("expected tail '-incoming defer', got: %v", out)
	}

	// -name sandbox-abcd should still be present.
	if !slices.Contains(out, "-name") || !slices.Contains(out, "sandbox-abcd") {
		t.Fatalf("expected -name sandbox-abcd to survive, got: %v", out)
	}
}

func TestTransformCmdline_EmptyArgsErrors(t *testing.T) {
	t.Parallel()
	if _, _, err := transformCmdline(nil, "", "", "", "", "", ""); err == nil {
		t.Fatal("expected error for empty args")
	}
}

func TestTransformCmdline_PathSubstitutions(t *testing.T) {
	t.Parallel()
	srcDir := "/run/vc/vm/SRC-UUID"
	dstDir := "/run/vc/vm/DST-UUID"
	args := []string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-qmp", "unix:" + srcDir + "/qmp.sock,server=on,wait=off",
		"-chardev", "socket,id=charfs,path=" + srcDir + "/vhost-fs.sock",
		// Tokens that match `sandbox-<id>` in the cmdline (e.g. -name)
		// must be remapped to the dest sandbox id.
		"-name", "sandbox-SRC-UUID",
	}
	_, out, err := transformCmdline(args, srcDir, dstDir, "SRC-UUID", "DST-UUID", "", "")
	if err != nil {
		t.Fatalf("transformCmdline: %v", err)
	}
	joined := strings.Join(out, " ")
	if !strings.Contains(joined, "/run/vc/vm/DST-UUID/qmp.sock") {
		t.Fatalf("expected dst qmp socket path, got: %s", joined)
	}
	if !strings.Contains(joined, "/run/vc/vm/DST-UUID/vhost-fs.sock") {
		t.Fatalf("expected dst vhost-fs socket path, got: %s", joined)
	}
	if !strings.Contains(joined, "sandbox-DST-UUID") {
		t.Fatalf("expected sandbox-DST-UUID, got: %s", joined)
	}
	if strings.Contains(joined, "sandbox-SRC-UUID") {
		t.Fatalf("source sandbox-<id> token survived: %s", joined)
	}
	if strings.Contains(joined, "/run/vc/vm/SRC-UUID") {
		t.Fatalf("source sandbox dir survived: %s", joined)
	}
}

func TestTransformCmdline_StripsReadonly(t *testing.T) {
	t.Parallel()
	args := []string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-object", "memory-backend-file,id=nvdimm,mem-path=/img,size=512M,readonly=on",
		"-drive", "file=/img,readonly=true,if=none",
	}
	_, out, err := transformCmdline(args, "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("transformCmdline: %v", err)
	}
	for _, a := range out {
		if strings.Contains(a, "readonly=on") || strings.Contains(a, "readonly=true") {
			t.Fatalf("readonly clause survived: %q (full: %v)", a, out)
		}
	}
}

func TestTransformCmdline_NvdimmPathSubstitution(t *testing.T) {
	t.Parallel()
	srcImg := "/opt/kata/share/kata-containers/kata-ubuntu-noble.image"
	dstImg := "/tmp/kata-dst-nvdimm-xyz.img"
	args := []string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-object", "memory-backend-file,id=nvdimm,mem-path=" + srcImg + ",size=512M,readonly=on",
	}
	_, out, err := transformCmdline(args, "", "", "", "", srcImg, dstImg)
	if err != nil {
		t.Fatalf("transformCmdline: %v", err)
	}
	joined := strings.Join(out, " ")
	if strings.Contains(joined, srcImg) {
		t.Fatalf("source nvdimm path %q leaked into output: %s", srcImg, joined)
	}
	if !strings.Contains(joined, dstImg) {
		t.Fatalf("expected dst nvdimm path %q in output, got: %s", dstImg, joined)
	}
	if strings.Contains(joined, "readonly=on") {
		t.Fatalf("readonly=on survived: %s", joined)
	}
}

func TestTransformCmdline_DropsBareIncoming(t *testing.T) {
	t.Parallel()
	// e.g. an existing -incoming with no positional follower (defer-style)
	// followed by an unrelated flag — we must consume exactly one slot after
	// -incoming, even when that slot is benign-looking.
	args := []string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-incoming", "defer",
		"-no-shutdown",
	}
	_, out, err := transformCmdline(args, "", "", "", "", "", "")
	if err != nil {
		t.Fatalf("transformCmdline: %v", err)
	}
	if slices.Contains(out[:len(out)-3], "defer") {
		t.Fatalf("source -incoming arg leaked into mid-output: %v", out)
	}
	if !slices.Contains(out, "-no-shutdown") {
		t.Fatalf("expected -no-shutdown to survive, got: %v", out)
	}
}

func TestReadCmdlineFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "cmdline")
	body := "/opt/kata/bin/qemu-system-x86_64\n-name\nsandbox-x\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readCmdlineFile(p)
	if err != nil {
		t.Fatalf("readCmdlineFile: %v", err)
	}
	want := []string{"/opt/kata/bin/qemu-system-x86_64", "-name", "sandbox-x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readCmdlineFile = %v, want %v", got, want)
	}
}

func TestReadCmdlineFile_Missing(t *testing.T) {
	t.Parallel()
	if _, err := readCmdlineFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCaptureSourceCmdline(t *testing.T) {
	t.Parallel()
	// /proc/self/cmdline always exists for the test binary; capture it and
	// assert the file is non-empty and parses to at least one arg.
	tmp := filepath.Join(t.TempDir(), "out", "cmdline")
	if err := captureSourceCmdline(os.Getpid(), tmp); err != nil {
		t.Fatalf("captureSourceCmdline: %v", err)
	}
	got, err := readCmdlineFile(tmp)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("captured empty cmdline")
	}
	// argv[0] of the test binary should be a path containing 'test'.
	if got[0] == "" {
		t.Fatalf("argv[0] is empty: %v", got)
	}
}

func TestSpawnReplayedQEMU_MissingFile(t *testing.T) {
	t.Parallel()
	cfg := DestConfig{ReplayCmdlineFile: filepath.Join(t.TempDir(), "missing")}
	if err := spawnReplayedQEMU(context.Background(), &cfg); err == nil {
		t.Fatal("expected error for missing cmdline file")
	}
}

func TestSpawnReplayedQEMU_TooFewArgs(t *testing.T) {
	t.Parallel()
	tmp := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(tmp, []byte("/opt/kata/bin/qemu-system-x86_64\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := DestConfig{ReplayCmdlineFile: tmp}
	err := spawnReplayedQEMU(context.Background(), &cfg)
	if err == nil || !strings.Contains(err.Error(), "too few args") {
		t.Fatalf("expected too-few-args error, got %v", err)
	}
}

func TestSpawnReplayedQEMU_HappyPath_StubbedSpawn(t *testing.T) {
	// Not parallel: mutates package-level spawn / wait stubs and sandboxRoot.
	tmpDir := t.TempDir()

	// Write a synthetic cmdline that mentions a fake source sandbox under a
	// per-test sandboxRoot, plus a non-/dev/shm mem-path so the nvdimm copy
	// branch runs against a real (but tiny) source file.
	srcSandboxRoot := filepath.Join(tmpDir, "vm")
	srcSandboxDir := filepath.Join(srcSandboxRoot, "src-uuid")
	if err := os.MkdirAll(srcSandboxDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srcNvdimm := filepath.Join(tmpDir, "src-nvdimm.img")
	if err := os.WriteFile(srcNvdimm, []byte("nvdimm-bytes"), 0o644); err != nil {
		t.Fatalf("write src nvdimm: %v", err)
	}

	cmdlinePath := filepath.Join(tmpDir, "cmdline")
	body := strings.Join([]string{
		"/opt/kata/bin/qemu-system-x86_64",
		"-name", "sandbox-src-uuid",
		"-qmp", "unix:" + srcSandboxDir + "/qmp.sock,server=on,wait=off",
		"-object", "memory-backend-file,id=nvdimm,mem-path=" + srcNvdimm + ",size=512M,readonly=on",
		"-incoming", "tcp:[::]:4444",
		"-daemonize",
	}, "\n") + "\n"
	if err := os.WriteFile(cmdlinePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}

	// Swap sandboxRoot to a per-test root, and capture the dest sandbox dir
	// the test expects spawnReplayedQEMU to derive.
	prevRoot := sandboxRoot
	sandboxRoot = srcSandboxRoot
	t.Cleanup(func() { sandboxRoot = prevRoot })

	prevShared := kataSharedSandboxRoot
	kataSharedSandboxRoot = filepath.Join(tmpDir, "kata-shared")
	t.Cleanup(func() { kataSharedSandboxRoot = prevShared })

	dstSandboxID := "katamaran-dest-test"
	dstSandboxDir := filepath.Join(srcSandboxRoot, dstSandboxID)
	dstSocket := filepath.Join(dstSandboxDir, "extra-monitor.sock")
	dstVhostSock := filepath.Join(dstSandboxDir, "vhost-fs.sock")

	// Stub the spawn function: record what would have been launched, and
	// (when asked to spawn QEMU) create the QMP socket so waitForSocket
	// terminates. Same for virtiofsd → vhost-fs socket.
	type spawnRec struct {
		name string
		args []string
	}
	var spawned []spawnRec
	prevSpawn := spawnDetachedProcess
	spawnDetachedProcess = func(_ context.Context, name string, args []string) error {
		spawned = append(spawned, spawnRec{name: name, args: append([]string(nil), args...)})
		switch {
		case strings.Contains(name, "virtiofsd"):
			// Drop a fake UNIX socket where virtiofsd would have created one.
			if err := os.MkdirAll(filepath.Dir(dstVhostSock), 0o755); err != nil {
				return err
			}
			return createFakeSocket(dstVhostSock)
		case strings.Contains(name, "qemu"):
			if err := os.MkdirAll(filepath.Dir(dstSocket), 0o755); err != nil {
				return err
			}
			return createFakeSocket(dstSocket)
		}
		return nil
	}
	t.Cleanup(func() { spawnDetachedProcess = prevSpawn })

	// Speed up waitForSocket.
	prevWait := waitForSocket
	waitForSocket = func(ctx context.Context, path string, _ time.Duration) error {
		// Wait briefly so the spawn callback above had a chance to run, but
		// the goroutine ordering is in fact synchronous in this test.
		deadline := time.Now().Add(2 * time.Second)
		for {
			if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
				return nil
			}
			if time.Now().After(deadline) {
				return errors.New("test wait timeout")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	t.Cleanup(func() { waitForSocket = prevWait })

	cfg := DestConfig{
		ReplayCmdlineFile: cmdlinePath,
		SandboxID:         dstSandboxID,
	}
	if err := spawnReplayedQEMU(context.Background(), &cfg); err != nil {
		t.Fatalf("spawnReplayedQEMU: %v", err)
	}

	if cfg.QMPSocket != dstSocket {
		t.Fatalf("cfg.QMPSocket = %q, want %q", cfg.QMPSocket, dstSocket)
	}

	if len(spawned) != 2 {
		t.Fatalf("expected 2 spawns (virtiofsd + qemu), got %d: %+v", len(spawned), spawned)
	}
	if !strings.Contains(spawned[0].name, "virtiofsd") {
		t.Fatalf("first spawn should be virtiofsd, got %s", spawned[0].name)
	}
	// Verify virtiofsd got --migration-on-error=guest-error (project memory Fix 2).
	if !slices.Contains(spawned[0].args, "--migration-on-error=guest-error") {
		t.Fatalf("virtiofsd missing --migration-on-error=guest-error: %v", spawned[0].args)
	}
	if !strings.Contains(spawned[1].name, "qemu") {
		t.Fatalf("second spawn should be qemu, got %s", spawned[1].name)
	}
	// Verify QEMU got -incoming defer at tail (no -daemonize: we run foreground).
	q := spawned[1].args
	if len(q) < 2 || q[len(q)-2] != "-incoming" || q[len(q)-1] != "defer" {
		t.Fatalf("qemu args missing trailing -incoming defer: %v", q)
	}
	// Original source -incoming positional must be gone.
	if slices.Contains(q[:len(q)-2], "tcp:[::]:4444") {
		t.Fatalf("source -incoming URI leaked into qemu args: %v", q)
	}
	// Nvdimm path must have been substituted to a /tmp temp.
	joined := strings.Join(q, " ")
	if strings.Contains(joined, srcNvdimm) {
		t.Fatalf("source nvdimm path leaked into qemu args: %s", joined)
	}
	if !strings.Contains(joined, "/tmp/kata-dst-nvdimm-") {
		t.Fatalf("expected /tmp/kata-dst-nvdimm-* in qemu args, got: %s", joined)
	}
	// Sandbox UUID substitution must have happened.
	if strings.Contains(joined, "src-uuid") {
		t.Fatalf("source sandbox UUID leaked into qemu args: %s", joined)
	}
	if !strings.Contains(joined, dstSandboxID) {
		t.Fatalf("expected dst sandbox id %q in qemu args, got: %s", dstSandboxID, joined)
	}
}

// createFakeSocket binds a UNIX socket at path so os.Stat reports it as a
// socket type. We use a UnixListener with SetUnlinkOnClose(false) so the
// inode survives the Close() call. The listener itself is leaked (closed
// FD) and reaped by Go runtime / test cleanup.
func createFakeSocket(path string) error {
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return err
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return err
	}
	l.SetUnlinkOnClose(false)
	return l.Close()
}
