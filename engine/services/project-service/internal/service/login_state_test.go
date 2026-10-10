package service

// 第四轮审计 P1 锁定测试：Login 在密码验证通过后必须校验 user.state——不校验则
// 停用用户（R87 推进纪元"停用即踢"后）直接重登即拿全新 token 对，停用语义整体
// 落空。密码已验证=身份已知，专用错误文案不构成用户名枚举面。

import (
	"strings"
	"testing"

	v1 "github.com/codeaudit/proto-gen"
)

func TestLogin_RejectsDisabledUser(t *testing.T) {
	s := newEpochSvc(t)
	u, err := s.CreateUser("bob", "bob@example.com", "bob-password-1", v1.Role_ROLE_DEVELOPER)

	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// ACTIVE 态可正常登录
	if _, err := s.Login("bob", "bob-password-1"); err != nil {
		t.Fatalf("ACTIVE 用户登录应放行: %v", err)
	}
	// 停用（UpdateUser 的 state 离开 ACTIVE 会推进会话纪元=R87"停用即踢"）
	if _, ok := s.UpdateUser(&v1.User{UserId: u.GetUserId(), State: v1.User_USER_STATE_INACTIVE}); !ok {
		t.Fatalf("disable user failed")
	}
	_, err = s.Login("bob", "bob-password-1")
	if err == nil {
		t.Fatalf("停用用户重登拿到新 token 对——R87 停用即踢被整体绕过")
	}
	if !strings.Contains(err.Error(), "not active") {
		t.Fatalf("拒绝原因应为账号态而非凭据错误（密码本已验证）: %v", err)
	}
}
