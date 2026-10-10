package sandbox

// R122 graph-reseed 切片 2 测试（人类裁决共识 2026-09-20，设计=主仓库
// docs/designs/graph-reseed-task-handoff.md）：种子切片注入 prompt（unit）+
// 失败史挂接 node_seeded（e2e）。DoD：重派 prompt 含 lesson + digest 确定性。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// prompt 注入单测：种子非空 → 注入「前任失败要点」段+lesson 文本；空种子 → 无段。
// 同时验 projectionOf 失败史标记（[FAILED] 进前序摘要——重派/接手必读）。
func TestBuildGraphNodePrompt_SeedSection(t *testing.T) {
	st := newGraphState()
	n0 := st.declare("executor", "前任节点：改策略文件", "pack", nil)
	_ = st.apply(&graphEnvelope{Type: evNodeResult, Seq: 1,
		Data: map[string]any{"nodeId": n0.ID, "outcome": "failed", "resultRef": "执行失败：REFUSED 策略拒绝"}})
	self := st.declare("executor", "重派节点：修复后重试", "pack", nil)
	spec := &GraphTaskSpec{TaskID: "t", Goal: "g"}
	seed := "lesson: 工具拒绝执行（REFUSED）——先修策略面"

	p := buildGraphNodePrompt(spec, self, st, seed, true)
	if !strings.Contains(p, "前任失败要点") || !strings.Contains(p, seed) {
		t.Fatalf("种子段未注入:\n%s", p)
	}
	if !strings.Contains(p, "[FAILED]") {
		t.Fatalf("projectionOf 失败史标记缺失:\n%s", p)
	}

	p2 := buildGraphNodePrompt(spec, self, st, "", true)
	if strings.Contains(p2, "前任失败要点") {
		t.Fatalf("空种子不应注入种子段:\n%s", p2)
	}
}

// e2e：reject 旋钮 → 节点 rejected → planner expand 重派新节点（失败史成立）→
// node_seeded 事件挂接（digest=种子 sha256 前 12 hex，确定性）。
func TestRunGraph_ReseedCarriesNodeSeeded(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"会拒答的任务"}
	seed := "lesson: 工具拒绝执行（REFUSED）——先修策略面\nlesson: 回合以 error 收束"
	h.spec.SeedBrief = func() string { return seed }
	t.Setenv("CODEAUDIT_MOCK_GRAPH_REJECT", "executor")
	t.Setenv("CODEAUDIT_MOCK_GRAPH_PLAN", "expand:1")
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(seed))
	wantDigest := hex.EncodeToString(sum[:6])
	// 断言收紧：**非 planner 波次节点**必挂种子
	// ——宽断言"任一 seeded"会被 planner 旧路径的种子喂绿（变异摘波次
	// seedTextFor 后 planner seeded 仍发=假绿逃逸，波次重构批实测）。
	waveNodes := map[string]bool{}
	var waveSeeded []string
	for _, ev := range h.eventsOf() {
		switch ev.Type {
		case evNodeDeclared:
			if t2, _ := ev.Data["type"].(string); t2 == "executor" || t2 == "verifier" {
				if id, _ := ev.Data["nodeId"].(string); id != "" {
					waveNodes[id] = true
				}
			}
		case evNodeSeeded:
			if id, _ := ev.Data["nodeId"].(string); id != "" {
				waveSeeded = append(waveSeeded, id)
			}
		}
	}
	if len(waveNodes) == 0 {
		t.Fatal("场景失真：无波次节点")
	}
	// 失败史建立（首个 rejected）后的波次节点必挂种子：本场景=最后一个 declared
	// 波次节点（planner 扩图产物 n003）。M91 变异（摘波次 seedTextFor）下它缺席
	// 而 planner 旧路径 seeded 仍发——故必须按节点集合断言而非"任一 seeded"。
	var lastWave string
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeDeclared {
			if t2, _ := ev.Data["type"].(string); t2 == "executor" || t2 == "verifier" {
				if id, _ := ev.Data["nodeId"].(string); id != "" && waveNodes[id] {
					lastWave = id
				}
			}
		}
	}
	seededSet := map[string]bool{}
	for _, id := range waveSeeded {
		seededSet[id] = true
	}
	if lastWave != "" && !seededSet[lastWave] {
		t.Fatalf("失败史后的波次节点 %s 未挂种子（seedTextFor 摘除形态——R122 挂接失效）", lastWave)
	}
	sawSeeded := len(waveSeeded) > 0
	var seededNode, digest string
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeSeeded {
			seededNode, _ = ev.Data["nodeId"].(string)
			digest, _ = ev.Data["digest"].(string)
			break
		}
	}
	if !sawSeeded {
		t.Fatal("失败史存在但未挂种子（node_seeded 缺席——R122 挂接失效）")
	}
	if digest != wantDigest {
		t.Fatalf("种子 digest 漂移: got %s want %s", digest, wantDigest)
	}
	if seededNode == "" {
		t.Fatal("node_seeded 缺 nodeId")
	}
	if !res.Converged {
		t.Fatalf("重派后应收敛: %+v", res)
	}
}

// 降级纪律（⚑2）：SeedBrief 返回空串 → 无种子派发（node_seeded 缺席），执行不阻塞。
func TestRunGraph_ReseedDegradedOnEmpty(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"会拒答的任务"}
	h.spec.SeedBrief = func() string { return "" }
	t.Setenv("CODEAUDIT_MOCK_GRAPH_REJECT", "executor")
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range h.eventsOf() {
		if ev.Type == evNodeSeeded {
			t.Fatal("空种子不应发射 node_seeded")
		}
	}
	if !res.Converged {
		t.Fatalf("降级仍应收敛: %+v", res)
	}
}

// R124：node_seeded 行回放显式登记（旧代码 apply default fatal——含种子行的档
// resume 即打瘫，设计 §1"审计/回放可重建"落空）。回放面=审计摘要无状态折叠，
// 但引用完整性校验（未声明先引用仍 fatal，与 node_started 同纪律）。
func TestReplay_NodeSeededLineReplayable(t *testing.T) {
	st, err := replayLines(t,
		`{"type":"graph.node_declared","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}`,
		`{"type":"graph.node_result","seq":2,"time":"2026-09-20T10:00:02Z","data":{"nodeId":"n001","outcome":"failed","resultRef":"REFUSED 策略拒绝"}}`,
		`{"type":"graph.node_declared","seq":3,"time":"2026-09-20T10:01:00Z","data":{"nodeId":"n002","type":"executor","intent":"重派：修复后重试","declaredBy":"planner","from":["n001"]}}`,
		`{"type":"graph.node_seeded","seq":4,"time":"2026-09-20T10:01:01Z","data":{"nodeId":"n002","digest":"deadbeefcafe","lines":2}}`,
		`{"type":"graph.node_started","seq":5,"time":"2026-09-20T10:01:02Z","data":{"nodeId":"n002","sessionId":"g-n002","turnRef":"g-n002"}}`,
		`{"type":"graph.node_result","seq":6,"time":"2026-09-20T10:02:00Z","data":{"nodeId":"n002","outcome":"completed","resultRef":"ok"}}`,
	)
	if err != nil {
		t.Fatalf("含 node_seeded 的档回放应成功（旧代码此处 fatal）: %v", err)
	}
	if n := st.nodes["n002"]; n == nil || n.Status != stCompleted {
		t.Fatalf("种子后继节点状态失真: %+v", n)
	}
	// 引用完整性：未声明先引用 = 结构性污染，保持 fatal。
	if _, err := replayLines(t,
		`{"type":"graph.node_seeded","seq":1,"time":"2026-09-20T10:00:00Z","data":{"nodeId":"n999","digest":"aa","lines":1}}`,
	); err == nil {
		t.Fatal("node_seeded 未声明先引用应 fatal（结构性污染不容忍）")
	}
}

// TestGraphNodePromptContractByCaps — executor 结果契约按 descriptor caps 分岔
//structured=true→submit_findings 工具契约；
// false→围栏 JSON 契约（text-fence 通道,非工具 agent 的图任务通路）。
func TestGraphNodePromptContractByCaps(t *testing.T) {
	spec := &GraphTaskSpec{TaskID: "t", Goal: "g"}
	st := newGraphState()
	self := st.declare("executor", "审计", "pack", nil)
	toolP := buildGraphNodePrompt(spec, self, st, "", true)
	if !strings.Contains(toolP, "submit_findings") || strings.Contains(toolP, "围栏输出") {
		t.Fatalf("structured 契约漂移:\n%s", toolP)
	}
	fenceP := buildGraphNodePrompt(spec, self, st, "", false)
	if !strings.Contains(fenceP, "```json 围栏输出") || !strings.Contains(fenceP, `"findings"`) {
		t.Fatalf("围栏契约缺失:\n%s", fenceP)
	}
	if strings.Contains(fenceP, "调用 submit_findings") {
		t.Fatalf("非结构化 agent 不应收到工具契约:\n%s", fenceP)
	}
}
