package middleware

// ADR-212 回归：限流键此前=原始 Authorization 头且位于 JWT 之外——任意垃圾头
// 每请求换新桶，限流对最该限的对象完全失效且桶无界增长。修复后保护链上键取
// JWT sub（context UserIDKey），未认证回落客户端 IP。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimit_KeyedByJWTSub(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := RateLimitMiddleware(false, 2, inner)

	// 同一 sub 打满 2 次（bucket=2/min）
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/v1/tasks", nil)
		req = req.WithContext(context.WithValue(req.Context(), UserIDKey, "alice"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d for alice: want 200, got %d", i, rec.Code)
		}
	}
	req := httptest.NewRequest("GET", "/v1/tasks", nil)
	req = req.WithContext(context.WithValue(req.Context(), UserIDKey, "alice"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice 3rd req: want 429, got %d", rec.Code)
	}

	// 垃圾 Authorization 头不再换桶：bob 不受 alice 限流影响，且无 sub 的
	// 请求按 IP 与 alice 分桶
	req2 := httptest.NewRequest("GET", "/v1/tasks", nil)
	req2.Header.Set("Authorization", "Bearer junk-1")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("request without sub must key by IP (separate bucket), got %d", rec2.Code)
	}
}
