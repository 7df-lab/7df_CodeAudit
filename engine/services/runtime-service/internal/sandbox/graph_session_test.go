package sandbox

// 焦点会话化测试（切片 5+6 TP18-T2，ADR-249 审计 H1）：
//   - 焦点会话 idle 收敛/事件全投影/stream 渲染视同主会话（否则图节点回合永不收敛）；
//   - 非焦点节点会话维持子任务骨架（ADR-181 口径不回退）；
//   - 轮账分会话：焦点切换时 base/max 保存/恢复（未见过的会话从 0 起）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §4 会话化改造面 +
// .agent/evidence/slice56-audit-20260919/audit-report.md H1。

import (
	"sync"
	"testing"
)

func newFocusParser() (*sseParser, *sync.Map) {
	var human syncwriter
	return &sseParser{onHuman: human.writeString}, &sync.Map{}
}

func feedLines(p *sseParser, lines ...string) (evs []bridgeEvent) {
	for _, l := range lines {
		if ev := p.line([]byte(l + "\n")); ev != nil {
			evs = append(evs, *ev)
		}
	}
	return evs
}

// 焦点会话 idle=回合收敛（H1 核心：现状代码非 main 会话 idle 被吞 → 图回合死锁）。
func TestFocus_FocusedSessionIdleConverges(t *testing.T) {
	p2, _ := newFocusParser()
	p2.setFocus("g-n001")
	evs := feedLines(p2, "event: session.status\n",
		"data: {\"sessionId\":\"g-n001\",\"status\":\"idle\"}\n")
	if len(evs) != 1 || !evs[0].idle {
		t.Fatalf("focused session idle must project convergence, got %+v", evs)
	}
	// 非焦点会话 idle=骨架行不投影收敛（ADR-181 口径）
	p3, _ := newFocusParser()
	p3.setFocus("g-n002")
	evs = feedLines(p3, "event: session.status\n",
		"data: {\"sessionId\":\"g-n001\",\"status\":\"idle\"}\n")
	if len(evs) != 0 {
		t.Fatalf("non-focused session idle must stay skeleton, got %+v", evs)
	}
	// 未设焦点（现网审计路径）：任何非 main 会话维持子任务口径
	p4, _ := newFocusParser()
	evs4 := feedLines(p4, "event: session.status\n",
		"data: {\"sessionId\":\"g-n001\",\"status\":\"idle\"}\n")
	if len(evs4) != 0 {
		t.Fatalf("without focus, non-main session idle must stay skeleton, got %+v", evs4)
	}
}

// 焦点会话 session.event 全投影（assistantText/toolCall），非焦点走 agentEvent 骨架。
func TestFocus_FocusedSessionEventsProject(t *testing.T) {
	p, _ := newFocusParser()
	p.setFocus("g-n001")
	evs := feedLines(p,
		"event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"turn/start\",\"data\":{\"turn\":1}}}\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":1,\"step\":1,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"节点结论\"}]}}}}\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"tool/call\",\"data\":{\"name\":\"submit_findings\",\"arguments\":\"{\\\"findings\\\":[]}\"}}}\n",
	)
	var sawText, sawTool bool
	for _, ev := range evs {
		if ev.assistantText == "节点结论" {
			sawText = true
		}
		if ev.toolCall != nil && ev.toolCall.Name == SubmitFindingsTool {
			sawTool = true
		}
	}
	if !sawText || !sawTool {
		t.Fatalf("focused session must project assistantText+toolCall, saw text=%v tool=%v", sawText, sawTool)
	}
	// 非焦点：骨架（无 turnStart/assistantText 投影）
	p2, _ := newFocusParser()
	p2.setFocus("g-n002")
	evs = feedLines(p2,
		"event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"turn/start\",\"data\":{\"turn\":1}}}\n",
		"event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"assistant/message\",\"data\":{\"turn\":1,\"step\":1,\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"骨架\"}]}}}}\n",
	)
	for _, ev := range evs {
		if ev.assistantText != "" || ev.turnStart || ev.toolCall != nil {
			t.Fatalf("non-focused session must stay skeleton, got %+v", ev)
		}
	}
}

// 焦点会话 session.stream 渲染不丢（H1：现状直接 return nil 丢帧）。
func TestFocus_FocusedStreamRenders(t *testing.T) {
	p, _ := newFocusParser()
	p.setFocus("g-n001")
	evs := feedLines(p,
		"event: session.stream\n",
		"data: {\"sessionId\":\"g-n001\",\"frame\":{\"type\":\"start\",\"attemptId\":\"a1\",\"turn\":1,\"step\":1}}\n",
		"event: session.stream\n",
		"data: {\"sessionId\":\"g-n001\",\"frame\":{\"type\":\"chunk\",\"attemptId\":\"a1\",\"chunk\":{\"type\":\"text-delta\",\"text\":\"逐字\"}}}\n",
	)
	_ = evs // 渲染走 onHuman；不 panic 且 liveSeen 置位即证不丢
	if !p.liveSeen {
		t.Fatalf("focused session stream chunk must render (liveSeen)")
	}
	// 非焦点会话 stream 静默（不渲染不 panic）
	p2, _ := newFocusParser()
	p2.setFocus("g-n002")
	_ = feedLines(p2,
		"event: session.stream\n",
		"data: {\"sessionId\":\"g-n001\",\"frame\":{\"type\":\"chunk\",\"attemptId\":\"a1\",\"chunk\":{\"type\":\"text-delta\",\"text\":\"他者\"}}}\n",
	)
	if p2.liveSeen {
		t.Fatalf("non-focused stream must stay silent")
	}
}

// 轮账分会话：焦点切换保存/恢复；未见过的会话从 0 起。
func TestFocus_TurnAccountingSwap(t *testing.T) {
	p, _ := newFocusParser()
	p.setFocus("g-n001")
	feedLines(p, "event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"turn/start\",\"data\":{\"turn\":3}}}\n")
	if p.maxTurn != 3 {
		t.Fatalf("n001 maxTurn = %v, want 3", p.maxTurn)
	}
	p.setFocus("g-n002") // 切走：n001 账目保存
	if p.maxTurn != 0 || p.turnBase != 0 {
		t.Fatalf("fresh session accounting must start at 0, got base=%v max=%v", p.turnBase, p.maxTurn)
	}
	feedLines(p, "event: session.event\n",
		"data: {\"sessionId\":\"g-n002\",\"event\":{\"type\":\"turn/start\",\"data\":{\"turn\":1}}}\n")
	if p.maxTurn != 1 {
		t.Fatalf("n002 maxTurn = %v, want 1", p.maxTurn)
	}
	p.setFocus("g-n001") // 切回：n001 账目恢复（重派同会话轮次连续）
	if p.maxTurn != 3 {
		t.Fatalf("n001 accounting must restore to 3, got %v", p.maxTurn)
	}
	// n001 新 turn 4 继续
	feedLines(p, "event: session.event\n",
		"data: {\"sessionId\":\"g-n001\",\"event\":{\"type\":\"turn/start\",\"data\":{\"turn\":4}}}\n")
	if p.maxTurn != 4 {
		t.Fatalf("n001 maxTurn after resume = %v, want 4", p.maxTurn)
	}
}

// 回主会话（setFocus("")）：现网审计口径（main 视同主会话）不受图模式污染。
func TestFocus_ReturnToMain(t *testing.T) {
	p, _ := newFocusParser()
	p.setFocus("g-n001")
	p.setFocus("") // 回 main
	evs := feedLines(p, "event: session.status\n",
		"data: {\"sessionId\":\"main\",\"status\":\"idle\"}\n")
	if len(evs) != 1 || !evs[0].idle {
		t.Fatalf("main must converge after focus cleared, got %+v", evs)
	}
}
