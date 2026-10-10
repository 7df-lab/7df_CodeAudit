package sandbox

// handoff hold 测试（R4，切片 1）：保活登记/过期/释放 + 对账过滤 + teardown 跳过回收。
// 依据: 主仓库 baton/docs/engine-integration-req.md R4（窗口内沙箱存活，窗口后正常回收）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHoldSandbox_Lifecycle(t *testing.T) {
	const name = "ca-holdtest0001"
	t.Cleanup(func() { ReleaseHold(name) })
	if sandboxHeld(name) {
		t.Fatal("fresh name must not be held")
	}
	HoldSandbox(name, 50*time.Millisecond)
	if !sandboxHeld(name) {
		t.Fatal("hold must be active within TTL")
	}
	ReleaseHold(name)
	if sandboxHeld(name) {
		t.Fatal("release must clear hold")
	}
	HoldSandbox(name, 20*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	if sandboxHeld(name) {
		t.Fatal("expired hold must read as not held")
	}
}

// 对账过滤：保活中的沙箱不是孤儿；过期后自然落回回收面（窗口后正常回收）。
func TestOrphanNames_SkipsHeld(t *testing.T) {
	const name = "ca-000000000002"
	t.Cleanup(func() { ReleaseHold(name) })
	HoldSandbox(name, time.Hour)
	refs := []managerSandboxRef{{Name: name}}
	if got := orphanNames(refs, func(string) bool { return false }); len(got) != 0 {
		t.Fatalf("held sandbox must not be orphan, got %v", got)
	}
	handoffHolds.Store(name, time.Now().Add(-time.Second)) // 直接过期
	if got := orphanNames(refs, func(string) bool { return false }); len(got) != 1 {
		t.Fatalf("expired hold must fall back to orphan, got %v", got)
	}
}

// teardown：hold 有效 → 零 DELETE（换手窗口存活）；无 hold → 照常 DELETE。
func TestTeardown_SkipsDeleteWhenHeld(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	newLS := func() *liveSession {
		r := NewManagerRunner(Config{Mode: "openshell", Workspace: "default", HTTPClientTimeout: 2 * time.Second})
		return &liveSession{r: r, url: srv.URL, token: "t",
			ref: sandboxRef{Name: "ca-holdtest0003", ID: "uuid-1"}}
	}

	const heldName = "ca-holdtest0003"
	t.Cleanup(func() { ReleaseHold(heldName) })
	HoldSandbox(heldName, time.Hour)
	ls := newLS()
	ls.teardown()
	if deletes.Load() != 0 {
		t.Fatalf("held teardown must skip DELETE, got %d", deletes.Load())
	}
	if sandboxActive(heldName) {
		t.Fatal("held teardown must still deregister from active registry (TTL 后对账可回收)")
	}
	ReleaseHold(heldName)

	ls2 := newLS()
	activeSandboxes.Store(heldName, struct{}{})
	ls2.teardown()
	if deletes.Load() != 1 {
		t.Fatalf("plain teardown must DELETE once, got %d", deletes.Load())
	}
}

// RunSession 挂起取消路径的 hold 登记（R4 接线点）：PauseSession 后 ctx 取消 →
// 沙箱名进入 holds。会话级集成走 pause_freeze 既有夹具形态（此处直测注册表语义）。
func TestHoldTTL_EnvOverride(t *testing.T) {
	t.Setenv("CODEAUDIT_SANDBOX_HOLD_TTL", "1m30s")
	if got := HoldTTL(); got != 90*time.Second {
		t.Fatalf("env override = %v, want 90s", got)
	}
	t.Setenv("CODEAUDIT_SANDBOX_HOLD_TTL", "bogus")
	if got := HoldTTL(); got != holdTTLDefault {
		t.Fatalf("bogus env must fall back to default, got %v", got)
	}
	_ = context.Background()
}
