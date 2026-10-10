// agent provider descriptor（R6 完整版 v0，切片 1）：4 处硬编码（launch/session/results/seeds）
// 的数据驱动化——加一个 descriptor 条目即可拉起新 agent，4 处代码零改动。
// 依据: 主仓库 docs/designs/platform-slice1-r6-descriptor.md §2（数据模型草案）/§3（capability
// 消费规则）/§6（mock 方言生产禁载）；主仓库 baton/docs/engine-integration-req.md R6；
// ADR-240（R6 首批的 cfg 点升级为条目化）；ADR-244（本批实现决策）。
//
// 承载形态：独立 providers 文件（configs/agent_providers.yaml，与 go-config 仅提供标量/
// 字符串切片读取 API 相匹配——设计 §2 明示"或独立 providers.yaml"备选）。文件缺失 =
// 合成缺省 DSH 条目（现网零改动兼容，字节等价由既有 prompt/解析测试锁定）。
package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ProviderDescriptor — 一个可拉起的 agent provider 条目（设计 §2 数据模型）。
// 字段未声明时按零值参与校验/消费（capability 未声明=false 保守缺省，设计 §3）。
type ProviderDescriptor struct {
	ID           string       `yaml:"id" json:"id"`           // 稳定标识（首条=缺省）
	Dialect      string       `yaml:"dialect" json:"dialect"` // 方言注册表键（§2 方言注册表）
	Launch       LaunchSpec   `yaml:"launch" json:"launch"`
	Session      SessionSpec  `yaml:"session" json:"session"`
	Results      ResultsSpec  `yaml:"results" json:"results"`
	Seeds        SeedsSpec    `yaml:"seeds" json:"seeds"`
	Capabilities Capabilities `yaml:"capabilities" json:"capabilities"`
	Healthcheck  HealthSpec   `yaml:"healthcheck" json:"healthcheck"`
}

// LaunchSpec — 拉起面（硬编码之一）。EnvMap 是声明面：运行时 LLM env 仍按当前推理
// 路由分派（llmRouteEnv，ADR-227）——声明值用于校验与未来 per-provider 覆盖（ADR-244）。
// SandboxImage（切片 7）：agent 级沙箱镜像；空=全局 runtime.sandbox.image
// （dsh 条目零改动兼容）。maka 等非 bridge 形态 agent 的载体。
type LaunchSpec struct {
	BridgeCmd      string `yaml:"bridge_cmd" json:"bridge_cmd"`           // 空=legacy cfg>DSH bridge 常量
	EnvMap         string `yaml:"env_map" json:"env_map"`                 // anthropic-relay|openai-compat|""（运行时路由分派）
	PermissionMode string `yaml:"permission_mode" json:"permission_mode"` // 空=danger-full-access（ADR-187 补遗）
	SandboxImage   string `yaml:"sandbox_image" json:"sandbox_image"`     // 空=全局 cfg.Image（切片 7）
}

// SessionSpec — 会话/流协议面（硬编码之二）。Resume 是能力声明非承诺（设计 §1 不变量 4）。
type SessionSpec struct {
	Transport       string `yaml:"transport" json:"transport"`               // sse-bridge|acp-stdio|mock-inproc
	IdleConvergence string `yaml:"idle_convergence" json:"idle_convergence"` // turn-based（ADR-190/192 收敛纪律）
	Resume          string `yaml:"resume" json:"resume"`                     // none|session-id|acp-session-load
}

// ResultsSpec — 结果解析面（硬编码之三）：解析器注册名。
type ResultsSpec struct {
	Parser string `yaml:"parser" json:"parser"` // codeaudit-submit|text-final
}

// SeedsSpec — 种子面（硬编码之四）：baton brief 渲染器复用（设计 §1 不变量 3，不另发明种子机制）。
type SeedsSpec struct {
	BriefRenderer string `yaml:"brief_renderer" json:"brief_renderer"` // baton-brief
	RenderTarget  string `yaml:"render_target" json:"render_target"`   // task-card-prefix|initial-prompt
}

// Capabilities — capability profile：如实声明，缺=false（设计 §2/§3）。
type Capabilities struct {
	ToolEvents        bool `yaml:"tool_events" json:"tool_events"`
	StructuredSubmit  bool `yaml:"structured_submit" json:"structured_submit"`
	Interaction       bool `yaml:"interaction" json:"interaction"`
	SessionResume     bool `yaml:"session_resume" json:"session_resume"`
	ConcludeSemantics bool `yaml:"conclude_semantics" json:"conclude_semantics"` // conclude 注入的能力闸门（v0 校验/备案，注入面随 S2）
}

// HealthSpec — 健康探测面。
type HealthSpec struct {
	Mode string `yaml:"mode" json:"mode"` // bridge-healthz|cli-help|none
}

// 方言/注册名值域（设计 §2 方言注册表与各 spec 字段；新增=加值+适配器实现）。
var (
	knownDialects   = map[string]bool{"dsh-sse": true, "acp-stdio": true, "maka-runtime-host": true, "mock-stdio": true}
	knownTransports = map[string]bool{"sse-bridge": true, "acp-stdio": true, "mock-inproc": true}
	knownParsers    = map[string]bool{"codeaudit-submit": true, "text-final": true}
	knownRenderers  = map[string]bool{"baton-brief": true}
	knownTargets    = map[string]bool{"task-card-prefix": true, "initial-prompt": true}
	knownEnvMaps    = map[string]bool{"anthropic-relay": true, "openai-compat": true, "": true}
	knownHealth     = map[string]bool{"bridge-healthz": true, "cli-help": true, "none": true}
	knownConverge   = map[string]bool{"turn-based": true}
	// mock 方言生产禁载开关（设计 §6：outcome 概率配置只进测试环境配置，生产配置禁载）。
	mockDialectAllowEnv = "CODEAUDIT_MOCK_DIALECT_ALLOW"
)

// idRe — descriptor 稳定标识格式（DNS-1123 同型小写段，与 pack 名纪律一致）。
var idRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// DefaultDescriptor — 合成缺省 DSH 条目：providers 文件缺失/未选条目时的兼容形态，
// 值即现行硬编码行为（字节等价由 TestBuildTurnPrompt_Phase1DetectionContract 等既有
// golden 测试锁定）。capabilities 按 DSH 实况：结构化提交/工具事件/收笔语义有，
// 交互（policy=never，S1 实证）与会话恢复（resume:none）无。
func DefaultDescriptor() ProviderDescriptor {
	return ProviderDescriptor{
		ID:      "dsh",
		Dialect: "dsh-sse",
		Launch:  LaunchSpec{BridgeCmd: "", EnvMap: "", PermissionMode: "danger-full-access"},
		Session: SessionSpec{Transport: "sse-bridge", IdleConvergence: "turn-based", Resume: "none"},
		Results: ResultsSpec{Parser: "codeaudit-submit"},
		Seeds:   SeedsSpec{BriefRenderer: "baton-brief", RenderTarget: "task-card-prefix"},
		Capabilities: Capabilities{
			ToolEvents: true, StructuredSubmit: true, Interaction: false,
			SessionResume: false, ConcludeSemantics: true,
		},
		Healthcheck: HealthSpec{Mode: "bridge-healthz"},
	}
}

// LoadDescriptor — 从 providers 文件加载并选中一个条目（严格解析+引用完整性校验，
// 设计 §3：未知字段拒载，校验失败给字段路径）。语义：
//   - 文件缺失 → DefaultDescriptor()（现网零改动兼容）；
//   - selectedID 空 → 首条（设计 §2"首条=缺省"）；
//   - mock 方言条目仅在 CODEAUDIT_MOCK_DIALECT_ALLOW=1 时可载（生产禁载，§6）。
func LoadDescriptor(path, selectedID string) (ProviderDescriptor, error) {
	entries, missing, err := loadEntries(path)
	if err != nil {
		return ProviderDescriptor{}, err
	}
	if missing {
		if selectedID != "" && selectedID != DefaultDescriptor().ID {
			return ProviderDescriptor{}, fmt.Errorf("providers 文件缺失（%s）且指定了非缺省条目 %q", path, selectedID)
		}
		return DefaultDescriptor(), nil
	}
	for _, d := range entries {
		if selectedID != "" && d.ID != selectedID {
			continue
		}
		return d, nil
	}
	return ProviderDescriptor{}, fmt.Errorf("providers 文件（%s）无条目 id=%q（可选: 首条缺省=不设 agent 键）", path, selectedID)
}

// ListDescriptors — 全量条目（TP20-T2 ListAgents 数据源）：与 LoadDescriptor 同一
// 解析/校验链（含 mock 生产禁载），文件缺失=合成 DSH 单条目（现网兼容）。序=文件序
// （首条=缺省，is_default 由消费方按下标判定）。
func ListDescriptors(path string) ([]ProviderDescriptor, error) {
	entries, missing, err := loadEntries(path)
	if err != nil {
		return nil, err
	}
	if missing {
		return []ProviderDescriptor{DefaultDescriptor()}, nil
	}
	return entries, nil
}

// loadEntries — 解析+全条目校验（LoadDescriptor/ListDescriptors 共用内核）。
// missing=providers 文件不存在（调用方各自决定缺省语义）。
func loadEntries(path string) (entries []ProviderDescriptor, missing bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("read providers: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // 未知字段拒载（防部分应用，U6 同型纪律）
	if err := dec.Decode(&entries); err != nil {
		return nil, false, fmt.Errorf("providers 解析（%s）: %w", path, err)
	}
	if len(entries) == 0 {
		return nil, false, fmt.Errorf("providers 文件为空（%s）", path)
	}
	seen := map[string]bool{}
	for i := range entries {
		if err := validateDescriptor(&entries[i]); err != nil {
			return nil, false, fmt.Errorf("providers 条目 #%d: %w", i+1, err)
		}
		if seen[entries[i].ID] {
			return nil, false, fmt.Errorf("providers 条目 #%d: id %q 重复", i+1, entries[i].ID)
		}
		seen[entries[i].ID] = true
	}
	return entries, false, nil
}

// validateDescriptor — 条目级校验（引用完整性+方言不变量；错误给字段路径，D1）。
func validateDescriptor(d *ProviderDescriptor) error {
	if !idRe.MatchString(d.ID) {
		return fmt.Errorf("id: %q 不合规格（小写字母开头的 [a-z0-9-] 段）", d.ID)
	}
	if !knownDialects[d.Dialect] {
		return fmt.Errorf("dialect: %q 不在方言注册表（已知: dsh-sse/acp-stdio/maka-runtime-host/mock-stdio）", d.Dialect)
	}
	if d.Dialect == "mock-stdio" && os.Getenv(mockDialectAllowEnv) != "1" {
		return fmt.Errorf("dialect: mock-stdio 生产禁载（设计 §6）；测试环境需设 %s=1", mockDialectAllowEnv)
	}
	if !knownTransports[d.Session.Transport] {
		return fmt.Errorf("session.transport: %q 不在注册表（sse-bridge/acp-stdio/mock-inproc）", d.Session.Transport)
	}
	if !knownConverge[d.Session.IdleConvergence] {
		return fmt.Errorf("session.idle_convergence: %q 只支持 turn-based（ADR-190/192）", d.Session.IdleConvergence)
	}
	if !knownEnvMaps[d.Launch.EnvMap] {
		return fmt.Errorf("launch.env_map: %q 不在注册表（anthropic-relay/openai-compat/空=运行时路由分派）", d.Launch.EnvMap)
	}
	if !knownParsers[d.Results.Parser] {
		return fmt.Errorf("results.parser: %q 不在解析器注册表（codeaudit-submit/text-final）", d.Results.Parser)
	}
	if !knownRenderers[d.Seeds.BriefRenderer] {
		return fmt.Errorf("seeds.brief_renderer: %q 不在渲染器注册表（baton-brief）", d.Seeds.BriefRenderer)
	}
	if !knownTargets[d.Seeds.RenderTarget] {
		return fmt.Errorf("seeds.render_target: %q 不在注册表（task-card-prefix/initial-prompt）", d.Seeds.RenderTarget)
	}
	if !knownHealth[d.Healthcheck.Mode] {
		return fmt.Errorf("healthcheck.mode: %q 不在注册表（bridge-healthz/cli-help/none）", d.Healthcheck.Mode)
	}
	// 方言不变量：dsh-sse 的会话面/健康面/解析面即现行 bridge 通道（选 dsh-sse 即选该组合）。
	if d.Dialect == "dsh-sse" {
		if d.Session.Transport != "sse-bridge" || d.Healthcheck.Mode != "bridge-healthz" {
			return fmt.Errorf("dialect dsh-sse 要求 session.transport=sse-bridge 且 healthcheck.mode=bridge-healthz")
		}
	}
	if d.Dialect == "mock-stdio" && d.Session.Transport != "mock-inproc" {
		return fmt.Errorf("dialect mock-stdio 要求 session.transport=mock-inproc（进程内会话，不创建沙箱）")
	}
	// capability/解析器一致性：text-final 解析器即结构化提交不可用的声明形态（设计 §3）。
	if d.Results.Parser == "text-final" && d.Capabilities.StructuredSubmit {
		return fmt.Errorf("results.parser: text-final 与 capabilities.structured_submit=true 矛盾（诚实声明二选一）")
	}
	return nil
}

// SelectAgent — 切片 7：请求级 Agent 覆盖。agentID 空或等于当前条目=no-op
// （字节等价面）；非空时按装配期同路径重载（文件级重解析——低频任务粒度可接受，
// 严格解析与启动期同纪律）。错误如实上浮（AI 阶段诚实失败，不静默回落）。
func SelectAgent(cfg *Config, agentID string) error {
	if agentID == "" || agentID == cfg.Agent.ID {
		return nil
	}
	d, err := LoadDescriptor(cfg.ProvidersPath, agentID)
	if err != nil {
		return fmt.Errorf("任务级 agent %q 装配失败: %w", agentID, err)
	}
	cfg.Agent = d
	return nil
}
