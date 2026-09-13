package service

// R87 锁定测试：用户会话纪元——iat 早于纪元的 refresh token 一律拒绝。
// 触发面：登出 / 改密成功 / 管理员重置 / 停用（停用即踢）。
// 此前 Logout 只黑名单 access（且仅 GetCurrentUser 消费），被盗 refresh（7d）
// 在登出/改密后仍可持续换新 access 对。

import (
	"testing"

	"google.golang.org/grpc/status"

	v1 "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/project-service/internal/repo"
)

const epochRejection = "session has been invalidated, please login again"

// newEpochSvc + loginAsAdmin — 种子 admin/admin 走真实签发链取一对真 token。
func newEpochSvc(t *testing.T) *UserService {
	t.Helper()
	t.Setenv("CODEAUDIT_JWT_SECRET", "test-secret-r87")
	return NewUserService(repo.NewMemoryStore(true))
}

func loginAsAdmin(t *testing.T, s *UserService) (access, refresh string) {
	t.Helper()
	resp, err := s.Login("admin", "admin")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return resp.GetAccessToken(), resp.GetRefreshToken()
}

func assertEpochRejected(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 旧 refresh 仍可换新（会话纪元缺失，R87）", what)
	}
	if status.Convert(err).Message() != epochRejection {
		t.Fatalf("%s: 拒绝原因未锚定纪元守卫: %v", what, err)
	}
}

func TestRefreshToken_WithoutInvalidationStillWorks(t *testing.T) {
	s := newEpochSvc(t)
	_, refresh := loginAsAdmin(t, s)
	if _, err := s.RefreshToken(refresh); err != nil {
		t.Fatalf("未触发纪元时 refresh 应放行: %v", err)
	}
}

func TestRefreshToken_RejectsAfterLogout(t *testing.T) {
	s := newEpochSvc(t)
	access, refresh := loginAsAdmin(t, s)
	s.Logout(access)
	_, rerr := s.RefreshToken(refresh)
	assertEpochRejected(t, rerr, "登出后")
}

func TestRefreshToken_RejectsAfterPasswordChange(t *testing.T) {
	s := newEpochSvc(t)
	_, refresh := loginAsAdmin(t, s)
	if err := s.ChangePassword("user-001", "admin", "NewStrong#2026"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	_, rerr := s.RefreshToken(refresh)
	assertEpochRejected(t, rerr, "改密后")
}

func TestRefreshToken_RejectsAfterAdminReset(t *testing.T) {
	s := newEpochSvc(t)
	_, refresh := loginAsAdmin(t, s)
	if _, err := s.ResetPassword("user-001"); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	_, rerr := s.RefreshToken(refresh)
	assertEpochRejected(t, rerr, "管理员重置后")
}

func TestRefreshToken_RejectsAfterSuspension(t *testing.T) {
	s := newEpochSvc(t)
	_, refresh := loginAsAdmin(t, s)
	// 停用（state=LOCKED，管理员 UpdateUser 通道；R85 起网关已剥非 admin self 的 state）
	if _, ok := s.UpdateUser(&v1.User{UserId: "user-001", State: v1.User_USER_STATE_LOCKED}); !ok {
		t.Fatal("UpdateUser failed")
	}
	_, rerr := s.RefreshToken(refresh)
	assertEpochRejected(t, rerr, "停用后")
}

// R87 对照面：ACTIVE→ACTIVE 的"更新"不推进纪元（恢复/普通资料更新不踢旧会话）。
func TestRefreshToken_ActiveStateUpdateDoesNotInvalidate(t *testing.T) {
	s := newEpochSvc(t)
	_, refresh := loginAsAdmin(t, s)
	if _, ok := s.UpdateUser(&v1.User{UserId: "user-001", State: v1.User_USER_STATE_ACTIVE, Email: "x@y"}); !ok {
		t.Fatal("UpdateUser failed")
	}
	if _, err := s.RefreshToken(refresh); err != nil {
		t.Fatalf("ACTIVE 状态更新不应踢会话: %v", err)
	}
}
