package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/task-service/internal/reconciler"
	"github.com/codeaudit/services/task-service/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// 依据: ADR-113 端口 50054(gRPC)；值在全局配置 ports.task（ADR-137，无代码缺省）
func main() {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	port, err := cfg.Int("ports.task", "CODEAUDIT_TASK_PORT")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()

	// Register TaskService
	taskService := service.NewTaskService()
	pb.RegisterTaskServiceServer(s, taskService)

	// ADR-199: task 事件发布器（09 §2 task→Kafka 行）；CODEAUDIT_KAFKA_OPTIONAL=1 禁用
	kafkaOptional := os.Getenv("CODEAUDIT_KAFKA_OPTIONAL") == "1"
	if !kafkaOptional {
		brokers, berr := cfg.StrSlice("result.kafka.brokers", "CODEAUDIT_KAFKA_BROKERS")
		if berr != nil {
			log.Fatalf("load global config: %v", berr)
		}
		taskService.SetEventProducer(service.NewTaskEventProducer(brokers))
	} else {
		log.Println("[task-events] skipped (CODEAUDIT_KAFKA_OPTIONAL)")
	}

	// Register health service
	// 依据: 技术决策 "注册 grpc health service"
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus("TaskService", grpc_health_v1.HealthCheckResponse_SERVING)

	// 对账任务接线（ADR-131）：04 §1 "超时对账每天 02:00 全量扫描长时间 RUNNING 任务转 TIMEOUT"；
	// 存储适配器 = TaskServiceImpl（GetRunningTasks/UpdateTaskStatus）。
	// ADR-196: 活跃度探针——AI 交互日志（interaction_dir，与 dsh-runtime-service 同宿主同 CWD 部署）
	// 任一 .ai.log/.sse.log 有更新 mtime 即视为任务活跃，updated_at 陈旧不判死。
	interactionDir, err := cfg.Str("dsh_runtime.sandbox.interaction_dir")
	if err != nil {
		log.Fatalf("load interaction_dir: %v", err)
	}
	rec := reconciler.New(taskService,
		reconciler.WithActivityLookup(func(taskID string) (time.Time, bool) {
			return latestInteractionLogMtime(interactionDir, taskID)
		}),
	)
	rec.Start()

	log.Printf("Starting task-service on :%d", port)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down: stopping reconciler and draining gRPC server")
		rec.Stop()
		healthServer.Shutdown()
		done := make(chan struct{})
		go func() { s.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			s.Stop()
		}
	}()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

// latestInteractionLogMtime returns the newest mtime among a task's AI interaction
// log files (ADR-196). ok=false when neither exists — reconciler falls back to updated_at.
func latestInteractionLogMtime(dir, taskID string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, suffix := range []string{".ai.log", ".sse.log"} {
		fi, err := os.Stat(filepath.Join(dir, taskID+suffix))
		if err != nil {
			continue
		}
		if !found || fi.ModTime().After(latest) {
			latest = fi.ModTime()
			found = true
		}
	}
	return latest, found
}
