package middleware

// 第四轮审计 P1 锁定测试：JWT 中间件必须拒绝 type≠access 的令牌——refresh(7d)
// 与 access 同为 HS256、仅 type 可辨；不校验则被盗 refresh 可直接作 Bearer 打全部
// 业务路由至多 7 天，且 R87 纪元/R88 撤销集（只掐 access 刷新链路）双双失效。

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func mintTypedToken(secret, typ string) string {
	claims := jwt.MapClaims{
		"sub":  "user-a",
		"role": "USER",
		"exp":  time.Now().Add(time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	}
	if typ != "" {
		claims["type"] = typ
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, _ := tok.SignedString([]byte(secret))
	return s
}

func TestJWTMiddleware_RejectsNonAccessToken(t *testing.T) {
	const secret = "test-secret-type"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := JWTMiddleware(secret, next)

	cases := []struct {
		typ  string
		want int
		what string
	}{
		{"refresh", http.StatusUnauthorized, "refresh 型作 Bearer（被盗 7d 凭据）"},
		{"", http.StatusUnauthorized, "缺 type claim（签发面恒携带，缺失即异常来源，fail-closed）"},
		{"access", http.StatusOK, "access 型正常放行"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
		req.Header.Set("Authorization", "Bearer "+mintTypedToken(secret, c.typ))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("%s: want %d got %d（body=%s）", c.what, c.want, rec.Code, rec.Body.String())
		}
	}
}
