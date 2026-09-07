package middleware

// ADR-212 回归：ADR-172 专为浏览器 WS 接受 ?token=<JWT>，但日志中间件打
// RequestURI 全文把有效令牌写进网关日志。脱敏必须只动 token 的值。

import (
	"net/url"
	"testing"
)

func TestRedactToken(t *testing.T) {
	// 多参数场景只断言语义（token 值被替换、其余参数保留），不断言参数顺序
	// ——url.Encode 会按字典序归一化。
	multi := map[string]struct{ in string; drop map[string]string }{
		"token-first":  {"/v1/tasks/t-1/ws?token=abc&expand=1", map[string]string{"token": "REDACTED", "expand": "1"}},
		"token-middle": {"/v1/tasks/t-1/ws?expand=1&token=zz", map[string]string{"token": "REDACTED", "expand": "1"}},
	}
	for name, c := range multi {
		got := redactToken(c.in)
		u, err := url.ParseRequestURI(got)
		if err != nil {
			t.Fatalf("%s: %q unparsable: %v", name, got, err)
		}
		q := u.Query()
		for k, want := range c.drop {
			if q.Get(k) != want {
				t.Fatalf("%s: %s=%q, want %q (uri %q)", name, k, q.Get(k), want, got)
			}
		}
	}

	exact := []struct{ in, want string }{
		{"/v1/tasks/t-1/ws?token=abc.def.ghi", "/v1/tasks/t-1/ws?token=REDACTED"},
		{"/v1/tasks/t-1?limit=10", "/v1/tasks/t-1?limit=10"},
		{"/healthz", "/healthz"},
	}
	for _, c := range exact {
		if got := redactToken(c.in); got != c.want {
			t.Fatalf("redactToken(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}
