package migration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maci0/katamaran/internal/qmp"
	"github.com/maci0/katamaran/internal/qmptest"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written during the call. The VMConfig emission markers are
// printed on stdout (not slog) precisely so they survive log re-formatting
// when scraped from pod logs, so stdout is the observable behavior here.
// Not safe for parallel use; callers must not be parallel.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	read := make(chan string, 1)
	go func() {
		var buf strings.Builder
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			buf.WriteString(sc.Text() + "\n")
		}
		read <- buf.String()
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	return <-read
}

func markerLines(t *testing.T, out, marker string) []string {
	t.Helper()

	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, marker) {
			lines = append(lines, strings.TrimPrefix(line, marker))
		}
	}
	return lines
}

// writeSandboxPersist seeds kataSBSRoot-rooted sandbox dirs the way Kata
// stages them: <root>/<uuid>/persist.json with HypervisorState.Pid and a
// Config block. tag distinguishes sandboxes inside HypervisorConfig.
func writeSandboxPersist(t *testing.T, root, uuid string, pid int, tag string) {
	t.Helper()

	dir := filepath.Join(root, uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sandbox %s: %v", uuid, err)
	}
	persist := fmt.Sprintf(
		`{"HypervisorState":{"Pid":%d},"Config":{"HypervisorType":"qemu","HypervisorConfig":{"path":"/opt/kata/bin/qemu-%s"},"KataAgentConfig":{"debug":true}}}`,
		pid, tag,
	)
	if err := os.WriteFile(filepath.Join(dir, "persist.json"), []byte(persist), 0o600); err != nil {
		t.Fatalf("write persist.json: %v", err)
	}
}

// withKataSBSRoot points kataSBSRoot at dir for the duration of the test.
// Callers must not be parallel (package-level seam).
func withKataSBSRoot(t *testing.T, dir string) {
	t.Helper()
	prev := kataSBSRoot
	kataSBSRoot = dir
	t.Cleanup(func() { kataSBSRoot = prev })
}

// TestMarshalVMConfig pins the JSON contract consumed by the factory's
// node VMConfig loader (internal/factory tryLoadFromSandbox): three keys,
// raw passthrough of both config blobs, typed hypervisor field.
func TestMarshalVMConfig(t *testing.T) {
	t.Parallel()
	hypCfg := json.RawMessage(`{"path":"/opt/kata/bin/qemu-system-x86_64"}`)
	agentCfg := json.RawMessage(`{"debug":true}`)

	got := MarshalVMConfig("qemu", hypCfg, agentCfg)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("unmarshal MarshalVMConfig output %s: %v", got, err)
	}
	if len(m) != 3 {
		t.Fatalf("MarshalVMConfig keys = %v, want exactly 3 (factory reads these names)", m)
	}
	if string(m["HypervisorType"]) != `"qemu"` {
		t.Fatalf("HypervisorType = %s, want %q", m["HypervisorType"], "qemu")
	}
	if compactJSONBytes(t, m["HypervisorConfig"]) != compactJSONBytes(t, hypCfg) {
		t.Fatalf("HypervisorConfig = %s, want passthrough of %s", m["HypervisorConfig"], hypCfg)
	}
	if compactJSONBytes(t, m["AgentConfig"]) != compactJSONBytes(t, agentCfg) {
		t.Fatalf("AgentConfig = %s, want passthrough of %s", m["AgentConfig"], agentCfg)
	}
}

func compactJSONBytes(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		t.Fatalf("compact JSON %s: %v", data, err)
	}
	return buf.String()
}

// TestEmitVMConfig_MatchingPidEmitsMarkers locks the source side of the
// adoption pipeline: emitVMConfig must find the persist.json whose
// HypervisorState.Pid matches the live QEMU pid (skipping other sandboxes)
// and print exactly one KATAMARAN_VMCONFIG_B64 plus one
// KATAMARAN_AGENTCONFIG_B64 line carrying base64 payloads that decode to
// the shapes the dest binary and factory consume. A wrong or missing
// marker here silently degrades every migration to a cold-start VM.
func TestEmitVMConfig_MatchingPidEmitsMarkers(t *testing.T) {
	// Not parallel: swaps kataSBSRoot and captures process stdout.
	root := t.TempDir()
	withKataSBSRoot(t, root)

	const qemuPID = 4242
	writeSandboxPersist(t, root, "sb-other", 9999, "other")
	writeSandboxPersist(t, root, "sb-mine", qemuPID, "mine")

	out := captureStdout(t, func() { emitVMConfig(qemuPID) })

	vmPayloads := markerLines(t, out, "KATAMARAN_VMCONFIG_B64=")
	if len(vmPayloads) != 1 {
		t.Fatalf("emitted %d KATAMARAN_VMCONFIG_B64 markers, want exactly 1; stdout:\n%s", len(vmPayloads), out)
	}
	agentPayloads := markerLines(t, out, "KATAMARAN_AGENTCONFIG_B64=")
	if len(agentPayloads) != 1 {
		t.Fatalf("emitted %d KATAMARAN_AGENTCONFIG_B64 markers, want exactly 1; stdout:\n%s", len(agentPayloads), out)
	}

	vmRaw, err := base64.StdEncoding.DecodeString(vmPayloads[0])
	if err != nil {
		t.Fatalf("decode VMConfig payload: %v", err)
	}
	var vm struct {
		HypervisorType   string          `json:"HypervisorType"`
		HypervisorConfig json.RawMessage `json:"HypervisorConfig"`
		AgentConfig      json.RawMessage `json:"AgentConfig"`
	}
	if err := json.Unmarshal(vmRaw, &vm); err != nil {
		t.Fatalf("unmarshal decoded VMConfig %s: %v", vmRaw, err)
	}
	if vm.HypervisorType != "qemu" {
		t.Fatalf("HypervisorType = %q, want qemu", vm.HypervisorType)
	}
	// Must come from sb-mine (the PID match), not sb-other.
	if !strings.Contains(string(vm.HypervisorConfig), "qemu-mine") {
		t.Fatalf("HypervisorConfig = %s, want the sb-mine payload (PID matching picked the wrong sandbox)", vm.HypervisorConfig)
	}

	agentRaw, err := base64.StdEncoding.DecodeString(agentPayloads[0])
	if err != nil {
		t.Fatalf("decode AgentConfig payload: %v", err)
	}
	if compactJSONBytes(t, agentRaw) != `{"debug":true}` {
		t.Fatalf("decoded AgentConfig = %s, want persisted KataAgentConfig", agentRaw)
	}
}

// TestEmitVMConfig_NoMatchingPidEmitsNothing confirms a sandbox tree without
// a persist.json matching the QEMU pid emits no partial or empty markers:
// consumers treat the absence of the marker as "no adoption data", but an
// empty-payload marker would decode to garbage and fail downstream parsing.
func TestEmitVMConfig_NoMatchingPidEmitsNothing(t *testing.T) {
	// Not parallel: swaps kataSBSRoot and captures process stdout.
	root := t.TempDir()
	withKataSBSRoot(t, root)

	writeSandboxPersist(t, root, "sb-a", 1111, "a")

	out := captureStdout(t, func() { emitVMConfig(4242) })

	for _, marker := range []string{"KATAMARAN_VMCONFIG_B64=", "KATAMARAN_AGENTCONFIG_B64="} {
		if got := markerLines(t, out, marker); len(got) != 0 {
			t.Fatalf("%s emitted despite no PID match: %v", marker, got)
		}
	}
}

// TestFindSandboxPersist covers the sandbox persist.json scanner shared by
// the dest-side migration-meta enrichment and the factory's node VMConfig
// loader.
func TestFindSandboxPersist_PicksFirstReadableSandbox(t *testing.T) {
	// Not parallel: swaps kataSBSRoot.
	root := t.TempDir()
	withKataSBSRoot(t, root)

	if err := os.MkdirAll(filepath.Join(root, "sb-empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty sandbox: %v", err)
	}
	writeSandboxPersist(t, root, "sb-full", 7, "full")

	got := FindSandboxPersist(kataSBSRoot)
	if got == nil {
		t.Fatal("FindSandboxPersist returned nil for a readable persist.json")
	}
	if want := filepath.Join(root, "sb-full", "persist.json"); got.Path != want {
		t.Fatalf("path = %q, want %q", got.Path, want)
	}
	if !strings.Contains(string(got.HypervisorState), `"Pid":7`) {
		t.Fatalf("HypervisorState = %s, want persist.json HypervisorState passthrough", got.HypervisorState)
	}
	if !strings.Contains(string(got.Config.HypervisorConfig), `qemu-full`) {
		t.Fatalf("HypervisorConfig = %s, want the seeded payload", got.Config.HypervisorConfig)
	}
}

func TestFindSandboxPersist_MissingRootReturnsNil(t *testing.T) {
	// Not parallel: swaps kataSBSRoot.
	withKataSBSRoot(t, filepath.Join(t.TempDir(), "no-such-root"))

	if got := FindSandboxPersist(kataSBSRoot); got != nil {
		t.Fatalf("FindSandboxPersist = %+v, want nil for missing root", got)
	}
}

// TestWriteMigrationMeta_EnrichesFromPersistJSON drives the dest-side
// adoption hand-off end to end: after RESUME, migration-meta.json written
// next to the QMP socket must carry the QEMU pid read from the sibling pid
// file plus HypervisorState/VMConfig/AgentConfig enriched from the node's
// persist.json — the fields the factory Watcher turns into a GetBaseVM
// offer. Without this enrichment the factory serves adoption-less VMs.
func TestWriteMigrationMeta_EnrichesFromPersistJSON(t *testing.T) {
	// Not parallel: swaps kataSBSRoot.
	root := t.TempDir()
	withKataSBSRoot(t, root)
	writeSandboxPersist(t, root, "sb-dest", 12345, "destnode")

	qmpDir := t.TempDir()
	sockPath := filepath.Join(qmpDir, extraMonitorSocketName)
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		qmptest.QMPHandshake(conn)
		buf := make([]byte, 8192)
		for {
			n, readErr := conn.Read(buf)
			if readErr != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "query-status") {
				_, _ = conn.Write([]byte(`{"return":{"status":"running"}}` + "\n"))
			} else {
				_, _ = conn.Write([]byte(`{"return":{}}` + "\n"))
			}
		}
	}()

	if err := os.WriteFile(filepath.Join(qmpDir, "pid"), []byte("12345\n"), 0o644); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}

	ctx := context.Background()
	client, err := qmp.NewClient(ctx, sockPath)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	writeMigrationMeta(ctx, DestConfig{QMPSocket: sockPath}, client)

	raw, err := os.ReadFile(filepath.Join(qmpDir, MigrationMetaFile))
	if err != nil {
		t.Fatalf("read migration-meta.json: %v", err)
	}
	var meta MigrationMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal migration-meta.json %s: %v", raw, err)
	}
	if meta.ID != filepath.Base(qmpDir) {
		t.Errorf("id = %q, want sandbox dir name", meta.ID)
	}
	if meta.QEMUPid != 12345 {
		t.Errorf("qemu_pid = %d, want 12345 (adoption is skipped when this is zero)", meta.QEMUPid)
	}
	if meta.QMPSocket != sockPath {
		t.Errorf("qmp_socket = %q, want %q", meta.QMPSocket, sockPath)
	}
	if !strings.Contains(string(meta.VMConfig), "qemu-destnode") {
		t.Errorf("vm_config = %s, want payload enriched from local persist.json", meta.VMConfig)
	}
	if compactJSONBytes(t, meta.AgentConfig) != `{"debug":true}` {
		t.Errorf("agent_config = %s, want persisted KataAgentConfig", meta.AgentConfig)
	}
	// MarshalIndent reformats the embedded RawMessage, so compare compacted.
	if !strings.Contains(compactJSONBytes(t, meta.HypervisorState), `"Pid":12345`) {
		t.Errorf("hypervisor_state = %s, want persist.json HypervisorState passthrough", meta.HypervisorState)
	}
	// No leftover temp file from the atomic write.
	if _, err := os.Stat(filepath.Join(qmpDir, MigrationMetaFile+".tmp")); !os.IsNotExist(err) {
		t.Error("atomic-write temp file was left behind next to migration-meta.json")
	}
}
