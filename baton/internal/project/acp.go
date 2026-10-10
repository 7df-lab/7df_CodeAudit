package project

// acp.go — ACP 方言适配器（graph-ui-channels 后续批，2026-09-24 
// "ACP 日志与 baton 接线"）：acp-out.jsonl（engine acpSession 驱动器逐帧留存的
// JSON-RPC 线序：响应/通知/_driver 注记）→ 合成 SSE 帧流。**转译器而非平行
// 投影器**——与 maka.go 同模式：合成帧喂既有 sseframe.Scan+projector（回合
// 状态机/blob/lessons/handoff 全量复用，零双实现）。
//
// 映射规范（ACP 线格式=官方 v1，agentclientprotocol.com schema；
// engine acp_session.go acpStream 为消费对位）：
//   initialize 响应(id=1)          → bridge.hello（agent 名+版本；provider=acp:<name>）
//   session/update user_message_chunk → session.event user/message（我方提示词回显）
//   session/update agent_message_chunk → session.event assistant/message（text 块）
//   session/update tool_call       → session.event tool/call（name=title/kind）
//   _driver turn_done / prompt 响应(id=3) → session.event turn/end（reason.kind=stopReason）
//   _driver auto_permission        → 不投影（v0：注记无 toolCallId，硬合成审批对
//                                     污染对账信号；代答审计留痕在 engine humanLog ⚠ 行）
//
// 金丝雀纪律（同 maka）：帧形状白名单，白名单外计 Parked（投影继续不丢行），
// .tasklog/PARKED 标注不确定窗口。
// 时间源：acp-out.jsonl 行不带到达时刻（驱动器逐帧 verbatim 留存）——ts 留空，
// projector 降级投影时刻（R5 语义，不虚构）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// acpKnown — 帧形状白名单（金丝雀判据）：JSON-RPC 响应 id 面 + sessionUpdate 标签面 + _driver 注记面。
var acpKnown = map[string]bool{
	"agent_message_chunk": true, "user_message_chunk": true,
	"tool_call": true, "tool_call_update": true, "plan": true,
	"available_commands_update": true, "current_mode_update": true, "config_option_update": true,
	"turn_done": true, "fatal": true, "auto_permission": true, "unsupported_request": true,
}

// acpStats 复用 dialectStats 共用型（方言转译统计：Warnings/Parked 金丝雀同链路）。

// transpileACP 把 acp-out.jsonl 转译为合成 SSE 帧流（与 engine writeRaw 产物同构：
// ": ts" 注释行 + event:/data: 行；ts 恒空=投影时刻降级）。
func transpileACP(path string) (*bytes.Buffer, *dialectStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	as := &dialectStats{}
	out := &bytes.Buffer{}
	helloDone := false
	sid := "main" // ACP 单会话模型（v0 每回合新会话）；会话号入事件不参与路由
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var fr map[string]any
		if json.Unmarshal([]byte(line), &fr) != nil {
			as.Parked = append(as.Parked, fmt.Sprintf("line %d: 帧 非 JSON 对象", lineNo))
			continue
		}
		for _, s := range acpFrames(fr, &helloDone, as, lineNo, &sid) {
			fmt.Fprintf(out, ": ts \nevent: %s\ndata: %s\n\n", s.name, s.data)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("acp 帧流扫描失败: %w", err)
	}
	return out, as, nil
}

// acpFrames — 单帧 → 合成 SSE 帧组（白名单外返回空并计 Parked）。
func acpFrames(fr map[string]any, helloDone *bool, as *dialectStats, lineNo int, sid *string) []sseFrame {
	if note, ok := fr["_driver"].(string); ok {
		if !acpKnown[note] {
			as.Parked = append(as.Parked, fmt.Sprintf("line %d: _driver 注记 %q 白名单外（版本漂移？）", lineNo, note))
			return nil
		}
		switch note {
		case "turn_done":
			sr, _ := fr["stopReason"].(string)
			return []sseFrame{acpTurnEnd(*sid, sr)}
		case "auto_permission":
			// v0 不投影：projector 的审批对账挂真实工具调用（asked.callId→待发
			// 缓冲），而驱动器注记不带 toolCallId——硬合成=对账告警污染档案质量
			// 信号。代答审计在 engine humanLog ⚠ 行
			// 已留痕。升级路径：驱动器 auto_permission 注记补 toolCallId（ACP
			// request_permission 参数自带）→ asked/decided 挂真实调用。
			return nil
		default:
			return nil // fatal/unsupported_request 等驱动器面：投影不消费（engine 侧已处置）
		}
	}
	id, hasID := fr["id"].(float64)
	method, _ := fr["method"].(string)
	switch {
	case hasID && method == "":
		res, _ := fr["result"].(map[string]any)
		switch int(id) {
		case 1:
			if *helloDone {
				return nil
			}
			*helloDone = true
			provider := "acp"
			if ai, _ := res["agentInfo"].(map[string]any); ai != nil {
				if n, _ := ai["name"].(string); n != "" {
					provider = "acp:" + n
				}
			}
			return []sseFrame{mustFrame("bridge.hello", map[string]any{"provider": provider, "model": ""})}
		case 2:
			if s, ok := res["sessionId"].(string); ok && s != "" {
				*sid = s
			}
			return nil
		case 3:
			sr, _ := res["stopReason"].(string)
			return []sseFrame{acpTurnEnd(*sid, sr)}
		}
		return nil // 其他响应 id：白名单内（握手面）但不投影
	case method == "session/update":
		params, _ := fr["params"].(map[string]any)
		upd, _ := params["update"].(map[string]any)
		if upd == nil {
			return nil
		}
		tag, _ := upd["sessionUpdate"].(string)
		if !acpKnown[tag] {
			as.Parked = append(as.Parked, fmt.Sprintf("line %d: sessionUpdate %q 白名单外（版本漂移？）", lineNo, tag))
			return nil
		}
		if s, ok := params["sessionId"].(string); ok && s != "" {
			*sid = s
		}
		content, _ := upd["content"].(map[string]any)
		switch tag {
		case "agent_message_chunk":
			if t, _ := content["text"].(string); t != "" {
				return []sseFrame{mustFrame("session.event", map[string]any{
					"sessionId": *sid,
					"event": map[string]any{"type": "assistant/message",
						"data": map[string]any{"message": map[string]any{
							"content": []any{map[string]any{"type": "text", "text": t}}}}},
				})}
			}
			return nil
		case "user_message_chunk":
			if t, _ := content["text"].(string); t != "" {
				return []sseFrame{mustFrame("session.event", map[string]any{
					"sessionId": *sid,
					"event": map[string]any{"type": "user/message",
						"data": map[string]any{"content": []any{map[string]any{"type": "text", "text": t}}}},
				})}
			}
			return nil
		case "tool_call":
			name, _ := upd["title"].(string)
			if name == "" {
				name, _ = upd["kind"].(string)
			}
			args := ""
			if ri, _ := upd["rawInput"].(string); ri != "" {
				args = ri
			} else if ri, ok := upd["rawInput"].(map[string]any); ok {
				if b, err := json.Marshal(ri); err == nil {
					args = string(b)
				}
			}
			return []sseFrame{mustFrame("session.event", map[string]any{
				"sessionId": *sid,
				"event": map[string]any{"type": "tool/call",
					"data": map[string]any{"name": name, "arguments": args}},
			})}
		default:
			return nil // plan/mode/tool_call_update 等过程面：白名单内不投影（对齐 DSH 未映射词表先例）
		}
	}
	return nil // 客户端→agent 方向帧（驱动器不落）与未知方法：不投影
}

// acpTurnEnd — stopReason → turn/end（reason.kind 语义对齐 DSH：end_turn 等直传；
// sessionId 必须与 update 帧同源——错号会被 projector 判会话 EOF 中断，冒烟实证）。
func acpTurnEnd(sid, stopReason string) sseFrame {
	if stopReason == "" {
		stopReason = "end_turn"
	}
	return mustFrame("session.event", map[string]any{
		"sessionId": sid,
		"event":     map[string]any{"type": "turn/end", "data": map[string]any{"reason": map[string]any{"kind": stopReason}}},
	})
}
