package main

import (
	"fmt"
	"log"
	"net"

	"github.com/codeaudit/common-go/grpcrecover"
	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/sast-adapter-service/internal/handler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	// ADR-137: 端口与 result 地址来自全局配置（env 可覆盖），代码不留缺省
	gcfg, err := codeauditcfg.Default()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	port, err := gcfg.Int("ports.sast_adapter", "CODEAUDIT_SAST_ADAPTER_PORT")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	// result-service 地址：09 §2 行 sast-adapter→result 落盘 finding
	resultAddr, err := gcfg.Str("addresses.result", "CODEAUDIT_RESULT_ADDR")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}

	// Create gRPC server
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer(
		grpc.ChainUnaryInterceptor(grpcrecover.UnaryServerInterceptor()), // ADR-212: panic 杀请求不杀进程
		grpc.ChainStreamInterceptor(grpcrecover.StreamServerInterceptor()),
	)

	// SASTAdapterService — HA P2（ADR-250 Q7a 拆桥）：ShareStore 同进程桥已删
	// （多副本下 fusion/adapter 独立扩缩；解析主路径=result-service 权威存储）。
	adapterHandler := handler.NewSASTAdapterHandler(resultAddr)

	fusionHandler := handler.NewSASTFusionHandler(resultAddr)

	pb.RegisterSASTAdapterServiceServer(s, adapterHandler)

	// Register SASTFusionService
	// 依据: codeaudit_common.proto L1047-L1059
	pb.RegisterSASTFusionServiceServer(s, fusionHandler)

	// P0 探针补面：gRPC health 注册
	// （此前缺失——compose 探针只测 TCP；消费面=k8s 原生 grpc probe+gateway /health 聚合）。
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus("SASTAdapterService", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("SASTFusionService", grpc_health_v1.HealthCheckResponse_SERVING)

	log.Printf("Starting sast-adapter-service on :%d", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
