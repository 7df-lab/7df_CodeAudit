package handler

// REGRESSIONS.md 随建锁定测试（2026-09-07，接口契约三件套会话）：
//   R11 — 通知中心 IDOR：ListNotifications.user_id 必须取 JWT 身份，query 传入被忽略（ADR-212⑩）；
//   R26 — 写路由幂等键必须在 decodeBody 之后注入（protojson.Unmarshal 会重置消息，TP12-T3）；
//   R25 — taskwatch 连接寿命下界必须显著超过最长审计任务（gw-f6a3523：32.5min 审计撞
//         30min 旧值中途断流；活性由 ping/pong+读限承担，寿命只作泄漏兜底，不得回缩）。
// 变异条目见 tests/mutation/run_mutations.py（M9/M23/M22）。

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
	"google.golang.org/grpc"
)

// notifCaptureBackend — 捕获 ListNotifications 实收 UserId 的进程内真实后端。
type notifCaptureBackend struct {
	pb.UnimplementedNotificationServiceServer
	addr     string
	srv      *grpc.Server
	mu       sync.Mutex
	gotUser  string
	gotCalls int
}

func startNotifCaptureBackend(t *testing.T) *notifCaptureBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &notifCaptureBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterNotificationServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *notifCaptureBackend) ListNotifications(ctx context.Context, req *pb.ListNotificationsRequest) (*pb.ListNotificationsResponse, error) {
	b.mu.Lock()
	b.gotUser = req.GetUserId()
	b.gotCalls++
	b.mu.Unlock()
	return &pb.ListNotificationsResponse{Notifications: []*pb.Notification{{NotificationId: "n-1", UserId: req.GetUserId()}}}, nil
}

// R11（ADR-212⑩）：user_id 一律取 JWT 身份——query 里的 user_id 必须被忽略。
// 回归形态：接口层改回读 query（或后端仅校验非空）→ 任何登录用户可读任意用户通知。
func TestNotifications_UserIdFromJWTNotQuery(t *testing.T) {
	b := startNotifCaptureBackend(t)
	tr := NewTranscoder(BackendAddrs{StorageAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	// 经中间件同款 context key 注入 JWT 身份（生产链路由 JWTMiddleware 写入）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), middleware.UserIDKey, "u-real-victim")
		tr.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/notifications?user_id=u-attacker")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gotCalls == 0 {
		t.Fatalf("backend never called")
	}
	if b.gotUser != "u-real-victim" {
		t.Fatalf("IDOR regression: ListNotifications received user_id=%q (query param honored over JWT identity)", b.gotUser)
	}
}

// projectCaptureBackend — 捕获 CreateProject 实收幂等键的进程内真实后端。
type projectCaptureBackend struct {
	pb.UnimplementedProjectServiceServer
	addr      string
	srv       *grpc.Server
	gotReqID  string
}

func startProjectCaptureBackend(t *testing.T) *projectCaptureBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &projectCaptureBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterProjectServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *projectCaptureBackend) CreateProject(ctx context.Context, req *pb.CreateProjectRequest) (*pb.Project, error) {
	b.gotReqID = req.GetMetadata().GetRequestId()
	return &pb.Project{ProjectId: "p-1", Name: req.GetProject().GetName()}, nil
}

// R26（TP12-T3 旅程回归）：幂等键注入必须在 decodeBody **之后**——
// protojson.Unmarshal 会重置整个消息，注入先于解码 = request_id 恒空，
// 后端 R4 幂等校验（request_id is required）全数 400。
func TestCreateProject_IdempotencyInjectedAfterDecode(t *testing.T) {
	b := startProjectCaptureBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/projects", "application/json",
		strings.NewReader(`{"project":{"name":"demo"}}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if b.gotReqID == "" {
		t.Fatalf("idempotency regression: metadata.request_id empty at backend (injection order broken by protojson.Unmarshal reset)")
	}
}

// R25（gw-f6a3523③）：连接寿命下界——旧值 30min 使 32.5min 审计的观测页中途断流
// （并与 30min access token TTL 竞态）。活性不靠它（读限 90s+无条件 ping/pong 承担），
// 它只是泄漏兜底，因此必须**显著超过**最长审计任务：下界锁 6h。
func TestTaskWatch_LifetimeCoversLongAudit(t *testing.T) {
	if wsMaxLifetime < 6*time.Hour {
		t.Fatalf("wsMaxLifetime=%v below 6h floor: long audits (>30min incidents on record) would be force-disconnected mid-flight", wsMaxLifetime)
	}
}
