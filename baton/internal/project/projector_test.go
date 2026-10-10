package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

const fixedWS = "11111111-1111-4111-8111-111111111111"

func baseOpts(t *testing.T, sse string) Options {
	t.Helper()
	return Options{
		SSEPath: sse, OutRoot: t.TempDir(),
		Task: "T-1042", Tenant: "acme", User: "xiaowang",
		Goal: "修复订单回调重复扣款", Constraints: []string{"不动支付网关SDK", "回归测试全绿"},
		BlobThreshold: 64, WorkspaceID: fixedWS, Now: fixedNow,
	}
}

func readTasklog(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".tasklog", name))
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	return string(b)
}

func parseLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("events.jsonl 行非法: %v\n行: %s", err, ln)
		}
		out = append(out, m)
	}
	return out
}

func TestGoldenBasic(t *testing.T) {
	opts := baseOpts(t, "testdata/basic.sse.log")
	stats, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Warnings) != 0 {
		t.Fatalf("basic 夹具不应有告警: %v", stats.Warnings)
	}

	goldenDir := "testdata/golden_basic"
	if os.Getenv("BATON_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"events.jsonl", "spine.json", "WORKSPACE.json"} {
			b, err := os.ReadFile(filepath.Join(opts.OutRoot, ".tasklog", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(goldenDir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"events.jsonl", "spine.json", "WORKSPACE.json"} {
		want, err := os.ReadFile(filepath.Join(goldenDir, name))
		if err != nil {
			t.Fatalf("读黄金 %s（先 BATON_UPDATE_GOLDEN=1 生成）: %v", name, err)
		}
		if got := readTasklog(t, opts.OutRoot, name); got != string(want) {
			t.Errorf("%s 与黄金不一致\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
		}
	}

	// 结构断言（黄金之外的关键语义）
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	byType := map[string]int{}
	for _, ev := range evs {
		byType[ev["type"].(string)]++
	}
	for _, c := range []struct {
		typ  string
		want int
	}{
		{"task.created", 1}, {"spine.updated", 1}, {"model.message", 4},
		{"usage.recorded", 1}, {"tool.call", 1}, {"tool.result", 1}, {"task.subagent", 2},
	} {
		if byType[c.typ] != c.want {
			t.Errorf("%s = %d, want %d", c.typ, byType[c.typ], c.want)
		}
	}

	// usage 语义：input=未缓存；模型名来自 bridge.hello
	var usage map[string]any
	for _, ev := range evs {
		if ev["type"] == "usage.recorded" {
			usage = ev["usage"].(map[string]any)
		}
	}
	if usage == nil || usage["model"] != "deepseek-chat-v3.2" ||
		usage["input_tokens"].(float64) != 1200 || usage["cache_read_tokens"].(float64) != 300 {
		t.Errorf("usage 语义不符: %v", usage)
	}

	// blob 分离：findings 参数走引用不内联
	for _, ev := range evs {
		if ev["type"] == "tool.call" {
			call := ev["call"].(map[string]any)
			if ref, _ := call["args_ref"].(string); !strings.HasPrefix(ref, "blob:") {
				t.Errorf("大参数应走 blob 引用: %v", call)
			}
		}
	}
	if stats.Blobs != 1 {
		t.Errorf("blobs = %d, want 1", stats.Blobs)
	}

	// 子任务会话 envelope
	sawChild := false
	for _, ev := range evs {
		if ev["type"] == "model.message" && ev["session"] == "8f3a-demo-child-0001" {
			sawChild = true
		}
	}
	if !sawChild {
		t.Error("子任务 user 消息应带 session envelope")
	}
}

func TestTornTailIndeterminate(t *testing.T) {
	opts := baseOpts(t, "testdata/torn.sse.log")
	stats, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Warnings) != 2 { // 撕裂行 + EOF 中断
		t.Fatalf("warnings = %v", stats.Warnings)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	var outcome any
	for _, ev := range evs {
		if ev["type"] == "tool.result" {
			outcome = ev["result"].(map[string]any)["outcome"]
		}
	}
	if outcome != "indeterminate" {
		t.Errorf("中断回合的未闭合 tool.call 应为 indeterminate（公理），got %v", outcome)
	}
}

func TestRejectsAIlog(t *testing.T) {
	opts := baseOpts(t, "testdata/basic.ai.log")
	if _, err := Run(opts); err == nil || !strings.Contains(err.Error(), ".sse.log") {
		t.Errorf("应拒绝 .ai.log 输入，got err=%v", err)
	}
}

func TestRerunIdempotent(t *testing.T) {
	opts := baseOpts(t, "testdata/basic.sse.log")
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	first := readTasklog(t, opts.OutRoot, "events.jsonl")
	firstWS := readTasklog(t, opts.OutRoot, "WORKSPACE.json")
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	if second := readTasklog(t, opts.OutRoot, "events.jsonl"); second != first {
		t.Error("固定时钟下重跑 events.jsonl 应逐字节一致")
	}
	if second := readTasklog(t, opts.OutRoot, "WORKSPACE.json"); second != firstWS {
		t.Error("重跑不得更换工作区身份（WORKSPACE.json 保留）")
	}
}

// TestApprovalDenied — DSH v1 审计对真实形状（user-approval/src/index.ts）：
// 持久层序 tool/call → approval/asked{id,toolName,callId} → approval/decided{id,
// outcome} → tool/result。decided.outcome=rejected → tool.call.approval.denied，
// 结果 outcome=denied。
func TestApprovalDenied(t *testing.T) {
	sse := strings.Join([]string{
		"event: bridge.hello",
		`data: {"provider":"dsh","model":"m"}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"turn/start","data":{"turn":1}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"tool/call","data":{"name":"submit_findings","callId":"call_d1","arguments":"{\"findings\":[]}"}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"approval/asked","data":{"id":"apr1","toolName":"submit_findings","callId":"call_d1"}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"approval/decided","data":{"id":"apr1","outcome":"rejected"}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"tool/result","data":{"message":{"content":[{"type":"tool-result","toolCallId":"call_d1","isError":true,"content":[{"type":"text","text":"denied by user"}]}]}}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"turn/end","data":{"reason":{"kind":"stop"}}}}`,
		"",
	}, "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sse.log")
	if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, path)
	stats, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Warnings) != 0 {
		t.Fatalf("真实审计对不应产生告警: %v", stats.Warnings)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	var callIdx, resultIdx int
	var approval, outcome any
	for i, ev := range evs {
		if ev["type"] == "tool.call" {
			callIdx = i
			approval = ev["call"].(map[string]any)["approval"]
		}
		if ev["type"] == "tool.result" {
			resultIdx = i
			outcome = ev["result"].(map[string]any)["outcome"]
		}
	}
	if ap, _ := approval.(map[string]any); ap == nil || ap["decision"] != "denied" {
		t.Errorf("rejected 应挂 tool.call.approval.denied: %v", approval)
	}
	if outcome != "denied" {
		t.Errorf("被拒调用的 outcome 应为 denied，got %v", outcome)
	}
	if callIdx > resultIdx {
		t.Errorf("tool.call(seq=%d) 必须先于 tool.result(seq=%d)——因果序", callIdx, resultIdx)
	}
}

// TestApprovalOutcomeFailClosed — decided.outcome 非授权值一律 denied：
// cancelled（信号中止）与 unavailable（应答器缺失，DSH 明示 fail closed）
// 在旧启发式下被误投影为 granted（S1 开放项根因），未知值同拒。
func TestApprovalOutcomeFailClosed(t *testing.T) {
	for _, outcome := range []string{"cancelled", "unavailable", "granted-by-mistake", ""} {
		t.Run(outcome, func(t *testing.T) {
			sse := strings.Join([]string{
				"event: session.event",
				`data: {"sessionId":"main","event":{"type":"turn/start","data":{"turn":1}}}`,
				"",
				"event: session.event",
				`data: {"sessionId":"main","event":{"type":"tool/call","data":{"name":"bash","callId":"call_f1","arguments":"{}"}}}`,
				"",
				"event: session.event",
				`data: {"sessionId":"main","event":{"type":"approval/asked","data":{"id":"aprF","toolName":"bash","callId":"call_f1"}}}`,
				"",
				"event: session.event",
				fmt.Sprintf(`data: {"sessionId":"main","event":{"type":"approval/decided","data":{"id":"aprF","outcome":%q}}}`, outcome),
				"",
				"event: session.event",
				`data: {"sessionId":"main","event":{"type":"turn/end","data":{"reason":{"kind":"stop"}}}}`,
				"",
			}, "\n")
			dir := t.TempDir()
			path := filepath.Join(dir, "t.sse.log")
			if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
				t.Fatal(err)
			}
			opts := baseOpts(t, path)
			if _, err := Run(opts); err != nil {
				t.Fatal(err)
			}
			evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
			for _, ev := range evs {
				if ev["type"] == "tool.call" {
					ap, _ := ev["call"].(map[string]any)["approval"].(map[string]any)
					if ap == nil || ap["decision"] != "denied" {
						t.Fatalf("outcome=%q 必须 fail-closed 为 denied，实际 %v", outcome, ap)
					}
				}
				if ev["type"] == "tool.result" {
					if oc := ev["result"].(map[string]any)["outcome"]; oc != "denied" {
						t.Fatalf("outcome=%q 的调用结果应为 denied，实际 %v", outcome, oc)
					}
				}
			}
		})
	}
}

// TestApprovalOrphanDecided — 无 asked 对账的 decided（重复/撕裂/乱序）：
// 告警留痕+决策丢弃，不猜测挂载。
func TestApprovalOrphanDecided(t *testing.T) {
	sse := strings.Join([]string{
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"turn/start","data":{"turn":1}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"tool/call","data":{"name":"bash","callId":"call_o1","arguments":"{}"}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"approval/decided","data":{"id":"aprGhost","outcome":"allowed-once"}}}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"turn/end","data":{"reason":{"kind":"stop"}}}}`,
		"",
	}, "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sse.log")
	if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, path)
	stats, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Warnings) == 0 {
		t.Fatal("孤儿 decided 应产生告警")
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	for _, ev := range evs {
		if ev["type"] == "tool.call" {
			if ap, _ := ev["call"].(map[string]any)["approval"]; ap != nil {
				t.Fatalf("孤儿 decided 不得挂载: %v", ap)
			}
		}
	}
}

// TestSubagentSdkShapes — SDK 协议面真实形状直通锁（dsh-runtime
// packages/sdk/protocol/src/types.ts + server.ts，2026-09-19 源码实读）：
// subagent.started {parentSessionId, childSessionId} 与 subagent.finished
// {provider, agentId, parentSessionId, childSessionId, status: ok|error,
// stopReason: completed|aborted|error|max-tokens|refusal} 均为顶层通知帧
// （不入 session.event envelope）；子会话事件经 session.event 同通道到达
// （sessionId=childSessionId）。status/stop_reason 直通（baton schema 自由
// string），lastAssistantMessage 不投影（子会话消息已走 session.event）。
func TestSubagentSdkShapes(t *testing.T) {
	sse := strings.Join([]string{
		"event: subagent.started",
		`data: {"parentSessionId":"main","childSessionId":"sess-child-7f"}`,
		"",
		"event: session.event",
		`data: {"sessionId":"sess-child-7f","event":{"type":"user/message","data":{"content":[{"type":"text","text":"子任务正文"}]}}}`,
		"",
		"event: subagent.finished",
		`data: {"provider":"dsh-subagent-in-process","agentId":"sess-child-7f","parentSessionId":"main","childSessionId":"sess-child-7f","status":"error","stopReason":"max-tokens"}`,
		"",
	}, "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sse.log")
	if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, path)
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	var started, finished, childMsg bool
	for _, ev := range evs {
		if ev["type"] != "task.subagent" {
			if ev["type"] == "model.message" && ev["session"] == "sess-child-7f" {
				childMsg = true
			}
			continue
		}
		sa, _ := ev["subagent"].(map[string]any)
		if sa["parent_session"] != "main" || sa["child_session"] != "sess-child-7f" {
			t.Fatalf("parent/child 会话直通错误: %v", sa)
		}
		switch sa["phase"] {
		case "started":
			started = true
			if ev["session"] != "sess-child-7f" {
				t.Fatalf("started 事件 session 应=child: %v", ev["session"])
			}
		case "finished":
			finished = true
			if sa["status"] != "error" || sa["stop_reason"] != "max-tokens" {
				t.Fatalf("status/stop_reason 应 SDK 枚举直通: %v", sa)
			}
		}
	}
	if !started || !finished || !childMsg {
		t.Fatalf("started/finished/子会话消息三要素缺一: %v %v %v", started, finished, childMsg)
	}
}

func TestMessageContentWins(t *testing.T) {
	sse := strings.Join([]string{
		"event: bridge.hello",
		`data: {"provider":"dsh","model":"m"}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"assistant/message","data":{"turn":1,"step":0,"stream":[{"type":"text-chunks","texts":["重复正文"]}],"message":{"content":[{"type":"text","text":"权威正文"}]}}}}`,
		"",
	}, "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sse.log")
	if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, path)
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	var assistant []map[string]any
	for _, ev := range evs {
		if ev["type"] == "model.message" && ev["message"].(map[string]any)["role"] == "assistant" {
			assistant = append(assistant, ev["message"].(map[string]any))
		}
	}
	if len(assistant) != 1 {
		t.Fatalf("assistant 消息 = %d, want 1（content 权威，stream 防双记）", len(assistant))
	}
	parts := assistant[0]["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "权威正文" {
		t.Errorf("parts 应只含权威 content: %v", parts)
	}
}

// R5 帧时间戳：帧驱动事件 ts=帧接收时刻；无注释帧降级投影时刻；非法注释告警。
func TestFrameTimestamps(t *testing.T) {
	opts := baseOpts(t, "testdata/basic.sse.log")
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	// seq1/2=扫描前合成（投影时刻）；seq3 起帧驱动（帧 ts，纳秒精度可区分）
	if evs[0]["ts"] != "2026-09-17T10:00:00Z" || evs[1]["ts"] != "2026-09-17T10:00:00Z" {
		t.Errorf("合成事件应为投影时刻: %v / %v", evs[0]["ts"], evs[1]["ts"])
	}
	frameDriven := false
	for _, ev := range evs[2:] {
		ts, _ := ev["ts"].(string)
		if ts != "2026-09-17T10:00:00Z" && !strings.Contains(ts, ".123456789Z") {
			t.Errorf("帧驱动事件 ts 应为帧时刻或投影时刻, got %v", ts)
		}
		if strings.Contains(ts, ".123456789Z") {
			frameDriven = true
		}
	}
	if !frameDriven {
		t.Error("夹具含 ts 注释，应有帧时刻透传的事件")
	}

	// torn 夹具：前两帧有注释（帧时刻），其余降级投影时刻——混合文件双向兼容
	opts = baseOpts(t, "testdata/torn.sse.log")
	if _, err := Run(opts); err != nil {
		t.Fatal(err)
	}
	torn := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	sawFrameTs, sawFallback := false, false
	for _, ev := range torn[2:] {
		ts, _ := ev["ts"].(string)
		if strings.HasPrefix(ts, "2026-09-17T11:00:") {
			sawFrameTs = true
		}
		if ts == "2026-09-17T10:00:00Z" {
			sawFallback = true
		}
	}
	if !sawFrameTs || !sawFallback {
		t.Errorf("混合夹具应同时出现帧时刻与降级时刻: frameTs=%v fallback=%v", sawFrameTs, sawFallback)
	}
}

// R5 非法时间戳注释：降级投影时刻 + Warning（诚实告警不炸流）。
func TestBadTimestampDegrades(t *testing.T) {
	sse := strings.Join([]string{
		": ts not-a-time",
		"event: bridge.hello",
		`data: {"provider":"dsh","model":"m"}`,
		"",
		"event: session.event",
		`data: {"sessionId":"main","event":{"type":"user/message","data":{"content":[{"type":"text","text":"x"}]}}}`,
		"",
	}, "\n")
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sse.log")
	if err := os.WriteFile(path, []byte(sse), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := baseOpts(t, path)
	stats, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range stats.Warnings {
		if strings.Contains(w, "帧时间戳非法") {
			found = true
		}
	}
	if !found {
		t.Errorf("非法 ts 应告警: %v", stats.Warnings)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))
	if evs[2]["ts"] != "2026-09-17T10:00:00Z" {
		t.Errorf("非法 ts 后应降级投影时刻, got %v", evs[2]["ts"])
	}
}

// TestRealStreamCalibration — S1 真实流校准回归锁（2026-09-17 sim 栈 gw-dc46007…
// 实证后立）：①session.stream finish + assistant/message 双通道不双记；②真实
// callId 保真（合成仅兜底）；③permission/preset 粘性挂载；④tool/result 真实帧
// 落账（输出文本+isError），回合收束不再重复合成。
func TestRealStreamCalibration(t *testing.T) {
	opts := baseOpts(t, "testdata/real_stream.sse.log")
	stats, err := Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Warnings) != 0 {
		t.Fatalf("real_stream 夹具不应有告警: %v", stats.Warnings)
	}
	evs := parseLines(t, readTasklog(t, opts.OutRoot, "events.jsonl"))

	var assistant []map[string]any
	var calls, results, usages []map[string]any
	for _, e := range evs {
		switch e["type"] {
		case "model.message":
			if e["message"].(map[string]any)["role"] == "assistant" {
				assistant = append(assistant, e)
			}
		case "tool.call":
			calls = append(calls, e)
		case "tool.result":
			results = append(results, e)
		case "usage.recorded":
			usages = append(usages, e)
		}
	}

	// ① 双通道不双记：stream 缓冲不再于 finish 发射，assistant/message 唯一落账
	if len(assistant) != 1 {
		t.Fatalf("assistant 消息应恰好 1 条（双通道防双记），实际 %d", len(assistant))
	}
	parts := assistant[0]["message"].(map[string]any)["parts"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "先看目录结构。" ||
		parts[1].(map[string]any)["text"] != "项目共 2 个文件。" {
		t.Fatalf("assistant parts 应为 content 的 reasoning+text: %v", parts)
	}

	// ② 真实 callId 保真 ③ preset 粘性挂载
	if len(calls) != 1 {
		t.Fatalf("tool.call 应 1 条，实际 %d", len(calls))
	}
	c := calls[0]["call"].(map[string]any)
	if c["call_id"] != "call_41093a751d144ea7a3762d39" {
		t.Fatalf("call_id 应保真真实帧值，实际 %v", c["call_id"])
	}
	ap, _ := c["approval"].(map[string]any)
	if ap == nil || ap["decision"] != "preset" || ap["policy"] != "danger-full-access" {
		t.Fatalf("permission/preset 应粘性挂载到 tool.call，实际 %v", ap)
	}
	args, ok := c["args"].(map[string]any)
	if !ok {
		t.Fatalf("args 应为内联 JSON 对象: %v", c["args"])
	}
	if args["command"] != "ls /sandbox/project" {
		t.Fatalf("args.command 应内联保留: %v", args)
	}

	// ④ 真实 tool/result 帧落账：输出+isError 映射，回合收束不重复合成
	if len(results) != 1 {
		t.Fatalf("tool.result 应恰好 1 条（真实帧闭合后收束不合成），实际 %d", len(results))
	}
	r := results[0]["result"].(map[string]any)
	if r["call_id"] != "call_41093a751d144ea7a3762d39" || r["ok"] != true || r["outcome"] != "completed" {
		t.Fatalf("tool.result 事实映射错: %v", r)
	}
	if r["result"] != "/sandbox/project/.codeaudit/cpg.json\n/sandbox/project/app.py\n" {
		t.Fatalf("tool.result 输出应内联保留: %q", r["result"])
	}
	if n, _ := r["bytes"].(float64); n != 61 {
		t.Fatalf("bytes 应为输出字节数 61: %v", r["bytes"])
	}

	// usage：session.stream usage chunk 通道（真实流唯一 usage 载体）
	if len(usages) != 1 {
		t.Fatalf("usage.recorded 应 1 条，实际 %d", len(usages))
	}
	u := usages[0]["usage"].(map[string]any)
	if u["model"] != "glm-4.5" || u["input_tokens"].(float64) != 7234 || u["total_tokens"].(float64) != 8572 {
		t.Fatalf("usage 数值应与帧一致: %v", u)
	}
}
