// Package pack — workload pack 文件格式 v0：schema、校验器与 canonical 哈希。
// 依据: 主仓库 docs/designs/platform-slice2-pack-schema.md §2（schema 草案）/§1（pack=
// 租户侧平台资源对象，非 engine 部署配置；不含编排逻辑 v0 铁律）；公理锚定 C1/C3/C4/D1；
// ADR-245（v0 落点=task-service：任务创建即钉修订版的唯一消费方）。
//
// 校验纪律（D1）：一切拒载错误带字段路径；未知字段拒载（防部分应用）；模板与 schema
// 引用一律锚定不可变版本（@commit/@tag，浮动引用拒载）；已知引用集经 Refs 注入——
// 桌面演练/新 pack 品类扩集不返工 schema。
package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// APIVersionV0 / KindWorkloadPack — 文件头契约（设计 §2）。
	APIVersionV0      = "codeaudit.pack/v0"
	KindWorkloadPack  = "WorkloadPack"
	dialectDSHSSE     = "dsh-sse"
	profileRestricted = "restricted"
	profileTargeted   = "targeted"
)

// Pack — workload pack 顶层文件形状（设计 §2 schema 草案；字段增删=设计级变更）。
type Pack struct {
	APIVersion  string `yaml:"apiVersion" json:"apiVersion"`
	Kind        string `yaml:"kind" json:"kind"`
	Name        string `yaml:"name" json:"name"` // 全局唯一，DNS-1123 风格
	Description string `yaml:"description" json:"description"`
	Spec        Spec   `yaml:"spec" json:"spec"`
}

// Spec — "一类负载怎么跑"的声明面（编排逻辑一律不入，C4）。
type Spec struct {
	Image             string       `yaml:"image" json:"image"`     // 沙箱镜像引用（三层链产物）
	Dialect           string       `yaml:"dialect" json:"dialect"` // 引用切片 1 方言注册表
	Network           NetworkSpec  `yaml:"network" json:"network"`
	Seeds             SeedsSpec    `yaml:"seeds" json:"seeds"`
	Results           ResultsSpec  `yaml:"results" json:"results"`
	Timeouts          TimeoutsSpec `yaml:"timeouts" json:"timeouts"`
	ConstraintsSchema []string     `yaml:"constraints_schema" json:"constraints_schema"` // 本 pack 接受的 task 级约束键
	// Execution — 执行形态（切片 5+6，ADR-249；依据=主仓库 platform-slice5-6-graph-engine.md §5）：
	// ""（缺省=session，现网单会话路径零变化）|graph（GraphRunner 串行图）。
	Execution string    `yaml:"execution" json:"execution"`
	Graph     GraphSpec `yaml:"graph" json:"graph"` // execution=graph 时必填（零值拒载，防部分应用）
}

// GraphSpec — 图执行配置（设计 §5 schema 增量，向后兼容：execution 缺省 session 时
// 本段必须为零值）。0 值数值=消费侧缺省注入（max_nodes=24/max_planner_replans=5/
// max_attempts_per_node=2/timeouts 180|900|300 秒）。
type GraphSpec struct {
	SeedIntents []string         `yaml:"seed_intents" json:"seed_intents"` // 初始待办（固定阶段/初始攻击面）
	Constraints GraphConstraints `yaml:"constraints" json:"constraints"`
	Timeouts    GraphTimeouts    `yaml:"timeouts" json:"timeouts"` // 秒
}

// GraphConstraints — 图规模与重试封顶（超限拒绝声明+留痕，D1 不静默截断）。
type GraphConstraints struct {
	MaxNodes           int      `yaml:"max_nodes" json:"max_nodes"`
	AllowedTypes       []string `yaml:"allowed_types" json:"allowed_types"` // ⊆ {planner,executor,verifier}
	MaxPlannerReplans  int      `yaml:"max_planner_replans" json:"max_planner_replans"`
	MaxAttemptsPerNode int      `yaml:"max_attempts_per_node" json:"max_attempts_per_node"`
}

// GraphTimeouts — 类型化回合超时（B3 三权，pack 可覆写缺省档）。
type GraphTimeouts struct {
	Planner  int `yaml:"planner" json:"planner"`
	Executor int `yaml:"executor" json:"executor"`
	Verifier int `yaml:"verifier" json:"verifier"`
}

// GraphNodeTypes — 图节点类型全集（B3 三权；v0 固定三类型，扩集=设计级变更）。
var GraphNodeTypes = map[string]bool{"planner": true, "executor": true, "verifier": true}

// NetworkSpec — 网络档位（授权目标网段不在 pack 里——那是 task 的资源字段，运行时叠加）。
type NetworkSpec struct {
	Profile string `yaml:"profile" json:"profile"` // restricted|targeted
}

// SeedsSpec — 恒定种子契约（baton brief 复用，切片 1 §1 不变量 3）。
type SeedsSpec struct {
	BriefRenderer string `yaml:"brief_renderer" json:"brief_renderer"`
	RenderTarget  string `yaml:"render_target" json:"render_target"`
	TemplateRef   string `yaml:"template_ref" json:"template_ref"` // 锚定不可变版本（@frozen/@commit）
}

// ResultsSpec — 结构化提交面（ADR-184 schema 引用 + 解析器注册名）。
type ResultsSpec struct {
	SchemaRef string `yaml:"schema_ref" json:"schema_ref"` // 锚定不可变版本
	Parser    string `yaml:"parser" json:"parser"`
}

// TimeoutsSpec — 超时档位引用（v0 为档位名字符串，档位注册表随编排面另立）。
type TimeoutsSpec struct {
	PerTurn   string `yaml:"per_turn" json:"per_turn"`
	TaskTotal string `yaml:"task_total" json:"task_total"`
}

// Refs — 引用完整性已知集（注入式：v0 内建 DefaultRefs，桌面演练/扩品类经注入扩集）。
type Refs struct {
	Dialects    map[string]bool
	Parsers     map[string]bool
	Renderers   map[string]bool
	Targets     map[string]bool
	Profiles    map[string]bool
	TemplateIDs map[string]bool
	SchemaRefs  map[string]bool
}

// DefaultRefs — v0 内建已知集（方言=切片 1 §2 注册表；schema=ADR-184 提交通道）。
func DefaultRefs() *Refs {
	return &Refs{
		Dialects:    map[string]bool{"dsh-sse": true, "acp-stdio": true, "maka-runtime-host": true, "mock-stdio": true},
		Parsers:     map[string]bool{"codeaudit-submit": true, "text-final": true},
		Renderers:   map[string]bool{"baton-brief": true},
		Targets:     map[string]bool{"task-card-prefix": true, "initial-prompt": true},
		Profiles:    map[string]bool{profileRestricted: true, profileTargeted: true},
		TemplateIDs: map[string]bool{"assignmentTemplate": true, "patchPhaseTemplate": true},
		SchemaRefs:  map[string]bool{"findings/submit_findings": true, "findings/submit_patches": true},
	}
}

var (
	nameRe       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`) // DNS-1123
	floatTags    = map[string]bool{"latest": true, "stable": true, "head": true, "main": true, "master": true, "dev": true, "next": true, "frozen-draft": true}
	constraintRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,62}$`)
)

// anchoredRef — 锚定不可变版本引用校验（C3：禁浮动引用）：`<id>@<version>`，version
// 非空且不在浮动 tag 黑名单。返回规范化拆解或字段路径错误。
func anchoredRef(field, ref string, ids map[string]bool) error {
	i := strings.LastIndex(ref, "@")
	if i <= 0 || i == len(ref)-1 {
		return fmt.Errorf("%s: %q 非锚定引用（须 <id>@<version>，禁浮动）", field, ref)
	}
	id, ver := ref[:i], ref[i+1:]
	if floatTags[strings.ToLower(ver)] {
		return fmt.Errorf("%s: %q 版本段 %q 是浮动引用（禁 latest/stable/head 等；对齐发布纪律一个 sha 只构建一次）", field, ref, ver)
	}
	if ids != nil && !ids[id] {
		return fmt.Errorf("%s: 引用 id %q 不在注册表", field, id)
	}
	return nil
}

// Validate — 全量校验（错误给字段路径，D1）。refs 空=DefaultRefs。
func (p *Pack) Validate(refs *Refs) error {
	if refs == nil {
		refs = DefaultRefs()
	}
	if p.APIVersion != APIVersionV0 {
		return fmt.Errorf("apiVersion: %q 不受支持（当前 %s）", p.APIVersion, APIVersionV0)
	}
	if p.Kind != KindWorkloadPack {
		return fmt.Errorf("kind: %q 不受支持（当前 %s）", p.Kind, KindWorkloadPack)
	}
	if !nameRe.MatchString(p.Name) {
		return fmt.Errorf("name: %q 不合 DNS-1123 风格", p.Name)
	}
	if strings.TrimSpace(p.Description) == "" {
		return fmt.Errorf("description: 不可为空（pack 是平台资源对象，须可读的用途说明）")
	}
	if strings.TrimSpace(p.Spec.Image) == "" {
		return fmt.Errorf("spec.image: 不可为空（沙箱镜像引用）")
	}
	if !refs.Dialects[p.Spec.Dialect] {
		return fmt.Errorf("spec.dialect: %q 不在方言注册表（引用切片 1 descriptor 方言面）", p.Spec.Dialect)
	}
	if !refs.Profiles[p.Spec.Network.Profile] {
		return fmt.Errorf("spec.network.profile: %q 不在档位表（restricted|targeted）", p.Spec.Network.Profile)
	}
	if !refs.Renderers[p.Spec.Seeds.BriefRenderer] {
		return fmt.Errorf("spec.seeds.brief_renderer: %q 不在渲染器注册表", p.Spec.Seeds.BriefRenderer)
	}
	if !refs.Targets[p.Spec.Seeds.RenderTarget] {
		return fmt.Errorf("spec.seeds.render_target: %q 不在注册表（task-card-prefix/initial-prompt）", p.Spec.Seeds.RenderTarget)
	}
	if err := anchoredRef("spec.seeds.template_ref", p.Spec.Seeds.TemplateRef, refs.TemplateIDs); err != nil {
		return err
	}
	if err := anchoredRef("spec.results.schema_ref", p.Spec.Results.SchemaRef, refs.SchemaRefs); err != nil {
		return err
	}
	if !refs.Parsers[p.Spec.Results.Parser] {
		return fmt.Errorf("spec.results.parser: %q 不在解析器注册表", p.Spec.Results.Parser)
	}
	if strings.TrimSpace(p.Spec.Timeouts.PerTurn) == "" || strings.TrimSpace(p.Spec.Timeouts.TaskTotal) == "" {
		return fmt.Errorf("spec.timeouts: per_turn/task_total 档位引用不可为空")
	}
	seen := map[string]bool{}
	for _, c := range p.Spec.ConstraintsSchema {
		if !constraintRe.MatchString(c) {
			return fmt.Errorf("spec.constraints_schema: 约束键 %q 不合 [a-z][a-z0-9_] 规格", c)
		}
		if seen[c] {
			return fmt.Errorf("spec.constraints_schema: 约束键 %q 重复", c)
		}
		seen[c] = true
	}
	// 执行形态（切片 5+6，ADR-249；设计 §5）：缺省 session=现网路径零变化。
	switch p.Spec.Execution {
	case "", "session":
		if !p.Spec.Graph.isZero() {
			return fmt.Errorf("spec.graph: execution 非 graph 时 graph 段必须为零值（防部分应用/形态矛盾，D1）")
		}
	case "graph":
		if err := p.Spec.Graph.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("spec.execution: %q 不在枚举（session|graph）", p.Spec.Execution)
	}
	return nil
}

// isZero — graph 段零值判定（execution=session 时的矛盾配置拒载判据）。
func (g GraphSpec) isZero() bool {
	return len(g.SeedIntents) == 0 && g.Constraints.isZero() && g.Timeouts.isZero()
}

func (c GraphConstraints) isZero() bool {
	return c.MaxNodes == 0 && len(c.AllowedTypes) == 0 && c.MaxPlannerReplans == 0 && c.MaxAttemptsPerNode == 0
}

func (t GraphTimeouts) isZero() bool {
	return t.Planner == 0 && t.Executor == 0 && t.Verifier == 0
}

// validate — execution=graph 时的图段校验（0 值=消费侧缺省注入；负值/非枚举拒载，
// 错误带字段路径）。
func (g GraphSpec) validate() error {
	for i, s := range g.SeedIntents {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("spec.graph.seed_intents[%d]: 不可为空串", i)
		}
	}
	c := g.Constraints
	if c.MaxNodes < 0 {
		return fmt.Errorf("spec.graph.constraints.max_nodes: %d 不合法（0=缺省 24，负值拒载）", c.MaxNodes)
	}
	for _, t := range c.AllowedTypes {
		if !GraphNodeTypes[t] {
			return fmt.Errorf("spec.graph.constraints.allowed_types: %q 不在类型表（planner|executor|verifier）", t)
		}
	}
	if c.MaxPlannerReplans < 0 {
		return fmt.Errorf("spec.graph.constraints.max_planner_replans: %d 不合法（0=缺省 5，负值拒载）", c.MaxPlannerReplans)
	}
	if c.MaxAttemptsPerNode < 0 {
		return fmt.Errorf("spec.graph.constraints.max_attempts_per_node: %d 不合法（0=缺省 2，负值拒载）", c.MaxAttemptsPerNode)
	}
	if g.Timeouts.Planner < 0 || g.Timeouts.Executor < 0 || g.Timeouts.Verifier < 0 {
		return fmt.Errorf("spec.graph.timeouts: 负值不合法（0=缺省档 180|900|300 秒）")
	}
	return nil
}

// SpecHash — canonical 哈希（C3/A2）：归一化 Spec 的 JSON（Go struct 序列化字段序固定=
// 字段序不敏感），sha256 hex。同 spec 必同哈希（revision 复用判定依据）。
func (p *Pack) SpecHash() (string, error) {
	b, err := json.Marshal(p.Spec)
	if err != nil {
		return "", fmt.Errorf("spec marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Parse — 严格解析+校验（未知字段拒载；U6 同型纪律）。
func Parse(data []byte, refs *Refs) (*Pack, error) {
	var p Pack
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("pack 解析: %w", err)
	}
	if err := p.Validate(refs); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadDir — 目录加载全部 pack（仅 *.yaml；*.draft 等草稿后缀不入加载面）。同名重复
// 即错。返回解析后的 pack 列表（调用方逐个 Register）。
func LoadDir(dir string, refs *Refs) ([]*Pack, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("packs dir: %w", err)
	}
	var out []*Pack
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		p, err := Parse(data, refs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("%s: pack name %q 重复", e.Name(), p.Name)
		}
		seen[p.Name] = true
		out = append(out, p)
	}
	return out, nil
}
