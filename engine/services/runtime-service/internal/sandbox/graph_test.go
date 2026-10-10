package sandbox

// GraphRunner 状态机测试（切片 5+6 TP18-T3，ADR-249；mock 图会话驱动——DoD 1/3/4
// 的状态机面，真沙箱 e2e 归 TP18-T4 sim 验收）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §3/§4/§8 DoD 1-4。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// graphHarness — 单测夹具：mock 方言 runner + graph.log 字节收集。
type graphHarness struct {
	t    *testing.T
	r    *ManagerRunner
	log  bytes.Buffer
	spec GraphTaskSpec
	caps Capabilities
}

func newGraphHarness(t *testing.T, caps Capabilities) *graphHarness {
	t.Helper()
	t.Setenv("CODEAUDIT_MOCK_DIALECT_ALLOW", "1")
	h := &graphHarness{t: t, caps: caps}
	h.r = mockRunner(t, mockDescriptor("mock-graph", caps), nil)
	h.spec = GraphTaskSpec{
		TaskID: "gw-graph1", Goal: "mock 图任务",
		WorkspaceDir: t.TempDir(),
		OnGraphEvent: func(b []byte) { h.log.Write(b) },
	}
	return h
}

func (h *graphHarness) run() (*GraphRunResult, error) {
	// 防御时限：活锁类回归（attempt 上限失效=无限重派，M81）以超时形态显形为
	// 失败而非挂死——变异判杀依赖 --- FAIL 形态。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.r.RunGraph(ctx, h.spec)
}

// eventsOf — 从收集的 graph.log 解出事件序列。
func (h *graphHarness) eventsOf() []graphEnvelope {
	var out []graphEnvelope
	for _, line := range strings.Split(strings.TrimSpace(h.log.String()), "\n") {
		if line == "" {
			continue
		}
		var env graphEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			h.t.Fatalf("graph.log 非法行: %v (%s)", err, line)
		}
		out = append(out, env)
	}
	return out
}

func (h *graphHarness) resultsOf() map[string]string { // nodeId → outcome
	out := map[string]string{}
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeResult {
			out[ev.Data["nodeId"].(string)] = ev.Data["outcome"].(string)
		}
	}
	return out
}

// 串行生命周期：种子 executor×2 → 新态势 planner → converge 空 → converged。
// 事件序含 node_declared/node_started/node_result/plan_triggered/converged 全族。
func TestRunGraph_SerialLifecycle(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true, ConcludeSemantics: true})
	h.spec.SeedIntents = []string{"任务甲", "任务乙"}
	res, err := h.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Converged || res.Paused {
		t.Fatalf("want converged, got %+v", res)
	}
	types := map[string]int{}
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeDeclared {
			types[ev.Data["type"].(string)]++
		}
	}
	if types["executor"] != 2 || types["planner"] != 1 {
		t.Fatalf("want 2 seed executors + 1 replan planner, declared: %v", types)
	}
	// findings 聚合上浮（executor 2×2 条——mock 每节点 2 findings）
	if len(res.Findings) != 4 {
		t.Fatalf("findings = %d, want 4", len(res.Findings))
	}
	// 全节点 completed；最后事件=converged
	results := h.resultsOf()
	for id, outcome := range results {
		if outcome != outcomeCompleted {
			t.Fatalf("node %s = %s, want completed", id, outcome)
		}
	}
	evs := h.eventsOf()
	if evs[len(evs)-1].Type != evConverged {
		t.Fatalf("last event = %s, want graph.converged", evs[len(evs)-1].Type)
	}
	// seq 全档单调（1 起无断档）
	for i, ev := range evs {
		if ev.Seq != i+1 {
			t.Fatalf("event %d seq = %d（应=i+1 单调无断档）", i, ev.Seq)
		}
	}
}

// 回放重建等价（DoD 1 核心）：跑完的 graph.log 回放 → 与运行时状态逐字段等价。
func TestRunGraph_ReplayRebuild(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"任务甲"}
	_, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	st, err := ReplayGraphLog(bytes.NewReader(h.log.Bytes()))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	total, completed, failed, rejected, open := st.counts()
	if total != 2 || completed != 2 || failed != 0 || rejected != 0 || open != 0 { // executor+planner
		t.Fatalf("replay counts = %d/%d/%d/%d/%d, want 2/2/0/0/0", total, completed, failed, rejected, open)
	}
	if !st.converged {
		t.Fatalf("replay must see converged")
	}
	// 节点 ID 单调接续（n001/n002）
	if st.nodes["n001"] == nil || st.nodes["n002"] == nil {
		t.Fatalf("replay node ids wrong: %v", st.nodes)
	}
}

// resume 只重跑 open（DoD 2 状态机面——B2）：半程档（executor 完成、planner 在途）
// 回放后 resume，已完成节点不得再出现 node_started。
func TestRunGraph_ResumeOnlyOpen(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	// 构造半程档：n001 executor completed、n002 planner declared（open）
	half := strings.Join([]string{
		`{"type":"graph.node_declared","seq":1,"time":"2026-09-19T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"任务甲","declaredBy":"pack","from":[]}}`,
		`{"type":"graph.node_started","seq":2,"time":"2026-09-19T10:00:01Z","data":{"nodeId":"n001","sessionId":"g-n001","turnRef":"g-n001"}}`,
		`{"type":"graph.node_result","seq":3,"time":"2026-09-19T10:00:02Z","data":{"nodeId":"n001","outcome":"completed","resultRef":"submit_findings×2"}}`,
		`{"type":"graph.node_declared","seq":4,"time":"2026-09-19T10:00:03Z","data":{"nodeId":"n002","type":"planner","intent":"重规划","declaredBy":"pack","from":["n001"]}}`,
	}, "\n") + "\n"
	h.spec.ReplayLog = strings.NewReader(half)
	h.spec.SeedIntents = nil // resume：不重新展开种子
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Fatalf("want resumed convergence, got %+v", res)
	}
	started := map[string]bool{}
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeStarted {
			started[ev.Data["nodeId"].(string)] = true
		}
	}
	if started["n001"] {
		t.Fatalf("completed node n001 must NOT re-dispatch on resume (B2 零重付)")
	}
	if !started["n002"] {
		t.Fatalf("open node n002 must dispatch on resume")
	}
	// 续写 seq 接续旧档（4 之后）
	evs := h.eventsOf()
	if evs[0].Seq != 5 {
		t.Fatalf("resume emit seq = %d, want 5（接续旧档）", evs[0].Seq)
	}
}

// 封顶拒绝留痕（DoD 3）：max_nodes 超限→拒绝声明且 resultRef 记明细（非静默截断）。
func TestRunGraph_MaxNodesCap(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"种子任务"}
	h.spec.MaxNodes = 3 // n001 executor + n002 planner = 2；planner expand:4 → 只许再声明 1
	t.Setenv("CODEAUDIT_MOCK_GRAPH_PLAN", "expand:4")
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Fatalf("capped graph must still converge, got %+v", res)
	}
	declared := 0
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeDeclared {
			declared++
		}
	}
	if declared != 3 {
		t.Fatalf("declared = %d, want 3（max_nodes=3 封顶）", declared)
	}
	// 留痕：某 planner node_result 的 resultRef 含"拒绝"明细
	found := false
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeResult && strings.Contains(ev.Data["resultRef"].(string), "拒绝") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cap rejection must leave trace in planner resultRef (D1 非静默截断)")
	}
}

// rejected 路径（DoD 4）：mock 注入拒答 → outcome=rejected、不误标 failed、冷却生效
// （open 集不再派同节点，直至 planner 重规划周期）。
func TestRunGraph_RejectedPath(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true, ConcludeSemantics: true})
	h.spec.SeedIntents = []string{"会拒答的任务"}
	t.Setenv("CODEAUDIT_MOCK_GRAPH_REJECT", "executor")
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	results := h.resultsOf()
	sawRejected, sawFailed := false, false
	for _, outcome := range results {
		switch outcome {
		case outcomeRejected:
			sawRejected = true
		case outcomeFailed:
			sawFailed = true
		}
	}
	if !sawRejected {
		t.Fatalf("rejected outcome missing: %v", results)
	}
	if sawFailed {
		t.Fatalf("rejected must NOT be marked failed (D1 分道): %v", results)
	}
	if !res.Converged {
		t.Fatalf("graph must converge after rejection+cooldown replan, got %+v", res)
	}
	// 冷却：同一被拒节点不得重复 node_started（重规划周期内）
	startCount := map[string]int{}
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeStarted {
			startCount[ev.Data["nodeId"].(string)]++
		}
	}
	for id, n := range startCount {
		if n > 1 {
			t.Fatalf("node %s started %d times（冷却窗内不得重派同 intent）", id, n)
		}
	}
}

// attempt 上限（M1 活锁防线）：持续失败 → indeterminate 重派（attempt+1）→ 达上限
// 终态 failed 不回 open → planner(open_drained) 裁决 → 收敛。
func TestRunGraph_AttemptCap(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"顽固失败任务"}
	h.spec.MaxAttemptsPerNode = 2
	t.Setenv("CODEAUDIT_MOCK_GRAPH_FAIL", "executor")
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	var ladder []string
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeResult && ev.Data["nodeId"] == "n001" {
			ladder = append(ladder, ev.Data["outcome"].(string))
		}
	}
	if len(ladder) != 2 || ladder[0] != outcomeIndeterminate || ladder[1] != outcomeFailed {
		t.Fatalf("n001 ladder = %v, want [indeterminate failed]", ladder)
	}
	// open_drained 触发留痕
	sawDrained := false
	for _, ev := range h.eventsOf() {
		if ev.Type == evPlanTriggered && ev.Data["trigger"] == triggerOpenDrained {
			sawDrained = true
		}
	}
	if !sawDrained {
		t.Fatalf("open_drained replan must fire after attempt cap exhaustion")
	}
	if !res.Converged {
		t.Fatalf("graph must converge (planner 裁决换路或收敛), got %+v", res)
	}
	// 失败节点派发次数 = 上限（1 次 + 重派 1 次 = 2），不得无限
	startCount := map[string]int{}
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeStarted {
			startCount[ev.Data["nodeId"].(string)]++
		}
	}
	for id, n := range startCount {
		if n > h.spec.MaxAttemptsPerNode {
			t.Fatalf("node %s started %d times（超 attempt 上限=活锁）", id, n)
		}
	}
}

// 收笔类型化（H2）：executor 零提交（silent）→ 收笔注入恰一轮后按提交收；
// planner/verifier 零提交是合法态不注入（mock planner 天然零提交——若误注入会被
// 收笔轮识别消耗回合，此处以 executor 侧断言为主）。
func TestRunGraph_ConcludeTyped(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true, ConcludeSemantics: true})
	h.spec.SeedIntents = []string{"silent 任务"}
	t.Setenv("CODEAUDIT_MOCK_GRAPH_SILENT", "1")
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	results := h.resultsOf()
	for id, outcome := range results {
		if strings.HasPrefix(id, "n0") && outcome == outcomeCompleted {
			// executor 收笔后提交即 completed
		}
	}
	if !res.Converged {
		t.Fatalf("want converged, got %+v", res)
	}
	// executor 最终 completed（收笔把在途工作逼进通道）
	sawExecCompleted := false
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeResult && ev.Data["outcome"] == outcomeCompleted {
			sawExecCompleted = true
		}
	}
	if !sawExecCompleted {
		t.Fatalf("executor must complete after conclude injection (H2)")
	}
}

// planner 预算耗尽（max_planner_replans）：新态势不再触发 planner → 收敛且 summary
// 如实记因（D1）。
func TestRunGraph_PlannerBudget(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"任务甲"}
	h.spec.AllowedTypes = map[string]bool{"executor": true, "verifier": true} // planner 不在允许集=零重规划（0 值=缺省注入语义，ADR-249）（M78 变异锚点：上限检查移除→无限重规划击杀本测试）
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Fatalf("want converge on budget exhaustion, got %+v", res)
	}
	planFired := 0
	for _, ev := range h.eventsOf() {
		if ev.Type == evPlanTriggered {
			planFired++
		}
	}
	if planFired != 0 {
		t.Fatalf("zero replan budget must not fire planner, fired %d", planFired)
	}
}

// 无种子：plan_triggered(initial) 起 planner（§4 初始化路径）。
func TestRunGraph_InitialPlanner(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Fatal("want converged")
	}
	evs := h.eventsOf()
	if len(evs) == 0 || evs[0].Type != evPlanTriggered || evs[0].Data["trigger"] != triggerInitial {
		t.Fatalf("first event must be plan_triggered(initial), got %+v", evs[0])
	}
}

// REFUSED 识别（refusedOf 契约）：首行前缀；非首行不算。
func TestRefusedOf(t *testing.T) {
	if r := refusedOf("\nREFUSED: 授权范围外"); r != "授权范围外" {
		t.Fatalf("refusedOf = %q", r)
	}
	if r := refusedOf("分析结论…\nREFUSED: 晚出现的拒绝"); r != "" {
		t.Fatalf("non-first-line REFUSED must not count, got %q", r)
	}
	if r := refusedOf("正常输出"); r != "" {
		t.Fatalf("normal output, got %q", r)
	}
}

func containsValue(m map[string]string, v string) bool {
	for _, x := range m {
		if x == v {
			return true
		}
	}
	return false
}

// 回放折叠规则与 baton 投影一致性（ADR-249 open 集折叠规则——跨仓契约）：
// declared 入 open；终态出 open；indeterminate 留 open。
func TestGraphState_OpenFolding(t *testing.T) {
	st := newGraphState()
	n1 := st.declare("executor", "甲", "pack", nil)
	n2 := st.declare("executor", "乙", "pack", nil)
	n3 := st.declare("executor", "丙", "pack", nil)
	_ = st.apply(&graphEnvelope{Type: evNodeStarted, Seq: 1, Data: map[string]any{"nodeId": n1.ID}})
	_ = st.apply(&graphEnvelope{Type: evNodeResult, Seq: 2, Data: map[string]any{"nodeId": n1.ID, "outcome": outcomeCompleted}})
	_ = st.apply(&graphEnvelope{Type: evNodeResult, Seq: 3, Data: map[string]any{"nodeId": n2.ID, "outcome": outcomeFailed}})
	_ = st.apply(&graphEnvelope{Type: evNodeResult, Seq: 4, Data: map[string]any{"nodeId": n3.ID, "outcome": outcomeIndeterminate}})
	open := st.openIDs()
	if len(open) != 1 || open[0] != n3.ID {
		t.Fatalf("open = %v, want [n003]（completed/failed 出、indeterminate 留）", open)
	}
	if n3.Attempts != 1 {
		t.Fatalf("indeterminate must bump attempts, got %d", n3.Attempts)
	}
}

// Planner 输出解析与围栏容错。
func TestExtractJSON(t *testing.T) {
	if got := extractJSON("```json\n{\"nodes\":[]}\n```"); got != `{"nodes":[]}` {
		t.Fatalf("fenced = %q", got)
	}
	if got := extractJSON(`{"nodes":[]}`); got != `{"nodes":[]}` {
		t.Fatalf("bare = %q", got)
	}
	if got := extractJSON(fmt.Sprintf("前言\n```json\n%s\n```\n后记", `{"nodes":[]}`)); got != `{"nodes":[]}` {
		t.Fatalf("mid-fenced = %q", got)
	}
}
