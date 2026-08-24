package main

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// TestRecoverUnaryInterceptor_PanicBecomesInternal pins the panic
// containment of the factory's gRPC interceptor: the factory is a
// long-lived node daemon, so a panicking handler must surface as a clean
// codes.Internal to the peer instead of tearing down the process (which
// would also kill the watcher goroutine feeding it).
func TestRecoverUnaryInterceptor_PanicBecomesInternal(t *testing.T) {
	t.Parallel()

	info := &grpc.UnaryServerInfo{FullMethod: "/cachepb.CacheService/GetBaseVM"}
	resp, err := recoverUnaryInterceptor(context.Background(), &emptypb.Empty{}, info,
		func(context.Context, any) (any, error) { panic("synthetic handler panic") })

	if resp != nil {
		t.Fatalf("resp = %v, want nil on panic", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("status = %v, want %v; err=%v", status.Code(err), codes.Internal, err)
	}
	if errText := status.Convert(err).Message(); errText != "internal server error" {
		t.Fatalf("message = %q, want sanitized generic message (panic detail must not leak)", errText)
	}
}

// The interceptor must be transparent for healthy handlers: response and
// error pass through unchanged.
func TestRecoverUnaryInterceptor_PassesThrough(t *testing.T) {
	t.Parallel()

	wantResp := &emptypb.Empty{}
	wantErr := errors.New("handler error")
	gotResp, gotErr := recoverUnaryInterceptor(context.Background(), &emptypb.Empty{},
		&grpc.UnaryServerInfo{FullMethod: "/cachepb.CacheService/Status"},
		func(context.Context, any) (any, error) { return wantResp, wantErr })

	if gotResp != wantResp || !errors.Is(gotErr, wantErr) {
		t.Fatalf("pass-through broken: resp=%v err=%v, want unchanged handler results", gotResp, gotErr)
	}
}
