// project-service main entry point.
// Serves ProjectService (11 RPCs) and UserService (13 RPCs, V2.1 ADR-205) on
// port 50052 (ADR-113). Registers gRPC health service for liveness probes.
package main

import (
	"fmt"
	codeauditcfg "github.com/codeaudit/go-config"
	"log"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/handler"
	"github.com/codeaudit/services/project-service/internal/idempotency"
	"github.com/codeaudit/services/project-service/internal/repo"
	"github.com/codeaudit/services/project-service/internal/service"
)

func main() {
	// Port 50052 per ADR-113.
	// ADR-137: 端口来自全局配置 ports.project（ADR-113，env 可覆盖）
	gcfg, err := codeauditcfg.Default()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	portN, err := gcfg.Int("ports.project", "CODEAUDIT_PROJECT_PORT")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", portN))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// Shared infrastructure.
	store := repo.NewMemoryStore()
	idm := idempotency.New()

	// Business logic.
	projectSvc := service.NewProjectService(store)
	userSvc := service.NewUserService(store)

	// V2.1 (ADR-205): 注册策略装配（invitation|open|disabled + 邀请码表）。
	// 键缺省时取安全缺省 invitation + 空码表（不 fail-fast：策略键为增量可选项）。
	regMode := "invitation"
	if v, err := gcfg.Str("auth.registration_mode", "CODEAUDIT_REGISTRATION_MODE"); err == nil && v != "" {
		regMode = v
	}
	var inviteCodes []string
	if v, err := gcfg.StrSlice("auth.invite_codes", "CODEAUDIT_INVITE_CODES"); err == nil {
		inviteCodes = v
	}
	userSvc.SetAuthConfig(service.AuthConfig{RegistrationMode: regMode, InviteCodes: inviteCodes})

	// gRPC handlers.
	projectHandler := handler.NewProjectHandler(projectSvc, idm)
	userHandler := handler.NewUserHandler(userSvc, idm)

	// Create gRPC server and register services.
	s := grpc.NewServer()
	v1.RegisterProjectServiceServer(s, projectHandler)
	v1.RegisterUserServiceServer(s, userHandler)

	// Register gRPC health service for liveness probes.
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus("codeaudit.common.v1.ProjectService", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("codeaudit.common.v1.UserService", grpc_health_v1.HealthCheckResponse_SERVING)

	log.Printf("project-service starting on :%d (ADR-113)", portN)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
