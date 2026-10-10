package project

// 2026-09-20 复查修复批守卫锁（消融台账 3 号存活项 + 评审 P1/P2 回归锁）：
// turn/end 收束白名单、多会话 EOF 确定性、WORKSPACE 损坏再生/任务硬闸、
// graph.log 扫描错误上抛、pending 帧 ts 快照、未映射名计收、他任期告警、
// arguments 非字符串保形。变异杀灭依据见主仓库 .agent/evidence/baton-review-20260920/。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// turn/end reason.kind 白名单：干净收束三值 completed，其余（含 engine 枚举
// interrupted/max-tokens/blocked 与未知值）一律 indeterminate（公理方向性）。
func TestTurnEndKindWhitelist(t *testing.T) {
	cases := map[string]string{ // kind → 期望 outcome
		"completed": "completed", "stop": "completed", "tool-calls": "completed",
		"interrupted": "indeterminate", "max-tokens": "indeterminate",
		"blocked": "indeterminate", "aborted": "indeterminate", "error": "indeterminate",
		"rogue-value": "indeterminate",
	}
	for kind, want := range cases {
		sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n" +
			": ts 2026-09-17T10:00:02Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\"}}\n\n" +
			": ts 2026-09-17T10:00:03Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/call\",\"data\":{\"name\":\"shell\",\"callId\":\"call_1\",\"arguments\":\"{}\"}}}\n\n" +
			": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"data\":{\"reason\":{\"kind\":\"" + kind + "\"}}}}\n\n"
		dir := t.TempDir()
		p := filepath.Join(dir, "in.sse.log")
		if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
			t.Fatal(err)
		}
		o := baseOpts(t, p)
		if _, err := Run(o); err != nil {
			t.Fatalf("kind=%s: %v", kind, err)
		}
		var got string
		for _, ev := range parseLines(t, readTasklog(t, o.OutRoot, "events.jsonl")) {
			if ev["type"] == "tool.result" {
				res, _ := ev["result"].(map[string]any)
				got, _ = res["outcome"].(string)
			}
		}
		if got != want {
			t.Fatalf("kind=%s: outcome=%q, want %q（白名单=stop/completed/tool-calls）", kind, got, want)
		}
	}
}

// 多会话 EOF：固定时钟下两次投影逐字节一致（确定性契约；closeAll 排序锁）。
func TestMultiSessionEOFDeterministic(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n" +
		": ts 2026-09-17T10:00:02Z\nevent: subagent.started\ndata: {\"parentSessionId\":\"main\",\"childSessionId\":\"child-A\"}\n\n" +
		": ts 2026-09-17T10:00:03Z\nevent: subagent.started\ndata: {\"parentSessionId\":\"main\",\"childSessionId\":\"child-B\"}\n\n" +
		": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"child-A\",\"event\":{\"type\":\"turn/start\"}}\n\n" +
		": ts 2026-09-17T10:00:05Z\nevent: session.event\ndata: {\"sessionId\":\"child-A\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"text-delta\",\"text\":\"AAA\"}}}}\n\n" +
		": ts 2026-09-17T10:00:06Z\nevent: session.event\ndata: {\"sessionId\":\"child-B\",\"event\":{\"type\":\"turn/start\"}}\n\n" +
		": ts 2026-09-17T10:00:07Z\nevent: session.event\ndata: {\"sessionId\":\"child-B\",\"event\":{\"type\":\"assistant/chunk\",\"data\":{\"chunk\":{\"type\":\"text-delta\",\"text\":\"BBB\"}}}}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	var runs []string
	for i := 0; i < 6; i++ {
		o := baseOpts(t, p)
		if _, err := Run(o); err != nil {
			t.Fatal(err)
		}
		runs = append(runs, readTasklog(t, o.OutRoot, "events.jsonl"))
	}
	for i := 1; i < len(runs); i++ {
		if runs[i] != runs[0] {
			t.Fatalf("run %d differs from run 0——多会话 EOF 输出不确定（确定性契约被破）", i)
		}
	}
}

// WORKSPACE.json 损坏：告警 + 重新生成 + 覆写盘上文件（events 与盘不得引用两身份）。
func TestWorkspaceCorruptRegenerates(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	tl := filepath.Join(o.OutRoot, ".tasklog")
	if err := os.MkdirAll(tl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tl, "WORKSPACE.json"), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(o)
	if err != nil {
		t.Fatalf("损坏档案应降级再生而非失败: %v", err)
	}
	if !strings.Contains(strings.Join(stats.Warnings, "\n"), "重新生成工作区身份") {
		t.Fatalf("缺少再生成告警: %v", stats.Warnings)
	}
	var ws struct {
		WorkspaceID string `json:"workspace_id"`
	}
	wb, err := os.ReadFile(filepath.Join(tl, "WORKSPACE.json"))
	if err != nil || json.Unmarshal(wb, &ws) != nil {
		t.Fatalf("盘上 WORKSPACE.json 仍损坏: %s err=%v", wb, err)
	}
	if ws.WorkspaceID != fixedWS {
		t.Fatalf("盘上身份 %q ≠ events 引用 %q（身份分叉）", ws.WorkspaceID, fixedWS)
	}
}

// WORKSPACE.json task 不符：硬闸（projection-dsh §5"重挂前核对，不符即硬闸"）。
func TestWorkspaceTaskMismatchHardGate(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	tl := filepath.Join(o.OutRoot, ".tasklog")
	if err := os.MkdirAll(tl, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := `{"workspace_id":"99999999-9999-4999-8999-999999999999","task":"T-OTHER","created_ts":"2026-09-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(tl, "WORKSPACE.json"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(o)
	if err == nil || !strings.Contains(err.Error(), "硬闸") {
		t.Fatalf("task 不符应硬闸报错, got %v", err)
	}
}

// graph.log 超 1MiB 行：扫描错误上抛（退出 1），不静默截断剩余事件。
func TestGraphScanErrorFails(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 1<<20+64)
	gl := "{\"type\":\"graph.converged\",\"seq\":1,\"time\":\"2026-09-17T10:00:00Z\",\"data\":{\"summary\":\"" + big + "\",\"openRemaining\":0}}\n"
	gp := filepath.Join(dir, "in.graph.log")
	if err := os.WriteFile(gp, []byte(gl), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	o.GraphPath = gp
	if _, err := Run(o); err == nil || !strings.Contains(err.Error(), "graph.log 扫描失败") {
		t.Fatalf("超限行应报扫描错误, got %v", err)
	}
}

// pending 帧 ts 快照：缓冲发射的 tool.call 用构造时刻帧 ts，不漂移到 flush 帧时刻
// （消融台账 3 号存活项——本测试补锁，变异应被杀）。
func TestPendingCallTsSnapshot(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n" +
		": ts 2026-09-17T10:00:02Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\"}}\n\n" +
		": ts 2026-09-17T10:00:03Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/call\",\"data\":{\"name\":\"shell\",\"callId\":\"call_1\",\"arguments\":\"{}\"}}}\n\n" +
		": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"approval/asked\",\"data\":{\"id\":\"a1\",\"toolName\":\"shell\",\"callId\":\"call_1\"}}}\n\n" +
		": ts 2026-09-17T10:00:05Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"approval/decided\",\"data\":{\"id\":\"a1\",\"outcome\":\"allowed-once\"}}}\n\n" +
		": ts 2026-09-17T10:00:06Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/result\",\"data\":{\"message\":{\"content\":[{\"type\":\"tool-result\",\"toolCallId\":\"call_1\",\"isError\":false,\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}}}}\n\n" +
		": ts 2026-09-17T10:00:07Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"data\":{\"reason\":{\"kind\":\"stop\"}}}}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	if _, err := Run(o); err != nil {
		t.Fatal(err)
	}
	for _, ev := range parseLines(t, readTasklog(t, o.OutRoot, "events.jsonl")) {
		if ev["type"] == "tool.call" {
			if ts, _ := ev["ts"].(string); ts != "2026-09-17T10:00:03Z" {
				t.Fatalf("tool.call ts=%q, want 构造时刻 10:00:03（快照不得漂移到 flush 时刻）", ts)
			}
			return
		}
	}
	t.Fatal("未找到 tool.call 事件")
}

// 未映射帧/事件名计收（含未知子事件），词表白名单内不计。
func TestUnknownFrameCounted(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n" +
		": ts 2026-09-17T10:00:02Z\nevent: mystery/frame\ndata: {\"x\":1}\n\n" +
		": ts 2026-09-17T10:00:03Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"todo/write\",\"data\":{}}}\n\n" +
		": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"future/thing\",\"data\":{}}}\n\n" +
		": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"future/thing\",\"data\":{}}}\n\n"
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
	all := strings.Join(stats.Warnings, "\n")
	if !strings.Contains(all, `"mystery/frame" ×1`) {
		t.Fatalf("未知顶层帧名未计收: %v", stats.Warnings)
	}
	if !strings.Contains(all, `"future/thing" ×2`) {
		t.Fatalf("未知子事件名未按次计收: %v", stats.Warnings)
	}
	if strings.Contains(all, "todo/write") {
		t.Fatalf("词表白名单内的 todo/write 不应计收: %v", stats.Warnings)
	}
}

// 旧档案前缀含他任期事件：重投影告警（多任期 append 未实现的诚实降级）。
func TestForeignPenPrefixWarns(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "in.sse.log")
	if err := os.WriteFile(p, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	o := baseOpts(t, p)
	o.Agent = "dsh"
	tl := filepath.Join(o.OutRoot, ".tasklog")
	if err := os.MkdirAll(tl, 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"seq":1,"ts":"2026-09-17T09:00:00Z","task":"T-1042","session":"main","pen":{"agent":"maka"},"type":"model.message","message":{"role":"assistant","parts":[{"kind":"text","text":"前任任期事件"}]}}
{"seq":2,"ts":"2026-09-17T09:30:00Z","task":"T-1042","session":"main","pen":{"agent":"maka"},"type":"handoff.checkpointed","handoff_checkpoint":{"log_seq":1,"workspace":{}}}
`
	if err := os.WriteFile(filepath.Join(tl, "events.jsonl"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	stats, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(stats.Warnings, "\n")
	if !strings.Contains(all, "他任期") || !strings.Contains(all, "1 条") {
		t.Fatalf("他任期事件应计数告警: %v", stats.Warnings)
	}
	if !strings.Contains(readTasklog(t, o.OutRoot, "events.jsonl"), "handoff.checkpointed") {
		t.Fatal("干净 handoff 尾段应保留")
	}
}

// arguments 非字符串形状：原值序列化入库（不静默空串）+ 告警。
func TestArgumentsNonStringPreserved(t *testing.T) {
	sse := ": ts 2026-09-17T10:00:01Z\nevent: bridge.hello\ndata: {\"provider\":\"p\",\"model\":\"m\"}\n\n" +
		": ts 2026-09-17T10:00:02Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/start\"}}\n\n" +
		": ts 2026-09-17T10:00:03Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"tool/call\",\"data\":{\"name\":\"fs\",\"callId\":\"call_1\",\"arguments\":{\"path\":\"/etc\",\"mode\":\"w\"}}}}\n\n" +
		": ts 2026-09-17T10:00:04Z\nevent: session.event\ndata: {\"sessionId\":\"main\",\"event\":{\"type\":\"turn/end\",\"data\":{\"reason\":{\"kind\":\"stop\"}}}}\n\n"
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
	if !strings.Contains(strings.Join(stats.Warnings, "\n"), "arguments 非字符串形状") {
		t.Fatalf("形状漂移应告警: %v", stats.Warnings)
	}
	for _, ev := range parseLines(t, readTasklog(t, o.OutRoot, "events.jsonl")) {
		if ev["type"] == "tool.call" {
			call, _ := ev["call"].(map[string]any)
			args, _ := call["args"].(map[string]any)
			if args == nil || args["path"] != "/etc" || args["mode"] != "w" {
				t.Fatalf("arguments 原值未保形入库: %v", call)
			}
			return
		}
	}
	t.Fatal("未找到 tool.call 事件")
}
