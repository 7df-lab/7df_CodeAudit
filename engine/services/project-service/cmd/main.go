// project-service main entry point.
// Serves ProjectService (11 RPCs) and UserService (13 RPCs, V2.1 ADR-205) on
// port 50052 (ADR-113). Registers gRPC health service for liveness probes.
package main

import (
	"fmt"
	"github.com/codeaudit/common-go/grpcrecover"
	codeauditcfg "github.com/codeaudit/go-config"
	"log"
	"net"
	"os"

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
	// R72: 种子 admin 默认关闭（生产不得有默认凭据；bootstrap=invitation 注册）；
	// sim/联调经 CODEAUDIT_SEED_ADMIN=true 显式开启。宽松读取，缺键不 fail-fast。
	seedAdmin := false
	if v, err := gcfg.Bool("auth.seed_admin", "CODEAUDIT_SEED_ADMIN"); err == nil {
		seedAdmin = v
	}
	store := repo.NewMemoryStore(seedAdmin)
	var idm idempotency.Store = idempotency.New()

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
	// HA P1-a（ADR-250）：PG 档装配——DSN 空=内存档（现状零变化）；非空=写穿镜像
	// +读回落+幂等外置+黑名单/纪元回放；PG 不可用=fail-loud（不静默内存分叉）。
	if pgDSN := os.Getenv("CODEAUDIT_PROJECT_PG_DSN"); pgDSN != "" {
		mirror, merr := repo.NewPGMirror(pgDSN)
		if merr != nil {
			log.Fatalf("project PG mirror: %v (CODEAUDIT_PROJECT_PG_DSN 已配置但不可用——拒绝静默内存分叉)", merr)
		}
		projectSvc.SetMirror(mirror)
		userSvc.SetMirror(mirror)       // 注入即全量回放黑名单/纪元
		userSvc.SetInviteMirror(mirror) // V2.2 (ADR-255)：邀请码 PG 权威消费+写穿
		// PG 档下 seedAdmin 若空库补种（走镜像写穿，跨副本可见）
		if seedAdmin {
			if users, lerr := mirror.ListUsers(); lerr == nil && len(users) == 0 {
				log.Println("[project-mirror] 空库：seedAdmin 经正常路径补种（写穿）")
				// 内存构造已种 user-001——补写穿即可
				if rec, ok := store.GetUser("user-001"); ok {
					if err := mirror.UpsertUser(rec); err != nil {
						log.Printf("[project-mirror] seed admin 写穿失败: %v", err)
					}
				}
			}
		}
		// 实体全量回放（跨副本已建数据起步可见）
		if projs, lerr := mirror.ListProjects(); lerr == nil {
			for _, rec := range projs {
				if _, ok := store.GetProject(rec.Project.GetProjectId()); !ok {
					store.CreateProject(rec)
				}
			}
		} else {
			log.Printf("[project-mirror] projects 回放失败: %v", lerr)
		}
		if users, lerr := mirror.ListUsers(); lerr == nil {
			for _, rec := range users {
				if _, ok := store.GetUser(rec.User.GetUserId()); !ok {
					store.UpdateUser(rec)
				}
			}
		}
		// V2.2 (ADR-255)：邀请码回放（跨副本已建码起步可见；空库 no-op）
		if invites, lerr := mirror.ListInviteCodes(); lerr == nil {
			for _, rec := range invites {
				store.PutInviteCode(rec)
			}
		} else {
			log.Printf("[project-mirror] invite codes 回放失败: %v", lerr)
		}
		if pgIdm, ierr := idempotency.NewPGStore(mirror.DB()); ierr == nil {
			idm = pgIdm
		} else {
			log.Fatalf("project PG idempotency: %v", ierr)
		}
		log.Printf("[project-mirror] PG 档启用（写穿+读回落+幂等外置）")
	}

	// gRPC handlers.
	projectHandler := handler.NewProjectHandler(projectSvc, idm)
	userHandler := handler.NewUserHandler(userSvc, idm)

	// Create gRPC server and register services.
	s := grpc.NewServer(
		grpc.ChainUnaryInterceptor(grpcrecover.UnaryServerInterceptor()), // ADR-212: panic 杀请求不杀进程
		grpc.ChainStreamInterceptor(grpcrecover.StreamServerInterceptor()),
	)
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
