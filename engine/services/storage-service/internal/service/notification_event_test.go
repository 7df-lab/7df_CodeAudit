// MapEventToNotification 单元测试（ADR-199）：事件→通知映射 + 收件人解析 + 诚实跳过。
package service

import (
	"encoding/json"
	"testing"

	v1 "github.com/codeaudit/proto-gen"
)

func mapEvt(t *testing.T, topic string, payload map[string]string, key string) (*v1.Notification, string, string) {
	t.Helper()
	b, _ := json.Marshal(payload)
	return MapEventToNotification(topic, []byte(key), b)
}

func TestMapTaskCreated(t *testing.T) {
	n, idemKey, skip := mapEvt(t, "task.created", map[string]string{
		"task_id": "t-1", "project_id": "p-1", "status": "TASK_STATUS_CREATED", "created_by": "u-1",
	}, "t-1")
	if n == nil {
		t.Fatalf("expected notification, skipped: %s", skip)
	}
	if n.Event != v1.NotificationEvent_NOTIFICATION_EVENT_TASK_CREATED || n.UserId != "u-1" {
		t.Fatalf("wrong mapping: event=%v user=%s", n.Event, n.UserId)
	}
	if n.Type != v1.NotificationType_NOTIFICATION_TYPE_IN_APP {
		t.Fatalf("type must be IN_APP, got %v", n.Type)
	}
	if idemKey != "event:task.created:t-1" {
		t.Fatalf("idem key = %q", idemKey)
	}
}

func TestMapTaskCompletedVsFailed(t *testing.T) {
	n, _, _ := mapEvt(t, "task.completed", map[string]string{"task_id": "t-1", "status": "TASK_STATUS_COMPLETED", "created_by": "u-1"}, "k")
	if n.Event != v1.NotificationEvent_NOTIFICATION_EVENT_TASK_COMPLETED {
		t.Fatalf("want COMPLETED, got %v", n.Event)
	}
	n, _, _ = mapEvt(t, "task.completed", map[string]string{"task_id": "t-1", "status": "TASK_STATUS_DEAD", "created_by": "u-1"}, "k")
	if n.Event != v1.NotificationEvent_NOTIFICATION_EVENT_TASK_FAILED {
		t.Fatalf("want FAILED for DEAD, got %v", n.Event)
	}
}

func TestMapFindingCreatedSeverityGate(t *testing.T) {
	n, _, skip := mapEvt(t, "finding.created", map[string]string{"finding_id": "f-1", "severity": "SEVERITY_LOW", "created_by": "u-1"}, "k")
	if n != nil {
		t.Fatal("LOW severity must not notify")
	}
	n, _, skip = mapEvt(t, "finding.created", map[string]string{"finding_id": "f-1", "severity": "SEVERITY_CRITICAL", "created_by": "u-1"}, "k")
	if n == nil || n.Event != v1.NotificationEvent_NOTIFICATION_EVENT_HIGH_SEVERITY_FOUND {
		t.Fatalf("CRITICAL must notify HIGH_SEVERITY_FOUND, got %v (%s)", n, skip)
	}
}

func TestMapNoRecipientSkips(t *testing.T) {
	n, _, skip := mapEvt(t, "task.created", map[string]string{"task_id": "t-1"}, "k")
	if n != nil || skip == "" {
		t.Fatal("empty recipient must skip with reason")
	}
}

func TestMapUnmappedTopics(t *testing.T) {
	for _, topic := range []string{"task.stage.completed", "finding.verdict.updated"} {
		n, _, skip := mapEvt(t, topic, map[string]string{"task_id": "t", "created_by": "u"}, "k")
		if n != nil || skip == "" {
			t.Fatalf("%s must skip with reason (enum缺位)", topic)
		}
	}
}
