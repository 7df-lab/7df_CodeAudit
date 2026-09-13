package middleware

// R88 锁定测试：网关本地 access 撤销集——登出即时性。
// ① 集 TTL 行为（30min=access TTL 口径，测试经 var 注入负值模拟过期+惰性清扫）；
// ② JWTMiddleware 对已撤销 token 的业务请求返回 401 "token has been revoked"。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAccessRevocation_TTLAndLazyPurge(t *testing.T) {
	oldTTL := accessRevocationTTL
	t.Cleanup(func() { accessRevocationTTL = oldTTL })

	RevokeAccess("tok-live")
	if !AccessRevoked("tok-live") {
		t.Fatal("撤销后应命中")
	}

	accessRevocationTTL = -time.Second // 注入过期：条目立即超时
	if AccessRevoked("tok-live") {
		t.Fatal("超 TTL 条目应视为未撤销")
	}
	accessRevokedMu.RLock()
	_, stillThere := accessRevoked["tok-live"]
	accessRevokedMu.RUnlock()
	if stillThere {
		t.Fatal("超 TTL 条目未被惰性清扫")
	}
}

// signAccess — 真实 HS256 签发（链路测试用；claims 形态与 project-service 签发一致）。
// nonce 用于区分同秒内的两次签发（claims 全同时签名串相同=同一 token）。
func signAccess(t *testing.T, secret, sub, nonce string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": sub, "exp": time.Now().Add(30 * time.Minute).Unix(), "iat": time.Now().Unix(),
		"type": "access", "nonce": nonce,
	})
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestJWTMiddleware_RejectsRevokedToken(t *testing.T) {
	const secret = "test-secret-r88"
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	srv := httptest.NewServer(JWTMiddleware(secret, next))
	defer srv.Close()

	do := func(token string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/tasks", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	tok := signAccess(t, secret, "u-9", "login-1")
	if code := do(tok); code != http.StatusOK {
		t.Fatalf("有效 token 应放行, got %d", code)
	}

	RevokeAccess(tok)
	called = false // 复位：第一段合法请求已置位，只统计撤销段的触达
	if code := do(tok); code != http.StatusUnauthorized {
		t.Fatalf("已撤销 token 应 401, got %d", code)
	}
	if called {
		t.Fatal("已撤销 token 触达了下游 handler")
	}

	// 同一用户重新登录拿到的新 token（不同串）不受影响
	tok2 := signAccess(t, secret, "u-9", "relogin-2")
	if code := do(tok2); code != http.StatusOK {
		t.Fatalf("重登新 token 应放行, got %d", code)
	}
}
