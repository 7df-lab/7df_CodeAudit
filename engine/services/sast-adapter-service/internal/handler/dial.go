// Package handler — gRPC 拨号辅助（供 persistToResult 使用；测试可注入）。
package handler

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// grpcDial — 服务间 gRPC 直连（09 §2 同步通信口径；mTLS 在正式部署启用）。
func grpcDial(addr string) (*grpc.ClientConn, error) {
	return grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithTimeout(10*time.Second), // 09 §2 行 sast-adapter→result 10s
	)
}
