package sandbox

// R76 锁定测试：07 §8 超时矩阵接线——Task/SessionTask.Timeout 施加 ctx deadline，
// HTTPClientTimeout 作挂起兜底。原字段填值后零消费（死字段）：manager/bridge 挂起即
// 永久挂起，沙箱留 activeSandboxes 被 reconciler 永久跳过。
// 测试形态：create 路由悬挂 30s 的假 manager，断言 Run/RunSession 在时限内带
// deadline 错误返回（Run 同步跑会连带拖死变异运行，故 goroutine+select 断言）。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newHangingManager — 指定路由悬挂（直到客户端断开或 30s 上限），其余走契约假件。
func newHangingManager(t *testing.T, hangPath string) *httptest.Server {
	t.Helper()
	fm := &fakeManager{}
	inner := fm.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == hangPath {
			go func() { _, _ = io.Copy(io.Discard, r.Body) }() // 排空 body：服务端 abort 检测需 body 已消费
			select {
			case <-r.Context().Done(): // 客户端超时断开即清理
			case <-time.After(30 * time.Second):
			}
			return
		}
		inner.ServeHTTP(w, r)
	}))
	// 先强断连接再 Close：悬挂 handler 未消费请求体，服务端察觉不到客户端取消，
	// 直接 Close 会等满悬挂时长（实测 30s/条）。
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

// waitInterrupted — 被测调用必须在 5s 内返回（否则超时矩阵未接线）。
func waitInterrupted(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s 未被时限打断（R76 超时接线缺失）", what)
		return nil
	}
}

func TestRun_TaskTimeoutStopsHangingLaunch(t *testing.T) {
	srv := newHangingManager(t, "/api/v1/sandboxes") // launch 首步 create 悬挂
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL,
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 5,
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), Task{
			TaskID: "t-r76-timeout", WorkspaceDir: newTestWorkspace(t), Assignment: "x",
			Timeout: 300 * time.Millisecond,
		})
		done <- err
	}()
	err := waitInterrupted(t, done, "Run(t.Timeout=300ms)")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestRunSession_TaskTimeoutStopsHangingLaunch(t *testing.T) {
	srv := newHangingManager(t, "/api/v1/sandboxes")
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL,
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 5,
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.RunSession(context.Background(), SessionTask{
			TaskID: "t-r76-sess", Prompts: []string{"p"},
			Timeout: 300 * time.Millisecond,
		})
		done <- err
	}()
	err := waitInterrupted(t, done, "RunSession(t.Timeout=300ms)")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}

func TestManagerRunner_HTTPClientTimeoutBoundsHang(t *testing.T) {
	srv := newHangingManager(t, "/api/v1/sandboxes")
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL,
		Workspace: "w", Image: "img", WaitReadyTimeoutS: 5,
		HTTPClientTimeout: 300 * time.Millisecond, // 挂起兜底（Timeout=0 不施加外层时限）
	})
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(context.Background(), Task{
			TaskID: "t-r76-hc", WorkspaceDir: newTestWorkspace(t), Assignment: "x",
		})
		done <- err
	}()
	err := waitInterrupted(t, done, "Run(HTTPClientTimeout=300ms 兜底)")
	// ResponseHeaderTimeout 错误形态：net/http: timeout awaiting response headers
	if err == nil || !(strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout") ||
		strings.Contains(err.Error(), "timeout awaiting response headers")) {
		t.Fatalf("want client/header timeout, got %v", err)
	}
}
