// 图会话方言实现（切片 5+6 TP18-T3，ADR-249）：dsh-sse=同一沙箱多 session 焦点
// 复用（H1 三件接线），mock-stdio=进程内按节点类型分叉（测试/DoD 拒答注入面）。
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// openGraphSession — 按 descriptor 方言打开图会话（一次 launch/upload，N 节点复用）。
func (r *ManagerRunner) openGraphSession(ctx context.Context, spec *GraphTaskSpec) (graphSession, error) {
	switch r.cfg.Agent.Dialect {
	case "dsh-sse":
		ls, err := r.launch(ctx, spec.TaskID)
		if err != nil {
			return nil, err
		}
		if spec.WorkspaceDir != "" {
			if err := r.uploadProject(ctx, ls, spec.WorkspaceDir); err != nil {
				ls.teardown()
				return nil, fmt.Errorf("upload project: %w", err)
			}
		}
		return &dshGraphSession{ls: ls, r: r}, nil
	case "mock-stdio":
		mgs, err := newMockGraphSession(r)
		if err != nil {
			return nil, err
		}
		return mgs, nil
	default:
		return nil, fmt.Errorf("方言 %q 无图会话适配器（v0: dsh-sse/mock-stdio）", r.cfg.Agent.Dialect)
	}
}

// dshGraphSession — dsh-sse 图会话：节点回合=liveSession.turn 经焦点会话寻址
// （H1：焦点会话 idle 收敛/stream 渲染视同主会话；断流续跑/子任务恢复由 turn()
// 既有机器承接——免费继承 ADR-190/192）。
// v0.5 并发纪律（code-review 修复）：setFocus 为 liveSession 级单焦点——并发
// dispatch 会把 prompt 路由到他人会话（焦点互踩）。dispatchMu 串行化执行体：
// 波次调度的事件/认领语义不变，真正并行等待 bridge per-call 寻址改造（设计档
// §2.3 注记）。注入闭包经同锁，与波次派发互斥。
type dshGraphSession struct {
	ls         *liveSession
	r          *ManagerRunner
	dispatchMu sync.Mutex
}

func (s *dshGraphSession) dispatch(ctx context.Context, node *graphNode, prompt string) (string, []ToolCall, float64, error) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	s.ls.setFocus(node.SessionID()) // 派发前设定焦点（node_started 已先行——因果安全）
	text, calls, err := s.ls.turn(ctx, prompt)
	return text, calls, s.ls.nodeTurnsFor(node.SessionID()), err
}

func (s *dshGraphSession) close() { s.ls.teardown() }
func (s *dshGraphSession) name() string {
	if s.ls.ref.Name != "" {
		return s.ls.ref.Name
	}
	return s.ls.ref.ID
}
func (s *dshGraphSession) turnsTotal() float64 { return s.ls.focusTurnsTotal() }

// mock env 旋钮（图会话测试面；ADR-244 同型：只进测试环境，生产禁载由加载校验承担）。
const (
	mockEnvGraphReject = "CODEAUDIT_MOCK_GRAPH_REJECT" // planner|executor|verifier|all → REFUSED 首行
	mockEnvGraphPlan   = "CODEAUDIT_MOCK_GRAPH_PLAN"   // converge（缺省）|expand:N（planner 输出控制）
	mockEnvGraphFail   = "CODEAUDIT_MOCK_GRAPH_FAIL"   // planner|executor|verifier|all → 回合错误（attempt 上限路径）
	mockEnvGraphSilent = "CODEAUDIT_MOCK_GRAPH_SILENT" // executor 零提交收敛（收笔类型化触发面）
)

// mockGraphSession — 进程内图会话（无 launch/上传面——诚实边界同 mockSession）。
type mockGraphSession struct {
	mu    sync.Mutex // turns 计数与并发 dispatch 的互斥（v0.5 波次派发面）
	r     *ManagerRunner
	turns float64
}

func newMockGraphSession(r *ManagerRunner) (*mockGraphSession, error) {
	return &mockGraphSession{r: r}, nil
}

func (m *mockGraphSession) dispatch(ctx context.Context, node *graphNode, prompt string) (string, []ToolCall, float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns++
	// graph-ui-channels 批1c：mock 方言合成通道内容（行协议对齐 dsh——golden/e2e
	// 断言面；真实渲染走 dsh-sse 的 channelRenderer 双轨）。
	m.r.channelLog(node.ID, fmt.Sprintf(
		"══ 节点会话 %s ══\n── 第 1 轮开始 ──\n📋 [任务下发]（%d 字节）\n%s\n",
		node.SessionID(), len(prompt), prompt))
	text, calls, _, err := m.dispatchLocked(node, prompt)
	if err != nil { // 错误回合行协议对齐 dsh（── 回合结束: ❌ 错误），非收束行
		m.r.channelLog(node.ID, fmt.Sprintf("── 回合结束: ❌ 错误（%v）──\n", err))
	} else {
		m.r.channelLog(node.ID, fmt.Sprintf("✍ [输出]\n%s\n■ 会话空闲（收束）\n", text))
	}
	return text, calls, m.turns, err
}

// dispatchLocked — 既有 mock 回合体（dispatch 持锁内调用）。
func (m *mockGraphSession) dispatchLocked(node *graphNode, prompt string) (string, []ToolCall, float64, error) {
	if rej := mockEnvGet(mockEnvGraphReject); rej == node.Type || rej == "all" {
		return fmt.Sprintf("REFUSED: mock 注入拒答（%s=%s）", mockEnvGraphReject, rej), nil, m.turns, nil
	}
	if f := mockEnvGet(mockEnvGraphFail); f == node.Type || f == "all" {
		return "", nil, m.turns, fmt.Errorf("mock: 注入图节点回合失败（%s=%s）", mockEnvGraphFail, f)
	}
	if strings.Contains(prompt, "收笔（conclude）") { // 收笔轮识别（ADR-246 契约同源）
		return m.conclude()
	}
	switch node.Type {
	case "executor":
		if mockEnvGet(mockEnvGraphSilent) != "" { // 零提交收敛：触发宿主类型化收笔判据（H2）
			return "执行完成（silent：未走提交通道）。", nil, m.turns, nil
		}
		return m.executor()
	case "planner":
		return m.planner()
	default: // verifier
		return "裁决：确认。依据：前序节点结果自洽（mock）。建议：按序推进后续节点。", nil, m.turns, nil
	}
}

func (m *mockGraphSession) executor() (string, []ToolCall, float64, error) {
	findings := []map[string]any{{
		"title":       "mock 图节点发现 #1",
		"description": "GraphRunner executor 节点结构化提交（切片 5+6 回归）",
		"severity":    "LOW", "cwe_id": "CWE-000",
		"file_path": "src/graph.go", "start_line": 1, "confidence": 0.5,
		"reasoning": "mock graph", "fix_suggestion": "", "diff_patch": "",
	}, {
		"title":       "mock 图节点侦察观察（INFO）",
		"description": "severity=INFO 携带侦察观察（渗透报告视图过滤面，§5 L1 裁决）",
		"severity":    "INFO", "cwe_id": "CWE-000",
		"file_path": "src/graph.go", "start_line": 2, "confidence": 0.5,
		"reasoning": "mock graph recon", "fix_suggestion": "", "diff_patch": "",
	}}
	payload, _ := json.Marshal(map[string]any{"findings": findings})
	return "执行完成（mock 图结构化提交）。",
		[]ToolCall{{Name: SubmitFindingsTool, Arguments: string(payload)}}, m.turns, nil
}

// planner — 输出 intents JSON（schema 钉形状）：converge=空 nodes；expand:N=声明 N 个
// executor（封顶/预算测试驱动面）。
func (m *mockGraphSession) planner() (string, []ToolCall, float64, error) {
	plan := "converge"
	if v := mockEnvGet(mockEnvGraphPlan); v != "" {
		plan = v
	}
	if plan == "converge" {
		return `{"nodes":[]}`, nil, m.turns, nil
	}
	var nodes []map[string]any
	for i := 0; i < parseExpandN(plan); i++ {
		nodes = append(nodes, map[string]any{
			"type": "executor", "intent": fmt.Sprintf("mock 扩展任务 #%d", i+1), "from": []string{}})
	}
	b, _ := json.Marshal(map[string]any{"nodes": nodes})
	return string(b), nil, m.turns, nil
}

func (m *mockGraphSession) conclude() (string, []ToolCall, float64, error) {
	payload, _ := json.Marshal(map[string]any{"findings": []any{}})
	return "收笔定稿完成。", []ToolCall{{Name: SubmitFindingsTool, Arguments: string(payload)}}, m.turns, nil
}

func (m *mockGraphSession) close()              {}
func (m *mockGraphSession) name() string        { return "mock-graph" }
func (m *mockGraphSession) turnsTotal() float64 { return m.turns }

// parseExpandN — "expand:3" → 3（非法=0）。
func parseExpandN(s string) int {
	rest, ok := strings.CutPrefix(s, "expand:")
	if !ok {
		return 0
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
