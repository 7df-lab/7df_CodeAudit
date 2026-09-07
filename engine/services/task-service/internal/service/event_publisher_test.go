package service

// ADR-212 回归：消费端（result event_consumer）按 event_type 头分发——此前
// 生产消息不带任何头，task.created/completed 全部落入 "Unknown event type"
// 被静默丢弃且 offset 照常提交（ADR-006 Kafka 主路径自上线即死路径）；
// 载荷字段亦须与消费端 TaskCompletedEvent 的 JSON tag 对齐。

import (
	"encoding/json"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBuildTaskEvent_HeaderAndPayloadAligned(t *testing.T) {
	now := time.Unix(1720000000, 0)
	task := &pb.ScanTask{
		TaskId:    "t-1",
		ProjectId: "p-1",
		Status:    pb.TaskStatus_TASK_STATUS_COMPLETED,
		ScanMode:  pb.ScanMode_SCAN_MODE_PARALLEL,
		CreatedBy: "user-a",
		UpdatedAt: timestamppb.New(now),
	}
	msg := buildTaskEvent("task.completed", task)

	var et string
	for _, h := range msg.Headers {
		if h.Key == "event_type" {
			et = string(h.Value)
		}
	}
	if et != "task.completed" {
		t.Fatalf("event_type header missing/wrong: %q", et)
	}

	var payload map[string]any
	if err := json.Unmarshal(msg.Value, &payload); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	for key, want := range map[string]any{
		"task_id": "t-1", "project_id": "p-1", "created_by": "user-a",
		"task_type": "SCAN_MODE_PARALLEL", "status": "TASK_STATUS_COMPLETED",
	} {
		if payload[key] != want {
			t.Fatalf("payload[%s]=%v, want %v", key, payload[key], want)
		}
	}
	if got, ok := payload["completed_at"].(float64); !ok || int64(got) != now.Unix() {
		t.Fatalf("completed_at=%v, want %d", payload["completed_at"], now.Unix())
	}
}
