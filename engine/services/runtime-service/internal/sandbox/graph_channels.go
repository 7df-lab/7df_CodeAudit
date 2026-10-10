// graph_channels.go — 图节点会话（g-<nodeID>）的逐节点 AI 交互通道渲染
// 。
//
// 立场（plan §1/Q6-Q11 裁决）：会话身份在数据面全程在帧上（session.stream/
// session.event/session.status 均带 sessionId），拍平发生在 sseParser 的渲染选择
// （非焦点会话正文静默=ADR-181）。本文件恢复复用：g-* 会话的事件**双轨**——
//
//	主 flat 流：行为字节等价不变（非焦点=骨架行/静默；焦点=全投影）——既有 golden
//	            全量锁定，零回归面；
//	节点通道：  每节点一个 channelRenderer 实例，独立渲染状态（mode/逐字去重/
//	            轮账），完整 💭/✍/回合/收束 时间线写入 OnChannelLog。
//
// 独立实例而非重构 sessionEvent 的理由：主路径是 ADR-181/236/190/192 回归锁
// 覆盖的最热路径，参数化重构的回归面远大于 ~150 行忠实移植；等价性由
// golden 交叉对照测试锁死（同帧序列：焦点主渲染 vs 通道渲染必须逐字节相等）。
//
// 同步义务：sessionEvent/assistantChunk/assistantAttempt/userMessage 的渲染语义
// 变更时，本文件的移植必须同步（TestGraphChannel_FocusedEquivalence 会红）。
package sandbox

import (
	"fmt"
	"strings"
)

// graphNodeSIDPrefix — 图节点会话 id 前缀（graphNode.SessionID()="g-"+ID）。
const graphNodeSIDPrefix = "g-"

// nodeIDOfSID — g-<nodeID> → nodeID（非图会话返回空）。
func nodeIDOfSID(sid string) string {
	if strings.HasPrefix(sid, graphNodeSIDPrefix) && len(sid) > len(graphNodeSIDPrefix) {
		return sid[len(graphNodeSIDPrefix):]
	}
	return ""
}

// channelRenderer — 单节点会话的通道渲染器（状态口径对齐 sseParser 的会话级子集）。
// 消费侧：consume() 的 session.event / session.stream / session.status 三路由点，
// 仅 stream goroutine 单线程触达（同 parser 状态的线程模型）。
type channelRenderer struct {
	nodeID string
	out    func(text string)

	mode     string  // 当前流式块类型（💭/✍ 头控制；对齐 p.mode）
	liveTurn float64 // R93 逐字去重状态（对齐 p.liveTurn/liveStep/liveSeen）
	liveStep float64
	liveSeen bool
	attempts map[string][2]float64
	maxTurn  float64 // 本节点会话已见最大轮号（预留计量面；节点会话从 1 起，无偏移）
}

// emit / emitLine — 通道出口（对齐 p.emit/p.emitLine 的换行语义）。
func (c *channelRenderer) emit(s string) {
	if s != "" {
		c.out(s)
	}
}

func (c *channelRenderer) emitLine(s string) {
	if s != "" {
		c.out(s + "\n")
	}
}

// renderEvent — session.event 的通道投影（sessionEvent 的忠实移植，去掉
// *bridgeEvent 收敛信号——通道只渲染不参与 turn() 收敛判据）。
func (c *channelRenderer) renderEvent(ev map[string]any) {
	et, _ := ev["type"].(string)
	data, _ := ev["data"].(map[string]any)
	switch et {
	case "permission/preset", "sandbox/mode", "approval/policy", "approval/asked", "approval/decided",
		"session/title", "request/header", "request/context", "todo/write",
		"step/start", "step/end", "agent/inbox/spliced":
		// 机器噪音/步骤粒度静默（ADR-181 同口径）
	case "turn/start":
		tn, _ := data["turn"].(float64)
		if tn > c.maxTurn {
			c.maxTurn = tn
		}
		c.emitLine(fmt.Sprintf("── 第 %v 轮开始 ──", tn)) // 节点会话轮次从 1 起（无 turnBase 偏移）
		c.liveSeen = false
	case "user/message":
		c.userMessage(data)
	case "assistant/chunk":
		chunk, _ := data["chunk"].(map[string]any)
		c.assistantChunk(chunk)
	case "assistant/attempt":
		c.assistantAttempt(data)
	case "assistant/message":
		// ADR-236（R93）同口径：本 (turn,step) 已有 session.stream 逐字渲染时
		// 不再展开内嵌 stream（防双渲染）。
		turn, _ := data["turn"].(float64)
		step, _ := data["step"].(float64)
		live := c.liveSeen && turn == c.liveTurn && step == c.liveStep
		c.liveSeen = false
		if !live {
			if stream, _ := data["stream"].([]any); len(stream) > 0 {
				c.assistantAttempt(data)
			}
		}
	case "tool/call":
		name, _ := data["name"].(string)
		args, _ := data["arguments"].(string)
		c.emitLine(fmt.Sprintf("⟐ 工具调用 %s（参数 %d 字节）", name, len(args)))
	case "turn/end":
		reason, _ := data["reason"].(map[string]any)
		kind, _ := reason["kind"].(string)
		if kind == "error" {
			eo, _ := reason["error"].(map[string]any)
			msg, _ := eo["message"].(string)
			c.emitLine(fmt.Sprintf("── 回合结束: ❌ 错误（%s）──", msg))
			return
		}
		c.emitLine(fmt.Sprintf("── 回合结束: %s ──", kind))
	}
}

// renderStreamChunk — session.stream 帧的通道投影（对齐 consume() 的
// start/end/chunk 三元处理：start 登记 (turn,step) 查表、end 清表、chunk 渲染）。
func (c *channelRenderer) renderStreamChunk(fm map[string]any) {
	ft, _ := fm["type"].(string)
	attemptID, _ := fm["attemptId"].(string)
	switch ft {
	case "start":
		if attemptID != "" {
			if c.attempts == nil {
				c.attempts = map[string][2]float64{}
			}
			t, _ := fm["turn"].(float64)
			st, _ := fm["step"].(float64)
			c.attempts[attemptID] = [2]float64{t, st}
		}
	case "end":
		delete(c.attempts, attemptID)
	case "chunk":
		if ts, ok := c.attempts[attemptID]; ok {
			c.liveTurn, c.liveStep = ts[0], ts[1]
		}
		chunk, _ := fm["chunk"].(map[string]any)
		if chunk == nil {
			return
		}
		if ct, _ := chunk["type"].(string); ct == "text-delta" || ct == "reasoning-delta" {
			if txt, _ := chunk["text"].(string); txt != "" {
				c.liveSeen = true
			}
		}
		c.assistantChunk(chunk)
	}
}

// renderStatus — session.status 的通道投影：节点会话 idle=该通道收束行
// （"■ 会话空闲（收束）"，对齐焦点路径口径）；running=过程噪音静默。
func (c *channelRenderer) renderStatus(st string) {
	if st == "idle" {
		c.emitLine("■ 会话空闲（收束）")
	}
}

// userMessage — 任务下发/系统上下文/子任务回报判定（p.userMessage 忠实移植）。
func (c *channelRenderer) userMessage(data map[string]any) {
	text := userText(data)
	if text == "" || isSystemContext(text) {
		return
	}
	if strings.HasPrefix(text, "Background subagent") && strings.Contains(text, "reported:") {
		c.emitLine(fmt.Sprintf("📋 [子任务回报]（%d 字节）", len(text)))
		c.emitLine(text)
		return
	}
	c.emitLine(fmt.Sprintf("📋 [任务下发]（%d 字节）", len(text)))
	c.emitLine(text)
}

// assistantChunk — 流式 chunk（p.assistantChunk 忠实移植：💭/✍ 分块头+正文原样；
// 工具/用量静默）。
func (c *channelRenderer) assistantChunk(chunk map[string]any) {
	ct, _ := chunk["type"].(string)
	switch ct {
	case "block-start":
		bt, _ := chunk["blockType"].(string)
		c.mode = bt
		switch bt {
		case "reasoning":
			c.emit("\n💭 [思考]\n")
		case "text":
			c.emit("\n✍ [输出]\n")
		}
	case "block-end":
		if c.mode == "reasoning" || c.mode == "text" {
			c.emit("\n")
		}
		c.mode = ""
	case "reasoning-delta":
		if c.mode != "reasoning" {
			c.mode = "reasoning"
			c.emit("\n💭 [思考]\n")
		}
		if t, _ := chunk["text"].(string); t != "" {
			c.emit(t)
		}
	case "text-delta":
		if c.mode != "text" {
			c.mode = "text"
			c.emit("\n✍ [输出]\n")
		}
		if t, _ := chunk["text"].(string); t != "" {
			c.emit(t)
		}
	case "tool-call-delta", "usage", "finish":
		// 静默（ADR-181）
	}
}

// assistantAttempt — 收敛聚合展开（p.assistantAttempt 忠实移植：块身份=(类型,index)，
// index 变化补尾换行+新头；tool-call-chunks 关闭正文块）。
func (c *channelRenderer) assistantAttempt(data map[string]any) {
	stream, _ := data["stream"].([]any)
	mode := ""
	idx := -1
	closeBlock := func() {
		if mode != "" {
			c.emit("\n")
			mode = ""
			idx = -1
		}
	}
	open := func(want string, index int, head string) {
		if mode != want || idx != index {
			closeBlock()
			mode = want
			idx = index
			c.emit(head)
		}
	}
	recordIndex := func(rec map[string]any) int {
		if f, ok := rec["index"].(float64); ok {
			return int(f)
		}
		return -1
	}
	texts := func(v any) []string {
		arr, _ := v.([]any)
		out := make([]string, 0, len(arr))
		for _, t := range arr {
			if s, ok := t.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	emitTexts := func(want, head string, index int, ss []string) {
		if len(ss) == 0 {
			return
		}
		open(want, index, head)
		for _, s := range ss {
			c.emit(s)
		}
	}
	for _, item := range stream {
		rec, _ := item.(map[string]any)
		if rec == nil {
			continue
		}
		switch rt, _ := rec["type"].(string); rt {
		case "text-chunks":
			emitTexts("text", "\n✍ [输出]\n", recordIndex(rec), texts(rec["texts"]))
		case "reasoning-chunks":
			emitTexts("reasoning", "\n💭 [思考]\n", recordIndex(rec), texts(rec["texts"]))
		case "tool-call-chunks":
			closeBlock()
		case "chunk":
			cm, _ := rec["chunk"].(map[string]any)
			switch ct, _ := cm["type"].(string); ct {
			case "text-delta":
				open("text", recordIndex(cm), "\n✍ [输出]\n")
				if t, _ := cm["text"].(string); t != "" {
					c.emit(t)
				}
			case "reasoning-delta":
				open("reasoning", recordIndex(cm), "\n💭 [思考]\n")
				if t, _ := cm["text"].(string); t != "" {
					c.emit(t)
				}
			}
		}
	}
	closeBlock()
}

// channelFor — 取（或惰性建）sid 对应的节点通道渲染器；非图会话/未接
// OnChannelLog 返回 nil（路由点据此零开销跳过）。首建即出会话头行。
func (p *sseParser) channelFor(sid string) *channelRenderer {
	nodeID := nodeIDOfSID(sid)
	if nodeID == "" || p.onChannel == nil {
		return nil
	}
	if p.channels == nil {
		p.channels = map[string]*channelRenderer{}
	}
	if c, ok := p.channels[sid]; ok {
		return c
	}
	c := &channelRenderer{nodeID: nodeID, out: func(t string) { p.onChannel(nodeID, t) }}
	p.channels[sid] = c
	c.emitLine(fmt.Sprintf("══ 节点会话 %s ══", sid))
	return c
}
