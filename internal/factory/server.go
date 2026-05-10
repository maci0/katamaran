// Package factory implements a gRPC CacheService server that serves
// migrated QEMU VM state to Kata Containers' VM cache protocol.
//
// After a live migration completes, the destination katamaran process
// writes a migration-meta.json file next to the QMP socket. The
// factory's directory watcher picks it up and offers it to the server
// via OfferVM. The Kata shim then connects over the Unix socket, calls
// GetBaseVM, and adopts the already-running VM instead of cold-booting
// a new one.
package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"

	"github.com/maci0/katamaran/internal/factory/cachepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// MigrationState holds the metadata written by the destination
// katamaran process after a successful incoming live migration. The factory
// translates this into the GrpcVM proto that Kata's shim expects.
type MigrationState struct {
	ID              string          `json:"id"`
	QEMUPid         int             `json:"qemu_pid"`
	QMPSocket       string          `json:"qmp_socket"`
	VsockCID        uint32          `json:"vsock_cid"`
	UUID            string          `json:"uuid"`
	VirtiofsdPid    int             `json:"virtiofsd_pid"`
	HypervisorState json.RawMessage `json:"hypervisor_state"`
	CPU             uint32          `json:"cpu"`
	Memory          uint32          `json:"memory"`
	VMConfig        json.RawMessage `json:"vm_config,omitempty"`
	AgentConfig     json.RawMessage `json:"agent_config,omitempty"`
}

// Server implements cachepb.CacheServiceServer, serving migrated VMs
// to Kata shims that connect over the factory's Unix socket.
type Server struct {
	cachepb.UnimplementedCacheServiceServer

	mu                        sync.Mutex
	queue                     []MigrationState
	quit                      chan struct{}
	quitOnce                  sync.Once
	vmConfig                  []byte // JSON-serialized VMConfig for Config() RPC
	agentConfig               []byte // JSON-serialized AgentConfig for Config() RPC
	vmConfigUnavailableLogged bool
}

// NewServer returns a Server ready to accept OfferVM calls and serve
// gRPC requests from Kata shims.
func NewServer() *Server {
	return &Server{
		quit: make(chan struct{}),
	}
}

// OfferVM enqueues a migrated VM so the next GetBaseVM caller can
// adopt it. Called by the directory watcher when a new
// migration-meta.json appears.
func (s *Server) OfferVM(state MigrationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, state)
	slog.Info("VM offered to factory", "id", state.ID, "qemu_pid", state.QEMUPid, "queue_depth", len(s.queue))
}

// Config returns the stored VMConfig + AgentConfig from the most recent
// migration-meta.json. Returns Unavailable until SetConfig has been called,
// so the shim falls back to direct VM creation in that window.
func (s *Server) Config(_ context.Context, _ *emptypb.Empty) (*cachepb.GrpcVMConfig, error) {
	s.mu.Lock()
	if len(s.vmConfig) > 0 {
		vmConfig := bytes.Clone(s.vmConfig)
		agentConfig := bytes.Clone(s.agentConfig)
		s.mu.Unlock()
		return &cachepb.GrpcVMConfig{
			Data:        vmConfig,
			AgentConfig: agentConfig,
		}, nil
	}
	queueDepth := len(s.queue)
	shouldLog := !s.vmConfigUnavailableLogged
	s.vmConfigUnavailableLogged = true
	s.mu.Unlock()
	if shouldLog {
		slog.Warn("Factory VMConfig unavailable; Config RPC callers will fall back to cold VM creation", "queue_depth", queueDepth)
	}
	return nil, status.Errorf(codes.Unavailable, "VMConfig not yet available")
}

// SetConfig sets the VMConfig and AgentConfig returned by Config().
// Called at startup with the Kata sandbox persist.json contents and again
// by the directory watcher whenever a fresh migration-meta.json carries
// VMConfig from the source pod.
func (s *Server) SetConfig(vmConfig, agentConfig []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vmConfig = bytes.Clone(vmConfig)
	s.agentConfig = bytes.Clone(agentConfig)
	s.vmConfigUnavailableLogged = false
}

// GetBaseVM serves the next queued migration state, or returns Unavailable
// when the queue is empty so the kata-shim falls back to cold VM creation.
//
// Why not block: with vm_cache_number=1 in Kata's config, kata-shim calls
// GetBaseVM for EVERY new sandbox, not just adoption pods. Blocking here
// would stall every fresh pod creation until either a migration occurs or
// the shim's CheckRequest deadline trips, breaking the cluster for normal
// workloads.
//
// Race vs. adoption: the reconciler writes migration-meta.json on the
// dest node BEFORE creating the adoption pod, and waits 5s before the
// pod create call. The watcher's 2s poll picks the meta file up and
// calls OfferVM in that window, so by the time the adoption pod's
// kata-shim hits GetBaseVM, the queue is populated.
func (s *Server) GetBaseVM(ctx context.Context, _ *emptypb.Empty) (*cachepb.GrpcVM, error) {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, status.Error(codes.DeadlineExceeded, err.Error())
		}
		return nil, status.Error(codes.Canceled, err.Error())
	}
	s.mu.Lock()
	select {
	case <-s.quit:
		s.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "factory shutting down")
	default:
	}
	if len(s.queue) == 0 {
		s.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "no migrated VM available; shim should cold-create")
	}
	state := s.queue[0]
	s.queue[0] = MigrationState{}
	s.queue = s.queue[1:]
	s.mu.Unlock()

	slog.Info("Serving VM to shim", "id", state.ID, "qemu_pid", state.QEMUPid)
	return &cachepb.GrpcVM{
		Id:         state.ID,
		Hypervisor: state.HypervisorState,
		ProxyPid:   int64(state.VirtiofsdPid),
		Cpu:        state.CPU,
		Memory:     state.Memory,
	}, nil
}

// Status returns the number of VMs currently queued and the server's PID.
func (s *Server) Status(_ context.Context, _ *emptypb.Empty) (*cachepb.GrpcStatus, error) {
	s.mu.Lock()
	n := len(s.queue)
	vms := make([]*cachepb.GrpcVMStatus, n)
	for i, st := range s.queue {
		vms[i] = &cachepb.GrpcVMStatus{
			Pid:    int64(st.QEMUPid),
			Cpu:    st.CPU,
			Memory: st.Memory,
		}
	}
	s.mu.Unlock()
	return &cachepb.GrpcStatus{
		Pid:      int64(os.Getpid()),
		Vmstatus: vms,
	}, nil
}

// Quit signals the server to shut down gracefully. Subsequent GetBaseVM
// calls return Unavailable on the closed quit channel.
func (s *Server) Quit(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	s.quitOnce.Do(func() {
		slog.Info("Quit requested via gRPC")
		close(s.quit)
	})
	return &emptypb.Empty{}, nil
}

// QuitCh returns a channel that is closed when Quit is called.
// The main entrypoint uses this to trigger a graceful shutdown.
func (s *Server) QuitCh() <-chan struct{} {
	return s.quit
}
