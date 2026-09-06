package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codeaudit/services/gateway-service/internal/config"
	"github.com/codeaudit/services/gateway-service/internal/handler"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
)

func main() {
	log.Println("Starting gateway-service...")

	// Load configuration - ADR-137：全局配置文件 + env 覆盖，缺键 fail-fast
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}

	// Validate required configuration
	// 依据: 09 §安全 空密钥=任何伪造 token 可通过 HMAC 校验 → fail-fast（ADR-132）
	if cfg.JWTSecret == "" {
		log.Fatal(cfg.JWTSecretName() + " is not set: refusing to start (empty secret would accept forged tokens)")
	}

	// ---- 公共路由（免认证免限流）----
	// /health 豁免 JWT+限流：compose healthcheck 与 LB 探活依赖它（ADR-132；
	// 此前被中间件包住导致容器永远 unhealthy）。
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/health", handler.HealthHandler)

	// ---- 业务路由 /v1/*：真实 JSON↔gRPC 转码（03 §1.1，ADR-132）----
	// 此前是 HTTP 反代直打 gRPC 端口（协议错配必失败），已删除。
	transcoder := handler.NewTranscoder(handler.BackendAddrs{
		ProjectAddr:     cfg.ProjectServiceAddr,
		TaskAddr:        cfg.TaskServiceAddr,
		ResultAddr:      cfg.ResultServiceAddr,
		StorageAddr:     cfg.StorageAddr,
		SastAdapterAddr: cfg.SastAdapterAddr,
		DSHRuntimeAddr:    cfg.DSHRuntimeAddr,
		CallTimeoutS:    cfg.GRPCCallTimeoutS,
	})
	defer transcoder.Close()

	apiMux := http.NewServeMux()
	// 代码压缩包上传（ADR-200: gateway 不落盘，原始压缩包流式直传 storage；JWT 保护链内）
	handler.UploadsDir = cfg.UploadsDir // ADR-195 遗留读路径（source-file 解析历史上传件）
	handler.ReposDir = cfg.ReposDir     // ADR-195: source-file 端点 repo 流任务源根
	apiMux.HandleFunc("/v1/uploads/archive", transcoder.UploadArchive)
	apiMux.Handle("/v1/", transcoder.Handler())

	// Middleware chains
	// 1. Logging (outermost)
	// 2. Rate limiting - 依据: 07 §7 限流（XFF 仅可信代理时信任，ADR-132）
	// 3. JWT - 依据: 03 §4
	// TP12-T3 回归修复：/v1/auth/* 必须免 JWT（登录是令牌的来源），但仍限流
	rateLimited := func(next http.Handler) http.Handler {
		return middleware.LoggingMiddleware(middleware.RateLimitMiddleware(cfg.TrustProxy, cfg.RateLimitPerMin, next))
	}
	protected := rateLimited(middleware.JWTMiddleware(cfg.JWTSecret, apiMux))

	// /v1/auth/* 免认证链；/health 公共；其余 /v1/* 走 JWT 保护链
	authMux := http.NewServeMux()
	authMux.Handle("/v1/", transcoder.Handler())

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			publicMux.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1/auth/"):
			rateLimited(authMux).ServeHTTP(w, r)
		default:
			protected.ServeHTTP(w, r)
		}
	})

	// Create server
	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: root,
	}

	// Start server in a goroutine
	go func() {
		log.Printf("Gateway service listening on :%s", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	// Wait for interrupt signal; 真正优雅停机：server.Shutdown 排空在途请求（ADR-132）
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down gateway service (draining in-flight requests)...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownGraceS)*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	log.Println("Gateway service stopped")
}
