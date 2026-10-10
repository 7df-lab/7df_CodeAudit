// graph_channels 通道渲染回归：
// ①双通道零串扰（交错帧各自归位）②焦点等价 golden（通道渲染与主渲染逐字节等价
// ——sessionEvent 移植漂移锁）③主流骨架不变锁（非焦点 g-* 在主 flat 流仍只出
// 骨架行，ADR-181 口径零回退）④nil 通道零开销（未接 OnChannelLog 时主流不受扰）。
// wire 帧一律 json.Marshal 构造（经验：手写转义错误被 consume 静默吞帧）。
package sandbox

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

type chanCapture struct {
	main     strings.Builder
	channels map[string]*strings.Builder
}

func newChanCapture() *chanCapture {
	return &chanCapture{channels: map[string]*strings.Builder{}}
}

func (c *chanCapture) parser() *sseParser {
	return &sseParser{
		onHuman: func(s string) { c.main.WriteString(s) },
		onChannel: func(nodeID, s string) {
			if c.channels[nodeID] == nil {
				c.channels[nodeID] = &strings.Builder{}
			}
			c.channels[nodeID].WriteString(s)
		},
	}
}

func chanMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// sessionEventFrame — session.event 通知 wire。
func chanEventFrame(t *testing.T, sid string, ev map[string]any) string {
	return chanMarshal(t, map[string]any{"sessionId": sid, "event": ev})
}

// streamChunkFrame — session.stream chunk 通知 wire。
func chanChunkFrame(t *testing.T, sid, attemptID string, chunk map[string]any) string {
	return chanMarshal(t, map[string]any{"sessionId": sid, "frame": map[string]any{
		"type": "chunk", "attemptId": attemptID, "chunk": chunk}})
}

// 一条完整回合的事件序列（下发→轮次→逐字思考→逐字输出→收敛→回合结束）。
func turnEvents(t *testing.T) []struct {
	kind string
	data string
} {
	return []struct{ kind, data string }{
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "user/message",
			"data": map[string]any{"content": []any{map[string]any{"type": "text", "text": "执行审计任务"}}}})},
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "turn/start", "data": map[string]any{"turn": 1}})},
		{"session.stream", chanChunkFrame(t, "SID", "a1", map[string]any{"type": "reasoning-delta", "text": "思考中…"})},
		{"session.stream", chanChunkFrame(t, "SID", "a1", map[string]any{"type": "text-delta", "text": "结论正文。"})},
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "assistant/message",
			"data": map[string]any{"turn": 1, "step": 1, "message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "结论正文。"}}}}})},
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "turn/end", "data": map[string]any{"reason": map[string]any{"kind": "end_turn"}}})},
	}
}

// TestGraphChannel_TwoNodesNoCrossTalk — 双节点交错帧：各通道只含自己的内容。
func TestGraphChannel_TwoNodesNoCrossTalk(t *testing.T) {
	c := newChanCapture()
	p := c.parser()
	for _, ev := range turnEvents(t) {
		p.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"g-n001"`, 1))
		p.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"g-n002"`, 1))
	}
	for node, want := range map[string]string{
		"n001": "执行审计任务",
		"n002": "执行审计任务",
	} {
		got := c.channels[node].String()
		if !strings.Contains(got, want) || !strings.Contains(got, "💭 [思考]") || !strings.Contains(got, "✍ [输出]") {
			t.Errorf("%s 通道内容缺失:\n%s", node, got)
		}
	}
	// 对侧 id 严格不出现（头行是自身的，需剔除后再查）
	for node, other := range map[string]string{"n001": "n002", "n002": "n001"} {
		if strings.Contains(c.channels[node].String(), other) {
			t.Errorf("%s 通道混入 %s 内容:\n%s", node, other, c.channels[node].String())
		}
	}
}

// TestGraphChannel_FocusedEquivalence — 焦点等价 golden：同帧序列，焦点态主渲染
// 与通道渲染必须逐字节等价（除通道会话头行）——sessionEvent 移植的漂移锁。
func TestGraphChannel_FocusedEquivalence(t *testing.T) {
	// 主渲染（焦点会话：focused("") 即主会话口径）
	cMain := newChanCapture()
	pMain := &sseParser{onHuman: func(s string) { cMain.main.WriteString(s) }}
	// 通道渲染（g-* 非焦点：主流走骨架，通道走全投影）
	cCh := newChanCapture()
	pCh := cCh.parser()

	for _, ev := range turnEvents(t) {
		pMain.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"main"`, 1)) // bridge 主会话（prompt 固定 sessionId=main）
		pCh.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"g-n001"`, 1))
	}
	mainOut := cMain.main.String()
	chOut := cCh.channels["n001"].String()
	// 通道多出：会话头行（惰性建实例时产出）
	if !strings.HasPrefix(chOut, "══ 节点会话 g-n001 ══\n") {
		t.Errorf("通道缺会话头行:\n%s", chOut)
	}
	chBody := strings.TrimPrefix(chOut, "══ 节点会话 g-n001 ══\n")
	if chBody != mainOut {
		t.Errorf("焦点等价破缺（移植漂移）：\n--- 主渲染 ---\n%q\n--- 通道 ---\n%q", mainOut, chBody)
	}
}

// TestGraphChannel_MainSkeletonUnchanged — 主 flat 流不变锁：非焦点 g-* 在主流
// 仍只出骨架行（无 💭/✍ 正文），ADR-181 口径零回退。
func TestGraphChannel_MainSkeletonUnchanged(t *testing.T) {
	c := newChanCapture()
	p := c.parser()
	for _, ev := range turnEvents(t) {
		p.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"g-n001"`, 1))
	}
	m := c.main.String()
	for _, banned := range []string{"💭", "✍", "📋 [任务下发]"} {
		if strings.Contains(m, banned) {
			t.Errorf("主流出现正文（骨架口径回退）: %q in\n%s", banned, m)
		}
	}
}

// TestGraphChannel_NilChannelZeroOverhead — 未接 OnChannelLog：主流照常、零 panic。
func TestGraphChannel_NilChannelZeroOverhead(t *testing.T) {
	var main strings.Builder
	p := &sseParser{onHuman: func(s string) { main.WriteString(s) }}
	for _, ev := range turnEvents(t) {
		p.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"g-n001"`, 1))
	}
	if !strings.Contains(main.String(), "[子任务 g-n001] 任务") {
		t.Errorf("主流骨架行缺失:\n%s", main.String())
	}
}

// TestGraphChannel_StreamDedupNoDoubleRender — 逐字去重（ADR-236 同口径）：
// session.stream 已渲染的 (turn,step)，assistant/message 不再展开内嵌 stream。
func TestGraphChannel_StreamDedupNoDoubleRender(t *testing.T) {
	c := newChanCapture()
	p := c.parser()
	p.consume("session.event", chanEventFrame(t, "g-n001", map[string]any{
		"type": "turn/start", "data": map[string]any{"turn": 2}}))
	p.consume("session.stream", chanMarshal(t, map[string]any{"sessionId": "g-n001", "frame": map[string]any{
		"type": "start", "attemptId": "a9", "turn": 2, "step": 1}}))
	p.consume("session.stream", chanChunkFrame(t, "g-n001", "a9", map[string]any{"type": "text-delta", "text": "唯一正文。"}))
	p.consume("session.event", chanEventFrame(t, "g-n001", map[string]any{
		"type": "assistant/message",
		"data": map[string]any{"turn": 2, "step": 1, "stream": []any{
			map[string]any{"type": "text-chunks", "texts": []string{"唯一正文。"}}}}}))
	out := c.channels["n001"].String()
	if n := strings.Count(out, "唯一正文。"); n != 1 {
		t.Errorf("正文渲染 %d 次（应 1——去重失效）:\n%s", n, out)
	}
}

// TestNodeIDOfSID — g- 前缀解析（非图会话返回空）。
func TestNodeIDOfSID(t *testing.T) {
	for sid, want := range map[string]string{"g-n001": "n001", "g-": "", "sub-abc": "", "": ""} {
		if got := nodeIDOfSID(sid); got != want {
			t.Errorf("nodeIDOfSID(%q)=%q want %q", sid, got, want)
		}
	}
}

// TestGraphChannel_MockRunPopulatesChannels — mock 图任务全程后 OnChannelLog 收到
// 逐节点合成内容（批1c 集成面：dispatch 包装写通道，行协议对齐 dsh）。
func TestGraphChannel_MockRunPopulatesChannels(t *testing.T) {
	h := newGraphHarness(t, Capabilities{StructuredSubmit: true})
	var mu sync.Mutex
	chLog := map[string]string{}
	h.r.cfg.OnChannelLog = func(nodeID, s string) {
		mu.Lock()
		chLog[nodeID] += s
		mu.Unlock()
	}
	res, err := h.run()
	if err != nil || !res.Converged {
		t.Fatalf("mock 图任务应收敛: err=%v res=%+v", err, res)
	}
	if len(chLog) == 0 {
		t.Fatal("零通道输出——mock dispatch 包装未生效")
	}
	for node, body := range chLog {
		if !strings.Contains(body, "══ 节点会话 g-"+node+" ══") ||
			!strings.Contains(body, "📋 [任务下发]") ||
			!strings.Contains(body, "✍ [输出]") ||
			!strings.Contains(body, "■ 会话空闲（收束）") {
			t.Errorf("节点 %s 通道行协议缺失:\n%s", node, body)
		}
	}
	// 完成节点的通道含其 prompt 意图（图节点任务标记正文在 prompt 内）
	seen := strings.Join(mapValues(chLog), "\n")
	if !strings.Contains(seen, "# 图节点任务") {
		t.Errorf("通道未见图节点任务标记（prompt 未入通道）:\n%s", seen)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestGraphChannel_MalformedStatusNoPanic — 畸形 session.status 帧（缺 status 键/
// 非字符串）不 panic 且主流骨架照常（P1 回归锁：旁路失败不得影响主流）。
func TestGraphChannel_MalformedStatusNoPanic(t *testing.T) {
	c := newChanCapture()
	p := c.parser()
	p.consume("session.status", chanMarshal(t, map[string]any{"sessionId": "g-n001"}))
	p.consume("session.status", chanMarshal(t, map[string]any{"sessionId": "g-n002", "status": 42}))
	p.consume("session.status", chanMarshal(t, map[string]any{"sessionId": "g-n003", "status": "idle"}))
	if !strings.Contains(c.main.String(), "[子任务 g-n003] 空闲") {
		t.Errorf("主流骨架行缺失:\n%s", c.main.String())
	}
}

// TestGraphChannel_FocusedDualWrite — 焦点态 g-* 会话双写：主 flat 全投影
// （💭/✍ 直出）与通道渲染同时发生（本批核心契约）。
func TestGraphChannel_FocusedDualWrite(t *testing.T) {
	c := newChanCapture()
	p := c.parser()
	p.setFocus("g-n001") // H1：图执行焦点会话视同主会话
	for _, ev := range turnEvents(t) {
		p.consume(ev.kind, strings.Replace(ev.data, `"SID"`, `"g-n001"`, 1))
	}
	if !strings.Contains(c.main.String(), "💭 [思考]") || !strings.Contains(c.main.String(), "✍ [输出]") {
		t.Errorf("焦点态主流应全投影:\n%s", c.main.String())
	}
	if !strings.Contains(c.channels["n001"].String(), "✍ [输出]") {
		t.Errorf("焦点态通道应同步渲染:\n%s", c.channels["n001"].String())
	}
}

// TestGraphChannel_EquivalenceExtendedCorpus — 等价 golden 扩面：工具调用/
// assistant/attempt 多 index 换块/错误回合终态——移植漂移锁的完整覆盖。
func TestGraphChannel_EquivalenceExtendedCorpus(t *testing.T) {
	frames := []struct{ kind, data string }{
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "tool/call", "data": map[string]any{"name": "submit_findings", "arguments": "[{}]"}})},
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "assistant/attempt",
			"data": map[string]any{"turn": 1, "step": 1, "stream": []any{
				map[string]any{"type": "text-chunks", "index": 0, "texts": []string{"第一块。"}},
				map[string]any{"type": "text-chunks", "index": 1, "texts": []string{"第二块。"}},
				map[string]any{"type": "tool-call-chunks"},
				map[string]any{"type": "reasoning-chunks", "index": 0, "texts": []string{"再思考。"}},
			}}})},
		{"session.event", chanEventFrame(t, "SID", map[string]any{
			"type": "turn/end", "data": map[string]any{"reason": map[string]any{
				"kind": "error", "error": map[string]any{"message": "boom"}}}})},
	}
	var main strings.Builder
	pMain := &sseParser{onHuman: func(s string) { main.WriteString(s) }}
	cCh := newChanCapture()
	pCh := cCh.parser()
	for _, f := range frames {
		pMain.consume(f.kind, strings.Replace(f.data, `"SID"`, `"main"`, 1))
		pCh.consume(f.kind, strings.Replace(f.data, `"SID"`, `"g-n009"`, 1))
	}
	chOut := strings.TrimPrefix(cCh.channels["n009"].String(), "══ 节点会话 g-n009 ══\n")
	if chOut != main.String() {
		t.Errorf("扩面等价破缺：\n--- 主 ---\n%q\n--- 通道 ---\n%q", main.String(), chOut)
	}
	for _, want := range []string{"⟐ 工具调用 submit_findings", "第二块。", "再思考。", "❌ 错误（boom）"} {
		if !strings.Contains(chOut, want) {
			t.Errorf("扩面语料缺 %q:\n%s", want, chOut)
		}
	}
}
