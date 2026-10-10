package sandbox

// descriptor 加载/校验测试（R6 完整版 v0，切片 1）。
// 依据: 主仓库 docs/designs/platform-slice1-r6-descriptor.md §2/§3/§6；DoD"含新增
// descriptor 加载校验用例"。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProviders(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "agent_providers.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 文件缺失 → 合成缺省 DSH 条目（现网零改动兼容，设计 §2 承载语义）。
func TestLoadDescriptor_MissingFileDefaultsToDSH(t *testing.T) {
	d, err := LoadDescriptor(filepath.Join(t.TempDir(), "absent.yaml"), "")
	if err != nil {
		t.Fatalf("missing file must default: %v", err)
	}
	want := DefaultDescriptor()
	if d != want {
		t.Fatalf("default descriptor = %+v, want %+v", d, want)
	}
	if d.Dialect != "dsh-sse" || !d.Capabilities.StructuredSubmit || d.Capabilities.Interaction {
		t.Fatalf("default DSH capability profile wrong: %+v", d.Capabilities)
	}
}

// 文件缺失但显式选了非缺省条目 → 如实报错（不静默回落缺省条目）。
func TestLoadDescriptor_MissingFileWithExplicitIDFails(t *testing.T) {
	_, err := LoadDescriptor(filepath.Join(t.TempDir(), "absent.yaml"), "maka")
	if err == nil || !strings.Contains(err.Error(), "maka") {
		t.Fatalf("explicit non-default id with missing file must fail naming it, got %v", err)
	}
}

// 未知字段拒载（设计 §3；U6 同型纪律）。
func TestLoadDescriptor_UnknownFieldRejected(t *testing.T) {
	p := writeProviders(t, "- id: dsh\n  dialect: dsh-sse\n  results:\n    parser: codeaudit-submit\n    unknown_knob: 1\n")
	_, err := LoadDescriptor(p, "")
	if err == nil || !strings.Contains(err.Error(), "unknown_knob") {
		t.Fatalf("unknown field must be rejected naming it, got %v", err)
	}
}

// 引用完整性：未知方言/解析器/渲染器逐项拒载并给字段路径（设计 §3，D1）。
func TestLoadDescriptor_ReferenceIntegrity(t *testing.T) {
	cases := []struct{ yaml, wantField string }{
		{"- id: prov\n  dialect: maka-unknown\n  session: {transport: mock-inproc, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: none}\n", "dialect"},
		{"- id: prov\n  dialect: dsh-sse\n  session: {transport: sse-bridge, idle_convergence: turn-based}\n  results: {parser: nope}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: bridge-healthz}\n", "results.parser"},
		{"- id: prov\n  dialect: dsh-sse\n  session: {transport: sse-bridge, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: other, render_target: task-card-prefix}\n  healthcheck: {mode: bridge-healthz}\n", "seeds.brief_renderer"},
		{"- id: prov\n  dialect: dsh-sse\n  session: {transport: carrier-pigeon, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: bridge-healthz}\n", "session.transport"},
	}
	for i, c := range cases {
		_, err := LoadDescriptor(writeProviders(t, c.yaml), "")
		if err == nil || !strings.Contains(err.Error(), c.wantField) {
			t.Fatalf("case %d: want field-path error %q, got %v", i, c.wantField, err)
		}
	}
}

// 方言不变量：dsh-sse 必须 sse-bridge+bridge-healthz；mock-stdio 必须 mock-inproc。
func TestLoadDescriptor_DialectInvariants(t *testing.T) {
	p := writeProviders(t, "- id: prov\n  dialect: dsh-sse\n  session: {transport: acp-stdio, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: bridge-healthz}\n")
	if _, err := LoadDescriptor(p, ""); err == nil || !strings.Contains(err.Error(), "dsh-sse 要求") {
		t.Fatalf("dsh-sse transport invariant must hold, got %v", err)
	}
}

// mock 方言生产禁载（设计 §6）；测试环境显式放行后可载。
func TestLoadDescriptor_MockDialectGated(t *testing.T) {
	p := writeProviders(t, "- id: mock\n  dialect: mock-stdio\n  session: {transport: mock-inproc, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  capabilities: {structured_submit: false}\n  healthcheck: {mode: none}\n")
	if _, err := LoadDescriptor(p, ""); err == nil || !strings.Contains(err.Error(), "生产禁载") {
		t.Fatalf("mock dialect must be blocked without allow env, got %v", err)
	}
	t.Setenv("CODEAUDIT_MOCK_DIALECT_ALLOW", "1")
	d, err := LoadDescriptor(p, "")
	if err != nil {
		t.Fatalf("mock dialect must load with allow env: %v", err)
	}
	if d.ID != "mock" || d.Capabilities.StructuredSubmit {
		t.Fatalf("mock entry mismatch: %+v", d)
	}
}

// 多条目：首条=缺省；agent 键选中指定条目；id 重复拒载。
func TestLoadDescriptor_SelectionAndDuplicateID(t *testing.T) {
	two := "- id: dsh\n  dialect: dsh-sse\n  session: {transport: sse-bridge, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: bridge-healthz}\n" +
		"- id: mock\n  dialect: mock-stdio\n  session: {transport: mock-inproc, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: none}\n"
	t.Setenv("CODEAUDIT_MOCK_DIALECT_ALLOW", "1")
	p := writeProviders(t, two)
	d, err := LoadDescriptor(p, "")
	if err != nil || d.ID != "dsh" {
		t.Fatalf("first entry must be default, got %v (%+v)", err, d)
	}
	d, err = LoadDescriptor(p, "mock")
	if err != nil || d.ID != "mock" {
		t.Fatalf("selection by id failed: %v (%+v)", err, d)
	}
	dup := "- id: aa\n  dialect: mock-stdio\n  session: {transport: mock-inproc, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: none}\n- id: aa\n  dialect: mock-stdio\n  session: {transport: mock-inproc, idle_convergence: turn-based}\n  results: {parser: codeaudit-submit}\n  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}\n  healthcheck: {mode: none}\n"
	if _, err := LoadDescriptor(writeProviders(t, dup), ""); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("duplicate id must be rejected, got %v", err)
	}
}

// 缺省条目经 NewManagerRunner 合成（零值 Config.Agent → DSH），既有调用方零改动。
func TestNewManagerRunner_SynthesizesDefaultDescriptor(t *testing.T) {
	r := NewManagerRunner(Config{Mode: "off"})
	if r.cfg.Agent.ID != "dsh" || r.cfg.Agent.Dialect != "dsh-sse" {
		t.Fatalf("default synthesis failed: %+v", r.cfg.Agent)
	}
	if got := r.agentBridgeCmd(); got != bridgeCmd {
		t.Fatalf("legacy bridge cmd fallback = %q, want %q", got, bridgeCmd)
	}
	if got := r.agentPermissionMode(); got != "danger-full-access" {
		t.Fatalf("permission mode fallback = %q", got)
	}
}

// 拉起面优先级：descriptor 条目值 > legacy cfg 键 > 常量（双轨过渡，ADR-244）。
func TestAgentBridgeCmd_Precedence(t *testing.T) {
	r := NewManagerRunner(Config{AgentBridgeCmd: "legacy-cmd", Agent: DefaultDescriptor()})
	if got := r.agentBridgeCmd(); got != "legacy-cmd" {
		t.Fatalf("empty descriptor cmd must fall to legacy cfg, got %q", got)
	}
	r = NewManagerRunner(Config{Agent: DefaultDescriptor()})
	if got := r.agentBridgeCmd(); got != bridgeCmd {
		t.Fatalf("empty descriptor+legacy must fall to const, got %q", got)
	}
	d := DefaultDescriptor()
	d.Launch.BridgeCmd = "custom-bridge"
	r = NewManagerRunner(Config{AgentBridgeCmd: "legacy-cmd", Agent: d})
	if got := r.agentBridgeCmd(); got != "custom-bridge" {
		t.Fatalf("descriptor cmd must win, got %q", got)
	}
}

// ---- 切片 7：任务级 Agent 选择 ----

// TestSelectAgent — 请求级覆盖语义：空=no-op（字节等价面）；同 id=no-op；
// 异 id=重载换条目；未知 id=错误（诚实失败不静默回落）。
func TestSelectAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent_providers.yaml")
	yaml := `- id: dsh
  dialect: dsh-sse
  launch: {bridge_cmd: "", env_map: "", permission_mode: danger-full-access, sandbox_image: ""}
  session: {transport: sse-bridge, idle_convergence: turn-based, resume: none}
  results: {parser: codeaudit-submit}
  seeds: {brief_renderer: baton-brief, render_target: task-card-prefix}
  capabilities: {tool_events: true, structured_submit: true, interaction: false, session_resume: false, conclude_semantics: true}
  healthcheck: {mode: bridge-healthz}
- id: maka
  dialect: maka-runtime-host
  launch: {sandbox_image: "maka-cli:s3"}
  session: {transport: acp-stdio, idle_convergence: turn-based, resume: none}
  results: {parser: text-final}
  seeds: {brief_renderer: baton-brief, render_target: initial-prompt}
  capabilities: {tool_events: false, structured_submit: false, interaction: false, session_resume: false, conclude_semantics: false}
  healthcheck: {mode: cli-help}
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{ProvidersPath: path}
	d, err := LoadDescriptor(path, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agent = d
	if cfg.Agent.ID != "dsh" {
		t.Fatalf("首条缺省应=dsh，got %s", cfg.Agent.ID)
	}
	// 空=no-op
	if err := SelectAgent(cfg, ""); err != nil || cfg.Agent.ID != "dsh" {
		t.Fatalf("空 agent 应 no-op: err=%v id=%s", err, cfg.Agent.ID)
	}
	// 同 id=no-op
	if err := SelectAgent(cfg, "dsh"); err != nil || cfg.Agent.ID != "dsh" {
		t.Fatalf("同 id 应 no-op: err=%v", err)
	}
	// 异 id=重载（maka 条目+镜像选择）
	if err := SelectAgent(cfg, "maka"); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.ID != "maka" || cfg.Agent.Launch.SandboxImage != "maka-cli:s3" {
		t.Fatalf("应换到 maka 条目（镜像 maka-cli:s3），got id=%s image=%s", cfg.Agent.ID, cfg.Agent.Launch.SandboxImage)
	}
	if effectiveImage(*cfg) != "maka-cli:s3" {
		t.Fatalf("effectiveImage 应取 descriptor 镜像，got %s", effectiveImage(*cfg))
	}
	// 未知 id=错误
	cfg2 := &Config{ProvidersPath: path, Agent: d}
	if err := SelectAgent(cfg2, "nope"); err == nil {
		t.Fatal("未知 agent 应报错（诚实失败）")
	}
}

// TestEffectiveImage_FallbackGlobal — descriptor 无镜像=全局（dsh 条目零改动兼容面）。
func TestEffectiveImage_FallbackGlobal(t *testing.T) {
	cfg := Config{Image: "dsh-pentest-sse:latest"}
	cfg.Agent = DefaultDescriptor() // sandbox_image 空
	if got := effectiveImage(cfg); got != "dsh-pentest-sse:latest" {
		t.Fatalf("空 sandbox_image 应回落全局镜像，got %s", got)
	}
}

// TestMakaEntryInRepoConfig — 仓内 configs/agent_providers.yaml 的 maka 条目可载
// （引用完整性+方言不变量全过——防条目与校验器漂移）。
func TestMakaEntryInRepoConfig(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "configs", "agent_providers.yaml")
	d, err := LoadDescriptor(path, "maka")
	if err != nil {
		t.Fatalf("仓内 maka 条目应可载: %v", err)
	}
	if d.Launch.SandboxImage != "maka-cli:s3" {
		t.Fatalf("maka 条目镜像应 maka-cli:s3，got %s", d.Launch.SandboxImage)
	}
	if d.Capabilities.StructuredSubmit || d.Results.Parser != "text-final" {
		t.Fatal("maka=非结构化声明（text-final），capability/parser 应一致")
	}
}

// TestOpencodeEntryInRepoConfig — 仓内 configs/agent_providers.yaml 的 opencode 条目
// 可载（acp-stdio 方言引用完整性+capability 诚实声明一致性——防条目与校验器漂移；
// 2026-09-23 ACP 接入批）。
func TestOpencodeEntryInRepoConfig(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "configs", "agent_providers.yaml")
	d, err := LoadDescriptor(path, "opencode")
	if err != nil {
		t.Fatalf("仓内 opencode 条目应可载: %v", err)
	}
	if d.Dialect != "dsh-sse" || d.Session.Transport != "sse-bridge" {
		t.Fatalf("opencode v1 桥形态=dsh-sse 方言+sse-bridge 传输（桥兼容层），got %s/%s", d.Dialect, d.Session.Transport)
	}
	if d.Launch.BridgeCmd != "node /opt/bridge/bridge.mjs --acp -- opencode acp" || d.Launch.SandboxImage != "opencode-cli:s1" {
		t.Fatalf("opencode 拉起面漂移: cmd=%q image=%q", d.Launch.BridgeCmd, d.Launch.SandboxImage)
	}
	if d.Capabilities.StructuredSubmit || d.Results.Parser != "text-final" {
		t.Fatal("opencode=非结构化声明（text-final），capability/parser 应一致")
	}
	if d.Capabilities.ToolEvents {
		t.Fatal("opencode tool_events=false（update 帧仅人性化渲染，无结构化消费）")
	}
}

// TestMakaAcpEntryInRepoConfig — 仓内 configs/agent_providers.yaml 的 maka-acp 条目
// 可载：v1 桥形态=dsh-sse+sse-bridge 不变量+拉起面钉扎
// （s4 entry 脚本=s3 分叉点裁决落地）+capability 诚实声明一致性。
func TestMakaAcpEntryInRepoConfig(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "configs", "agent_providers.yaml")
	d, err := LoadDescriptor(path, "maka-acp")
	if err != nil {
		t.Fatalf("仓内 maka-acp 条目应可载: %v", err)
	}
	if d.Dialect != "dsh-sse" || d.Session.Transport != "sse-bridge" || d.Healthcheck.Mode != "bridge-healthz" {
		t.Fatalf("maka-acp v1 桥形态不变量漂移: %s/%s/%s", d.Dialect, d.Session.Transport, d.Healthcheck.Mode)
	}
	if d.Launch.BridgeCmd != "bash /opt/bridge/entry-acp.sh" || d.Launch.SandboxImage != "maka-cli:s4" {
		t.Fatalf("maka-acp 拉起面漂移: cmd=%q image=%q", d.Launch.BridgeCmd, d.Launch.SandboxImage)
	}
	if d.Capabilities.StructuredSubmit || d.Results.Parser != "text-final" {
		t.Fatal("maka-acp=非结构化声明（text-final），capability/parser 应一致")
	}
	if d.Capabilities.Interaction {
		t.Fatal("maka-acp interaction=false（权限代答=桥内自动批准）")
	}
}

// TestListDescriptors — TP20-T2 ListAgents 数据源（计划 §2.4 用例①）：全量条目按
// 文件序（首条=缺省）+与 LoadDescriptor 同一校验链；消融列=文件缺失→合成 DSH 单条目。
func TestListDescriptors(t *testing.T) {
	p := writeProviders(t, `[
  {"id":"dsh","dialect":"dsh-sse",
   "session":{"transport":"sse-bridge","idle_convergence":"turn-based"},
   "results":{"parser":"codeaudit-submit"},
   "seeds":{"brief_renderer":"baton-brief","render_target":"task-card-prefix"},
   "healthcheck":{"mode":"bridge-healthz"},
   "capabilities":{"tool_events":true,"structured_submit":true}},
  {"id":"maka","dialect":"maka-runtime-host",
   "launch":{"bridge_cmd":"x","sandbox_image":"maka-cli:s4"},
   "session":{"transport":"sse-bridge","idle_convergence":"turn-based"},
   "results":{"parser":"text-final"},
   "seeds":{"brief_renderer":"baton-brief","render_target":"task-card-prefix"},
   "healthcheck":{"mode":"cli-help"},
   "capabilities":{}}
]`)
	entries, err := ListDescriptors(p)
	if err != nil {
		t.Fatalf("ListDescriptors: %v", err)
	}
	if len(entries) != 2 || entries[0].ID != "dsh" || entries[1].ID != "maka" {
		t.Fatalf("条目序漂移（首条=缺省）: %+v", entries)
	}
	// 与 LoadDescriptor 同链：选中条目字段逐面一致
	sel, err := LoadDescriptor(p, "maka")
	if err != nil || sel.ID != entries[1].ID || sel.Dialect != entries[1].Dialect {
		t.Fatalf("LoadDescriptor/ListDescriptors 链漂移: %v %+v", err, sel)
	}
	// 非法条目（mock 生产禁载）全量面同样拒绝
	p = writeProviders(t, `[{"id":"m","dialect":"mock-stdio",
		"session":{"transport":"mock-inproc","idle_convergence":"turn-based"},
		"results":{"parser":"text-final"},
		"seeds":{"brief_renderer":"baton-brief","render_target":"task-card-prefix"},
		"healthcheck":{"mode":"none"}}]`)
	if _, err := ListDescriptors(p); err == nil {
		t.Fatal("mock-stdio 生产禁载应在全量链同样拒绝")
	}

	// 消融：文件缺失 → 合成 DSH 单条目（现网兼容，与 LoadDescriptor 缺省同型）
	miss, err := ListDescriptors(filepath.Join(filepath.Dir(p), "absent.yaml"))
	if err != nil {
		t.Fatalf("缺失文件应走合成缺省: %v", err)
	}
	if len(miss) != 1 || miss[0].ID != "dsh" || miss[0].Dialect != "dsh-sse" {
		t.Fatalf("合成条目漂移: %+v", miss)
	}
}
