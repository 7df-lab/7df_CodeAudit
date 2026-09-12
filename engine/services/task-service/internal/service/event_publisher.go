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

// buildFindingCreatedEvent — finding.created 消息构造纯函数（可测，R64/D5）。
// 载荷对齐 storage-service eventPayload 消费映射（notification.go）：task_id/
// finding_id/severity/created_by（收件人链首个非空）。此前该 topic 只有消费端零
// 生产者——高危发现站内通知永远不触发（2026-09-11 跨仓审计死契约）。
func buildFindingCreatedEvent(taskID, createdBy string, severity pb.Severity, findingID string) kafka.Message {
	payload, _ := json.Marshal(map[string]any{
		"task_id":     taskID,
		"finding_id":  findingID,
		"severity":    severity.String(),
		"created_by":  createdBy,
	})
	return kafka.Message{
		Topic:   "finding.created",
		Key:     []byte(findingID),
		Value:   payload,
		Headers: []kafka.Header{{Key: "event_type", Value: []byte("finding.created")}},
	}
}

// PublishFindingsCreatedAsync — 任务成功收尾时批量发布高危 finding.created（非致命）。
func (p *TaskEventProducer) PublishFindingsCreatedAsync(taskID, createdBy string, findings []*pb.UnifiedFinding) {
	if p == nil || !p.enabled {
		return
	}
	// 只发高危（消费端 HIGH/CRITICAL 阈值同口径——低危事件纯噪音且消费端必跳过）
	msgs := make([]kafka.Message, 0, len(findings))
	for _, f := range findings {
		if f.GetSeverity() == pb.Severity_SEVERITY_HIGH || f.GetSeverity() == pb.Severity_SEVERITY_CRITICAL {
			msgs = append(msgs, buildFindingCreatedEvent(taskID, createdBy, f.GetSeverity(), f.GetFindingId()))
		}
	}
	if len(msgs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := p.writer.WriteMessages(ctx, msgs...); err != nil {
			log.Printf("[task-events] finding.created publish failed for %s (non-fatal): %v", taskID, err)
		}
	}()
}

// buildTaskEvent — 消息构造纯函数（可测）。
// ADR-212: 消费端按 event_type 头分发（result event_consumer.processMessage），
// 此前不带头→task.created/completed 全部落入 "Unknown event type" 被静默丢弃，
// offset 照常提交（ADR-006 Kafka 主路径自上线即死路径）；载荷字段亦与消费端
// TaskCompletedEvent JSON tag 对齐（补 task_type/completed_at）。
func buildTaskEvent(topic string, task *pb.ScanTask) kafka.Message {
	completedAt := task.GetUpdatedAt().AsTime().Unix()
	if task.GetUpdatedAt() == nil {
		completedAt = task.GetCreatedAt().AsTime().Unix()
	}
	payload, _ := json.Marshal(map[string]any{
		"task_id":      task.GetTaskId(),
		"project_id":   task.GetProjectId(),
		"task_type":    task.GetScanMode().String(),
		"status":       task.GetStatus().String(),
		"created_by":   task.GetCreatedBy(),
		"completed_at": completedAt,
	})
	return kafka.Message{
		Topic: topic,
		Key:   []byte(task.GetTaskId()),
		Value: payload,
		Headers: []kafka.Header{{Key: "event_type", Value: []byte(topic)}},
	}
}

// PublishAsync — 序列化同步（持锁调用点安全），网络发送异步非致命。
func (p *TaskEventProducer) PublishAsync(topic string, task *pb.ScanTask) {
	if p == nil || !p.enabled || task == nil {
		return
	}
	msg := buildTaskEvent(topic, task)
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
