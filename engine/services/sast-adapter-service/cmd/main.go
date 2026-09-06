package main

import (
	"fmt"
	"log"
	"net"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/sast-adapter-service/internal/handler"
	"google.golang.org/grpc"
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

	s := grpc.NewServer()

	// SASTAdapterService — 实体存储与 fusion 共享（同部署单元，01 §4.2）
	// 依据: codeaudit_common.proto L1022-L1031 + L1047-L1059
	adapterHandler := handler.NewSASTAdapterHandler(resultAddr)

	fusionHandler := handler.NewSASTFusionHandler(resultAddr)
	handler.ShareStore(fusionHandler, adapterHandler)

	pb.RegisterSASTAdapterServiceServer(s, adapterHandler)

	// Register SASTFusionService
	// 依据: codeaudit_common.proto L1047-L1059
	pb.RegisterSASTFusionServiceServer(s, fusionHandler)

	log.Printf("Starting sast-adapter-service on :%d", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
