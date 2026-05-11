// containerd-shim-katamaran-adopted-v2 is the containerd v2 shim that
// adopts a migrated QEMU process into a Kubernetes-managed Pod. It is
// step 2 of Approach E in docs/ROADMAP.md ("Kata Sandbox Adoption").
//
// Status: SCAFFOLDING. This binary currently only records the
// invocation envelope (CLI args + env + stdin Spec) and the
// surviving QEMU's migration-meta.json so the operator can see what
// containerd asks of it on a live install. It is not yet a working
// containerd v2 shim: it does NOT implement the TaskService ttrpc
// API, does NOT publish its socket address on stdout in the format
// containerd expects, and does NOT keep the adopted pod Running from
// kubelet's perspective. Installing the matching RuntimeClass and
// having containerd invoke this binary therefore causes the adoption
// pod to fail to start — which is OK while we are still iterating
// on the design.
//
// Why a scaffolding-first commit: containerd's shim v2 protocol is
// non-trivial (binary lifecycle: start → daemonize → ttrpc server,
// Task/Sandbox RPC surface, pidfd plumbing for Wait, cgroup
// management) and the smallest useful version still pulls in
// github.com/containerd/ttrpc + the runtime/task v3 proto package.
// Landing the scaffolding now lets the next contributor (or a
// follow-up commit) focus on the protocol implementation without
// also having to design the package layout, manifest, and
// controller wiring at the same time.
//
// Containerd v2 shim CLI envelope (for reference when implementing):
//
//	$ containerd-shim-<runtime>-v2 [global-flags] <command>
//
// where command is one of: start, delete. The shim's first
// invocation is `start`, with stdin set to the runtime spec JSON
// and env containing TTRPC_ADDRESS pointing at containerd's own
// ttrpc socket. The shim must:
//
//	1. Daemonize (fork into background; parent exits).
//	2. Bind a new ttrpc socket (typically /run/containerd/s/<rand>).
//	3. Write that socket's address to stdout on a single line.
//	4. Serve task.v3 TaskService until containerd issues
//	   Shutdown or the wrapped process exits.
//
// On `delete` the shim must remove any state left in the bundle
// directory and reap the process group of the adopted QEMU.
//
// Adopt-specific behaviour (next-commit material):
//   - On Create: read migration-meta.json from the bundle dir,
//     locate the surviving QEMU PID in
//     /sys/fs/cgroup/katamaran-adopted/<sandbox-id>/cgroup.procs.
//     If absent, return a containerd-shaped error rather than
//     cold-spawning a fresh VM.
//   - On Start: best-effort `cont` over QMP in case the migrated
//     VM is still in the post-migrate paused state.
//   - On Wait: open a pidfd for the QEMU pid and block on
//     epoll/POLLIN; surface QEMU exit as the container exit
//     event.
//   - On State: alive iff /proc/<pid>/comm still reads
//     qemu-system-* and the QMP socket responds to a
//     query-status ping.
//   - On Delete: SIGTERM QEMU, wait, SIGKILL on timeout, remove
//     the surviving cgroup directory.
//
// See cmd/containerd-shim-katamaran-adopted-v2/main_test.go (when
// it exists) for the live invocation envelope captured against
// containerd 2.2.x on the test cluster — the log written to
// /var/log/katamaran-shim/<container-id>.log here.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	// containerd may invoke the shim many times per container
	// lifecycle. Each invocation gets a fresh log line so the
	// operator can correlate against ContainerCreating events on
	// the adoption pod.
	logDir := "/var/log/katamaran-shim"
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "scaffolding.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		// Containerd reads stdout for the shim's ttrpc address;
		// printing diagnostics there would break parsing on a
		// production shim. We don't even attempt that — this is
		// scaffolding, no ttrpc socket to publish.
		fmt.Fprintf(os.Stderr, "katamaran-adopted shim: open log %s: %v\n", logPath, err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	fmt.Fprintf(f, "---\n%s invoked\n", stamp)
	fmt.Fprintf(f, "argv: %s\n", strings.Join(os.Args, " "))
	fmt.Fprintf(f, "cwd: %s\n", getCwd())
	fmt.Fprintf(f, "env:\n")
	for _, e := range os.Environ() {
		// Drop noisy env: we only care about the bits containerd sets
		// for the shim handshake.
		k := strings.SplitN(e, "=", 2)[0]
		switch k {
		case "TTRPC_ADDRESS", "GRPC_ADDRESS", "NAMESPACE", "DEBUG", "PUBLISH_BINARY", "MAX_SHIM_VERSION":
			fmt.Fprintf(f, "  %s\n", e)
		}
	}
	if stdinBytes, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024)); err == nil && len(stdinBytes) > 0 {
		fmt.Fprintf(f, "stdin (%d bytes):\n%s\n", len(stdinBytes), string(stdinBytes))
	}

	// Containerd expects the shim to print its ttrpc address on
	// stdout for the `start` command. We deliberately print nothing
	// here so containerd's create-task path fails fast with a clear
	// error — the matching Migration CR is the source of truth for
	// "this scaffolding lives here, real implementation TBD", and we
	// don't want to silently let the pod sit in ContainerCreating.
	fmt.Fprintln(os.Stderr, "katamaran-adopted shim is scaffolding-only — see cmd/containerd-shim-katamaran-adopted-v2/main.go package doc and docs/ROADMAP.md")
	os.Exit(1)
}

func getCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "<unknown>"
	}
	return wd
}
