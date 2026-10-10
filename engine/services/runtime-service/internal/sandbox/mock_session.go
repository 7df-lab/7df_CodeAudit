// mock-stdio 方言适配器（R6 v0，切片 1 实施切序第 1 步）：进程内概率化 phase 结果，
// descriptor 骨架自第一天起可全路径回归，不依赖真实 Maka 端点。
// 依据: 主仓库 docs/designs/platform-slice1-r6-descriptor.md §4.1（mock dialect 先行，
// Cairn mock driver 经验）/§6（outcome 概率配置只进测试环境，生产禁载由加载校验承担）；
// ADR-244（env 旋钮与确定性种子）。
//
// 进程内语义（诚实边界）：不创建沙箱、不经 manager（session.transport=mock-inproc，
// 加载校验强制）——launch/上传面不在本适配器覆盖内，R6 原验收"零改动拉起新 agent"
// 的验收口径=管线全路径（prompt→turn→解析→两段式→映射）由纯条目数据驱动。
package sandbox

import (
	"context"
	cryptrand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

// mock env 旋钮（测试环境专用；ADR-244）。
const (
	mockEnvOutcome  = "CODEAUDIT_MOCK_OUTCOME"  // structured|textfinal|empty|interaction|error（缺省 structured）
	mockEnvFindings = "CODEAUDIT_MOCK_FINDINGS" // 一阶段发现条数（缺省 2）
	mockEnvDelayMS  = "CODEAUDIT_MOCK_DELAY_MS" // 回合时延 ms："a-b" 区间或单值（缺省 0）
	mockEnvSeed     = "CODEAUDIT_MOCK_SEED"     // 确定性种子（缺省随机）

	mockFindingsDefault = 2 // 一阶段缺省发现条数
)

// mockEnvGet — env 读取注入点（测试替身用；生产即 os.Getenv）。
var mockEnvGet = os.Getenv

// mockSession — 进程内回合会话。
type mockSession struct {
	r            *ManagerRunner
	d            ProviderDescriptor
	label        string
	rng          *rand.Rand
	delay        [2]int // ms 区间
	lastManifest []int  // 最近一次 phase-2 清单 index（收笔定稿补交用，ADR-246）
}

// newMockSession — 构造（无外部资源；outcome/delay 自 env 解析，解析失败=缺省值+warn）。
func newMockSession(r *ManagerRunner, t Task) (turnSession, error) {
	var b [4]byte
	if _, err := cryptrand.Read(b[:]); err != nil {
		return nil, err
	}
	m := &mockSession{r: r, d: r.cfg.Agent, label: "mock-" + hex.EncodeToString(b[:2])}
	if s := mockEnvGet(mockEnvSeed); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			m.rng = rand.New(rand.NewSource(int64(n)))
		}
	}
	if m.rng == nil {
		m.rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	m.delay = parseDelayRange(mockEnvGet(mockEnvDelayMS))
	return m, nil
}

// turn — 按阶段产出概率化结果（阶段识别同 DSH 管线：phase-2 prompt 含"第二阶段"标题，
// 与 BuildPatchPhasePrompt 契约一致；产出经 ToolCall 走 ADR-184 同一解析面）。
func (m *mockSession) turn(ctx context.Context, prompt string) (string, []ToolCall, error) {
	m.sleep(ctx)
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	// 收笔轮识别（ADR-246）：prompt 带"（收笔）"标记 → 定稿响应（把上一阶段产出
	// 走提交通道交齐——模拟模型收到收笔指令后的合规行为）。
	if strings.Contains(prompt, "收笔（conclude）") { // 与 concludePromptFindings/Patches 开头契约同源
		return m.turnConclude()
	}
	outcome := m.pickOutcome()
	if outcome == "error" {
		return "", nil, fmt.Errorf("mock: 注入回合失败（outcome=error）")
	}
	// capability interaction=false 的方言面冒出交互请求：按该方言 documented 行为处置
	// 并记日志，不重试（设计 §3）——mock 的 documented 行为=拒绝后继续收敛。
	if outcome == "interaction" {
		if m.d.Capabilities.Interaction {
			m.r.event("info", "mock 交互请求已受理（capability interaction=true；v0 无审批面，仅备案）")
		} else {
			m.r.event("warn", "mock 方言面冒出交互请求（capability interaction=false）：按 documented 行为记日志不重试，继续收敛")
		}
	}
	if isPatchPhasePrompt(prompt) {
		return m.turnPatches(prompt)
	}
	return m.turnFindings(outcome)
}

// turnFindings — 一阶段：N 条合成发现经 submit_findings 工具参数提交（结构化）或
// 围栏 JSON 兜底（textfinal——structured_submit=false 的降级通道）。
func (m *mockSession) turnFindings(outcome string) (string, []ToolCall, error) {
	n := mockFindingsDefault
	if s := mockEnvGet(mockEnvFindings); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			n = v
		}
	}
	if outcome == "empty" {
		n = 0
	}
	if outcome == "silent" {
		// 零提交收敛（停在错误粒度）：无工具调用、无围栏——触发宿主收笔注入判据
		return "分析完成（silent：未走提交通道）。", nil, nil
	}
	findings := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		findings = append(findings, map[string]any{
			"title": fmt.Sprintf("mock 发现 #%d（%s）", i+1, m.d.ID), "description": "mock 产出（切片 1 descriptor 全路径回归）",
			"severity": []string{"LOW", "MEDIUM", "HIGH"}[m.rng.Intn(3)], "cwe_id": "CWE-000",
			"file_path": "src/mock.go", "start_line": 10 + i, "confidence": 0.5,
			"reasoning": "mock reasoning", "fix_suggestion": "mock fix（无把握可空）", "diff_patch": "",
		})
	}
	payload, _ := json.Marshal(map[string]any{"findings": findings})
	if outcome == "textfinal" || !m.d.Capabilities.StructuredSubmit {
		// 结构化不可用：围栏 JSON 兜底（text-final 通道，parseAuditResult(nil calls) 接住）
		return "分析完成。\n```json\n" + string(payload) + "\n```", nil, nil
	}
	return "分析完成（mock 结构化提交）。",
		[]ToolCall{{Name: SubmitFindingsTool, Arguments: string(payload)}}, nil
}

// turnPatches — 二阶段：从 prompt 内嵌清单逐项回填补丁（1/3 概率显式空串=无把握不代写，
// 与三阶段同口径），经 submit_patches 工具参数走 applyPatchPhase 合并面。
func (m *mockSession) turnPatches(prompt string) (string, []ToolCall, error) {
	manifest := extractManifest(prompt)
	if s := mockEnvGet(mockEnvOutcome); s == "silent" {
		m.lastManifest = manifest
		return "补丁生成完成（silent：未走提交通道）。", nil, nil
	}
	patches := make([]map[string]any, 0, len(manifest))
	for _, idx := range manifest {
		if m.rng.Intn(3) == 0 {
			patches = append(patches, map[string]any{"index": idx, "diff_patch": ""}) // 显式空：无把握
			continue
		}
		patches = append(patches, map[string]any{"index": idx, "diff_patch": mockPatchFor(idx)})
	}
	payload, _ := json.Marshal(map[string]any{"patches": patches})
	if !m.d.Capabilities.StructuredSubmit {
		return "补丁生成完成。\n```json\n" + string(payload) + "\n```", nil, nil
	}
	return "补丁生成完成（mock 结构化提交）。",
		[]ToolCall{{Name: SubmitPatchesTool, Arguments: string(payload)}}, nil
}

// pickOutcome — outcome 选择（分布串或单值；缺省 structured）。
func (m *mockSession) pickOutcome() string {
	s := mockEnvGet(mockEnvOutcome)
	if s == "" || s == "structured" || s == "textfinal" || s == "empty" || s == "interaction" || s == "error" || s == "silent" {
		if s == "" {
			return "structured"
		}
		return s
	}
	// 分布串 "a:0.7,b:0.3"
	type w struct {
		name string
		p    float64
	}
	var ws []w
	total := 0.0
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		p, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
		if err != nil {
			continue
		}
		ws = append(ws, w{strings.TrimSpace(kv[0]), p})
		total += p
	}
	if total <= 0 {
		return "structured"
	}
	pick := m.rng.Float64() * total
	for _, x := range ws {
		pick -= x.p
		if pick <= 0 {
			return x.name
		}
	}
	return ws[len(ws)-1].name
}

// sleep — 回合时延（delay[0]-delay[1] ms 区间均匀取值；ctx 可取消）。
func (m *mockSession) sleep(ctx context.Context) {
	lo, hi := m.delay[0], m.delay[1]
	if hi < lo {
		lo, hi = hi, lo
	}
	ms := lo
	if hi > lo {
		ms += m.rng.Intn(hi - lo + 1)
	}
	if ms <= 0 {
		return
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// isPatchPhasePrompt — 二阶段 prompt 识别（与 BuildPatchPhasePrompt 标题契约锁同一字符串，
// 漂移由 TestBuildPatchPhasePrompt 拦截）。
func isPatchPhasePrompt(prompt string) bool { return strings.Contains(prompt, "（第二阶段）") }

// extractManifest — 从 phase-2 prompt 提取清单 index 序列（"发现清单（JSON）："段，
// 与 patchPhaseTemplate 占位段契约同源）。
func extractManifest(prompt string) []int {
	marker := "发现清单（JSON）："
	i := strings.Index(prompt, marker)
	if i < 0 {
		return nil
	}
	rest := prompt[i+len(marker):]
	end := strings.Index(rest, "\n\n")
	if end >= 0 {
		rest = rest[:end]
	}
	var items []struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &items); err != nil {
		return nil
	}
	out := make([]int, 0, len(items))
	for _, it := range items {
		out = append(out, it.Index)
	}
	return out
}

// mockPatchFor — 合法规范化补丁（NormalizeDiffPatch 可过：上下文/删除行合成自洽）。
// 消费端按内容锚定，无真实文件依赖——校验面只验语法结构。
func mockPatchFor(idx int) string {
	return fmt.Sprintf("*** Begin Patch\n*** Update File: src/mock.go\n@@ // mock anchor %d\n context line %d\n-removed line %d\n+added line %d\n*** End Patch", idx, idx, idx, idx)
}

// parseDelayRange — "a-b" 区间或单值 ms（解析失败=0）。
func parseDelayRange(s string) [2]int {
	if s == "" {
		return [2]int{}
	}
	if a, b, ok := strings.Cut(s, "-"); ok {
		lo, e1 := strconv.Atoi(strings.TrimSpace(a))
		hi, e2 := strconv.Atoi(strings.TrimSpace(b))
		if e1 == nil && e2 == nil {
			return [2]int{lo, hi}
		}
		return [2]int{}
	}
	if v, err := strconv.Atoi(s); err == nil {
		return [2]int{v, v}
	}
	return [2]int{}
}

// close — 无外部资源（进程内会话）；幂等空实现。
func (m *mockSession) close() {}

// name — 会话标识（mock-<hex4>）。
func (m *mockSession) name() string { return m.label }

// turnsContribution — 单回合会话，无真实轮次面（0 即无偏移）。
func (m *mockSession) turnsContribution() float64 { return 0 }

// errorTail — 进程内会话无 bridge.log 诊断面（空）。
func (m *mockSession) errorTail(_ context.Context) string { return "" }

// turnConclude — 收笔轮定稿响应（ADR-246）：按最近阶段把产出经期望通道交齐
// （phase-2 清单在案=patches 通道；否则=findings 通道）。结构化能力缺失=围栏兜底。
func (m *mockSession) turnConclude() (string, []ToolCall, error) {
	if !m.d.Capabilities.StructuredSubmit {
		payload := map[string]any{"findings": []any{}}
		if m.lastManifest != nil {
			payload = map[string]any{"patches": []any{}}
		}
		b, _ := json.Marshal(payload)
		return "收笔定稿完成。\n```json\n" + string(b) + "\n```", nil, nil
	}
	if m.lastManifest != nil {
		patches := make([]map[string]any, 0, len(m.lastManifest))
		for _, idx := range m.lastManifest {
			patches = append(patches, map[string]any{"index": idx, "diff_patch": mockPatchFor(idx)})
		}
		b, _ := json.Marshal(map[string]any{"patches": patches})
		return "收笔定稿完成（补丁通道）。", []ToolCall{{Name: SubmitPatchesTool, Arguments: string(b)}}, nil
	}
	n := mockFindingsDefault
	if sv := mockEnvGet(mockEnvFindings); sv != "" {
		if v, err := strconv.Atoi(sv); err == nil && v >= 0 {
			n = v
		}
	}
	findings := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		findings = append(findings, map[string]any{
			"title": fmt.Sprintf("mock 收笔发现 #%d", i+1), "description": "收笔定稿产出",
			"severity": "MEDIUM", "cwe_id": "CWE-000", "file_path": "src/mock.go",
			"start_line": 10 + i, "confidence": 0.5, "reasoning": "conclude",
			"fix_suggestion": "", "diff_patch": "",
		})
	}
	b, _ := json.Marshal(map[string]any{"findings": findings})
	return "收笔定稿完成（发现通道）。", []ToolCall{{Name: SubmitFindingsTool, Arguments: string(b)}}, nil
}
