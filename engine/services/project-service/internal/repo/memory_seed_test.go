package repo

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// R72: 种子 admin 必须开关化——默认关闭（生产无默认凭据，防 admin/admin 登入 ROLE_ADMIN）。
func TestSeedAdmin_DisabledByDefault(t *testing.T) {
	s := NewMemoryStore(false)
	if _, ok := s.GetUserByUsername("admin"); ok {
		t.Fatalf("seedAdmin=false 仍预置 admin 账号——默认凭据面回归（R72）")
	}
	if len(s.users) != 0 {
		t.Fatalf("seedAdmin=false 应得空用户表, got %d users", len(s.users))
	}
}

// R72: 显式开启时种子行为不变（admin/admin bcrypt + ROLE_ADMIN，网关 admin 门禁依赖）。
func TestSeedAdmin_ExplicitEnablePreservesContract(t *testing.T) {
	s := NewMemoryStore(true)
	rec, ok := s.GetUserByUsername("admin")
	if !ok {
		t.Fatalf("seedAdmin=true 应预置 admin")
	}
	if rec.User.GetRole().String() != "ROLE_ADMIN" {
		t.Fatalf("种子账号 role = %v, want ROLE_ADMIN", rec.User.GetRole())
	}
	if bcrypt.CompareHashAndPassword([]byte(rec.Password), []byte("admin")) != nil {
		t.Fatalf("种子密码校验失败（bcrypt 契约破坏）")
	}
}
