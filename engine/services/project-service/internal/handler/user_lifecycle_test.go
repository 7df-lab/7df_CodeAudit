package handler_test

// V2.1 (ADR-205): 用户生命周期五接口单测——注册策略/幂等/角色/改密/重置。
// 密码策略数值见 07 §账号安全（ADR-205 提案行）。

import (
	"context"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/handler"
	"github.com/codeaudit/services/project-service/internal/idempotency"
	"github.com/codeaudit/services/project-service/internal/repo"
	"github.com/codeaudit/services/project-service/internal/service"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const strongPW = "passw0rd-X"

func setupUserLifecycle(t *testing.T, mode string, codes []string) *handler.UserHandler {
	t.Helper()
	t.Setenv("CODEAUDIT_JWT_SECRET", "test-secret-r65") // R65 fail-fast 后测试须显式供密钥
	store := repo.NewMemoryStore(true)
	idm := idempotency.New()
	svc := service.NewUserService(store)
	svc.SetAuthConfig(service.AuthConfig{RegistrationMode: mode, InviteCodes: codes})
	return handler.NewUserHandler(svc, idm)
}

func registerReq(reqID, username, email, pw, invite string) *v1.RegisterUserRequest {
	return &v1.RegisterUserRequest{
		Metadata:   &v1.RequestMetadata{RequestId: reqID},
		Username:   username,
		Email:      email,
		Password:   pw,
		InviteCode: invite,
	}
}

func TestRegisterUser_OpenMode_Success(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	resp, err := h.RegisterUser(context.Background(), registerReq("req-r1", "alice", "alice@x.io", strongPW, ""))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.GetAccessToken() == "" || resp.GetRefreshToken() == "" {
		t.Fatal("register should issue token pair (register-as-login, ADR-205)")
	}
	user, err := h.GetUser(context.Background(), &v1.GetUserRequest{UserId: subOf(t, resp.GetAccessToken())})
	if err != nil {
		t.Fatalf("get registered user: %v", err)
	}
	if user.GetRole() != v1.Role_ROLE_DEVELOPER {
		t.Fatalf("self-registered role = %v, want ROLE_DEVELOPER (不可自封 admin)", user.GetRole())
	}
}

func TestRegisterUser_InvitationMode(t *testing.T) {
	h := setupUserLifecycle(t, "invitation", []string{"INVITE-123"})
	if _, err := h.RegisterUser(context.Background(), registerReq("req-r2", "bob", "bob@x.io", strongPW, "")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing invite: got %v, want FailedPrecondition", err)
	}
	if _, err := h.RegisterUser(context.Background(), registerReq("req-r3", "bob", "bob@x.io", strongPW, "WRONG")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("wrong invite: got %v, want FailedPrecondition", err)
	}
	if _, err := h.RegisterUser(context.Background(), registerReq("req-r4", "bob", "bob@x.io", strongPW, "INVITE-123")); err != nil {
		t.Fatalf("valid invite register: %v", err)
	}
}

func TestRegisterUser_DisabledMode(t *testing.T) {
	h := setupUserLifecycle(t, "disabled", nil)
	if _, err := h.RegisterUser(context.Background(), registerReq("req-r5", "carol", "c@x.io", strongPW, "")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled mode: got %v, want FailedPrecondition", err)
	}
}

func TestRegisterUser_Validation(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	cases := []struct {
		name     string
		username string
		email    string
		pw       string
		want     codes.Code
	}{
		{"bad username", "a", "a@x.io", strongPW, codes.InvalidArgument},
		{"bad email", "alice", "not-an-email", strongPW, codes.InvalidArgument},
		{"weak password", "alice", "a@x.io", "short1", codes.InvalidArgument},
		{"no digit", "alice", "a@x.io", "onlyletters", codes.InvalidArgument},
	}
	for _, tc := range cases {
		if _, err := h.RegisterUser(context.Background(), registerReq("req-"+tc.name, tc.username, tc.email, tc.pw, "")); status.Code(err) != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := h.RegisterUser(context.Background(), &v1.RegisterUserRequest{Username: "alice"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing metadata: got %v, want InvalidArgument (03 §2)", err)
	}
}

func TestRegisterUser_DuplicateUsername(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	if _, err := h.RegisterUser(context.Background(), registerReq("req-d1", "alice", "a@x.io", strongPW, "")); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := h.RegisterUser(context.Background(), registerReq("req-d2", "alice", "other@x.io", strongPW, "")); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate: got %v, want AlreadyExists", err)
	}
	if _, err := h.RegisterUser(context.Background(), registerReq("req-d3", "alice2", "a@x.io", strongPW, "")); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate email: got %v, want AlreadyExists", err)
	}
}

func TestRegisterUser_IdempotentReplay(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	r1, err := h.RegisterUser(context.Background(), registerReq("req-same", "alice", "a@x.io", strongPW, ""))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := h.RegisterUser(context.Background(), registerReq("req-same", "alice", "a@x.io", strongPW, ""))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if r1.GetAccessToken() != r2.GetAccessToken() {
		t.Fatal("same request_id+body must replay cached response (R4/R6)")
	}
}

func TestListUsers_FilterAndPagination(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	ctx := context.Background()
	for i, name := range []string{"dev-one", "dev-two", "ops-three"} {
		if _, err := h.RegisterUser(ctx, registerReq("req-l"+string(rune('a'+i)), name, name+"@x.io", strongPW, "")); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	resp, err := h.ListUsers(ctx, &v1.ListUsersRequest{
		Pagination:      &v1.PaginationRequest{PageSize: 1},
		UsernameContains: "dev",
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.GetUsers()) != 1 || !resp.GetPagination().GetHasNext() {
		t.Fatalf("page1 = %d users, hasNext=%v; want 1/true", len(resp.GetUsers()), resp.GetPagination().GetHasNext())
	}
	page2, err := h.ListUsers(ctx, &v1.ListUsersRequest{
		Pagination:      &v1.PaginationRequest{Cursor: resp.GetPagination().GetNextCursor(), PageSize: 1},
		UsernameContains: "dev",
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.GetUsers()) != 1 || page2.GetPagination().GetHasNext() {
		t.Fatalf("page2 = %d users, hasNext=%v; want 1/false", len(page2.GetUsers()), page2.GetPagination().GetHasNext())
	}
}

func TestCreateUser_TempPasswordSemantics(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	u, err := h.CreateUser(context.Background(), &v1.CreateUserRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-c1"},
		Username: "carol", Email: "carol@x.io", Password: strongPW, Role: v1.Role_ROLE_VIEWER,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !u.GetMustChangePassword() {
		t.Fatal("admin-created user must carry must_change_password=true")
	}
	if u.GetRole() != v1.Role_ROLE_VIEWER {
		t.Fatalf("role = %v, want requested ROLE_VIEWER", u.GetRole())
	}
	// 缺省角色 → DEVELOPER
	u2, err := h.CreateUser(context.Background(), &v1.CreateUserRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-c2"},
		Username: "dave", Email: "dave@x.io", Password: strongPW,
	})
	if err != nil {
		t.Fatalf("create default role: %v", err)
	}
	if u2.GetRole() != v1.Role_ROLE_DEVELOPER {
		t.Fatalf("default role = %v, want ROLE_DEVELOPER", u2.GetRole())
	}
}

func TestChangePassword_Flow(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	ctx := context.Background()
	u, err := h.CreateUser(ctx, &v1.CreateUserRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-p0"},
		Username: "erin", Email: "erin@x.io", Password: strongPW,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 旧密码错误 → Unauthenticated
	if _, err := h.Login(ctx, &v1.LoginRequest{Username: "erin", Password: "wrong-pass1"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong password login: got %v, want Unauthenticated", err)
	}
	// 正确改密
	if _, err := h.ChangePassword(ctx, &v1.ChangePasswordRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-p1"},
		UserId:   u.GetUserId(), OldPassword: strongPW, NewPassword: "newpass99-Y",
	}); err != nil {
		t.Fatalf("change: %v", err)
	}
	fresh, _ := h.GetUser(ctx, &v1.GetUserRequest{UserId: u.GetUserId()})
	if fresh.GetMustChangePassword() {
		t.Fatal("must_change_password should be cleared after ChangePassword")
	}
	if _, err := h.Login(ctx, &v1.LoginRequest{Username: "erin", Password: "newpass99-Y"}); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
}

func TestResetPassword_FlowAndIdempotency(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	ctx := context.Background()
	u, _ := h.CreateUser(ctx, &v1.CreateUserRequest{
		Metadata: &v1.RequestMetadata{RequestId: "req-x0"},
		Username: "frank", Email: "frank@x.io", Password: strongPW,
	})
	r1, err := h.ResetPassword(ctx, &v1.ResetPasswordRequest{Metadata: &v1.RequestMetadata{RequestId: "req-x1"}, UserId: u.GetUserId()})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	r2, err := h.ResetPassword(ctx, &v1.ResetPasswordRequest{Metadata: &v1.RequestMetadata{RequestId: "req-x1"}, UserId: u.GetUserId()})
	if err != nil {
		t.Fatalf("reset replay: %v", err)
	}
	if r1.GetTemporaryPassword() != r2.GetTemporaryPassword() {
		t.Fatal("same request_id replay must return the same temp password")
	}
	if _, err := h.Login(ctx, &v1.LoginRequest{Username: "frank", Password: r1.GetTemporaryPassword()}); err != nil {
		t.Fatalf("login with temp password: %v", err)
	}
	fresh, _ := h.GetUser(ctx, &v1.GetUserRequest{UserId: u.GetUserId()})
	if !fresh.GetMustChangePassword() {
		t.Fatal("reset must set must_change_password=true")
	}
}

func TestLogin_BcryptSeedAndRoleClaim(t *testing.T) {
	h := setupUserLifecycle(t, "invitation", nil)
	resp, err := h.Login(context.Background(), &v1.LoginRequest{Username: "admin", Password: "admin"})
	if err != nil {
		t.Fatalf("seed admin login (bcrypt): %v", err)
	}
	role := roleOf(t, resp.GetAccessToken())
	if role != "ROLE_ADMIN" {
		t.Fatalf("role claim = %q, want ROLE_ADMIN (网关 admin 门禁依赖)", role)
	}
}

// subOf / roleOf — 解析 access token claims（HS256，与 service 侧同密钥缺省值）。
func parseClaims(t *testing.T, token string) jwt.MapClaims {
	t.Helper()
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(tk *jwt.Token) (interface{}, error) {
		return []byte("test-secret-r65"), nil // R65: 与 setupUserLifecycle 供的密钥一致
	})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	return claims
}

func subOf(t *testing.T, token string) string {
	t.Helper()
	sub, _ := parseClaims(t, token)["sub"].(string)
	if sub == "" {
		t.Fatal("empty sub claim")
	}
	return sub
}

func roleOf(t *testing.T, token string) string {
	t.Helper()
	role, _ := parseClaims(t, token)["role"].(string)
	return role
}

// R43（2026-09-11 修复批次）: ListUsers 游标超界必须钳制返回空页——end 有钳而
// offset 无，大 cursor 触发 slice 越界 panic（admin 面 DoS）。对齐 ListProjects 口径。
func TestListUsers_CursorBeyondEnd_EmptyPage(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	for i, name := range []string{"usr1", "usr2", "usr3"} {
		if _, err := h.CreateUser(context.Background(), &v1.CreateUserRequest{
			Metadata: &v1.RequestMetadata{RequestId: "req-lu" + string(rune('0'+i))},
			Username: name, Email: name + "@x.io", Password: strongPW,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	req := &v1.ListUsersRequest{Pagination: &v1.PaginationRequest{Cursor: "99999"}}
	resp, err := h.ListUsers(context.Background(), req)
	if err != nil {
		t.Fatalf("ListUsers cursor beyond end: %v", err)
	}
	if len(resp.GetUsers()) != 0 {
		t.Fatalf("expected empty page, got %d users", len(resp.GetUsers()))
	}
	if resp.GetPagination().GetHasNext() {
		t.Fatal("no next expected beyond end")
	}
}

// （D2 裁决 2026-09-11）: token TTL 对齐契约 30min/7d——实现 1h/24h 与契约
// （03 §4 / configs yaml access_ttl_min=30·refresh_ttl_day=7 / gateway taskwatch 推导）
// 三方冲突；改代码对齐（现网影响仅 token 提前续期）。exp-iat 直读 JWT claims 断言。
func TestTokenTTL_MatchesContract(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	resp, err := h.RegisterUser(context.Background(), registerReq("req-ttl", "ttluser", "ttl@x.io", strongPW, ""))
	if err != nil {
		t.Fatal(err)
	}
	ttlOf := func(tok string) int64 {
		var claims jwt.MapClaims
		if _, _, err := jwt.NewParser().ParseUnverified(tok, &claims); err != nil {
			t.Fatalf("parse token: %v", err)
		}
		iat, iok := claims["iat"].(float64)
		exp, eok := claims["exp"].(float64)
		if !iok || !eok {
			t.Fatalf("missing iat/exp claims")
		}
		return int64(exp) - int64(iat)
	}
	if got := ttlOf(resp.GetAccessToken()); got != 30*60 {
		t.Fatalf("access ttl = %ds, want 1800 (30min, 03 §4)", got)
	}
	if got := ttlOf(resp.GetRefreshToken()); got != 7*24*3600 {
		t.Fatalf("refresh ttl = %ds, want 604800 (7d, 03 §4)", got)
	}
}

// R65（2026-09-12 待办收尾·账号面三件）：
// ①登录失败文案必须合一——"user not found"vs"invalid password"经网关透传构成用户名枚举
func TestLogin_UnifiedFailureMessage(t *testing.T) {
	h := setupUserLifecycle(t, "open", nil)
	if _, err := h.RegisterUser(context.Background(), registerReq("r65a", "realuser65", "real65@x.io", strongPW, "")); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, errExist := h.Login(context.Background(), &v1.LoginRequest{Username: "realuser65", Password: "wrong-pass-X"})
	_, errNo := h.Login(context.Background(), &v1.LoginRequest{Username: "ghost65", Password: "wrong-pass-X"})
	if errExist == nil || errNo == nil {
		t.Fatal("both logins must fail")
	}
	if errExist.Error() != errNo.Error() {
		t.Fatalf("failure messages must be identical (anti user-enumeration, R65):\n%q\n%q", errExist.Error(), errNo.Error())
	}
}

