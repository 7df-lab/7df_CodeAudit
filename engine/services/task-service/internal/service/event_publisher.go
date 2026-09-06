// TaskEventProducer — task 事件发布（ADR-199）。
//
// 依据: 09 §2 通信矩阵「task/result → Kafka 异步 5 topic」的 task 侧承载；
// 01 §4.3 topic 口径 task.created / task.completed。此前 task 侧无生产者，
// storage 通知链（NOTIFICATION_EVENT_TASK_CREATED/COMPLETED/FAILED）无事件可消费。
// 容错: 发布全程非致命（goroutine + 3s 超时，失败仅 WARN 日志）——Kafka 缺席
// 不影响任务主链路（07 §10 降级精神）；CODEAUDIT_KAFKA_OPTIONAL=1 时整体禁用。
package service

import (
	"context"
	"encoding/json"
	"log"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/segmentio/kafka-go"
)

// TaskEventProducer — Kafka 任务事件发布器（nil 安全：未启用时全部 no-op）。
type TaskEventProducer struct {
	writer  *kafka.Writer
	enabled bool
}

// NewTaskEventProducer — brokers 为空即禁用档（诚实降级，不假装在发）。
func NewTaskEventProducer(brokers []string) *TaskEventProducer {
	if len(brokers) == 0 || brokers[0] == "" {
		log.Println("[task-events] disabled (no brokers configured)")
		return &TaskEventProducer{}
	}
	log.Printf("[task-events] enabled: brokers=%v topics=[task.created,task.completed]", brokers)
	return &TaskEventProducer{
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Balancer:               &kafka.LeastBytes{},
			AllowAutoTopicCreation: true, // 与 broker 端 auto.create.topics 一致
			RequiredAcks:           kafka.RequireOne,
			BatchTimeout:           50 * time.Millisecond,
		},
		enabled: true,
	}
}

// PublishAsync — 序列化同步（持锁调用点安全），网络发送异步非致命。
func (p *TaskEventProducer) PublishAsync(topic string, task *pb.ScanTask) {
	if p == nil || !p.enabled || task == nil {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"task_id":    task.GetTaskId(),
		"project_id": task.GetProjectId(),
		"status":     task.GetStatus().String(),
		"created_by": task.GetCreatedBy(),
	})
	if err != nil {
		log.Printf("[task-events] marshal %s/%s: %v", topic, task.GetTaskId(), err)
		return
	}
	msg := kafka.Message{Topic: topic, Key: []byte(task.GetTaskId()), Value: payload}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := p.writer.WriteMessages(ctx, msg); err != nil {
			log.Printf("[task-events] publish %s/%s FAILED: %v", topic, task.GetTaskId(), err)
		}
	}()
}

// Close — 优雅冲刷。
func (p *TaskEventProducer) Close() {
	if p != nil && p.writer != nil {
		_ = p.writer.Close()
	}
}
