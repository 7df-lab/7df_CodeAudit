package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/segmentio/kafka-go"
)

// 依据: ADR-006 Kafka 主路径
// 依据: codeaudit_common.proto finding.verdict.updated 事件
// writerInterface 抽象 kafka.Writer 便于测试注入(ADR-1xx: 依赖倒置)
type writerInterface interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

type EventProducer struct {
	writer writerInterface
	topic  string
}

func NewEventProducer(kafkaBrokers []string, topic string) *EventProducer {
	writer := &kafka.Writer{
		Addr:     kafka.TCP(kafkaBrokers...),
		Topic:    topic,
		Balancer: &kafka.LeastBytes{},
	}

	return &EventProducer{
		writer: writer,
		topic:  topic,
	}
}

// FindingVerdictUpdatedEvent - 依据: codeaudit_common.proto finding.verdict.updated
type FindingVerdictUpdatedEvent struct {
	FindingID  string `json:"finding_id"`
	TaskID     string `json:"task_id"`
	OldVerdict string `json:"old_verdict"`
	NewVerdict string `json:"new_verdict"`
	UpdatedBy  string `json:"updated_by"`
	UpdatedAt  int64  `json:"updated_at"`
}

// PublishVerdictUpdated - 依据: ADR-006 异步路径
func (p *EventProducer) PublishVerdictUpdated(ctx context.Context, finding *model.Finding, oldVerdict string, updatedBy string) error {
	event := FindingVerdictUpdatedEvent{
		FindingID:  finding.ID,
		TaskID:     finding.TaskID,
		OldVerdict: oldVerdict,
		NewVerdict: finding.Verdict,
		UpdatedBy:  updatedBy,
		UpdatedAt:  time.Now().Unix(),
	}

	eventBytes, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %v", err)
	}

	msg := kafka.Message{
		Key:   []byte(finding.ID),
		Value: eventBytes,
		Headers: []kafka.Header{
			{Key: "event_type", Value: []byte("finding.verdict.updated")},
			{Key: "correlation_id", Value: []byte(finding.ID)},
		},
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("failed to publish event: %v", err)
	}

	log.Printf("Published finding.verdict.updated event for finding %s", finding.ID)
	return nil
}

// Close closes the Kafka writer
func (p *EventProducer) Close() error {
	return p.writer.Close()
}
