// containerd-shim-katamaran-adopted-v2 is the containerd v2 shim that
// adopts a migrated QEMU process into a Kubernetes-managed Pod. See
// docs/ROADMAP.md "Kata Sandbox Adoption → Approach E" for the full
// design context.
//
// Containerd shim v2 lifecycle handled here:
//
//  1. Containerd invokes the shim binary with a `start` subcommand,
//     bundle dir cwd, and env containing the publisher socket etc.
//     The shim daemonizes (fork+exec self with no `start` arg),
//     binds /run/containerd/s/<random>.sock as a ttrpc server, and
//     prints "unix://<path>\n" to stdout. Parent exits 0.
//  2. Containerd connects to that ttrpc socket and calls the
//     TTRPCTaskService methods. The shim looks up the surviving
//     migrated QEMU pid (Approach E step 1 wrote it into
//     /sys/fs/cgroup/katamaran-adopted/<sandbox-id>/cgroup.procs)
//     and answers RPCs against that pid.
//  3. On Delete or QEMU exit the shim shuts down its ttrpc server
//     and exits.
//
// Adoption-specific contract:
//
//   - The pod's runtimeClassName MUST be `katamaran-adopted` (see
//     config/crd/runtimeclass-adopted.yaml).
//   - The pod's metadata.annotations MUST contain
//     `katamaran.io/adopted-sandbox-id` pointing at the sandbox id
//     the destination katamaran job wrote (defaults to
//     `katamaran-dest`, the well-known sandbox id from the dest job
//     template).
//   - katamaran-mgr's createAdoptionPod is responsible for stamping
//     both fields on the adoption pod.
//
// If no surviving QEMU is found in the configured cgroup, Create
// returns FailedPrecondition so containerd surfaces a clear error
// instead of silently cold-booting a fresh VM.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	taskTypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// adoptedCgroupRoot mirrors the path where Approach E step 1's
	// surviveContainerExit writes the migrated QEMU + its KVM helper
	// kernel threads. Keep in sync with internal/migration/dest.go.
	adoptedCgroupRoot = "/sys/fs/cgroup/katamaran-adopted"

	// shimSocketDir is the well-known location containerd uses for
	// shim ttrpc sockets.
	shimSocketDir = "/run/containerd/s"

	// adoptedSandboxAnnotation lets the controller pin which sandbox
	// id (under adoptedCgroupRoot) this pod adopts. Defaults to
	// `katamaran-dest` (the dest job template's well-known name) when
	// absent.
	adoptedSandboxAnnotation = "katamaran.io/adopted-sandbox-id"

	defaultAdoptedSandboxID = "katamaran-dest"

	// shimLogPath is where the shim writes operational logs since it
	// must not pollute stdout (containerd parses stdout for the ttrpc
	// address on the start command).
	shimLogPath = "/var/log/katamaran-shim/shim.log"
)

func main() {
	logf, _ := os.OpenFile(shimLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if logf != nil {
		defer func() { _ = logf.Close() }()
	}
	logFn := func(format string, args ...any) {
		if logf == nil {
			return
		}
		fmt.Fprintf(logf, "%s [%d] ", time.Now().UTC().Format(time.RFC3339Nano), os.Getpid())
		fmt.Fprintf(logf, format, args...)
		fmt.Fprintln(logf)
	}
	logFn("invoked argv=%v", os.Args)

	// Containerd invokes the shim multiple times during a container's
	// lifecycle. The first invocation is `start`; subsequent
	// invocations may be `delete`. Anything else means we are the
	// daemonized child started by our own `start` handler — fall
	// through to the ttrpc server loop.
	cmd := ""
	for i, arg := range os.Args {
		if arg == "start" || arg == "delete" {
			cmd = arg
			os.Args = append(os.Args[:i], os.Args[i+1:]...)
			break
		}
	}

	switch cmd {
	case "start":
		if err := runStart(logFn); err != nil {
			logFn("start: %v", err)
			fmt.Fprintf(os.Stderr, "katamaran-adopted shim start: %v\n", err)
			os.Exit(1)
		}
	case "delete":
		if err := runDelete(logFn); err != nil {
			logFn("delete: %v", err)
			fmt.Fprintf(os.Stderr, "katamaran-adopted shim delete: %v\n", err)
			os.Exit(1)
		}
	default:
		// Daemonized server invocation. Re-exec with no command, bind
		// the inherited socket fd, and serve until shutdown.
		if err := runServer(logFn); err != nil {
			logFn("server: %v", err)
			fmt.Fprintf(os.Stderr, "katamaran-adopted shim server: %v\n", err)
			os.Exit(1)
		}
	}
}

// runStart fork+execs this binary with no `start` argument and the
// listening socket on fd 3, then prints the ttrpc address to stdout
// and exits. Containerd reads the address from stdout to know where
// the shim's ttrpc server is reachable.
func runStart(logFn func(format string, args ...any)) error {
	if err := os.MkdirAll(shimSocketDir, 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	socketPath := filepath.Join(shimSocketDir, fmt.Sprintf("katamaran-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	addr, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return fmt.Errorf("resolve unix addr: %w", err)
	}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socketPath, err)
	}
	listener.SetUnlinkOnClose(false) // child handles cleanup on exit
	listenerFile, err := listener.File()
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("listener file: %w", err)
	}
	defer func() { _ = listenerFile.Close() }()

	// Re-exec self without the `start` arg. The listener fd becomes
	// fd 3 in the child via ExtraFiles. The child knows to use it
	// because we set the env var below.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	childArgs := os.Args[1:] // start was already removed from os.Args
	child := exec.Command(exe, childArgs...)
	child.ExtraFiles = []*os.File{listenerFile}
	child.Env = append(os.Environ(), "KATAMARAN_SHIM_LISTENER_FD=3")
	// Detach from parent's process group so we survive containerd's
	// reaping of the start-command child.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return fmt.Errorf("fork child: %w", err)
	}
	logFn("daemonized child pid=%d listening at unix://%s", child.Process.Pid, socketPath)

	// Containerd expects the shim's ttrpc address on stdout for the
	// start command. Single line, "unix://<path>".
	if _, err := fmt.Fprintf(os.Stdout, "unix://%s", socketPath); err != nil {
		return fmt.Errorf("write address to stdout: %w", err)
	}
	// Release the parent's reference to the listener so only the
	// child holds it.
	_ = listener.Close()
	return nil
}

// runDelete handles containerd's `delete` command, which is called
// after the task ends to give the shim a chance to clean up the
// bundle dir. With Approach E, the QEMU and its surviving cgroup
// outlive the pod by design — Delete only removes shim-local state
// (none yet).
func runDelete(logFn func(format string, args ...any)) error {
	logFn("delete: noop (QEMU + cgroup intentionally outlive the pod)")
	// Containerd reads a DeleteResponse from stdout for the delete
	// command. Empty exit info is fine.
	resp := &taskAPI.DeleteResponse{
		ExitedAt: timestamppb.New(time.Now()),
	}
	data, err := proto.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal DeleteResponse: %w", err)
	}
	if _, err := os.Stdout.Write(data); err != nil {
		return fmt.Errorf("write DeleteResponse: %w", err)
	}
	return nil
}

// runServer is the ttrpc server loop. The listening socket was passed
// in by runStart as fd 3 (per KATAMARAN_SHIM_LISTENER_FD env).
func runServer(logFn func(format string, args ...any)) error {
	if os.Getenv("KATAMARAN_SHIM_LISTENER_FD") == "" {
		return errors.New("invoked without start/delete and no listener fd; refusing to run")
	}
	f := os.NewFile(3, "ttrpc-listener")
	if f == nil {
		return errors.New("listener fd 3 not present")
	}
	defer func() { _ = f.Close() }()
	listener, err := net.FileListener(f)
	if err != nil {
		return fmt.Errorf("FileListener: %w", err)
	}
	srv, err := ttrpc.NewServer()
	if err != nil {
		return fmt.Errorf("ttrpc.NewServer: %w", err)
	}
	taskSvc := newAdoptedTaskService(logFn)
	taskAPI.RegisterTTRPCTaskService(srv, taskSvc)
	logFn("ttrpc server starting")

	go func() {
		if err := srv.Serve(context.Background(), listener); err != nil && !errors.Is(err, ttrpc.ErrServerClosed) {
			logFn("ttrpc Serve: %v", err)
		}
	}()

	// Block until Shutdown is called by containerd.
	<-taskSvc.shutdownCh
	logFn("shutdown received; closing ttrpc server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// adoptedTaskService implements TTRPCTaskService for an adoption pod.
// All interesting state is the QEMU pid we located on Create.
type adoptedTaskService struct {
	logFn      func(format string, args ...any)
	mu         sync.Mutex
	id         string
	qemuPid    int
	startedAt  time.Time
	exitedAt   time.Time
	exitCode   uint32
	exited     bool
	shutdownCh chan struct{}
	waiters    []chan struct{}
}

func newAdoptedTaskService(logFn func(format string, args ...any)) *adoptedTaskService {
	return &adoptedTaskService{
		logFn:      logFn,
		shutdownCh: make(chan struct{}),
	}
}

// Create looks up the surviving QEMU and answers with its pid. The
// CreateTaskRequest's Bundle field holds the OCI bundle dir whose
// config.json carries the pod annotations — we read the
// adoptedSandboxAnnotation from there.
func (s *adoptedTaskService) Create(_ context.Context, req *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = req.ID
	sandboxID := readAdoptedSandboxID(req.Bundle)
	if sandboxID == "" {
		sandboxID = defaultAdoptedSandboxID
	}
	pid, err := lookupAdoptedQEMUPid(sandboxID)
	if err != nil {
		s.logFn("Create: no surviving QEMU for sandbox=%s: %v", sandboxID, err)
		return nil, fmt.Errorf("no surviving migrated QEMU at %s/%s: %w", adoptedCgroupRoot, sandboxID, err)
	}
	s.qemuPid = pid
	s.startedAt = time.Now()
	s.logFn("Create: id=%s sandbox=%s adopted_qemu_pid=%d", req.ID, sandboxID, pid)
	return &taskAPI.CreateTaskResponse{Pid: uint32(pid)}, nil
}

// Start transitions the adopted task to running. With Approach E the
// VM is already running (RESUME fired on the dest binary); Start is a
// no-op other than recording the timestamp and kicking off the wait
// goroutine that signals exit.
func (s *adoptedTaskService) Start(_ context.Context, _ *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	s.mu.Lock()
	pid := s.qemuPid
	s.mu.Unlock()
	if pid == 0 {
		return nil, errors.New("Start before Create")
	}
	go s.watchExit()
	s.logFn("Start: pid=%d (already running, watch goroutine armed)", pid)
	return &taskAPI.StartResponse{Pid: uint32(pid)}, nil
}

// State reports whether the adopted QEMU is alive.
func (s *adoptedTaskService) State(_ context.Context, _ *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := taskTypes.Status_RUNNING
	if s.exited {
		status = taskTypes.Status_STOPPED
	}
	return &taskAPI.StateResponse{
		ID:         s.id,
		Pid:        uint32(s.qemuPid),
		Status:     status,
		ExitStatus: s.exitCode,
		ExitedAt:   timestamppb.New(s.exitedAt),
	}, nil
}

// Wait blocks until the adopted QEMU exits.
func (s *adoptedTaskService) Wait(ctx context.Context, _ *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	s.mu.Lock()
	if s.exited {
		exitCode := s.exitCode
		exitedAt := s.exitedAt
		s.mu.Unlock()
		return &taskAPI.WaitResponse{ExitStatus: exitCode, ExitedAt: timestamppb.New(exitedAt)}, nil
	}
	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	s.mu.Unlock()
	select {
	case <-ch:
		s.mu.Lock()
		defer s.mu.Unlock()
		return &taskAPI.WaitResponse{ExitStatus: s.exitCode, ExitedAt: timestamppb.New(s.exitedAt)}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Kill sends a signal to the adopted QEMU. SIGTERM/SIGKILL are
// honoured; everything else is best-effort.
func (s *adoptedTaskService) Kill(_ context.Context, req *taskAPI.KillRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	pid := s.qemuPid
	s.mu.Unlock()
	if pid == 0 {
		return &emptypb.Empty{}, nil
	}
	if err := syscall.Kill(pid, syscall.Signal(req.Signal)); err != nil {
		s.logFn("Kill pid=%d signal=%d failed: %v", pid, req.Signal, err)
	} else {
		s.logFn("Kill pid=%d signal=%d", pid, req.Signal)
	}
	return &emptypb.Empty{}, nil
}

// Delete waits for QEMU to exit (containerd has already issued Kill
// by the time it calls Delete) and removes the surviving cgroup dir.
// Returns the recorded exit code so containerd can close the task.
func (s *adoptedTaskService) Delete(ctx context.Context, _ *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	s.mu.Lock()
	pid := s.qemuPid
	exited := s.exited
	exitCode := s.exitCode
	exitedAt := s.exitedAt
	s.mu.Unlock()
	// Try to clean up the surviving cgroup. Best-effort.
	if pid > 0 {
		_ = removeAdoptedCgroup(pid)
	}
	if !exited {
		exitedAt = time.Now()
	}
	s.logFn("Delete: pid=%d exited=%v code=%d", pid, exited, exitCode)
	_ = ctx
	return &taskAPI.DeleteResponse{
		Pid:        uint32(pid),
		ExitStatus: exitCode,
		ExitedAt:   timestamppb.New(exitedAt),
	}, nil
}

// Connect lets containerd query our pid (the shim's pid).
func (s *adoptedTaskService) Connect(_ context.Context, _ *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	s.mu.Lock()
	taskPid := s.qemuPid
	s.mu.Unlock()
	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: uint32(taskPid),
	}, nil
}

// Shutdown stops the ttrpc server.
func (s *adoptedTaskService) Shutdown(_ context.Context, _ *taskAPI.ShutdownRequest) (*emptypb.Empty, error) {
	s.logFn("Shutdown received")
	select {
	case <-s.shutdownCh:
	default:
		close(s.shutdownCh)
	}
	return &emptypb.Empty{}, nil
}

// Pids reports the adopted QEMU pid (single-process task).
func (s *adoptedTaskService) Pids(_ context.Context, _ *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	s.mu.Lock()
	pid := s.qemuPid
	s.mu.Unlock()
	if pid == 0 {
		return &taskAPI.PidsResponse{}, nil
	}
	return &taskAPI.PidsResponse{
		Processes: []*taskTypes.ProcessInfo{{Pid: uint32(pid)}},
	}, nil
}

// watchExit polls /proc/<pid> for QEMU exit and signals waiters.
// Polling beats pidfd here because the shim must work across kernel
// versions where pidfd_open isn't available without CGO.
func (s *adoptedTaskService) watchExit() {
	s.mu.Lock()
	pid := s.qemuPid
	s.mu.Unlock()
	for {
		if !pidAlive(pid) {
			s.mu.Lock()
			s.exited = true
			s.exitedAt = time.Now()
			s.exitCode = 0 // we can't know the actual exit code of an adopted process
			waiters := s.waiters
			s.waiters = nil
			s.mu.Unlock()
			s.logFn("watchExit: pid=%d gone, signaling %d waiter(s)", pid, len(waiters))
			for _, ch := range waiters {
				close(ch)
			}
			return
		}
		time.Sleep(time.Second)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// lookupAdoptedQEMUPid reads the cgroup.procs file for the named
// sandbox, finds the QEMU pid (qemu-system-* in /proc/<pid>/comm),
// and returns it. Multiple pids in the cgroup is normal (qemu +
// kvm-nx-lpage-recovery-N kernel thread); we want the userspace one.
func lookupAdoptedQEMUPid(sandboxID string) (int, error) {
	procsPath := filepath.Join(adoptedCgroupRoot, sandboxID, "cgroup.procs")
	data, err := os.ReadFile(procsPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", procsPath, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		pidStr := strings.TrimSpace(line)
		if pidStr == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + pidStr + "/comm")
		if err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(comm)), "qemu-system") {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no qemu-system process in %s", procsPath)
}

// removeAdoptedCgroup attempts to rmdir the surviving cgroup tree
// after QEMU is gone. cgroup v2 requires the directory to be empty.
func removeAdoptedCgroup(qemuPid int) error {
	// Walk adoptedCgroupRoot looking for a cgroup containing the pid.
	entries, err := os.ReadDir(adoptedCgroupRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(adoptedCgroupRoot, e.Name())
		procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
		if err != nil {
			continue
		}
		if !strings.Contains(string(procs), strconv.Itoa(qemuPid)) && len(strings.TrimSpace(string(procs))) > 0 {
			continue
		}
		_ = os.Remove(dir)
	}
	return nil
}

// --- Unimplemented TTRPCTaskService methods. These are present so
// the type satisfies the interface; an adopted task does not need
// containerd-side checkpoint, exec, pty resize, IO redirection, or
// resource updates. Pause/Resume could later trigger QMP stop/cont
// but stock containerd doesn't issue them for kata-style tasks.

func (s *adoptedTaskService) Pause(_ context.Context, _ *taskAPI.PauseRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *adoptedTaskService) Resume(_ context.Context, _ *taskAPI.ResumeRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *adoptedTaskService) Checkpoint(_ context.Context, _ *taskAPI.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errors.New("checkpoint not supported on adopted tasks")
}
func (s *adoptedTaskService) Exec(_ context.Context, _ *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	return nil, errors.New("exec not supported on adopted tasks (use kata-runtime exec via vsock)")
}
func (s *adoptedTaskService) ResizePty(_ context.Context, _ *taskAPI.ResizePtyRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *adoptedTaskService) CloseIO(_ context.Context, _ *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *adoptedTaskService) Update(_ context.Context, _ *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (s *adoptedTaskService) Stats(_ context.Context, _ *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	return &taskAPI.StatsResponse{}, nil
}

// readAdoptedSandboxID extracts the
// `katamaran.io/adopted-sandbox-id` annotation from the OCI bundle
// dir's config.json. Returns "" if absent or unreadable.
func readAdoptedSandboxID(bundleDir string) string {
	if bundleDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return ""
	}
	// Avoid pulling in the full runtime-spec parser; the annotation
	// format is well-known and this is a shim with a small string
	// budget. Look for `"katamaran.io/adopted-sandbox-id":"<value>"`.
	needle := `"` + adoptedSandboxAnnotation + `":"`
	i := strings.Index(string(data), needle)
	if i < 0 {
		return ""
	}
	rest := string(data)[i+len(needle):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}
