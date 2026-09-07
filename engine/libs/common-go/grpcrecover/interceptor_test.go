package grpcrecover

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ADR-212：panic 必须被拦为 codes.Internal，进程语义存活。
func TestUnaryInterceptorRecoversPanic(t *testing.T) {
	ic := UnaryServerInterceptor()
	_, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test/Svc/Boom"},
		func(ctx context.Context, req any) (any, error) {
			panic("nil map write")
		})
	if err == nil {
		t.Fatal("panic must surface as error")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", status.Code(err))
	}
}

func TestUnaryInterceptorPassthrough(t *testing.T) {
	ic := UnaryServerInterceptor()
	wantErr := errors.New("normal")
	_, err := ic(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test/Svc/Ok"},
		func(ctx context.Context, req any) (any, error) {
			return "resp", wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("handler error must pass through unchanged: %v", err)
	}
}

type fakeStream struct{ grpc.ServerStream }

func TestStreamInterceptorRecoversPanic(t *testing.T) {
	ic := StreamServerInterceptor()
	err := ic(nil, &fakeStream{},
		&grpc.StreamServerInfo{FullMethod: "/test/Svc/StreamBoom"},
		func(srv any, ss grpc.ServerStream) error {
			panic("stream boom")
		})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", status.Code(err))
	}
}
