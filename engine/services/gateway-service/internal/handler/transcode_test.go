package handler

// 转码器回归锁（ADR-132）：REST JSON → gRPC → JSON 全链路真实生效；
// 未映射路由 501 诚实降级（而非假转发 502）。
// 依据: 03 §1.1 暴露接口清单。

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// loginBackend — 进程内真实 gRPC 后端（验证转码路径，不 mock HTTP 层）。
type loginBackend struct {
	pb.UnimplementedUserServiceServer
	addr string
	srv  *grpc.Server
}

func startLoginBackend(t *testing.T) *loginBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &loginBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterUserServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *loginBackend) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	if req.GetUsername() == "" || req.GetPassword() == "" {
		return nil, status.Error(codes.InvalidArgument, "username and password required")
	}
	return &pb.LoginResponse{AccessToken: "tok-" + req.GetUsername(), ExpiresInS: 1800}, nil
}

func TestTranscoder_LoginJSONOverGRPCEndToEnd(t *testing.T) {
	b := startLoginBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5}) // ADR-137: 超时来自全局配置
	defer tr.Close()

	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	// 正常登录：JSON 进 → gRPC → JSON 出
	body := `{"username":"alice","password":"secret"}`
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: want 200, got %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["access_token"] != "tok-alice" {
		t.Fatalf("access_token = %v, want tok-alice", out["access_token"])
	}

	// 后端 InvalidArgument → 400（03 §3 错误码映射）
	bad := `{"username":"","password":""}`
	resp2, err2 := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader(bad))
	if err2 != nil {
		t.Fatalf("POST invalid login: %v", err2)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid login: want 400, got %d", resp2.StatusCode)
	}

	// 非法 JSON → 400
	resp3, err3 := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader("{not-json"))
	if err3 != nil {
		t.Fatalf("POST bad json: %v", err3)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json: want 400, got %d", resp3.StatusCode)
	}
}

func TestTranscoder_UnknownRoute501(t *testing.T) {
	tr := &Transcoder{}
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/unknown/domain", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("unknown route: want 501, got %d", resp.StatusCode)
	}
}

func TestTranscoder_NoBackend503(t *testing.T) {
	tr := &Transcoder{} // 未配置任何后端连接
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader(`{"username":"a","password":"b"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no backend: want 503, got %d", resp.StatusCode)
	}
}

// ===================================================================
// TP12-T0 路由扩展契约测试（14号 §7 全表；完成标准=扩展路由端到端全绿）
// ===================================================================

// ---- fake 后端：仅实现被测 RPC，验证 JSON→gRPC→JSON 契约 ----

type fakeUsersExt struct {
	pb.UnimplementedUserServiceServer
}

func (f *fakeUsersExt) GetCurrentUser(ctx context.Context, req *pb.GetCurrentUserRequest) (*pb.User, error) {
	if req.GetAccessToken() == "" {
		return nil, status.Error(codes.Unauthenticated, "access_token required")
	}
	return &pb.User{UserId: "u-1", Username: "alice"}, nil
}

func (f *fakeUsersExt) GetUserPermissions(ctx context.Context, req *pb.GetUserPermissionsRequest) (*pb.UserPermissions, error) {
	return &pb.UserPermissions{UserId: req.GetUserId(), Permissions: []string{"task:create"}}, nil
}

type fakeTaskExt struct {
	pb.UnimplementedTaskServiceServer
}

// ADR-172 watch 用例：t-1 常驻 RUNNING（推流不收束）、t-done 终态（推完即关）
func (f *fakeTaskExt) GetScanTask(ctx context.Context, req *pb.GetScanTaskRequest) (*pb.ScanTask, error) {
	switch req.GetTaskId() {
	case "t-1":
		return &pb.ScanTask{TaskId: "t-1", Status: pb.TaskStatus_TASK_STATUS_RUNNING}, nil
	case "t-done":
		return &pb.ScanTask{TaskId: "t-done", Status: pb.TaskStatus_TASK_STATUS_COMPLETED}, nil
	default:
		return nil, status.Error(codes.NotFound, "task not found")
	}
}

func (f *fakeTaskExt) GetTaskProgress(ctx context.Context, req *pb.GetTaskProgressRequest) (*pb.TaskProgress, error) {
	return &pb.TaskProgress{TaskId: req.GetTaskId(), OverallPercent: 0}, nil
}

func (f *fakeTaskExt) GetTaskLogs(ctx context.Context, req *pb.GetTaskLogsRequest) (*pb.GetTaskLogsResponse, error) {
	return &pb.GetTaskLogsResponse{Logs: []*pb.TaskLogEntry{}}, nil
}

type fakeResultExt struct {
	pb.UnimplementedResultServiceServer
	pb.UnimplementedReportServiceServer
	verdicts []string
}

func (f *fakeResultExt) ListFindings(ctx context.Context, req *pb.ListFindingsRequest) (*pb.ListFindingsResponse, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id required (task-scoped query)")
	}
	return &pb.ListFindingsResponse{Findings: []*pb.UnifiedFinding{{
		FindingId: "f-1", TaskId: req.GetTaskId(), SourceTool: "bandit",
		Title: "hardcoded password", CweId: "CWE-798",
		Severity: pb.Severity_SEVERITY_HIGH, AiVerdict: pb.AIVerdict_AI_VERDICT_NEEDS_MANUAL,
	}}}, nil
}

func (f *fakeResultExt) UpdateVerdict(ctx context.Context, req *pb.UpdateVerdictRequest) (*pb.AuditFinding, error) {
	f.verdicts = append(f.verdicts, req.GetFindingId()+"="+req.GetVerdict().String())
	return &pb.AuditFinding{Finding: &pb.UnifiedFinding{FindingId: req.GetFindingId(), AiVerdict: req.GetVerdict()}}, nil
}

func (f *fakeResultExt) BatchUpdateVerdict(ctx context.Context, req *pb.BatchUpdateVerdictRequest) (*pb.BatchUpdateVerdictResponse, error) {
	return &pb.BatchUpdateVerdictResponse{UpdatedCount: int32(len(req.GetFindingIds()))}, nil
}

func (f *fakeResultExt) GenerateReport(ctx context.Context, req *pb.GenerateReportRequest) (*pb.GenerateReportResponse, error) {
	return &pb.GenerateReportResponse{Result: &pb.ReportResult{ReportId: "report_" + req.GetTaskId()}}, nil
}

func (f *fakeResultExt) DownloadReport(req *pb.DownloadReportRequest, stream pb.ReportService_DownloadReportServer) error {
	stream.Send(&pb.ReportChunk{Data: []byte("hello ")})
	return stream.Send(&pb.ReportChunk{Data: []byte("world")})
}

type fakeStorageExt struct {
	pb.UnimplementedNotificationServiceServer
}

func (f *fakeStorageExt) ListNotifications(ctx context.Context, req *pb.ListNotificationsRequest) (*pb.ListNotificationsResponse, error) {
	return &pb.ListNotificationsResponse{Notifications: []*pb.Notification{{
		NotificationId: "n-1", UserId: req.GetUserId(),
	}}}, nil
}

func (f *fakeStorageExt) MarkNotificationRead(ctx context.Context, req *pb.MarkNotificationReadRequest) (*pb.Notification, error) {
	return &pb.Notification{NotificationId: req.GetNotificationId()}, nil
}

type fakeAdapterExt struct {
	pb.UnimplementedSASTAdapterServiceServer
	pb.UnimplementedSASTFusionServiceServer
}

func (f *fakeAdapterExt) ListAvailableTools(ctx context.Context, req *pb.ListAvailableToolsRequest) (*pb.ListAvailableToolsResponse, error) {
	return &pb.ListAvailableToolsResponse{Tools: []*pb.SASTToolInfo{{ToolId: "bandit"}}}, nil
}

func (f *fakeAdapterExt) ValidateToolConfig(ctx context.Context, req *pb.ValidateToolConfigRequest) (*pb.ValidateToolConfigResponse, error) {
	return &pb.ValidateToolConfigResponse{Valid: req.GetToolId() == "bandit"}, nil
}

func (f *fakeAdapterExt) CalculateMetrics(ctx context.Context, req *pb.CalculateMetricsRequest) (*pb.ComparisonMetrics, error) {
	return &pb.ComparisonMetrics{TotalUnique: 3, SastF1: 0.5, AiF1: 0.75}, nil
}

// startBackends — 起全部 fake 后端并返回已接线的 Transcoder。
func startBackends(t *testing.T) *Transcoder {
	t.Helper()
	start := func(register func(s *grpc.Server)) string {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		s := grpc.NewServer()
		register(s)
		go func() { _ = s.Serve(lis) }()
		t.Cleanup(s.Stop)
		return lis.Addr().String()
	}
	fr := &fakeResultExt{}
	tr := NewTranscoder(BackendAddrs{
		ProjectAddr: start(func(s *grpc.Server) { pb.RegisterUserServiceServer(s, &fakeUsersExt{}) }),
		TaskAddr:    start(func(s *grpc.Server) { pb.RegisterTaskServiceServer(s, &fakeTaskExt{}) }),
		ResultAddr: start(func(s *grpc.Server) {
			pb.RegisterResultServiceServer(s, fr)
			pb.RegisterReportServiceServer(s, fr)
		}),
		StorageAddr: start(func(s *grpc.Server) { pb.RegisterNotificationServiceServer(s, &fakeStorageExt{}) }),
		SastAdapterAddr: start(func(s *grpc.Server) {
			pb.RegisterSASTAdapterServiceServer(s, &fakeAdapterExt{})
			pb.RegisterSASTFusionServiceServer(s, &fakeAdapterExt{})
		}),
		CallTimeoutS: 5,
	})
	t.Cleanup(tr.Close)
	return tr
}

func httpJSON(t *testing.T, tr *Transcoder, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, srv.URL+path, nil)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestTP12T0_UsersMeAndPermissions(t *testing.T) {
	tr := startBackends(t)
	code, out := httpJSON(t, tr, "GET", "/v1/users/me", "")
	_ = code
	// users/me 需要 Bearer（网关填入 access_token）——单独发一次带头部的请求
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer tok-test")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out = map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != 200 || out["user_id"] != "u-1" {
		t.Fatalf("users/me: code=%d out=%v", resp.StatusCode, out)
	}
	code, out = httpJSON(t, tr, "GET", "/v1/users/u-1/permissions", "")
	if code != 200 || out["user_id"] != "u-1" {
		t.Fatalf("permissions: code=%d out=%v", code, out)
	}
}

// ADR-172: WebSocket 秒级推送——升级成功、首帧同构 snapshot、终态任务推完即关
func TestTP12T0_TaskWatchFirstFrame(t *testing.T) {
	tr := startBackends(t)
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/tasks/t-1/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame1: %v", err)
	}
	var frame map[string]interface{}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("frame json: %v", err)
	}
	if frame["type"] != "snapshot" {
		t.Fatalf("want type=snapshot, got %v", frame["type"])
	}
	task, ok := frame["task"].(map[string]interface{})
	if !ok || task["task_id"] != "t-1" || task["status"] != "TASK_STATUS_RUNNING" {
		t.Fatalf("frame task: %v", frame["task"])
	}
	if _, ok := frame["logs"].(map[string]interface{}); !ok {
		t.Fatalf("frame logs missing: %v", frame["logs"])
	}
	if _, ok := frame["ai"].(map[string]interface{}); !ok {
		t.Fatalf("frame ai missing: %v", frame["ai"])
	}
}

func TestTP12T0_TaskWatchSettledCloses(t *testing.T) {
	tr := startBackends(t)
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/tasks/t-done/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	// 首帧（终态任务）→ 服务端应随后主动关闭
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := c.ReadMessage(); err != nil {
		t.Fatalf("read final frame: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = c.ReadMessage() // 期待关闭（ReadMessage 返回 err）
	if err == nil {
		t.Fatalf("server should close after settled task")
	}
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) &&
		!websocket.IsCloseError(err, websocket.CloseAbnormalClosure) {
		t.Fatalf("close err: %v", err)
	}
}

func TestTP12T0_TaskWatchUnknownTask(t *testing.T) {
	tr := startBackends(t)
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/tasks/none/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = c.ReadMessage() // 期待服务端报 NotFound 后关闭
	if err == nil {
		t.Fatalf("unknown task should close with error")
	}
}

// ADR-171 审批流废除：submit/approve/reject 路由必须不存在（404）
func TestTP12T0_ApprovalRoutesRemoved(t *testing.T) {
	tr := startBackends(t)
	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/tasks/t-9/submit"},
		{"POST", "/v1/tasks/t-9/approve"},
		{"POST", "/v1/tasks/t-9/reject"},
	} {
		code, _ := httpJSON(t, tr, route.method, route.path, `{"reason":"x"}`)
		if code != 404 {
			t.Fatalf("%s %s: want 404 (审批流已废除), got %d", route.method, route.path, code)
		}
	}
}

func TestTP12T0_VerdictRoutes(t *testing.T) {
	tr := startBackends(t)
	code, out := httpJSON(t, tr, "PUT", "/v1/findings/f-1/verdict", `{"verdict":"AI_VERDICT_TRUE_POSITIVE","reasoning":"ok"}`)
	if code != 200 {
		t.Fatalf("put verdict: code=%d out=%v", code, out)
	}
	if f, ok := out["finding"].(map[string]interface{}); !ok || f["finding_id"] != "f-1" {
		t.Fatalf("put verdict body: %v", out)
	}
	code, out = httpJSON(t, tr, "POST", "/v1/findings/verdict:batch", `{"finding_ids":["f-1","f-2"],"verdict":"AI_VERDICT_FALSE_POSITIVE"}`)
	if code != 200 || out["updated_count"] != float64(2) {
		t.Fatalf("batch verdict: code=%d out=%v", code, out)
	}
}

func TestTP12T0_ReportGenerateAndDownload(t *testing.T) {
	tr := startBackends(t)
	code, out := httpJSON(t, tr, "POST", "/v1/tasks/t-1/report", "")
	if code != 200 {
		t.Fatalf("generate report: code=%d out=%v", code, out)
	}
	if rr, ok := out["result"].(map[string]interface{}); !ok || rr["report_id"] != "report_t-1" {
		t.Fatalf("generate report body: %v", out)
	}
	// 下载：服务端流聚合
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/reports/r-1/download")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello world" {
		t.Fatalf("download chunks not aggregated: %q", string(b))
	}
}

func TestTP12T0_FindingsByTask(t *testing.T) {
	tr := startBackends(t)
	code, out := httpJSON(t, tr, "GET", "/v1/findings?task_id=t-77", "")
	if code != 200 {
		t.Fatalf("findings?task_id: code=%d out=%v", code, out)
	}
	arr, ok := out["findings"].([]interface{})
	if !ok || len(arr) != 1 {
		t.Fatalf("findings body: %v", out)
	}
	// 缺 task_id → 400（任务维度查询的诚实约束）
	code, _ = httpJSON(t, tr, "GET", "/v1/findings", "")
	if code != 400 {
		t.Fatalf("findings without task_id: want 400, got %d", code)
	}
}

func TestTP12T0_NotificationsAndSkillsAndTools(t *testing.T) {
	tr := startBackends(t)
	code, out := httpJSON(t, tr, "GET", "/v1/notifications?user_id=u-1", "")
	if code != 200 || out["notifications"] == nil {
		t.Fatalf("notifications: code=%d out=%v", code, out)
	}
	code, _ = httpJSON(t, tr, "POST", "/v1/notifications/n-1/read", "")
	if code != 200 {
		t.Fatalf("mark read: %d", code)
	}
	code, out = httpJSON(t, tr, "GET", "/v1/tools", "")
	if code != 200 || out["tools"] == nil {
		t.Fatalf("tools: code=%d out=%v", code, out)
	}
	code, out = httpJSON(t, tr, "GET", "/v1/tasks/t-1/metrics", "")
	if code != 200 || out["total_unique"] != float64(3) {
		t.Fatalf("metrics: code=%d out=%v", code, out)
	}
}

// TestGrpcToHTTP_AuthSemantics — 14号 §3.5 错误映射回归锁：
// Unauthenticated→401（登录失败/过期令牌, 前端静默刷新/跳登录语义）,
// PermissionDenied→403（权限不足页）。此前两者混映射 403, 破坏 401 语义。
func TestGrpcToHTTP_AuthSemantics(t *testing.T) {
	if got := grpcToHTTP(status.Error(codes.Unauthenticated, "login failed")); got != http.StatusUnauthorized {
		t.Fatalf("Unauthenticated → %d, want 401 (14号 §3.5)", got)
	}
	if got := grpcToHTTP(status.Error(codes.PermissionDenied, "no access")); got != http.StatusForbidden {
		t.Fatalf("PermissionDenied → %d, want 403", got)
	}
}
