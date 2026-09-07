// Package grpcrecover — gRPC panic 恢复拦截器。
//
// 依据/动机（ADR-212）：grpc-go 无内建 recover——此前 storage minio.go 等
// 处的 panic 依赖一句不存在的"gRPC recover 语义兜底"，任一 handler panic
// 即杀整个进程。本拦截器把 panic 语义从"杀进程"收敛为"杀请求"
// （codes.Internal，07 §9 错误口径），快速失败语义保留。
package grpcrecover

import (
	"context"
	"fmt"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor — 一元 RPC panic → codes.Internal。
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Errorf(codes.Internal, "internal panic: %v", r)
				logPanic(info.FullMethod, r)
			}
		}()
		return handler(ctx, req)
	}
}

// StreamServerInterceptor — 流式 RPC panic → codes.Internal（写侧已发出的
// 帧不受影响；收侧 panic 转为流错误终止该条流，进程存活）。
func StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Errorf(codes.Internal, "internal panic: %v", r)
				logPanic(info.FullMethod, r)
			}
		}()
		return handler(srv, ss)
	}
}

func logPanic(method string, r any) {
	fmt.Printf("[grpc-recover] panic in %s: %v\n%s\n", method, r, debug.Stack())
}
