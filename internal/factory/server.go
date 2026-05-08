// Package factory implements a gRPC CacheService server that serves
// migrated QEMU VM state to Kata Containers' VM cache protocol.
//
// After a live migration completes, the destination QEMU writes a
// migration-meta.json file. The factory's directory watcher picks it
// up and offers it to the server via OfferVM. The Kata shim then
// connects over the Unix socket, calls GetBaseVM, and adopts the
// already-running VM instead of cold-booting a new one.
package factory

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/maci0/katamaran/internal/factory/cachepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// MigrationState holds the metadata written by the destination QEMU
// process after a successful incoming live migration. The factory
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
}

// Server implements cachepb.CacheServiceServer, serving migrated VMs
// to Kata shims that connect over the factory's Unix socket.
type Server struct {
	cachepb.UnimplementedCacheServiceServer

	mu         sync.Mutex
	cond       *sync.Cond
	queue      []MigrationState
	quit       chan struct{}
	quitOnce   sync.Once
	vmConfig   []byte // JSON-serialized VMConfig for Config() RPC
	agentConfig []byte // JSON-serialized AgentConfig for Config() RPC
}

// NewServer returns a Server ready to accept OfferVM calls and serve
// gRPC requests from Kata shims.
func NewServer() *Server {
	s := &Server{
		quit: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// OfferVM enqueues a migrated VM so the next GetBaseVM caller can
// adopt it. Called by the directory watcher when a new
// migration-meta.json appears.
func (s *Server) OfferVM(state MigrationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, state)
	slog.Info("VM offered to factory", "id", state.ID, "qemu_pid", state.QEMUPid, "queue_depth", len(s.queue))
	s.cond.Signal()
}

// Config returns an empty VM config. The shim obtains its own
// configuration independently; this satisfies the protocol contract.
func (s *Server) Config(ctx context.Context, _ *emptypb.Empty) (*cachepb.GrpcVMConfig, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		if len(s.vmConfig) > 0 {
			cfg := &cachepb.GrpcVMConfig{
				Data:        s.vmConfig,
				AgentConfig: s.agentConfig,
			}
			s.mu.Unlock()
			return cfg, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, status.Errorf(codes.DeadlineExceeded, "waiting for VMConfig: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

// SetConfig sets the VMConfig and AgentConfig returned by Config().
// Called during startup after reading the Kata persist state or config.
func (s *Server) SetConfig(vmConfig, agentConfig []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vmConfig = vmConfig
	s.agentConfig = agentConfig
	s.cond.Broadcast()
}

// GetBaseVM blocks until a migrated VM is available, then returns it
// as a GrpcVM. Each migration state is consumed exactly once.
func (s *Server) GetBaseVM(ctx context.Context, _ *emptypb.Empty) (*cachepb.GrpcVM, error) {
	// Wait in a goroutine so we can also respect ctx cancellation and
	// server shutdown.
	type result struct {
		state MigrationState
		ok    bool
	}
	ch := make(chan result, 1)
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for len(s.queue) == 0 {
			select {
			case <-s.quit:
				ch <- result{ok: false}
				return
			default:
			}
			s.cond.Wait()
			// Re-check quit after waking.
			select {
			case <-s.quit:
				ch <- result{ok: false}
				return
			default:
			}
		}
		state := s.queue[0]
		s.queue = s.queue[1:]
		ch <- result{state: state, ok: true}
	}()

	select {
	case <-ctx.Done():
		// Unblock the waiter so it doesn't leak.
		s.cond.Broadcast()
		return nil, status.Error(codes.Canceled, ctx.Err().Error())
	case r := <-ch:
		if !r.ok {
			return nil, status.Error(codes.Unavailable, "factory shutting down")
		}
		hypervisorBytes := []byte(r.state.HypervisorState)
		slog.Info("Serving VM to shim", "id", r.state.ID, "qemu_pid", r.state.QEMUPid)
		return &cachepb.GrpcVM{
			Id:         r.state.ID,
			Hypervisor: hypervisorBytes,
			ProxyPid:   int64(r.state.VirtiofsdPid),
			Cpu:        r.state.CPU,
			Memory:     r.state.Memory,
		}, nil
	}
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

// Quit signals the server to shut down gracefully. Pending GetBaseVM
// callers are unblocked with an Unavailable error.
func (s *Server) Quit(_ context.Context, _ *emptypb.Empty) (*emptypb.Empty, error) {
	s.quitOnce.Do(func() {
		slog.Info("Quit requested via gRPC")
		close(s.quit)
		s.cond.Broadcast()
	})
	return &emptypb.Empty{}, nil
}

// QuitCh returns a channel that is closed when Quit is called.
// The main entrypoint uses this to trigger a graceful shutdown.
func (s *Server) QuitCh() <-chan struct{} {
	return s.quit
}
