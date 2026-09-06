// 任务执行日志上报（ADR-167）：dsh-runtime 流水线事件同步 task-service AppendTaskLog，
// GUI 任务详情页"执行日志"面板数据源。上报是尽力而为：task-service 不可达时吞错误
// 留 stdout——日志通道故障不得影响分析主链路（07 §10 降级纪律同源）。
package service

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TaskLogFunc — 沙箱/流水线事件出口（level: "info"|"warn"|"error"）。
type TaskLogFunc func(level, msg string)

// taskLogClient — task-service AppendTaskLog 的懒连接客户端（进程级共享）。
type taskLogClient struct {
	mu   sync.Mutex
	conn *grpc.ClientConn
}

var sharedTaskLog taskLogClient

// taskLogAddr — task-service 地址（ADR-137 全局配置，env CODEAUDIT_TASK_ADDR 覆盖；
// 2026-09-05 容器化实测补齐：无 env 覆盖时回落 yaml 的 localhost 即拨自身，
// 沙箱生命周期日志在容器部署下静默丢失）。
func taskLogAddr() string {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		return ""
	}
	v, err := cfg.Str("addresses.task", "CODEAUDIT_TASK_ADDR")
	if err != nil {
		return ""
	}
	return v
}

// emitTaskLog — 尽力而为上报；level 映射见 taskLogLevelProto。
func emitTaskLog(taskID string, level, source, msg string) {
	addr := taskLogAddr()
	if addr == "" || taskID == "" {
		return
	}
	sharedTaskLog.mu.Lock()
	if sharedTaskLog.conn == nil {
		conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			sharedTaskLog.mu.Unlock()
			log.Printf("[dsh-runtime][%s] task-log channel unavailable (%v)", taskID, err)
			return
		}
		sharedTaskLog.conn = conn
	}
	conn := sharedTaskLog.conn
	sharedTaskLog.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // ADR-167 上报宽限
	defer cancel()
	client := pb.NewTaskServiceClient(conn)
	_, err := client.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: fmt.Sprintf("%s-dshlog-%d", taskID, time.Now().UnixNano())},
		TaskId:   taskID,
		Level:    taskLogLevelProto(level),
		Source:   source,
		Message:  msg,
	})
	if err != nil {
		log.Printf("[dsh-runtime][%s] task-log append failed (%s): %v", taskID, source, err)
	}
}

// taskLogLevelProto — 字符串级别 → proto 枚举（未知归 INFO，不虚构级别）。
func taskLogLevelProto(level string) pb.TaskLogLevel {
	switch level {
	case "warn":
		return pb.TaskLogLevel_TASK_LOG_LEVEL_WARN
	case "error":
		return pb.TaskLogLevel_TASK_LOG_LEVEL_ERROR
	default:
		return pb.TaskLogLevel_TASK_LOG_LEVEL_INFO
	}
}

// taskLogSink — 组装沙箱事件钩子（source=sandbox）与流水线事件（source=dsh-runtime）。
func taskLogSink(taskID, source string) TaskLogFunc {
	return func(level, msg string) {
		log.Printf("[dsh-runtime][%s] %s", taskID, msg) // stdout 副本：本地调试与日志通道故障时的兜底
		emitTaskLog(taskID, level, source, msg)
	}
}
