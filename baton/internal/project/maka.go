package project

// maka.go — Maka 方言适配器：RuntimeEventLog
// （JSONL，"the log is the runtime"）→ 合成 SSE 帧流。**转译器而非平行投影器**——
// 合成帧喂给既有 sseframe.Scan+projector（回合状态机/blob/lessons/handoff 续写
// 全量复用，零双实现）。映射规范=maka-integration.md §3（九族映射，字段级搬运）。
//
// 金丝雀纪律（README §5 原文）：版本钉死 + 格式漂移即 park——schema 校验失败的行
// 计入 Parked，投影继续（丢行=丢档案，违背中立档案公理），但落 .tasklog/PARKED
// 标记文件，brief 渲染时标注不确定窗口。
//
// 形状锚点分级：
//   - S2 真实样本实锚（.agent/evidence/maka-s2-20260919/s2-success-session-events.jsonl，
//     六事件全序列）：invocation_opened/text/thinking/runtime_fact/completed；
//   - 上游源码形状：
//     function_call/function_response/permission 决策——**待真实样本校准**；
//   - continuation/handoff（invocation_opened content.source.kind）：continuation 随
//     多回合任务自然出现（样本校准）；handoff 类按源码形状识别+审计留痕，待真实
//     跨 harness 换手样本（v1 定档）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// makaKnownKinds — RuntimeEventLog event_kind 白名单（金丝雀判据）。
var makaKnownKinds = map[string]bool{
	"invocation_opened": true,
	"text":              true,
	"thinking":          true,
	"function_call":     true,
	"function_response": true,
	"runtime_fact":      true,
	"completed":         true,
	"error":             true,
	"system_note":       true, // 映射表 v0 不投影（对齐 DSH 未映射词表先例）——合法但无声
	"tool_dispatch":     true, // 系统调度事件（author=system；2026-09-22 sim 实锚）——不投影
}

// dialectStats 方言转译统计与金丝雀结果（maka/acp 共用型）。Parked 非空=投影不确定窗口（brief 标注）。
type dialectStats struct {
	Warnings []string
	Parked   []string // 每条=一行原因（行号+违规点）
}

// transpileMaka 把 RuntimeEventLog JSONL 转译为合成 SSE 帧流（": ts" 注释行 +
// event:/data: 行，与 engine writeRaw 产物同构）。schema 违规行不产出帧但计 Parked。
func transpileMaka(path string) (*bytes.Buffer, *dialectStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	ms := &dialectStats{}
	out := &bytes.Buffer{}
	helloDone := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			ms.Parked = append(ms.Parked, fmt.Sprintf("line %d: envelope 非 JSON 对象", lineNo))
			continue
		}
		kind, _ := env["event_kind"].(string)
		if !makaKnownKinds[kind] {
			ms.Parked = append(ms.Parked, fmt.Sprintf("line %d: event_kind %q 白名单外（版本漂移？）", lineNo, kind))
			continue
		}
		pj, _ := env["payload_json"].(string)
		var payload map[string]any
		if pj == "" || json.Unmarshal([]byte(pj), &payload) != nil {
			ms.Parked = append(ms.Parked, fmt.Sprintf("line %d: payload_json 缺失或非对象", lineNo))
			continue
		}
		ts := makaTs(env["committed_at"])
		sid := "main" // maka 单会话模型：session_id 恒定，归一到 baton mainSession
		var frames []sseFrame
		switch kind {
		case "invocation_opened":
			frames = makaInvocationOpened(payload, &helloDone, ms, lineNo)
		case "text":
			frames = makaText(payload, sid)
		case "thinking":
			frames = makaThinking(payload, sid)
		case "function_call":
			frames = makaFunctionCall(payload, sid)
		case "function_response":
			frames = makaFunctionResponse(payload, sid)
		case "runtime_fact":
			frames = makaRuntimeFact(payload, sid)
		case "completed":
			frames = makaCompleted(payload, sid)
		case "error":
			frames = makaError(payload, sid, ms, lineNo)
		}
		for _, fr := range frames {
			fmt.Fprintf(out, ": ts %s\nevent: %s\ndata: %s\n\n", ts, fr.name, fr.data)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("RuntimeEventLog 扫描失败: %w", err)
	}
	return out, ms, nil
}

// sseFrame 合成帧（event 名 + data JSON 文本）。
type sseFrame struct{ name, data string }

// mustFrame JSON 序列化兜底（map[string]any 序列化不会失败；失败=空对象占位）。
func mustFrame(name string, data map[string]any) sseFrame {
	b, err := json.Marshal(data)
	if err != nil {
		return sseFrame{name: name, data: "{}"}
	}
	return sseFrame{name: name, data: string(b)}
}

// makaTs committed_at（UnixMilli）→ RFC3339Nano（": ts" 注释口径）；非法=投影时刻
// （空 Ts 由 projector 降级，R5 语义复用）。
func makaTs(v any) string {
	ms, ok := v.(float64)
	if !ok {
		return ""
	}
	return time.UnixMilli(int64(ms)).UTC().Format(time.RFC3339Nano)
}

// makaContent payload.content 统一取对象（S2 实锚：content 恒为对象）。
func makaContent(payload map[string]any) map[string]any {
	c, _ := payload["content"].(map[string]any)
	return c
}

// makaAuthor payload.author（user|agent|system）。
func makaAuthor(payload map[string]any) string {
	a, _ := payload["author"].(string)
	return a
}

// makaInvocationOpened — 首个 invocation：bridge.hello（模型名唯一来源
// content.route.modelId，S2 实锚 glm-5.3-flash）+ turn/start；后续 invocation：
// turn/start。content.source.kind 非 fresh（continuation/handoff）→ 审计 warning
// 留痕（源指针字段级搬运进档案可观测面；v1 定档投影形状）。
func makaInvocationOpened(payload map[string]any, helloDone *bool, ms *dialectStats, lineNo int) []sseFrame {
	c := makaContent(payload)
	frames := []sseFrame{}
	if !*helloDone {
		*helloDone = true
		model := ""
		if c != nil {
			if route, ok := c["route"].(map[string]any); ok {
				model, _ = route["modelId"].(string)
			}
		}
		frames = append(frames, mustFrame("bridge.hello", map[string]any{
			"provider": "maka", "model": model,
		}))
	}
	// 每个 invocation=新回合（恰好一次 turn/start；hello 首发不叠加——B2 修复）
	if c != nil {
		if src, ok := c["source"].(map[string]any); ok {
			if k, _ := src["kind"].(string); k != "" && k != "fresh" {
				hw, _ := src["sourceRuntimeEventHighWater"].(float64)
				ms.Warnings = append(ms.Warnings, fmt.Sprintf(
					"line %d: maka invocation source=%s（源事件高位水 %.0f）——换手指针已留痕，投影形状 v1 定档", lineNo, k, hw))
			}
		}
	}
	return append(frames, mustFrame("session.event", map[string]any{
		"sessionId": "main",
		"event":     map[string]any{"type": "turn/start"},
	}))
}

// makaText — user→user/message；agent→assistant/message（content 权威形态，
// 防双记语义复用）。S2 实锚：payload.content={kind:"text",text}。
func makaText(payload map[string]any, sid string) []sseFrame {
	c := makaContent(payload)
	text, _ := c["text"].(string)
	if text == "" {
		return nil
	}
	if makaAuthor(payload) == "user" {
		// content=块数组（DSH user/message 真实流形状，contentText 消费口径）
		return []sseFrame{mustFrame("session.event", map[string]any{
			"sessionId": sid,
			"event": map[string]any{"type": "user/message",
				"data": map[string]any{"content": []any{
					map[string]any{"type": "text", "text": text}}}},
		})}
	}
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "assistant/message",
			"data": map[string]any{"message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}}}},
	})}
}

// makaThinking — thinking→assistant/message reasoning part（content 权威直译，
// 不走 chunk 缓冲——合成流无撕裂风险，缓冲是给真实流式的）。S2 实锚形状。
func makaThinking(payload map[string]any, sid string) []sseFrame {
	c := makaContent(payload)
	text, _ := c["text"].(string)
	if text == "" {
		return nil
	}
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "assistant/message",
			"data": map[string]any{"message": map[string]any{
				"content": []any{map[string]any{"type": "thinking", "text": text}}}}},
	})}
}

// makaFunctionCall — function_call→tool/call。sim 实锚（2026-09-22 真实工具回合
// 26 事件）：content={args:{...}, id:"call_…", kind, name}；args 对象原值直传
// （projector toolCall 非字符串形状原值入库纪律复用）。
func makaFunctionCall(payload map[string]any, sid string) []sseFrame {
	c := makaContent(payload)
	if c == nil {
		return nil
	}
	name, _ := c["name"].(string)
	id, _ := c["id"].(string)
	args := c["args"]
	if _, isStr := args.(string); !isStr && args != nil {
		// 对象形状原值保留（projector toolCall 非字符串纪律同源）
		if b, err := json.Marshal(args); err == nil {
			args = string(b)
		}
	}
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "tool/call",
			"data": map[string]any{"callId": id, "name": name, "arguments": args}},
	})}
}

// makaFunctionResponse — function_response→tool/result。sim 实锚：
// content={id, isError?, modelProjection{kind:text|json, text|value}, name, result}。
// 结果文本取 modelProjection（模型视图；json 归一时 value 序列化）；isError 透传；
// 无 modelProjection 回落 result 原值。
func makaFunctionResponse(payload map[string]any, sid string) []sseFrame {
	c := makaContent(payload)
	if c == nil {
		return nil
	}
	id, _ := c["id"].(string)
	isErr, _ := c["isError"].(bool)
	resText := ""
	if mp, ok := c["modelProjection"].(map[string]any); ok {
		if t, _ := mp["text"].(string); t != "" {
			resText = t
		} else if mp["value"] != nil {
			if b, err := json.Marshal(mp["value"]); err == nil {
				resText = string(b)
			}
		}
	}
	if resText == "" {
		switch r := c["result"].(type) {
		case string:
			resText = r
		default:
			if r != nil {
				if b, err := json.Marshal(r); err == nil {
					resText = string(b)
				}
			}
		}
	}
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "tool/result",
			"data": map[string]any{"message": map[string]any{"content": []any{map[string]any{
				"type": "tool-result", "toolCallId": id, "isError": isErr,
				"content": []any{map[string]any{"type": "text", "text": resText}},
			}}}}},
	})}
}

// makaRuntimeFact — tokenUsage→usage（S2 实锚 tokenUsage 键集；DSH usage 形状
// 映射：input=总输入/output/cacheHitInput→cacheRead/cacheWriteInput→cacheWrite）。
func makaRuntimeFact(payload map[string]any, sid string) []sseFrame {
	// S2 实锚：actions 在 payload 顶层（runtime_fact 无 content 包裹）；
	// content 包裹形态作兼容回落。
	actions, _ := payload["actions"].(map[string]any)
	if actions == nil {
		if c := makaContent(payload); c != nil {
			actions, _ = c["actions"].(map[string]any)
		}
	}
	if actions == nil {
		return nil
	}
	tu, _ := actions["tokenUsage"].(map[string]any)
	if tu == nil {
		return nil // runtime_fact 其他子类（锚点等）：合法但非 usage，不投影
	}
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "assistant/chunk",
			"data": map[string]any{"chunk": map[string]any{"type": "usage",
				"usage": map[string]any{
					"inputTokens":      numOr(tu["input"], tu["cacheHitInput"], tu["cacheMissInput"]),
					"outputTokens":     numOr(tu["output"]),
					"cacheReadTokens":  numOr(tu["cacheHitInput"]),
					"cacheWriteTokens": numOr(tu["cacheWriteInput"]),
					"totalTokens":      numOr(tu["total"]),
				}}}},
	})}
}

// makaCompleted — completed→turn/end（stateDelta.stopReason=end_turn→stop；
// 其余→error 语义）。S2 实锚 stopReason="end_turn"。
func makaCompleted(payload map[string]any, sid string) []sseFrame {
	kind := "stop"
	if c := makaContent(payload); c != nil {
		if a, ok := c["actions"].(map[string]any); ok {
			if sd, ok := a["stateDelta"].(map[string]any); ok {
				if sr, _ := sd["stopReason"].(string); sr != "" && sr != "end_turn" {
					kind = "error"
				}
			}
		}
	}
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "turn/end",
			"data": map[string]any{"reason": map[string]any{"kind": kind}}},
	})}
}

// makaError — error 事件→turn/end reason=error（失败要点提炼器的输入面；
// payload 文本摘要进 warning 可观测）。**待真实样本校准**。
func makaError(payload map[string]any, sid string, ms *dialectStats, lineNo int) []sseFrame {
	summary := ""
	if c := makaContent(payload); c != nil {
		if t, _ := c["text"].(string); t != "" {
			summary = t
		}
	}
	if len(summary) > 200 {
		summary = summary[:200]
	}
	ms.Warnings = append(ms.Warnings, fmt.Sprintf("line %d: maka error 事件: %s", lineNo, summary))
	return []sseFrame{mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event": map[string]any{"type": "turn/end",
			"data": map[string]any{"reason": map[string]any{"kind": "error"}}},
	})}
}

// numOr 取首个可数值化的键（maka tokenUsage 键名与 DSH 不同名的桥）。
func numOr(vals ...any) any {
	for _, v := range vals {
		switch n := v.(type) {
		case float64:
			return n
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return f
			}
		}
	}
	return 0
}
