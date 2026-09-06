package main

import (
	"strconv"

	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/result-service/internal/repository"
	"github.com/codeaudit/services/result-service/internal/service"
	"google.golang.org/grpc"
)

func main() {
	// ADR-137: 端口/Kafka/主题来自全局配置（env 可覆盖），代码不留缺省
	gcfg, err := codeauditcfg.Default()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	portN, err := gcfg.Int("ports.result", "CODEAUDIT_RESULT_PORT")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	port := strconv.Itoa(portN)

	// 存储模式: CODEAUDIT_STORE=memory → 内存仓储+Kafka可选（07 §10 降级策略精神，E2E/无PG环境用）
	storeMode := os.Getenv("CODEAUDIT_STORE")

	var findingRepo repository.FindingRepository
	var reportRepo repository.ReportRepository
	kafkaOptional := false

	// ADR-198: CODEAUDIT_KAFKA_OPTIONAL=1/true → 任意存储模式下跳过 Kafka consumer——
	// 真实 PG 存储与 Kafka broker 解耦（无 broker 环境下 consumer 否则会后台无限退避重试）。
	// producer 保持惰性接线（写失败仅记录日志，不阻塞 verdict 回写主链路）。
	if v := os.Getenv("CODEAUDIT_KAFKA_OPTIONAL"); v == "1" || strings.EqualFold(v, "true") {
		kafkaOptional = true
	}

	if storeMode == "memory" {
		memFinding := repository.NewMemoryFindingRepository()
		findingRepo = memFinding
		reportRepo = repository.NewMemoryReportRepository()
		kafkaOptional = true
		log.Println("result-service: memory store mode (07 §10 degradation path)")
	} else {
		// 依据: ADR-111 PostgreSQL 连接串
		pgDSN := os.Getenv("CODEAUDIT_PG_DSN")
		if pgDSN == "" {
			log.Fatal("CODEAUDIT_PG_DSN is required (or set CODEAUDIT_STORE=memory)")
		}
		fr, err := repository.NewPostgresFindingRepository(pgDSN)
		if err != nil {
			log.Fatalf("Failed to create finding repository: %v", err)
		}
		findingRepo = fr
		rr, rerr := repository.NewPostgresReportRepository(fr.DB())
		if rerr != nil {
			log.Fatalf("create report tables: %v", rerr)
		}
		reportRepo = rr
	}

	// 依据: ADR-006 Kafka 配置（值在全局配置 result.kafka，env CODEAUDIT_KAFKA_BROKERS 覆盖）
	brokerList, err := gcfg.StrSlice("result.kafka.brokers", "CODEAUDIT_KAFKA_BROKERS")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	kafkaBrokers := strings.Join(brokerList, ",")
	topicVerdict, err := gcfg.Str("result.kafka.topic_verdict_updated")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	topicTaskCompleted, err := gcfg.Str("result.kafka.topic_task_completed")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}

	// Initialize services
	resultService := service.NewResultServiceImpl(findingRepo)
	reportService := service.NewReportServiceImpl(reportRepo)
	reportService.SetFindingRepository(findingRepo) // 报告内容真实聚合(2026-08-27)
	// ADR-199: 报告归档至 storage（09 §2 行 result→storage；env 未配置=跳过，PG 本体不受影响）
	if storageAddr := os.Getenv("CODEAUDIT_STORAGE_ADDR"); storageAddr != "" {
		reportService.SetStorageAddr(storageAddr)
		resultService.SetStorageAddr(storageAddr)
		log.Printf("result-service: report archiving + export archiving enabled → %s", storageAddr)
	}

	// 依据: ADR-006 Kafka producer for finding.verdict.updated
	// ADR-135: producer 必须接线进 ResultService（此前构造后从未被调用，verdict 事件链断裂）
	eventProducer := service.NewEventProducer(
		strings.Split(kafkaBrokers, ","),
		topicVerdict,
	)
	defer eventProducer.Close()
	resultService.SetEventProducer(eventProducer)

	// 依据: ADR-006 Kafka consumer for task.completed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !kafkaOptional {
		eventConsumer := service.NewEventConsumer(
			strings.Split(kafkaBrokers, ","),
			topicTaskCompleted,
			"result-service",
			reportService,
		)

		// Start consumer in background
		go func() {
			if err := eventConsumer.Start(ctx); err != nil {
				log.Printf("Event consumer error: %v", err)
			}
		}()
	} else {
		log.Println("result-service: Kafka consumer skipped (memory mode / CODEAUDIT_KAFKA_OPTIONAL)")
	}

	// Create gRPC server
	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pb.RegisterResultServiceServer(s, resultService)
	pb.RegisterReportServiceServer(s, reportService)

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Shutting down result-service...")
		cancel()
		s.GracefulStop()
	}()

	log.Printf("Starting result-service on :%s", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
