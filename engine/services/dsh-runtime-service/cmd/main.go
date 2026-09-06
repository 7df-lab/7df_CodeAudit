package main

import (
	"fmt"
	"log"
	"net"
	"os"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/dsh-runtime-service/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// 依据: ADR-114 端口 50057(gRPC)；值在全局配置 ports.dsh_runtime（ADR-137）
func main() {
	gcfg, err := codeauditcfg.Default()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	portN, err := gcfg.Int("ports.dsh_runtime", "CODEAUDIT_DSH_RUNTIME_PORT")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	port := fmt.Sprintf(":%d", portN)
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()

	// Register DSHRuntimeService
	// 依据: codeaudit_common.proto L956-L973
	agentService := service.NewDSHRuntimeService()
	pb.RegisterDSHRuntimeServiceServer(s, agentService)

	// ADR-210: 孤儿沙箱对账回收——进程死亡/teardown 失败遗留的沙箱由周期任务兜底删除；
	// CODEAUDIT_SANDBOX_RECONCILE=off 可关闭。runner 与请求侧同构（活跃注册表为包级共享）。
	if os.Getenv("CODEAUDIT_SANDBOX_RECONCILE") != "off" {
		service.StartSandboxReconciler()
	}

	// Register CodeAnalysisService（dsh-runtime 内嵌模块，01 §4.2）
	// 依据: codeaudit_common.proto L976-L982
	pb.RegisterCodeAnalysisServiceServer(s, service.NewCodeAnalysisService())

	// Register health service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus("DSHRuntimeService", grpc_health_v1.HealthCheckResponse_SERVING)

	log.Printf("Starting dsh-runtime-service on %s", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
