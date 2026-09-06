// Package service implements business logic for the NotificationService.
// 通知来源双通道（ADR-199）: HTTP Send/SendBatch（幂等键 R4）+ Kafka 事件消费
// （01 §4.3 五 topic → MapEventToNotification）；持久化 = Redis（生产档）/ memory（07 §10 降级档）。
//
// Kafka topics consumed (01 §4.3):
//   - task.created
//   - task.completed
//   - task.stage.completed
//   - finding.created
//   - finding.verdict.updated
package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/storage-service/internal/repo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// NotificationSvc provides business-level operations for notifications.
type NotificationSvc struct {
	notifs repo.NotificationStore
	idem   repo.IdempotencyStore
}

// NewNotificationSvc creates a new NotificationSvc.
// 依赖拆分（ADR-199）: 通知持久化与幂等键可分别落 Redis（生产档）或 memory（降级档）。
func NewNotificationSvc(notifs repo.NotificationStore, idem repo.IdempotencyStore) *NotificationSvc {
	return &NotificationSvc{notifs: notifs, idem: idem}
}

// SendNotification sends a single notification with idempotency checking (R4, 03 §2).
// Write RPC: must read RequestMetadata for idempotency key.
//
// Idempotency rules:
//   - Same key + same body  → return cached response (idempotent replay)
//   - Same key + different body → ALREADY_EXISTS (code 9)
//   - Missing metadata       → INVALID_ARGUMENT (code 3)
func (s *NotificationSvc) SendNotification(req *v1.SendNotificationRequest) (*emptypb.Empty, error) {
	if req.Metadata == nil || req.Metadata.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (03 §2)")
	}

	bodyHash := notificationHash(req.Notification)
	entry, hit, err := s.idem.CheckIdempotency(req.Metadata.RequestId, bodyHash)
	if err != nil {
		return nil, err // ALREADY_EXISTS
	}
	if hit {
		log.Printf("[notification] idempotent replay for request_id=%s", req.Metadata.RequestId)
		return entry.Response.(*emptypb.Empty), nil
	}

	// Persist notification
	n := req.Notification
	if n.NotificationId == "" {
		n.NotificationId = repo.GenerateID("notif")
	}
	if n.CreatedAt == nil {
		n.CreatedAt = timestamppb.Now()
	}
	s.notifs.SaveNotification(n)

	resp := &emptypb.Empty{}
	s.idem.SetIdempotency(req.Metadata.RequestId, bodyHash, resp)
	// ADR-135 措辞诚实化: 本服务当前仅站内存储（Kafka 投递为已声明的 stub），
	// "sent" 措辞冒充真实投递——如实记录为 persisted。
	log.Printf("[notification] persisted notification %s for user %s (in-store; delivery channel not connected)", n.NotificationId, n.UserId)
	return resp, nil
}

// SendBatchNotification sends multiple notifications atomically with idempotency (R4, 03 §2).
func (s *NotificationSvc) SendBatchNotification(req *v1.SendBatchNotificationRequest) (*emptypb.Empty, error) {
	if req.Metadata == nil || req.Metadata.RequestId == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (03 §2)")
	}

	bodyHash := batchNotificationHash(req.Notifications)
	entry, hit, err := s.idem.CheckIdempotency(req.Metadata.RequestId, bodyHash)
	if err != nil {
		return nil, err
	}
	if hit {
		log.Printf("[notification] batch idempotent replay for request_id=%s", req.Metadata.RequestId)
		return entry.Response.(*emptypb.Empty), nil
	}

	for _, n := range req.Notifications {
		if n.NotificationId == "" {
			n.NotificationId = repo.GenerateID("notif")
		}
		if n.CreatedAt == nil {
			n.CreatedAt = timestamppb.Now()
		}
		s.notifs.SaveNotification(n)
	}

	resp := &emptypb.Empty{}
	s.idem.SetIdempotency(req.Metadata.RequestId, bodyHash, resp)
	log.Printf("[notification] sent batch of %d notifications", len(req.Notifications))
	return resp, nil
}

// ListNotifications returns notifications for a user.
func (s *NotificationSvc) ListNotifications(req *v1.ListNotificationsRequest) (*v1.ListNotificationsResponse, error) {
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	notifications := s.notifs.ListNotifications(req.UserId, req.UnreadOnly)
	return &v1.ListNotificationsResponse{
		Notifications: notifications,
	}, nil
}

// MarkNotificationRead marks a single notification as read.
func (s *NotificationSvc) MarkNotificationRead(req *v1.MarkNotificationReadRequest) (*v1.Notification, error) {
	if req.NotificationId == "" {
		return nil, status.Error(codes.InvalidArgument, "notification_id is required")
	}
	n, ok := s.notifs.MarkRead(req.NotificationId)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "notification %s not found", req.NotificationId)
	}
	log.Printf("[notification] marked %s as read", req.NotificationId)
	return n, nil
}

// ---- hashing helpers for idempotency ----

func notificationHash(n *v1.Notification) string {
	if n == nil {
		return ""
	}
	// Simple hash: notification type + event + user_id + title + body
	raw := fmt.Sprintf("%d|%d|%s|%s|%s", n.Type, n.Event, n.UserId, n.Title, n.Body)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

func batchNotificationHash(notifications []*v1.Notification) string {
	h := sha256.New()
	for _, n := range notifications {
		h.Write([]byte(notificationHash(n)))
		h.Write([]byte("|"))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Publish — Kafka 消费侧的通知落库入口（无 HTTP 幂等键；去重由事件 key 前缀
// 构造的幂等键承担，见 MapEventKey）。ADR-199: 消费组位点是主去重，此处兜底。
func (s *NotificationSvc) Publish(n *v1.Notification) {
	if n.CreatedAt == nil {
		n.CreatedAt = timestamppb.Now()
	}
	if n.NotificationId == "" {
		n.NotificationId = repo.GenerateID("notif")
	}
	s.notifs.SaveNotification(n)
	log.Printf("[notification] published from event: %s (user=%s event=%s)", n.NotificationId, n.UserId, n.Event)
}

// eventPayload — 五 topic 事件载荷的宽松解析形态（01 §4.3）。
// 字段缺省如实跳过映射，不臆造收件人。
type eventPayload struct {
	TaskID     string `json:"task_id"`
	ProjectID  string `json:"project_id"`
	Status     string `json:"status"`
	CreatedBy  string `json:"created_by"`
	UserID     string `json:"user_id"`
	UpdatedBy  string `json:"updated_by"`
	Severity   string `json:"severity"`
	FindingID  string `json:"finding_id"`
	NewVerdict string `json:"new_verdict"`
}

// MapEventToNotification — Kafka 事件 → Notification 的纯映射（ADR-199；可单测）。
//
// 枚举语义边界（proto NotificationEvent 只有 5 值，映射缺位=不产通知而非硬凑）：
//   - task.created            → TASK_CREATED（收件人=created_by）
//   - task.completed          → TASK_COMPLETED；payload.status 含 FAILED → TASK_FAILED
//   - finding.created         → HIGH_SEVERITY_FOUND（severity ∈ {HIGH,CRITICAL}）
//   - task.stage.completed    → 枚举缺位，仅消费记录（09 §2 "消费4 topic" 第4类）
//   - finding.verdict.updated → 枚举缺位，仅消费记录（webhook 出站预留，14号 §6）
//
// 收件人 = created_by | user_id | updated_by 首个非空；全空 → 返回 nil（跳过，
// 不臆造收件人）。返回值: (通知, 幂等键, 跳过原因)；通知为 nil 时幂等键为空。
func MapEventToNotification(topic string, key []byte, value []byte) (*v1.Notification, string, string) {
	var p eventPayload
	_ = json.Unmarshal(value, &p) // 载荷字段宽松解析；解析失败按空值走跳过分支
	recipient := firstNonEmpty(p.CreatedBy, p.UserID, p.UpdatedBy)

	var (
		event v1.NotificationEvent
		title string
	)
	switch topic {
	case "task.created":
		event = v1.NotificationEvent_NOTIFICATION_EVENT_TASK_CREATED
		title = "任务已创建"
	case "task.completed":
		if containsFailed(p.Status) {
			event = v1.NotificationEvent_NOTIFICATION_EVENT_TASK_FAILED
			title = "任务失败"
		} else {
			event = v1.NotificationEvent_NOTIFICATION_EVENT_TASK_COMPLETED
			title = "任务完成"
		}
	case "finding.created":
		if !isHighSeverity(p.Severity) {
			return nil, "", "severity below HIGH threshold"
		}
		event = v1.NotificationEvent_NOTIFICATION_EVENT_HIGH_SEVERITY_FOUND
		title = "高危发现"
	case "task.stage.completed", "finding.verdict.updated":
		return nil, "", "topic has no NotificationEvent enum mapping (logged only)"
	default:
		return nil, "", "unknown topic"
	}
	if recipient == "" {
		return nil, "", "no recipient in event payload (created_by/user_id/updated_by all empty)"
	}
	if event == v1.NotificationEvent_NOTIFICATION_EVENT_UNSPECIFIED {
		return nil, "", "unspecified event"
	}

	n := &v1.Notification{
		NotificationId: repo.GenerateID("notif"),
		UserId:         recipient,
		Type:           v1.NotificationType_NOTIFICATION_TYPE_IN_APP,
		Event:          event,
		Title:          title,
		Body:           title + "：task=" + p.TaskID,
		Payload: map[string]string{
			"task_id":    p.TaskID,
			"project_id": p.ProjectID,
			"finding_id": p.FindingID,
		},
	}
	idemKey := "event:" + topic + ":" + string(key)
	return n, idemKey, ""
}

// MapEventKey — 事件幂等键（Publish 路径的 03 §2 兜底去重）。
func (s *NotificationSvc) PublishFromEvent(n *v1.Notification, idemKey string) {
	if idemKey != "" {
		if _, hit, _ := s.idem.CheckIdempotency(idemKey, idemKey); hit {
			return
		}
		s.Publish(n)
		s.idem.SetIdempotency(idemKey, idemKey, &emptypb.Empty{})
		return
	}
	s.Publish(n)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsFailed(status string) bool {
	return status != "" && (status == "TASK_STATUS_FAILED" || status == "FAILED" || status == "TASK_STATUS_DEAD")
}

func isHighSeverity(sev string) bool {
	switch sev {
	case "SEVERITY_HIGH", "HIGH", "SEVERITY_CRITICAL", "CRITICAL":
		return true
	}
	return false
}
