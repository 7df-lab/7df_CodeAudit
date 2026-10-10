// llm_route_env 分派契约测试（ADR-227）：anthropic 型路由 → anthropic-relay env +
// 路由真实模型名；openai 兼容族/路由未设置/读取失败 → 既有 deepseek env（诚实降级）。
// 与 dsh-pentest-sse/bridge.mjs 的 anthropic-relay 烘焙约定互锁（两侧不可漂移）。
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// newRouteRunner — llmRouteEnv 分派用例共用的最小 runner（固定 token/workspace，指向给定 manager）。
func newRouteRunner(t *testing.T, managerURL string) *ManagerRunner {
	t.Helper()
	return NewManagerRunner(Config{Mode: "openshell", ManagerURL: managerURL, ManagerToken: "tok-1", Workspace: "default"})
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
	r := newRouteRunner(t, srv.URL)
	got := r.llmRouteEnv(context.Background())
	want := "DSH_PROVIDER=anthropic-relay DSH_MODEL='glm-5.3-flash' ANTHROPIC_API_KEY=openshell-injected"
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
	r := newRouteRunner(t, srv.URL)
	if got := r.llmRouteEnv(context.Background()); got != defaultLLMEnv {
		t.Fatalf("openai-type route:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

func TestLLMRouteEnv_UnsetRouteFallsBack(t *testing.T) {
	fm := newInferenceManager(t, `{"provider":"","model":"","version":0}`, nil)
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	if got := r.llmRouteEnv(context.Background()); got != defaultLLMEnv {
		t.Fatalf("unset route:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

func TestLLMRouteEnv_RouteFetchFailureFallsBack(t *testing.T) {
	// 无 /api/v1/inference/rote 端点（404）→ 降级不 panic
	fm := &fakeManager{token: "tok-1"}
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
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
	r := newRouteRunner(t, srv.URL)
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
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
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
		"DSH_MODEL='glm-5.3-flash'",
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

// R80: 读路径兜底——存量/直写 manager 的路由值不经白名单，拼入 launchScript 前
// 必须单引号包裹+内嵌单引号转义（bash -c 不再裸拼）。
func TestLLMRouteEnv_ModelValueShellQuoted(t *testing.T) {
	fm := newInferenceManager(t,
		`{"provider":"zhipu-anthropic","model":"m'; id","version":47}`,
		anthropicProviderJSON())
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	got := r.llmRouteEnv(context.Background())
	want := "DSH_MODEL='m'\\''; id'"
	if !strings.Contains(got, want) {
		t.Fatalf("model 值未单引号转义:\n got %q\nwant contains %q", got, want)
	}
	// R85（复审）: 单引号包裹下反引号/$()/换行均为字面量——防未来"优化"为双引号或去引号
	for _, hostile := range []string{"`id`", "$(id)", "a\nb"} {
		routeJSON, merr := json.Marshal(map[string]any{"provider": "zhipu-anthropic", "model": hostile, "version": 47})
		if merr != nil {
			t.Fatal(merr)
		}
		fm2 := newInferenceManager(t, string(routeJSON), anthropicProviderJSON())
		srv2 := httptest.NewServer(fm2.handler())
		got2 := newRouteRunner(t, srv2.URL).llmRouteEnv(context.Background())
		srv2.Close()
		if !strings.HasPrefix(got2[strings.Index(got2, "DSH_MODEL="):], `DSH_MODEL='`) {
			t.Fatalf("hostile model %q 未被单引号包裹: %q", hostile, got2)
		}
	}
}

// R80: provider type 大小写口径与写入口对齐（EqualFold）——"Anthropic" 型应走
// anthropic-relay 而非静默降级 deepseek。
func TestLLMRouteEnv_ProviderTypeCaseInsensitive(t *testing.T) {
	fm := newInferenceManager(t,
		`{"provider":"zhipu-anthropic","model":"glm-5.3-flash","version":47}`,
		map[string]string{
			"zhipu-anthropic": `{"name":"zhipu-anthropic","type":"Anthropic","config":{"BASE_URL":"https://open.bigmodel.cn/api/anthropic"}}`,
		})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	got := r.llmRouteEnv(context.Background())
	if !strings.Contains(got, "DSH_PROVIDER=anthropic-relay") {
		t.Fatalf("type=Anthropic 被静默降级 deepseek（大小写口径漂移）: %q", got)
	}
}

// --- M1/M2（provider attachment，契约 m1m2-impl-contract）---

// 直连形态开关关闭=现状逐字节回归（上线零行为变化）。
func TestLLMRouteEnv_DirectEgressOff_ByteIdentical(t *testing.T) {
	fm := newInferenceManager(t, anthropicRouteJSON(), anthropicProviderJSON())
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL) // Config.LLMDirectEgress 缺省 false
	got := r.llmRouteEnv(context.Background())
	want := "DSH_PROVIDER=anthropic-relay DSH_MODEL='glm-5.3-flash' ANTHROPIC_API_KEY=openshell-injected"
	if got != want {
		t.Fatalf("开关关闭必须现状逐字节不变:\n got %q\nwant %q", got, want)
	}
}

// 直连形态·anthropic 面：base_url 真实端点注入+假凭据退役（占位符由 supervisor 注入，
// engine 不注任何 key env）。@ai-sdk 语义 base_url 含 /v1 由 manager 写入口径保证。
func TestLLMRouteEnv_DirectEgress_AnthropicFace(t *testing.T) {
	fm := newInferenceManager(t, anthropicRouteJSON(), map[string]string{
		"zhipu-anthropic": `{"name":"zhipu-anthropic","type":"anthropic","config":{"ANTHROPIC_BASE_URL":"https://open.bigmodel.cn/api/anthropic"}}`,
	})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	r.cfg.LLMDirectEgress = true
	got := r.llmRouteEnv(context.Background())
	want := "DSH_PROVIDER=anthropic-relay DSH_MODEL='glm-5.3-flash' ANTHROPIC_BASE_URL='https://open.bigmodel.cn/api/anthropic'"
	if got != want {
		t.Fatalf("anthropic 直连 env:\n got %q\nwant %q", got, want)
	}
	for _, banned := range []string{"ANTHROPIC_API_KEY", "openshell-injected", "DEEPSEEK_"} {
		if strings.Contains(got, banned) {
			t.Fatalf("直连形态不得携带 %s（假值退役/占位符由 supervisor 注入）: %q", banned, got)
		}
	}
}

// 直连形态·openai 面：DEEPSEEK_BASE_URL=provider 的 OPENAI_BASE_URL 值（llm-deepseek
// 适配器读键），无 injected 假值。
func TestLLMRouteEnv_DirectEgress_OpenAIFace(t *testing.T) {
	fm := newInferenceManager(t,
		`{"provider":"zhipu-bigmodel","model":"glm-5.3-flash","version":47}`,
		map[string]string{
			"zhipu-bigmodel": `{"name":"zhipu-bigmodel","type":"openai","config":{"OPENAI_BASE_URL":"https://open.bigmodel.cn/api/coding/paas/v4"}}`,
		})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	r.cfg.LLMDirectEgress = true
	got := r.llmRouteEnv(context.Background())
	want := "DSH_MODEL='glm-5.3-flash' DEEPSEEK_BASE_URL='https://open.bigmodel.cn/api/coding/paas/v4'"
	if got != want {
		t.Fatalf("openai 直连 env:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "openshell-injected") || strings.Contains(got, "inference.local") {
		t.Fatalf("openai 直连形态不得残留假值/虚拟域名: %q", got)
	}
}

// 直连形态·base_url 缺失：warn+按现状形态继续（anthropic 型回落 injected 形态而非
// deepseek 缺省——读路径类型分派不变）。
func TestLLMRouteEnv_DirectEgress_MissingBaseURL_FallsBackToLegacy(t *testing.T) {
	fm := newInferenceManager(t, anthropicRouteJSON(),
		map[string]string{"zhipu-anthropic": `{"name":"zhipu-anthropic","type":"anthropic","config":{}}`})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	r.cfg.LLMDirectEgress = true
	got := r.llmRouteEnv(context.Background())
	want := "DSH_PROVIDER=anthropic-relay DSH_MODEL='glm-5.3-flash' ANTHROPIC_API_KEY=openshell-injected"
	if got != want {
		t.Fatalf("base_url 缺失须回落该类型现状形态:\n got %q\nwant %q", got, want)
	}
}

// M1：inferenceRouteSnapshot——路由快照透出；读失败→nil（挂载跳过不阻断）。
func TestInferenceRouteSnapshot(t *testing.T) {
	fm := newInferenceManager(t, anthropicRouteJSON(), anthropicProviderJSON())
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	snap := r.inferenceRouteSnapshot(context.Background())
	if snap == nil || snap.Provider != "zhipu-anthropic" || snap.Model != "glm-5.3-flash" {
		t.Fatalf("snapshot = %+v, want provider=zhipu-anthropic model=glm-5.3-flash", snap)
	}
	dead := newRouteRunner(t, "http://127.0.0.1:1") // 无人监听 → 读失败
	if got := dead.inferenceRouteSnapshot(context.Background()); got != nil {
		t.Fatalf("读失败须返回 nil（挂载跳过）, got %+v", got)
	}
}

// M1：无路由（空 provider）→ 快照非 nil 但 Provider 空（挂载条件不满足，sandboxSpec 缺省空数组保持）。
func TestInferenceRouteSnapshot_EmptyRoute(t *testing.T) {
	fm := newInferenceManager(t, `{"provider":"","model":"","version":0}`, nil)
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	snap := r.inferenceRouteSnapshot(context.Background())
	if snap == nil || snap.Provider != "" {
		t.Fatalf("空路由须返回 Provider 空的快照, got %+v", snap)
	}
}

// P2#1（code-review）：全生命周期挂载断言——RunSession 真实创建链上，路由有
// provider 时创建请求的 spec.providers 必须携带该名（防"helper 对了、spec 没挂上"
// 的分派/挂载脱节——TestRun_AnthropicRouteLaunchScript 同构防线）。
func TestRun_SpecProvidersAttached(t *testing.T) {
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
		WaitReadyTimeoutS: 5, DSHMaxTokens: 131072,
		GatewayDialAddr: strings.TrimPrefix(bridgeSrv.URL, "http://"),
		OnHumanLog:      human.writeString,
		OnRawLog:        raw.write,
	})
	if _, err := r.Run(context.Background(), Task{
		TaskID: "task-spec-prov", WorkspaceDir: newTestWorkspace(t), Assignment: "审计它",
		Timeout: 10 * time.Second,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	fm.mu.Lock()
	spec := fm.lastSpec
	fm.mu.Unlock()
	if spec == nil {
		t.Fatal("创建请求未捕获 spec（fakeManager.lastSpec）")
	}
	got, _ := spec["providers"].([]any)
	if len(got) != 1 || got[0] != "zhipu-anthropic" {
		t.Fatalf("spec.providers = %#v, want [zhipu-anthropic]（路由 provider 必须挂载）", spec["providers"])
	}
}

// P3#4（code-review）：openai 面 base_url 缺失回落——defaultLLMEnv（deepseek 缺省
// 形态，openai 型的现状分支）+无直连残留。
func TestLLMRouteEnv_DirectEgress_OpenAIMissingBaseURL_FallsBack(t *testing.T) {
	fm := newInferenceManager(t,
		`{"provider":"zhipu-bigmodel","model":"glm-5.3-flash","version":47}`,
		map[string]string{
			"zhipu-bigmodel": `{"name":"zhipu-bigmodel","type":"openai","config":{}}`,
		})
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := newRouteRunner(t, srv.URL)
	r.cfg.LLMDirectEgress = true
	got := r.llmRouteEnv(context.Background())
	if got != defaultLLMEnv {
		t.Fatalf("openai 面 base_url 缺失须回落 defaultLLMEnv:\n got %q\nwant %q", got, defaultLLMEnv)
	}
}

// ===== ADR-257②: 路由缺失分类 + llmRouteNote + 回合错误包装 =====

// TestLLMRouteNote_WorkspaceRoute404CarriesRemediation — 404（工作区未注册路由）:
// note 含工作区名与修复指引；env 仍 defaultLLMEnv（行为不回归）。
func TestLLMRouteNote_WorkspaceRoute404CarriesRemediation(t *testing.T) {
	fm := newInferenceManager(t, "", nil) // 不设 routeJSON → inference 面 404
	srv := httptest.NewServer(fm.handler())
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "sim-e2e-team"})
	got := r.llmRouteEnv(context.Background())
	if got != defaultLLMEnv {
		t.Fatalf("404 路径 env 必须仍为缺省 deepseek 形态（行为不回归）:\n got %q", got)
	}
	if r.llmRouteNote == "" {
		t.Fatal("404 必须产出 llmRouteNote（可行动失败面）")
	}
	for _, want := range []string{"sim-e2e-team", "未注册推理路由", "PUT /api/v1/inference/providers"} {
		if !strings.Contains(r.llmRouteNote, want) {
			t.Fatalf("note 缺可行动要素 %q:\n%s", want, r.llmRouteNote)
		}
	}
}

// TestLLMRouteNote_Non404ReadError — 非 404 读故障（500）: note=读取失败语义（非"未注册"）。
func TestLLMRouteNote_Non404ReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/inference/route") {
			w.WriteHeader(500)
			fmt.Fprintf(w, `{"error":"boom"}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	r := NewManagerRunner(Config{Mode: "openshell", ManagerURL: srv.URL, ManagerToken: "tok-1", Workspace: "default"})
	_ = r.llmRouteEnv(context.Background())
	if !strings.Contains(r.llmRouteNote, "推理路由读取失败") || strings.Contains(r.llmRouteNote, "未注册") {
		t.Fatalf("非 404 应归类读取失败而非未注册:\n%s", r.llmRouteNote)
	}
}

// TestWrapTurnErrWithRouteNote — 纯函数契约:空 note/空 err 原样返回;非空=前缀+保留原错误链。
func TestWrapTurnErrWithRouteNote(t *testing.T) {
	base := fmt.Errorf("dsh turn error: SSE ended")
	if got := wrapTurnErrWithRouteNote("", base); got != base {
		t.Fatalf("空 note 必须原样返回")
	}
	if got := wrapTurnErrWithRouteNote("某 note", nil); got != nil {
		t.Fatalf("空 err 必须原样返回")
	}
	wrapped := wrapTurnErrWithRouteNote("工作区未注册路由", base)
	if !strings.Contains(wrapped.Error(), "推理路由缺失") || !strings.Contains(wrapped.Error(), "SSE ended") {
		t.Fatalf("包装须含前缀且保留原错误:\n%v", wrapped)
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("包装必须保留原错误链（errors.Is）")
	}
}
