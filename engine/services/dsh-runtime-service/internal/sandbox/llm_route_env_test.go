// llm_route_env 分派契约测试（ADR-227）：anthropic 型路由 → anthropic-relay env +
// 路由真实模型名；openai 兼容族/路由未设置/读取失败 → 既有 deepseek env（诚实降级）。
// 与 dsh-pentest-sse/bridge.mjs 的 anthropic-relay 烘焙约定互锁（两侧不可漂移）。
package sandbox

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newInferenceManager(t *testing.T, routeJSON string, providerJSON map[string]string) *fakeManager {
	t.Helper()
	return &fakeManager{
		token:        "tok-1",
		routeJSON:    routeJSON,
		providerJSON: providerJSON,
	}
}

func anthropicRouteJSON() string {
	return `{"provider":"zhipu-anthropic","model":"glm-5.3-flash","version":49}`
}

func anthropicProviderJSON() map[string]string {
	return map[string]string{
		"zhipu-anthropic": `{"name":"zhipu-anthropic","type":"anthropic","config":{"BASE_URL":"https://open.bigmodel.cn/api/anthropic"}}`,
	}
}

func TestLLMRouteEnv_AnthropicRoute(t *testing.T) {
	fm := newInferenceManager(t, anthropicRouteJSON(), anthropicProviderJSON())
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "default"})
	got := r.llmRouteEnv(context.Background())
	want := "DSH_PROVIDER=anthropic-relay DSH_MODEL=glm-5.3-flash ANTHROPIC_API_KEY=openshell-injected"
	if got != want {
		t.Fatalf("anthropic route env:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "DEEPSEEK_") {
		t.Fatal("anthropic route must not carry deepseek env")
	}
}

func TestLLMRouteEnv_OpenAITypeKeepsDeepseekEnv(t *testing.T) {
	fm := newInferenceManager(t,
		`{"provider":"zhipu-bigmodel","model":"glm-5.3-flash","version":47}`,
		map[string]string{
			"zhipu-bigmodel": `{"name":"zhipu-bigmodel","type":"openai","config":{"OPENAI_BASE_URL":"https://example/v1"}}`,
		})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "default"})
	if got := r.llmRouteEnv(context.Background()); got != defaultLLMEnv {
		t.Fatalf("openai-type route:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

func TestLLMRouteEnv_UnsetRouteFallsBack(t *testing.T) {
	fm := newInferenceManager(t, `{"provider":"","model":"","version":0}`, nil)
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "default"})
	if got := r.llmRouteEnv(context.Background()); got != defaultLLMEnv {
		t.Fatalf("unset route:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

func TestLLMRouteEnv_RouteFetchFailureFallsBack(t *testing.T) {
	// 无 /api/v1/inference/rote 端点（404）→ 降级不 panic
	fm := &fakeManager{token: "tok-1"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "default"})
	if got := r.llmRouteEnv(context.Background()); got != defaultLLMEnv {
		t.Fatalf("route fetch failure:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

func TestLLMRouteEnv_ProviderFetchFailureFallsBack(t *testing.T) {
	// 路由指向已删除的 provider（404）→ 降级不 panic
	fm := newInferenceManager(t, anthropicRouteJSON(),
		map[string]string{"other": `{"name":"other","type":"anthropic","config":{}}`})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "default"})
	if got := r.llmRouteEnv(context.Background()); got != defaultLLMEnv {
		t.Fatalf("provider fetch failure:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

// TestRun_AnthropicRouteLaunchScript — 全生命周期：anthropic 路由在位时 bridge 拉起
// 脚本必须携带 anthropic-relay env 段（脚本接线断言，防 llmRouteEnv 与 launchScript
// 脱钩的"分派函数对了、脚本没带上"形态）。
func TestRun_AnthropicRouteLaunchScript(t *testing.T) {
	fb := &fakeBridge{script: frames_Success(stubFindings)}
	bridgeSrv := httptest.NewServer(fb.handler())
	defer bridgeSrv.Close()

	fm := &fakeManager{
		token: "tok-1", bridgeURL: bridgeSrv.URL + "/",
		routeJSON:    anthropicRouteJSON(),
		providerJSON: anthropicProviderJSON(),
	}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()

	var human, raw syncwriter
	r := NewManagerRunner(Config{
		Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1",
		Workspace: "codeaudit", Image: "dsh-pentest-sse:1.2.2",
		WaitReadyTimeoutS: 5, ExecTimeoutS: 30, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		OnHumanLog:      human.writeString,
		OnRawLog:        raw.write,
	})
	if _, err := r.Run(context.Background(), Task{
		TaskID: "task-anthropic", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它",
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	fm.mu.Lock()
	script := fm.launchScript
	fm.mu.Unlock()
	for _, part := range []string{
		"DSH_PROVIDER=anthropic-relay",
		"DSH_MODEL=glm-5.3-flash",
		"ANTHROPIC_API_KEY=openshell-injected",
	} {
		if !strings.Contains(script, part) {
			t.Fatalf("launch script missing %q: %.200s", part, script)
		}
	}
	if strings.Contains(script, "DEEPSEEK_BASE_URL") {
		t.Fatalf("anthropic route script must not carry deepseek env: %.200s", script)
	}
}
