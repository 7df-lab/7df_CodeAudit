package handler

// R84 锁定测试：JSON 转码口请求体上限 1MiB——decodeBody 无界 ReadAll，认证用户
// 一条大 body 即可 OOM 网关。错误消息锚定 body 读取失败（区分后端拨号错误）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServeHTTP_BodyCappedAt1MiB(t *testing.T) {
	tr := &Transcoder{}
	srv := httptest.NewServer(tr.Handler())
	defer srv.Close()

	big := strings.Repeat("a", 2<<20) // 2MiB > 1MiB 上限
	resp, err := http.Post(srv.URL+"/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"`+big+`","password":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 (body too large), got %d: %.120s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "read body") {
		t.Fatalf("错误未锚定 body 读取失败（疑似命中后端错误）: %.120s", body)
	}

	// 正常小 body 不受影响（路由继续工作——无后端时为可预期的拨号失败而非 400 body 错误）
	resp2, err := http.Post(srv.URL+"/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"u","password":"p"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if strings.Contains(string(b2), "read body") {
		t.Fatalf("小 body 误被上限拦截: %.120s", b2)
	}
}
