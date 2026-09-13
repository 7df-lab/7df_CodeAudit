package handler

// R73: /v1/users/{id} GET/PUT 门禁——任意认证用户此前可改任意用户（role=ROLE_ADMIN
// 显式传入即提权）并枚举任意用户资料。网关是当前唯一管理入口（V2.1 ADR-205）：
// PUT/GET 一律 admin 或 self；非 admin 的 self 更新强制剥离 role 变更
// （服务端 UpdateUser 对 ROLE_UNSPECIFIED 保全存量，剥离即阻断提权）。

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/gateway-service/internal/middleware"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// signAccessHandler — 链路测试用真实 HS256 签发（claims 形态与生产一致）。
func signAccessHandler(t *testing.T, secret, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": sub, "exp": time.Now().Add(30 * time.Minute).Unix(), "iat": time.Now().Unix(), "type": "access",
	})
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// userGateBackend — 进程内真实 gRPC 后端：捕获 UpdateUser 载荷、应答 GetUser。
type userGateBackend struct {
	pb.UnimplementedUserServiceServer
	addr string
	srv  *grpc.Server

	mu       sync.Mutex
	updateReqs []*pb.UpdateUserRequest
	loginToken string // 测试预签发的 access token（Login 应答返回）
}

func startUserGateBackend(t *testing.T) *userGateBackend {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &userGateBackend{addr: lis.Addr().String(), srv: grpc.NewServer()}
	pb.RegisterUserServiceServer(b.srv, b)
	go func() { _ = b.srv.Serve(lis) }()
	t.Cleanup(b.srv.Stop)
	return b
}

func (b *userGateBackend) GetCurrentUser(ctx context.Context, req *pb.GetCurrentUserRequest) (*pb.User, error) {
	return &pb.User{UserId: "u-9", Username: "u-9"}, nil
}

func (b *userGateBackend) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	return &pb.LoginResponse{AccessToken: b.loginToken, ExpiresInS: 1800}, nil
}

func (b *userGateBackend) Logout(ctx context.Context, req *pb.LogoutRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (b *userGateBackend) UpdateUser(ctx context.Context, req *pb.UpdateUserRequest) (*pb.User, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.updateReqs = append(b.updateReqs, req)
	return req.GetUser(), nil
}

func (b *userGateBackend) GetUser(ctx context.Context, req *pb.GetUserRequest) (*pb.User, error) {
	return &pb.User{UserId: req.GetUserId(), Username: "u-" + req.GetUserId()}, nil
}

func (b *userGateBackend) GetUserPermissions(ctx context.Context, req *pb.GetUserPermissionsRequest) (*pb.UserPermissions, error) {
	return &pb.UserPermissions{UserId: req.GetUserId()}, nil
}

func (b *userGateBackend) capturedUpdate(t *testing.T) *pb.UpdateUserRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.updateReqs) == 0 {
		t.Fatalf("后端未收到 UpdateUser 调用")
	}
	return b.updateReqs[len(b.updateReqs)-1]
}

// httpJSONAsUser — 注入指定 role + user id 后走转码器（生产链路由 JWTMiddleware 写入 claim）。
func httpJSONAsUser(t *testing.T, tr *Transcoder, role, userID, method, path, body string) (int, map[string]any) {
	t.Helper()
	return httpJSONWithClaims(t, tr, role, userID, method, path, body)
}

// R73 主锁：非 admin 的 self 更新携带 role=ROLE_ADMIN → 到达后端的 role 必须被剥离
// 为 UNSPECIFIED（服务端保全语义生效，提权链断裂）；改他人 → 403 且后端不触达。
func TestUpdateUser_NonAdminRoleEscalationBlocked(t *testing.T) {
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	// self 提权尝试：role 剥离，更新本身放行（自助改资料合法）
	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodPut, "/v1/users/u-9",
		`{"user":{"user_id":"u-9","email":"me@x","role":"ROLE_ADMIN"}}`)
	if code != http.StatusOK {
		t.Fatalf("self 更新应放行, got %d", code)
	}
	if got := b.capturedUpdate(t).GetUser().GetRole(); got != pb.Role_ROLE_UNSPECIFIED {
		t.Fatalf("非 admin self 更新到达后端的 role = %v, want UNSPECIFIED（提权未被剥离）", got)
	}

	// 改他人：403 且后端不触达
	before := len(b.updateReqs)
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodPut, "/v1/users/u-other",
		`{"user":{"user_id":"u-other","role":"ROLE_ADMIN"}}`)
	if code != http.StatusForbidden {
		t.Fatalf("非 admin 改他人应 403, got %d", code)
	}
	if len(b.updateReqs) != before {
		t.Fatalf("403 路径触达了后端（门禁被绕过）")
	}
}

// R73: admin 直改（含 role=ROLE_ADMIN）必须原样透传——门禁不得误伤管理能力。
func TestUpdateUser_AdminPassthrough(t *testing.T) {
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, _ := httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", http.MethodPut, "/v1/users/u-9",
		`{"user":{"user_id":"u-9","role":"ROLE_ADMIN"}}`)
	if code != http.StatusOK {
		t.Fatalf("admin 更新应放行, got %d", code)
	}
	if got := b.capturedUpdate(t).GetUser().GetRole(); got != pb.Role_ROLE_ADMIN {
		t.Fatalf("admin 传入的 role 到达后端 = %v, want ROLE_ADMIN（透传被误伤）", got)
	}
}

// R73: GET /v1/users/{id} 资料/权限枚举面——admin 或 self 放行，他人 403。
func TestGetUser_AdminOrSelfOnly(t *testing.T) {
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodGet, "/v1/users/u-other", "")
	if code != http.StatusForbidden {
		t.Fatalf("非 admin GET 他人资料应 403, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodGet, "/v1/users/u-9", "")
	if code != http.StatusOK {
		t.Fatalf("GET 自己资料应放行, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_ADMIN", "admin-1", http.MethodGet, "/v1/users/u-other", "")
	if code != http.StatusOK {
		t.Fatalf("admin GET 他人资料应放行, got %d", code)
	}
}

// R85（复审 P1 旁路收口）: body user_id 与路径 id 错位——服务端按 body 落改，
// 非 admin 的 self 通道必须把 body 钉死到 URL 身份（此前可借 self 门禁改他人
// email/username/state，仅 role 被剥）。
func TestUpdateUser_BodyIdMismatchPinnedToPath(t *testing.T) {
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodPut, "/v1/users/u-9",
		`{"user":{"user_id":"u-other","email":"attacker@x"}}`)
	if code != http.StatusOK {
		t.Fatalf("self 更新应放行, got %d", code)
	}
	if got := b.capturedUpdate(t).GetUser().GetUserId(); got != "u-9" {
		t.Fatalf("body user_id 未被钉死到路径身份: %q（跨用户改写旁路）", got)
	}
}

// R85: 非 admin 自助可变面收窄到 email——role/state/username 一律剥离
// （state 剥离依赖服务端 UNSPECIFIED 保全；username 空=服务端保全，防改名撞占 admin）。
func TestUpdateUser_NonAdminSelfStripsStateAndUsername(t *testing.T) {
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodPut, "/v1/users/u-9",
		`{"user":{"user_id":"u-9","username":"admin","state":"USER_STATE_ACTIVE","email":"me@x"}}`)
	if code != http.StatusOK {
		t.Fatalf("self 更新应放行, got %d", code)
	}
	u := b.capturedUpdate(t).GetUser()
	if u.GetUsername() != "" {
		t.Fatalf("非 admin 自助改 username 未剥离: %q", u.GetUsername())
	}
	if u.GetState() != pb.User_USER_STATE_UNSPECIFIED {
		t.Fatalf("非 admin 自助改 state 未剥离: %v（被停用者可自复活）", u.GetState())
	}
	if u.GetEmail() != "me@x" {
		t.Fatalf("email 自助更新被误伤: %q", u.GetEmail())
	}
}

// R85: /v1/users/{id}/permissions 与资料读取同门禁（权限矩阵是资料的超集敏感面）。
func TestGetUserPermissions_AdminOrSelfOnly(t *testing.T) {
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()

	code, _ := httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodGet, "/v1/users/u-other/permissions", "")
	if code != http.StatusForbidden {
		t.Fatalf("非 admin 查他人权限矩阵应 403, got %d", code)
	}
	code, _ = httpJSONAsUser(t, tr, "ROLE_DEVELOPER", "u-9", http.MethodGet, "/v1/users/u-9/permissions", "")
	if code != http.StatusOK {
		t.Fatalf("查自己权限矩阵应放行, got %d", code)
	}
}

// R88 锁定测试（全链路）：登出 → 网关本地撤销集即时生效——同一 access token 在
// 业务路由（users/me）立即 401 "token has been revoked"；服务端纪元（R87）另管 refresh。
// 真实 JWTMiddleware 链 + 真实签发 JWT（fake 后端返回测试签发 token）。
func TestLogout_KillsGatewayAccessImmediately(t *testing.T) {
	const secret = "test-secret-r88-chain"
	b := startUserGateBackend(t)
	tr := NewTranscoder(BackendAddrs{ProjectAddr: b.addr, CallTimeoutS: 5})
	defer tr.Close()
	srv := httptest.NewServer(middleware.JWTMiddleware(secret, tr.Handler()))
	defer srv.Close()

	// fake 后端应答真实签发的 JWT（claims 与生产同形态）
	access := signAccessHandler(t, secret, "u-9")
	b.loginToken = access

	do := func(method, path, body, token string) (int, map[string]any) {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode, decodeJSONBody(t, resp)
	}

	if code, _ := do("GET", "/v1/users/me", "", access); code != http.StatusOK {
		t.Fatalf("登出前 users/me 应 200, got %d", code)
	}
	// 真实客户端登出时携带 Bearer（main.go 装配里 /v1/auth/* 免 JWT，此处带上更贴近
	// 生产形态；撤销集按 body 的 access_token 记账，与头无关）
	if code, _ := do("POST", "/v1/auth/logout", `{"access_token":"`+access+`","refresh_token":"r"}`, access); code != http.StatusOK {
		t.Fatalf("logout 应 200, got %d", code)
	}
	code, out := do("GET", "/v1/users/me", "", access)
	if code != http.StatusUnauthorized {
		t.Fatalf("登出后同一 access 应立即 401, got %d (%v)", code, out)
	}
	if out["error"] != "token has been revoked" {
		t.Fatalf("错误未锚定撤销集: %v", out)
	}
}
