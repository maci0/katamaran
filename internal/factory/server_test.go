package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maci0/katamaran/internal/factory/cachepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestServerConfigUnavailableUntilSet(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	if _, err := srv.Config(context.Background(), &emptypb.Empty{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Config before SetConfig status = %v, want %v; err=%v", status.Code(err), codes.Unavailable, err)
	}

	vmConfig := []byte(`{"HypervisorType":"qemu","HypervisorConfig":{"machine":"q35"}}`)
	agentConfig := []byte(`{"Debug":true}`)
	srv.SetConfig(vmConfig, agentConfig)

	got, err := srv.Config(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Config after SetConfig: %v", err)
	}
	if !bytes.Equal(got.Data, vmConfig) {
		t.Fatalf("Config.Data = %s, want %s", got.Data, vmConfig)
	}
	if !bytes.Equal(got.AgentConfig, agentConfig) {
		t.Fatalf("Config.AgentConfig = %s, want %s", got.AgentConfig, agentConfig)
	}
}

func TestServerConfigCopiesConfigBytes(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	vmConfig := []byte(`{"HypervisorType":"qemu"}`)
	agentConfig := []byte(`{"Debug":true}`)
	wantVMConfig := bytes.Clone(vmConfig)
	wantAgentConfig := bytes.Clone(agentConfig)

	srv.SetConfig(vmConfig, agentConfig)
	vmConfig[0] = '['
	agentConfig[0] = '['

	got, err := srv.Config(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Config after SetConfig: %v", err)
	}
	if !bytes.Equal(got.Data, wantVMConfig) {
		t.Fatalf("Config.Data = %s, want %s", got.Data, wantVMConfig)
	}
	if !bytes.Equal(got.AgentConfig, wantAgentConfig) {
		t.Fatalf("Config.AgentConfig = %s, want %s", got.AgentConfig, wantAgentConfig)
	}

	got.Data[0] = '['
	got.AgentConfig[0] = '['
	got, err = srv.Config(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Config after response mutation: %v", err)
	}
	if !bytes.Equal(got.Data, wantVMConfig) {
		t.Fatalf("Config.Data after response mutation = %s, want %s", got.Data, wantVMConfig)
	}
	if !bytes.Equal(got.AgentConfig, wantAgentConfig) {
		t.Fatalf("Config.AgentConfig after response mutation = %s, want %s", got.AgentConfig, wantAgentConfig)
	}
}

func TestServerGetBaseVMConsumesFIFOAndUpdatesStatus(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	first := MigrationState{
		ID:              "mig-first",
		QEMUPid:         101,
		VirtiofsdPid:    201,
		HypervisorState: json.RawMessage(`{"pid":101}`),
		CPU:             2,
		Memory:          1024,
	}
	second := MigrationState{
		ID:              "mig-second",
		QEMUPid:         102,
		VirtiofsdPid:    202,
		HypervisorState: json.RawMessage(`{"pid":102}`),
		CPU:             4,
		Memory:          2048,
	}
	srv.OfferVM(first)
	srv.OfferVM(second)

	assertStatus(t, srv, []wantVMStatus{
		{pid: 101, cpu: 2, memory: 1024},
		{pid: 102, cpu: 4, memory: 2048},
	})

	got, err := srv.GetBaseVM(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetBaseVM first: %v", err)
	}
	assertVM(t, got, first)
	assertStatus(t, srv, []wantVMStatus{{pid: 102, cpu: 4, memory: 2048}})

	got, err = srv.GetBaseVM(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetBaseVM second: %v", err)
	}
	assertVM(t, got, second)
	assertStatus(t, srv, nil)
}

func TestServerGetBaseVMHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := srv.GetBaseVM(ctx, &emptypb.Empty{}); status.Code(err) != codes.Canceled {
		t.Fatalf("GetBaseVM canceled status = %v, want %v; err=%v", status.Code(err), codes.Canceled, err)
	}
}

func TestServerGetBaseVMHonorsDeadline(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	if _, err := srv.GetBaseVM(ctx, &emptypb.Empty{}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("GetBaseVM deadline status = %v, want %v; err=%v", status.Code(err), codes.DeadlineExceeded, err)
	}
}

// TestServerGetBaseVMNonBlockingWhenQueueEmpty locks the contract that
// GetBaseVM does NOT block when the migration queue is empty — it must
// return Unavailable so kata-shim falls back to cold VM creation. Live
// regression test: blocking here breaks every fresh sandbox creation
// when Kata is configured with vm_cache_number=1 + factory endpoint.
func TestServerGetBaseVMNonBlockingWhenQueueEmpty(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := srv.GetBaseVM(context.Background(), &emptypb.Empty{})
		if status.Code(err) != codes.Unavailable {
			t.Errorf("GetBaseVM with empty queue status = %v, want %v; err=%v", status.Code(err), codes.Unavailable, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("GetBaseVM blocked when queue empty; should return Unavailable immediately")
	}
}

func TestServerQuitReturnsUnavailableOnNextCall(t *testing.T) {
	t.Parallel()

	srv := NewServer()
	if _, err := srv.Quit(context.Background(), &emptypb.Empty{}); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	select {
	case <-srv.QuitCh():
	default:
		t.Fatal("QuitCh was not closed")
	}
	if _, err := srv.GetBaseVM(context.Background(), &emptypb.Empty{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("GetBaseVM after Quit status = %v, want %v; err=%v", status.Code(err), codes.Unavailable, err)
	}
}

type wantVMStatus struct {
	pid    int64
	cpu    uint32
	memory uint32
}

func assertStatus(t *testing.T, srv *Server, want []wantVMStatus) {
	t.Helper()

	got, err := srv.Status(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(got.Vmstatus) != len(want) {
		t.Fatalf("Status Vmstatus len = %d, want %d: %+v", len(got.Vmstatus), len(want), got.Vmstatus)
	}
	for i, w := range want {
		vm := got.Vmstatus[i]
		if vm.Pid != w.pid || vm.Cpu != w.cpu || vm.Memory != w.memory {
			t.Fatalf("Status Vmstatus[%d] = {pid:%d cpu:%d memory:%d}, want {pid:%d cpu:%d memory:%d}",
				i, vm.Pid, vm.Cpu, vm.Memory, w.pid, w.cpu, w.memory)
		}
	}
}

func assertVM(t *testing.T, got *cachepb.GrpcVM, want MigrationState) {
	t.Helper()

	if got.Id != want.ID {
		t.Fatalf("GrpcVM.Id = %q, want %q", got.Id, want.ID)
	}
	if got.ProxyPid != int64(want.VirtiofsdPid) {
		t.Fatalf("GrpcVM.ProxyPid = %d, want %d", got.ProxyPid, want.VirtiofsdPid)
	}
	if got.Cpu != want.CPU || got.Memory != want.Memory {
		t.Fatalf("GrpcVM sizing = cpu:%d memory:%d, want cpu:%d memory:%d", got.Cpu, got.Memory, want.CPU, want.Memory)
	}
	if !bytes.Equal(got.Hypervisor, []byte(want.HypervisorState)) {
		t.Fatalf("GrpcVM.Hypervisor = %s, want %s", got.Hypervisor, want.HypervisorState)
	}
}
