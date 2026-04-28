package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Script wraps deploy/migrate.sh and exposes it through the Orchestrator
// interface for ad-hoc CLI runs and CI smoke. Production paths
// (dashboard, katamaran-orchestrator, Migration CRD controller) use the
// Native orchestrator instead — Script is kept for backward-compat.
//
// Status fidelity is intentionally low: the script's stdout is a stream of
// human-readable lines, not structured progress events. Watch emits a single
// PhaseSubmitted update on Apply and a terminal Succeeded/Failed update when
// the script exits. Progress updates between the two are not parsed.
//
// Callers wanting per-step phase updates should use the Native orchestrator.
type Script struct {
	// ScriptPath is the absolute path to deploy/migrate.sh. When empty,
	// Apply searches the working directory and /usr/local/bin/migrate.sh.
	ScriptPath string

	mu       sync.Mutex
	inflight map[MigrationID]*scriptRun
}

type scriptRun struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	updates  chan StatusUpdate
	finished chan struct{}
}

// NewScript returns a Script orchestrator using the named script path. Pass
// empty to fall back to the in-image default.
func NewScript(scriptPath string) *Script {
	return &Script{ScriptPath: scriptPath, inflight: map[MigrationID]*scriptRun{}}
}

// Apply renders the script CLI from req, starts the process, and returns a
// fresh MigrationID. The script runs to completion in the background; Watch
// to observe.
func (s *Script) Apply(ctx context.Context, req Request) (MigrationID, error) {
	if err := validate(req); err != nil {
		return "", err
	}
	args, err := s.buildArgs(req)
	if err != nil {
		return "", err
	}

	id := newID()
	runCtx, cancel := context.WithCancel(context.Background()) // detached; Stop() cancels.
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...)
	cmd.Env = append(cmd.Environ(), "KATAMARAN_MIGRATION_ID="+string(id))

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start migrate.sh: %w", err)
	}

	run := &scriptRun{
		cmd:      cmd,
		cancel:   cancel,
		updates:  make(chan StatusUpdate, 4),
		finished: make(chan struct{}),
	}
	s.mu.Lock()
	s.inflight[id] = run
	s.mu.Unlock()

	run.updates <- StatusUpdate{ID: id, Phase: PhaseSubmitted, When: time.Now()}

	go func() {
		defer close(run.finished)
		err := cmd.Wait()
		final := StatusUpdate{ID: id, When: time.Now()}
		if err != nil {
			final.Phase = PhaseFailed
			final.Error = err
		} else {
			final.Phase = PhaseSucceeded
		}
		run.updates <- final
		close(run.updates)
		s.mu.Lock()
		delete(s.inflight, id)
		s.mu.Unlock()
	}()
	return id, nil
}

// Watch returns the channel of status updates for id. Returns ErrUnknownID if
// the migration completed before Watch was called.
func (s *Script) Watch(_ context.Context, id MigrationID) (<-chan StatusUpdate, error) {
	s.mu.Lock()
	run, ok := s.inflight[id]
	s.mu.Unlock()
	if !ok {
		return nil, ErrUnknownID
	}
	return run.updates, nil
}

// Stop cancels the migrate.sh subprocess for id. The watcher will receive a
// PhaseFailed update once the process exits.
func (s *Script) Stop(_ context.Context, id MigrationID) error {
	s.mu.Lock()
	run, ok := s.inflight[id]
	s.mu.Unlock()
	if !ok {
		return ErrUnknownID
	}
	run.cancel()
	return nil
}

// ErrUnknownID is returned by Watch/Stop for a migration ID that the
// orchestrator does not know about (already finished + cleaned, or never
// started).
var ErrUnknownID = errors.New("unknown migration ID")

// BuildArgs translates a Request into the deploy/migrate.sh CLI. Mirrors the
// argument layout that internal/dashboard/migrate.go assembles today, so the
// dashboard can switch over with no behavioural change.
func (s *Script) BuildArgs(req Request) ([]string, error) {
	if err := Validate(req); err != nil {
		return nil, err
	}
	return s.buildArgs(req)
}

func (s *Script) buildArgs(req Request) ([]string, error) {
	scriptPath := s.ScriptPath
	if scriptPath == "" {
		scriptPath = "deploy/migrate.sh"
	}
	args := []string{
		scriptPath,
		"--source-node", req.SourceNode,
		"--dest-node", req.DestNode,
		"--dest-ip", req.DestIP,
		"--image", req.Image,
	}
	if req.SourcePod != nil {
		args = append(args, "--pod-name", req.SourcePod.Name, "--pod-namespace", req.SourcePod.Namespace)
		// Advanced overrides in pod mode: a non-empty value replaces the
		// resolver's auto-derived default for that field. Empty = use the
		// auto-derived value.
		if req.SourceQMP != "" {
			args = append(args, "--qmp-source", req.SourceQMP)
		}
		if req.VMIP != "" {
			args = append(args, "--vm-ip", req.VMIP)
		}
	} else {
		args = append(args, "--qmp-source", req.SourceQMP, "--vm-ip", req.VMIP)
	}
	if req.DestPod != nil {
		args = append(args, "--dest-pod-name", req.DestPod.Name, "--dest-pod-namespace", req.DestPod.Namespace)
	}
	if req.DestQMP != "" {
		args = append(args, "--qmp-dest", req.DestQMP)
	}
	if req.TapIface != "" {
		args = append(args, "--tap", req.TapIface)
	}
	if req.TapNetns != "" {
		args = append(args, "--tap-netns", req.TapNetns)
	}
	if req.SharedStorage {
		args = append(args, "--shared-storage")
	}
	if req.ReplayCmdline {
		args = append(args, "--replay-cmdline")
	}
	if req.TunnelMode != "" {
		args = append(args, "--tunnel-mode", req.TunnelMode)
	}
	if req.DowntimeMS > 0 {
		args = append(args, "--downtime", strconv.Itoa(req.DowntimeMS))
	}
	if req.AutoDowntime {
		args = append(args, "--auto-downtime")
	}
	if req.MultifdChannels > 0 {
		args = append(args, "--multifd-channels", strconv.Itoa(req.MultifdChannels))
	}
	if req.LogLevel != "" {
		args = append(args, "--log-level", req.LogLevel)
	}
	if req.LogFormat != "" {
		args = append(args, "--log-format", req.LogFormat)
	}
	if req.KubectlContext != "" {
		args = append(args, "--context", req.KubectlContext)
	}
	return args, nil
}

// Validate checks a Request for required fields and mode consistency. Exposed
// so callers (e.g. the dashboard's HTTP handler) can pre-validate before
// calling Apply or BuildArgs.
func Validate(req Request) error {
	return validate(req)
}

func validate(req Request) error {
	if req.SourceNode == "" || req.DestNode == "" {
		return errors.New("SourceNode and DestNode are required")
	}
	if req.SourceNode == req.DestNode {
		return errors.New("SourceNode and DestNode must differ")
	}
	if req.DestIP == "" {
		return errors.New("DestIP is required")
	}
	if req.Image == "" {
		return errors.New("Image is required")
	}
	if req.SourcePod == nil {
		// Legacy mode: SourceQMP + VMIP required.
		if req.SourceQMP == "" || req.VMIP == "" {
			return errors.New("either SourcePod or (SourceQMP + VMIP) is required")
		}
	} else if req.SourcePod.Name == "" || req.SourcePod.Namespace == "" {
		return errors.New("SourcePod requires both Name and Namespace")
	}
	if req.DestPod != nil && (req.DestPod.Name == "" || req.DestPod.Namespace == "") {
		return errors.New("DestPod requires both Name and Namespace")
	}
	return nil
}

func newID() MigrationID {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return MigrationID(hex.EncodeToString(b[:]))
}
