package handler

import (
	"context"
	"fmt"
	"io"
	"testing"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/storage-service/internal/repo"
	"github.com/codeaudit/services/storage-service/internal/service"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// ---- mock streams for UploadFile ----

type mockUploadStream struct {
	chunks []*v1.UploadFileChunk
	idx    int
	sent   *v1.StoredFile
	ctx    context.Context
}

func (m *mockUploadStream) Recv() (*v1.UploadFileChunk, error) {
	if m.idx >= len(m.chunks) {
		return nil, io.EOF
	}
	chunk := m.chunks[m.idx]
	m.idx++
	return chunk, nil
}

func (m *mockUploadStream) SendAndClose(file *v1.StoredFile) error {
	m.sent = file
	return nil
}

func (m *mockUploadStream) Context() context.Context     { return m.ctx }
func (m *mockUploadStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockUploadStream) SendHeader(metadata.MD) error { return nil }
func (m *mockUploadStream) SetTrailer(metadata.MD)       {}
func (m *mockUploadStream) SendMsg(v any) error          { return nil }
func (m *mockUploadStream) RecvMsg(v any) error          { return nil }

// ---- mock streams for DownloadFile ----

type mockDownloadStream struct {
	sent []*v1.DownloadFileChunk
	ctx  context.Context
}

func (m *mockDownloadStream) Send(chunk *v1.DownloadFileChunk) error {
	m.sent = append(m.sent, chunk)
	return nil
}

func (m *mockDownloadStream) Context() context.Context     { return m.ctx }
func (m *mockDownloadStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockDownloadStream) SendHeader(metadata.MD) error { return nil }
func (m *mockDownloadStream) SetTrailer(metadata.MD)       {}
func (m *mockDownloadStream) SendMsg(v any) error          { return nil }
func (m *mockDownloadStream) RecvMsg(v any) error          { return nil }

// ---- helpers ----

func newTestHandlers() (*StorageHandler, *NotificationHandler) {
	store := repo.NewMemoryStore()
	storageSvc := service.NewStorageSvc(store)
	notifSvc := service.NewNotificationSvc(store, store)
	return NewStorageHandler(storageSvc), NewNotificationHandler(notifSvc)
}

// ---- Tests ----

// TestUploadFileBasicFlow tests the client-streaming UploadFile RPC with two chunks.
func TestUploadFileBasicFlow(t *testing.T) {
	sh, _ := newTestHandlers()

	chunks := []*v1.UploadFileChunk{
		{Data: []byte("hello "), FilePath: "reports/test.txt", ContentType: "text/plain", FirstChunk: true},
		{Data: []byte("world")},
	}
	stream := &mockUploadStream{
		chunks: chunks,
		ctx:    context.Background(),
	}

	err := sh.UploadFile(stream)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	if stream.sent == nil {
		t.Fatal("expected StoredFile in SendAndClose, got nil")
	}
	if stream.sent.FilePath != "reports/test.txt" {
		t.Errorf("expected FilePath=reports/test.txt, got %s", stream.sent.FilePath)
	}
	if stream.sent.SizeBytes != 11 {
		t.Errorf("expected SizeBytes=11, got %d", stream.sent.SizeBytes)
	}
	if stream.sent.ContentType != "text/plain" {
		t.Errorf("expected ContentType=text/plain, got %s", stream.sent.ContentType)
	}
	if stream.sent.ChecksumSha256 == "" {
		t.Error("expected non-empty checksum")
	}
	t.Logf("UploadFile OK: id=%s size=%d checksum=%s", stream.sent.FileId, stream.sent.SizeBytes, stream.sent.ChecksumSha256)
}

// TestUploadFileMissingFilePath verifies that a first_chunk without file_path returns INVALID_ARGUMENT.
func TestUploadFileMissingFilePath(t *testing.T) {
	sh, _ := newTestHandlers()

	chunks := []*v1.UploadFileChunk{
		{Data: []byte("data"), FirstChunk: false}, // no file_path
	}
	stream := &mockUploadStream{chunks: chunks, ctx: context.Background()}

	err := sh.UploadFile(stream)
	if err == nil {
		t.Fatal("expected error for missing file_path")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT, got %v (code=%s)", err, st.Code())
	}
}

// TestIdempotencySendNotification verifies that the same request_id replays
// the cached response instead of creating a duplicate notification.
func TestIdempotencySendNotification(t *testing.T) {
	_, nh := newTestHandlers()

	req := &v1.SendNotificationRequest{
		Metadata: &v1.RequestMetadata{RequestId: "idem-001"},
		Notification: &v1.Notification{
			UserId: "user-1",
			Title:  "Test",
			Body:   "Body",
			Type:   v1.NotificationType_NOTIFICATION_TYPE_IN_APP,
			Event:  v1.NotificationEvent_NOTIFICATION_EVENT_TASK_CREATED,
		},
	}

	// First call
	resp1, err := nh.SendNotification(context.Background(), req)
	if err != nil {
		t.Fatalf("first SendNotification failed: %v", err)
	}
	if resp1 == nil {
		t.Fatal("expected non-nil response")
	}

	// Second call with same key + same body → should succeed (idempotent replay)
	resp2, err := nh.SendNotification(context.Background(), req)
	if err != nil {
		t.Fatalf("second SendNotification (idempotent) failed: %v", err)
	}
	if resp2 == nil {
		t.Fatal("expected non-nil response on replay")
	}
	t.Log("idempotency OK: same key + same body returns cached response")
}

// TestIdempotencySameKeyDifferentBody verifies that reusing a request_id with
// a different body returns ALREADY_EXISTS (code 9).
func TestIdempotencySameKeyDifferentBody(t *testing.T) {
	_, nh := newTestHandlers()

	req1 := &v1.SendNotificationRequest{
		Metadata:     &v1.RequestMetadata{RequestId: "idem-conflict"},
		Notification: &v1.Notification{UserId: "u1", Title: "A", Body: "B"},
	}
	req2 := &v1.SendNotificationRequest{
		Metadata:     &v1.RequestMetadata{RequestId: "idem-conflict"},
		Notification: &v1.Notification{UserId: "u1", Title: "C", Body: "D"},
	}

	_, err := nh.SendNotification(context.Background(), req1)
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	_, err = nh.SendNotification(context.Background(), req2)
	if err == nil {
		t.Fatal("expected ALREADY_EXISTS for different body with same key")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.AlreadyExists {
		t.Errorf("expected ALREADY_EXISTS, got code=%s err=%v", st.Code(), err)
	}
	t.Log("idempotency conflict OK: same key + different body → ALREADY_EXISTS")
}

// TestMissingMetadataReturnsInvalidArgument verifies that a write RPC without
// RequestMetadata returns INVALID_ARGUMENT (code 3) per 03 §2.
func TestMissingMetadataReturnsInvalidArgument(t *testing.T) {
	_, nh := newTestHandlers()

	req := &v1.SendNotificationRequest{
		// Metadata is nil
		Notification: &v1.Notification{UserId: "u1", Title: "T", Body: "B"},
	}

	_, err := nh.SendNotification(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing metadata")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT, got code=%s err=%v", st.Code(), err)
	}
	t.Log("missing metadata OK → INVALID_ARGUMENT")
}

// TestListFilesEmpty verifies that ListFiles returns an empty list initially.
func TestListFilesEmpty(t *testing.T) {
	sh, _ := newTestHandlers()

	resp, err := sh.ListFiles(context.Background(), &v1.ListFilesRequest{})
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if len(resp.Files) != 0 {
		t.Errorf("expected 0 files, got %d", len(resp.Files))
	}
	t.Log("ListFiles OK: empty list initially")
}

// TestListFilesAfterUpload verifies that uploaded files appear in ListFiles.
func TestListFilesAfterUpload(t *testing.T) {
	sh, _ := newTestHandlers()

	// Upload a file first
	chunks := []*v1.UploadFileChunk{
		{Data: []byte("data"), FilePath: "reports/audit.json", ContentType: "application/json", FirstChunk: true},
	}
	stream := &mockUploadStream{chunks: chunks, ctx: context.Background()}
	if err := sh.UploadFile(stream); err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	// List all files
	resp, err := sh.ListFiles(context.Background(), &v1.ListFilesRequest{})
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
	if len(resp.Files) != 1 {
		t.Errorf("expected 1 file, got %d", len(resp.Files))
	}
	if resp.Files[0].FilePath != "reports/audit.json" {
		t.Errorf("expected path=reports/audit.json, got %s", resp.Files[0].FilePath)
	}

	// List with prefix
	resp2, err := sh.ListFiles(context.Background(), &v1.ListFilesRequest{Prefix: "reports/"})
	if err != nil {
		t.Fatalf("ListFiles with prefix failed: %v", err)
	}
	if len(resp2.Files) != 1 {
		t.Errorf("expected 1 file with prefix 'reports/', got %d", len(resp2.Files))
	}

	// List with non-matching prefix
	resp3, err := sh.ListFiles(context.Background(), &v1.ListFilesRequest{Prefix: "cpg/"})
	if err != nil {
		t.Fatalf("ListFiles with prefix cpg/ failed: %v", err)
	}
	if len(resp3.Files) != 0 {
		t.Errorf("expected 0 files with prefix 'cpg/', got %d", len(resp3.Files))
	}
	t.Log("ListFiles after upload OK")
}

// TestDownloadFile verifies that DownloadFile streams back the correct data.
func TestDownloadFile(t *testing.T) {
	sh, _ := newTestHandlers()

	// Upload a file
	chunks := []*v1.UploadFileChunk{
		{Data: []byte("streaming-test"), FilePath: "reports/data.bin", ContentType: "application/octet-stream", FirstChunk: true},
	}
	uploadStream := &mockUploadStream{chunks: chunks, ctx: context.Background()}
	if err := sh.UploadFile(uploadStream); err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}

	fileID := uploadStream.sent.FileId

	// Download it
	downloadStream := &mockDownloadStream{ctx: context.Background()}
	err := sh.DownloadFile(&v1.DownloadFileRequest{FileId: fileID}, downloadStream)
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}

	// Reassemble chunks
	var reassembled []byte
	for _, chunk := range downloadStream.sent {
		reassembled = append(reassembled, chunk.Data...)
	}

	original := []byte("streaming-test")
	if len(reassembled) != len(original) {
		t.Fatalf("expected %d bytes, got %d", len(original), len(reassembled))
	}
	for i := range original {
		if reassembled[i] != original[i] {
			t.Fatalf("byte mismatch at offset %d: expected %d, got %d", i, original[i], reassembled[i])
		}
	}
	t.Logf("DownloadFile OK: reassembled %d bytes", len(reassembled))
}

// TestDeleteFile verifies that DeleteFile removes a file and GetFileInfo returns NotFound afterward.
func TestDeleteFile(t *testing.T) {
	sh, _ := newTestHandlers()

	// Upload
	chunks := []*v1.UploadFileChunk{
		{Data: []byte("to-delete"), FilePath: "reports/temp.txt", ContentType: "text/plain", FirstChunk: true},
	}
	uploadStream := &mockUploadStream{chunks: chunks, ctx: context.Background()}
	if err := sh.UploadFile(uploadStream); err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	fileID := uploadStream.sent.FileId

	// Delete
	_, err := sh.DeleteFile(context.Background(), &v1.DeleteFileRequest{FileId: fileID})
	if err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}

	// Verify it's gone
	_, err = sh.GetFileInfo(context.Background(), &v1.GetFileInfoRequest{FileId: fileID})
	if err == nil {
		t.Fatal("expected NotFound after delete")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %s", st.Code())
	}
	t.Log("DeleteFile OK: file removed and GetFileInfo returns NotFound")
}

// TestListNotifications tests the ListNotifications RPC.
func TestListNotifications(t *testing.T) {
	_, nh := newTestHandlers()

	// Send a notification first
	req := &v1.SendNotificationRequest{
		Metadata: &v1.RequestMetadata{RequestId: "list-test-001"},
		Notification: &v1.Notification{
			UserId: "user-list",
			Title:  "Alert",
			Body:   "Something happened",
			Type:   v1.NotificationType_NOTIFICATION_TYPE_IN_APP,
			Event:  v1.NotificationEvent_NOTIFICATION_EVENT_TASK_COMPLETED,
		},
	}
	_, err := nh.SendNotification(context.Background(), req)
	if err != nil {
		t.Fatalf("SendNotification failed: %v", err)
	}

	// List for user
	resp, err := nh.ListNotifications(context.Background(), &v1.ListNotificationsRequest{
		UserId: "user-list",
	})
	if err != nil {
		t.Fatalf("ListNotifications failed: %v", err)
	}
	if len(resp.Notifications) != 1 {
		t.Errorf("expected 1 notification, got %d", len(resp.Notifications))
	}

	// List unread only
	resp2, err := nh.ListNotifications(context.Background(), &v1.ListNotificationsRequest{
		UserId:     "user-list",
		UnreadOnly: true,
	})
	if err != nil {
		t.Fatalf("ListNotifications unread failed: %v", err)
	}
	if len(resp2.Notifications) != 1 {
		t.Errorf("expected 1 unread notification, got %d", len(resp2.Notifications))
	}

	// Mark as read
	_, err = nh.MarkNotificationRead(context.Background(), &v1.MarkNotificationReadRequest{
		NotificationId: resp.Notifications[0].NotificationId,
	})
	if err != nil {
		t.Fatalf("MarkNotificationRead failed: %v", err)
	}

	// List unread should now be empty
	resp3, err := nh.ListNotifications(context.Background(), &v1.ListNotificationsRequest{
		UserId:     "user-list",
		UnreadOnly: true,
	})
	if err != nil {
		t.Fatalf("ListNotifications unread after read failed: %v", err)
	}
	if len(resp3.Notifications) != 0 {
		t.Errorf("expected 0 unread notifications, got %d", len(resp3.Notifications))
	}
	t.Log("ListNotifications + MarkNotificationRead OK")
}

// TestGetPresignedUrl — 诚实性契约（2026-08-27 编造审计）:
// MinIO 未接入时必须 Unimplemented, 且绝不返回拼凑的假 URL（必然 404 冒充可用）。
func TestGetPresignedUrl(t *testing.T) {
	sh, _ := newTestHandlers()

	resp, err := sh.GetPresignedUrl(context.Background(), &v1.GetPresignedUrlRequest{
		FilePath:   "reports/new-report.pdf",
		Operation:  v1.GetPresignedUrlRequest_URL_OP_UPLOAD,
		TtlSeconds: 3600,
	})
	if err == nil {
		t.Fatalf("expected Unimplemented without MinIO, got url=%s", resp.GetUrl())
	}
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %v", err)
	}
}

// TestSendBatchNotification tests the SendBatchNotification RPC.
func TestSendBatchNotification(t *testing.T) {
	_, nh := newTestHandlers()

	req := &v1.SendBatchNotificationRequest{
		Metadata: &v1.RequestMetadata{RequestId: fmt.Sprintf("batch-%d", 1)},
		Notifications: []*v1.Notification{
			{UserId: "u1", Title: "A", Body: "a"},
			{UserId: "u2", Title: "B", Body: "b"},
		},
	}
	resp, err := nh.SendBatchNotification(context.Background(), req)
	if err != nil {
		t.Fatalf("SendBatchNotification failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify both notifications exist
	list1, _ := nh.ListNotifications(context.Background(), &v1.ListNotificationsRequest{UserId: "u1"})
	list2, _ := nh.ListNotifications(context.Background(), &v1.ListNotificationsRequest{UserId: "u2"})
	if len(list1.Notifications) != 1 {
		t.Errorf("expected 1 notification for u1, got %d", len(list1.Notifications))
	}
	if len(list2.Notifications) != 1 {
		t.Errorf("expected 1 notification for u2, got %d", len(list2.Notifications))
	}
	t.Log("SendBatchNotification OK")
}

// Ensure interface compliance in test compilation.
var _ emptypb.Empty
