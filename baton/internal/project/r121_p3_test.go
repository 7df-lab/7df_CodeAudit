package project

// R121 P3 批守卫锁：
// flushPendingTo 乱序不裸发 / graph rogue outcome 告警留痕。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// result 对应调用不在待发缓冲（乱序/重复）→ 缓冲保持原序不裸发，result 照常投影。
func TestFlushPendingTo_NoFlushOnUnknownCall(t *testing.T) {
	// call_1 常规路径；随后一个未知 callId 的 result 帧（call_ghost）→ 不得把
	// 缓冲中的 call_2 提前裸发
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n" +
		": ts 2026-09-17T10:00:02Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\"}}\n\n" +
		": ts 2026-09-17T10:00:03Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/call\",\"data\":{\"name\":\"a\",\"callId\":\"call_1\",\"arguments\":\"{}\"}}}\n\n" +
		": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/call\",\"data\":{\"name\":\"b\",\"callId\":\"call_2\",\"arguments\":\"{}\"}}}\n\n" +
		": ts 2026-09-17T10:00:05Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/result\",\"data\":{\"message\":{\"content\":[{\"type\":\"tool-result\",\"toolCallId\":\"call_1\",\"isError\":false,\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}}}}\n\n" +
		": ts 2026-09-17T10:00:06Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/result\",\"data\":{\"message\":{\"content\":[{\"type\":\"tool-result\",\"toolCallId\":\"call_ghost\",\"isError\":false,\"content\":[{\"type\":\"text\",\"text\":\"ghost\"}]}]}}}}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	stats, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	evs := parseLines(t, readTasklog(t, o.OutRoot, "events.jsonl"))
	var callOrder []string
	ghostIdx, call2Idx := -1, -1
	for i, ev := range evs {
		switch ev["type"] {
		case "tool.call":
			call, _ := ev["call"].(map[string]any)
			id, _ := call["call_id"].(string)
			callOrder = append(callOrder, id)
			if id == "call_2" {
				call2Idx = i
			}
		case "tool.result":
			res, _ := ev["result"].(map[string]any)
			if res["call_id"] == "call_ghost" {
				ghostIdx = i
			}
		}
	}
	// 修复语义：幽灵 result 只落自身事件，不触发缓冲裸发——call_2 的出栈只能
	// 发生在 EOF 收束（seq 上晚于幽灵 result）。
	if ghostIdx < 0 || call2Idx < 0 {
		t.Fatalf("事件缺失: ghost@%d call2@%d（calls=%v）", ghostIdx, call2Idx, callOrder)
	}
	if call2Idx < ghostIdx {
		t.Fatalf("幽灵 result 触发缓冲裸发（call_2@%d 早于 ghost result@%d）", call2Idx, ghostIdx)
	}
	found := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "不在待发缓冲") {
			found = true
		}
	}
	if !found {
		t.Fatalf("乱序 result 应告警: %v", stats.Warnings)
	}
}

// graph.node_result outcome 不在枚举 → 告警留痕（事件仍如实入档）。
func TestGraphRogueOutcomeWarned(t *testing.T) {
	dir := t.TempDir()
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	gl := `{"type":"graph.node_declared","seq":1,"time":"2026-09-17T10:00:00Z","data":{"nodeId":"n001","type":"executor","intent":"甲","declaredBy":"pack","from":[]}}
{"type":"graph.node_result","seq":2,"time":"2026-09-17T10:00:01Z","data":{"nodeId":"n001","outcome":"compleated","resultRef":"typo"}}`
	gp := filepath.Join(dir, "in.graph.log")
	if err := os.WriteFile(gp, []byte(gl), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	o.GraphPath = gp
	stats, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "不在枚举") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rogue outcome 应告警: %v", stats.Warnings)
	}
}
