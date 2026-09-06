package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	codeauditcfg "github.com/codeaudit/go-config"
	"github.com/segmentio/kafka-go"
)

// 依据: ADR-006 Kafka 主路径
// 依据: codeaudit_common.proto task.completed 事件触发报告生成
type EventConsumer struct {
	reader  *kafka.Reader
	handler EventHandler
}

type EventHandler interface {
	HandleTaskCompleted(ctx context.Context, event *TaskCompletedEvent) error
}

// TaskCompletedEvent - 依据: codeaudit_common.proto task.completed
type TaskCompletedEvent struct {
	TaskID      string `json:"task_id"`
	TaskType    string `json:"task_type"`
	Status      string `json:"status"`
	CompletedAt int64  `json:"completed_at"`
}

func NewEventConsumer(kafkaBrokers []string, topic string, groupID string, handler EventHandler) *EventConsumer {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  kafkaBrokers,
		Topic:    topic,
		GroupID:  groupID,
		MinBytes: 10e3, // 10KB
		MaxBytes: 10e6, // 10MB
	})

	return &EventConsumer{
		reader:  reader,
		handler: handler,
	}
}

// Start starts consuming events - 依据: ADR-006 异步路径
func (c *EventConsumer) Start(ctx context.Context) error {
	log.Println("Starting event consumer...")

	for {
		select {
		case <-ctx.Done():
			log.Println("Event consumer stopped")
			return c.reader.Close()
		default:
			msg, err := c.reader.ReadMessage(ctx)
			if err != nil {
				// ADR-135: 退避重试（此前 continue 热循环——broker 不可用时 CPU 忙转刷错误）
				backoff := time.Duration(cfgConsumerBackoffS()) * time.Second // ADR-137: 全局配置 result.consumer_backoff_s
				log.Printf("Error reading message: %v (backoff %s)", err, backoff)
				select {
				case <-ctx.Done():
					return c.reader.Close()
				case <-time.After(backoff):
				}
				continue
			}

			if err := c.processMessage(ctx, msg); err != nil {
				log.Printf("Error processing message: %v", err)
				// In production, we would send to dead letter queue
				continue
			}
		}
	}
}

func (c *EventConsumer) processMessage(ctx context.Context, msg kafka.Message) error {
	// Extract event type from headers
	eventType := ""
	for _, header := range msg.Headers {
		if header.Key == "event_type" {
			eventType = string(header.Value)
			break
		}
	}

	log.Printf("Processing event: %s", eventType)

	switch eventType {
	case "task.completed":
		return c.handleTaskCompleted(ctx, msg.Value)
	default:
		log.Printf("Unknown event type: %s", eventType)
		return nil
	}
}

func (c *EventConsumer) handleTaskCompleted(ctx context.Context, data []byte) error {
	var event TaskCompletedEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("failed to unmarshal task.completed event: %v", err)
	}

	log.Printf("Handling task.completed event for task %s", event.TaskID)
	return c.handler.HandleTaskCompleted(ctx, &event)
}

// Close closes the Kafka reader
func (c *EventConsumer) Close() error {
	return c.reader.Close()
}

// cfgConsumerBackoffS — ADR-137: 消费错误退避秒数（全局配置 result.consumer_backoff_s）。
func cfgConsumerBackoffS() int {
	cfg, err := codeauditcfg.Default()
	if err != nil {
		panic(fmt.Sprintf("result-service config: %v (ADR-137)", err))
	}
	v, err := cfg.Int("result.consumer_backoff_s")
	if err != nil {
		panic(fmt.Sprintf("result-service config: %v (ADR-137)", err))
	}
	return v
}
