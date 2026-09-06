package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/codeaudit/services/result-service/internal/model"
	"github.com/segmentio/kafka-go"
)

var assertAnError = errors.New("assertion error")

// MockKafkaWriter is a mock Kafka writer
type MockKafkaWriter struct {
	WriteMessagesFn func(ctx context.Context, msgs ...kafka.Message) error
	CloseFn         func() error
}

func (m *MockKafkaWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	if m.WriteMessagesFn != nil {
		return m.WriteMessagesFn(ctx, msgs...)
	}
	return nil
}

func (m *MockKafkaWriter) Close() error {
	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

// TestPublishVerdictUpdated - 依据: ADR-006 + TP08-T3
func TestPublishVerdictUpdated(t *testing.T) {
	var capturedMsg kafka.Message
	mockWriter := &MockKafkaWriter{
		WriteMessagesFn: func(ctx context.Context, msgs ...kafka.Message) error {
			capturedMsg = msgs[0]
			return nil
		},
	}
	producer := &EventProducer{
		writer: mockWriter,
		topic:  "finding.verdict.updated",
	}

	finding := &model.Finding{
		ID:        "finding-1",
		TaskID:    "task-1",
		Verdict:   "AI_VERDICT_TRUE_POSITIVE",
		UpdatedAt: time.Now(),
	}

	err := producer.PublishVerdictUpdated(context.Background(), finding, "AI_VERDICT_UNSPECIFIED", "user-1")

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	var event FindingVerdictUpdatedEvent
	err = json.Unmarshal(capturedMsg.Value, &event)
	if err != nil {
		t.Errorf("Expected no error unmarshaling event, got %v", err)
	}

	if event.FindingID != "finding-1" {
		t.Errorf("Expected finding ID 'finding-1', got '%s'", event.FindingID)
	}
	if event.TaskID != "task-1" {
		t.Errorf("Expected task ID 'task-1', got '%s'", event.TaskID)
	}
	if event.OldVerdict != "AI_VERDICT_UNSPECIFIED" {
		t.Errorf("Expected old verdict 'AI_VERDICT_UNSPECIFIED', got '%s'", event.OldVerdict)
	}
	if event.NewVerdict != "AI_VERDICT_TRUE_POSITIVE" {
		t.Errorf("Expected new verdict 'AI_VERDICT_TRUE_POSITIVE', got '%s'", event.NewVerdict)
	}
	if event.UpdatedBy != "user-1" {
		t.Errorf("Expected updated by 'user-1', got '%s'", event.UpdatedBy)
	}
	if event.UpdatedAt == 0 {
		t.Error("Expected updated at to be set")
	}
}

// TestPublishVerdictUpdatedError - 依据: ADR-006 error handling
func TestPublishVerdictUpdatedError(t *testing.T) {
	mockWriter := &MockKafkaWriter{
		WriteMessagesFn: func(ctx context.Context, msgs ...kafka.Message) error {
			return assertAnError
		},
	}
	producer := &EventProducer{
		writer: mockWriter,
		topic:  "finding.verdict.updated",
	}

	finding := &model.Finding{
		ID:        "finding-1",
		TaskID:    "task-1",
		Verdict:   "AI_VERDICT_TRUE_POSITIVE",
		UpdatedAt: time.Now(),
	}

	err := producer.PublishVerdictUpdated(context.Background(), finding, "AI_VERDICT_UNSPECIFIED", "user-1")

	if err == nil {
		t.Error("Expected error, got nil")
	}
}
