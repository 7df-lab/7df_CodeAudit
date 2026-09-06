// Package handler implements gRPC server handlers for storage-service.
// NotificationServiceServer (codeaudit_common.proto L1080-L1085):
//
//	4 RPCs: SendNotification, SendBatchNotification, ListNotifications, MarkNotificationRead
package handler

import (
	"context"
	"log"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/storage-service/internal/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// NotificationHandler implements v1.NotificationServiceServer.
type NotificationHandler struct {
	v1.UnimplementedNotificationServiceServer
	svc *service.NotificationSvc
}

// NewNotificationHandler creates a new NotificationHandler.
func NewNotificationHandler(svc *service.NotificationSvc) *NotificationHandler {
	return &NotificationHandler{svc: svc}
}

// SendNotification implements unary RPC (write RPC).
// Has RequestMetadata.metadata — must implement idempotency (R4, 03 §2).
func (h *NotificationHandler) SendNotification(ctx context.Context, req *v1.SendNotificationRequest) (*emptypb.Empty, error) {
	if req.Notification == nil {
		return nil, status.Error(codes.InvalidArgument, "notification is required")
	}
	resp, err := h.svc.SendNotification(req)
	if err != nil {
		return nil, err
	}
	log.Printf("[handler] SendNotification: persisted for user %s (in-store)", req.Notification.UserId)
	return resp, nil
}

// SendBatchNotification implements unary RPC (write RPC).
// Has RequestMetadata.metadata — must implement idempotency (R4, 03 §2).
func (h *NotificationHandler) SendBatchNotification(ctx context.Context, req *v1.SendBatchNotificationRequest) (*emptypb.Empty, error) {
	if len(req.Notifications) == 0 {
		return nil, status.Error(codes.InvalidArgument, "notifications list must not be empty")
	}
	resp, err := h.svc.SendBatchNotification(req)
	if err != nil {
		return nil, err
	}
	log.Printf("[handler] SendBatchNotification: persisted %d notifications (in-store)", len(req.Notifications))
	return resp, nil
}

// ListNotifications implements unary RPC (read RPC).
func (h *NotificationHandler) ListNotifications(ctx context.Context, req *v1.ListNotificationsRequest) (*v1.ListNotificationsResponse, error) {
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	return h.svc.ListNotifications(req)
}

// MarkNotificationRead implements unary RPC (write RPC, no RequestMetadata).
// Marks a single notification as read and returns the updated Notification.
func (h *NotificationHandler) MarkNotificationRead(ctx context.Context, req *v1.MarkNotificationReadRequest) (*v1.Notification, error) {
	if req.NotificationId == "" {
		return nil, status.Error(codes.InvalidArgument, "notification_id is required")
	}
	n, err := h.svc.MarkNotificationRead(req)
	if err != nil {
		return nil, err
	}
	log.Printf("[handler] MarkNotificationRead: %s", req.NotificationId)
	return n, nil
}

// Ensure interface compliance at compile time.
var _ v1.NotificationServiceServer = (*NotificationHandler)(nil)
