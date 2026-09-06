// storage-service: serves StorageService (6 RPCs) and NotificationService (4 RPCs)
// on port 50055 (ADR-113).
//
// Architecture: 09 §1 — MinIO(文件) + Redis(通知/幂等) + Kafka(消费)，ADR-199 真实接线。
// 存储模式（同 result-service 口径，CODEAUDIT_STORE env 驱动）:
//
//	s3     → 文件=MinIO、通知/幂等=Redis（生产档；S3/Redis env 必配，缺省 fail-fast）
//	memory → 全内存（07 §10 降级档；重启即丢，演示/E2E 用）
//
// Kafka 消费者: broker 就位即真实消费（01 §4.3 五 topic → 通知映射）；
// CODEAUDIT_KAFKA_OPTIONAL=1 跳过（无 broker 环境，ADR-198 同款开关）。
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/storage-service/internal/handler"
	"github.com/codeaudit/services/storage-service/internal/kafka"
	"github.com/codeaudit/services/storage-service/internal/repo"
	"github.com/codeaudit/services/storage-service/internal/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// 依据: ADR-113 端口 50055；值在全局配置 ports.storage（ADR-137，无代码缺省）

func main() {
	gcfg, err := codeauditcfg.Default()
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	portN, err := gcfg.Int("ports.storage", "CODEAUDIT_STORAGE_PORT")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	port := fmt.Sprintf(":%d", portN)

	storeMode := os.Getenv("CODEAUDIT_STORE")
	kafkaOptional := false
	if v := os.Getenv("CODEAUDIT_KAFKA_OPTIONAL"); v == "1" || strings.EqualFold(v, "true") {
		kafkaOptional = true
	}

	// ---- 后端装配（ADR-199）----
	var fileStore repo.FileStore
	var notifStore repo.NotificationStore
	var idemStore repo.IdempotencyStore
	var presigner repo.Presigner

	switch storeMode {
	case "s3", "minio":
		endpoint := os.Getenv("CODEAUDIT_S3_ENDPOINT")
		bucket := os.Getenv("CODEAUDIT_S3_BUCKET")
		if endpoint == "" || bucket == "" {
			log.Fatal("CODEAUDIT_S3_ENDPOINT and CODEAUDIT_S3_BUCKET are required in s3 mode")
		}
		secure := os.Getenv("CODEAUDIT_S3_SECURE") == "1"
		fs, ferr := repo.NewMinioFileStore(endpoint,
			os.Getenv("CODEAUDIT_S3_ACCESS_KEY"), os.Getenv("CODEAUDIT_S3_SECRET_KEY"),
			bucket, secure)
		if ferr != nil {
			log.Fatalf("minio backend: %v", ferr)
		}
		fileStore, presigner = fs, fs

		redisAddr := os.Getenv("CODEAUDIT_REDIS_ADDR")
		if redisAddr == "" {
			log.Fatal("CODEAUDIT_REDIS_ADDR is required in s3 mode (notifications/idempotency)")
		}
		ns, nerr := repo.NewRedisNotificationStore(redisAddr)
		if nerr != nil {
			log.Fatalf("redis backend: %v", nerr)
		}
		notifStore, idemStore = ns, ns
		log.Printf("storage-service: s3 backend (minio=%s bucket=%s redis=%s)", endpoint, bucket, redisAddr)
	default:
		mem := repo.NewMemoryStore()
		fileStore, notifStore, idemStore = mem, mem, mem
		log.Printf("storage-service: memory store mode (07 §10 degradation path; data lost on restart)")
	}

	// ---- business services ----
	storageSvc := service.NewStorageSvc(fileStore)
	storageSvc.SetPresigner(presigner)
	notifSvc := service.NewNotificationSvc(notifStore, idemStore)

	// ---- gRPC handlers ----
	storageHandler := handler.NewStorageHandler(storageSvc)
	notifHandler := handler.NewNotificationHandler(notifSvc)

	// ---- gRPC server ----
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", port, err)
	}

	s := grpc.NewServer()

	// Register StorageService (6 RPCs, codeaudit_common.proto L1066-L1073)
	v1.RegisterStorageServiceServer(s, storageHandler)

	// Register NotificationService (4 RPCs, codeaudit_common.proto L1080-L1085)
	v1.RegisterNotificationServiceServer(s, notifHandler)

	// Register gRPC health service (standard health-check endpoint)
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(s, healthServer)
	healthServer.SetServingStatus("codeaudit.common.v1.StorageService", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("codeaudit.common.v1.NotificationService", grpc_health_v1.HealthCheckResponse_SERVING)
	// ADR-135 组件级标注保留: kafka-consumer 状态按真实配置给出（ADR-199）
	kafkaComponent := "codeaudit.common.v1.kafka-consumer"
	healthServer.SetServingStatus(kafkaComponent, grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	// ---- Kafka consumer（ADR-199 真实现；01 §4.3 五 topic → 通知映射）----
	kafkaBrokers, err := gcfg.StrSlice("result.kafka.brokers", "CODEAUDIT_KAFKA_BROKERS")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	backoffS, err := gcfg.Int("result.consumer_backoff_s", "CODEAUDIT_CONSUMER_BACKOFF_S")
	if err != nil {
		log.Fatalf("load global config: %v", err)
	}
	if kafkaOptional {
		log.Println("storage-service: Kafka consumer skipped (CODEAUDIT_KAFKA_OPTIONAL)")
	} else if len(kafkaBrokers) == 0 || kafkaBrokers[0] == "" {
		log.Println("storage-service: Kafka brokers not configured, consumer disabled (config honest)")
	} else {
		consumer := kafka.NewConsumer(kafkaBrokers, time.Duration(backoffS)*time.Second)
		healthServer.SetServingStatus(kafkaComponent, grpc_health_v1.HealthCheckResponse_SERVING)
		consumerCtx, consumerCancel := context.WithCancel(context.Background())
		defer consumerCancel()
		go func() {
			consumer.Start(consumerCtx, func(topic string, key []byte, value []byte) {
				n, idemKey, skipReason := service.MapEventToNotification(topic, key, value)
				switch {
				case n != nil:
					notifSvc.PublishFromEvent(n, idemKey)
				case skipReason != "":
					log.Printf("[kafka] %s: skipped (%s)", topic, skipReason)
				}
			})
		}()
	}

	log.Printf("storage-service starting on %s", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
