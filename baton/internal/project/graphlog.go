// graph.log 第二输入（切片 5+6 ADR-001）：engine graphEmitter 的 JSONL 事件族
// 读入与 ts 归并队列。信封形状 {type,seq,time,data}（engine sandbox/graph_events.go
// 同源契约）；撕裂行丢弃+Warning（SSE 撕裂同纪律）。
package project

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"baton/internal/model"
)

// graphEnvelope — engine graphEmitter 单行形状。
type graphEnvelope struct {
	Type string         `json:"type"`
	Seq  int            `json:"seq"`
	Time string         `json:"time"`
	Data map[string]any `json:"data"`
}

// graphTime — 事件时刻（非法=零值=排最早——诚实降级不丢事件，Warning 由调用侧记）。
func (g *graphEnvelope) graphTime() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, g.Time)
	return t
}

// loadGraphEvents — 读 graph.log 全量（不存在=nil=无图任务；非法行丢弃+Warning；
// 扫描错误（超 1MiB 行/读错误）返回 error 走退出 1——与 sseframe ErrTooLong 同纪律，
// 静默截断会让剩余 graph 事件无声消失、spine open 永久失真）。
func loadGraphEvents(path string) ([]*graphEnvelope, []string, error) {
	if path == "" {
		return nil, nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, nil // 无 graph.log=非图任务（向后兼容，非错误）
	}
	defer f.Close()
	var out []*graphEnvelope
	var warns []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env graphEnvelope
		if err := json.Unmarshal(line, &env); err != nil || env.Type == "" {
			warns = append(warns, "graph.log 非法行（丢弃）: "+truncateForWarn(string(line)))
			continue
		}
		if env.graphTime().IsZero() {
			warns = append(warns, "graph.log 事件时间戳非法/缺失（按最早序降级）: "+env.Type)
		}
		out = append(out, &env)
	}
	if err := sc.Err(); err != nil {
		return nil, warns, fmt.Errorf("graph.log 扫描失败（行 %d 起丢弃）: %w", len(out)+len(warns)+1, err)
	}
	return out, warns, nil
}

// flushGraphBefore — 归并发射时刻 <= 边界的 graph 事件（同刻 graph 先——ADR-001）。
func (p *projector) flushGraphBefore(edge time.Time) {
	for p.graphIdx < len(p.graphQ) && !p.graphQ[p.graphIdx].graphTime().After(edge) {
		p.applyGraphEnv(p.graphQ[p.graphIdx])
		p.graphIdx++
	}
}

// flushGraphRemaining — SSE 段后剩余 graph 事件（EOF 归并尾；含零值时刻的降级事件）。
func (p *projector) flushGraphRemaining() {
	for p.graphIdx < len(p.graphQ) {
		p.applyGraphEnv(p.graphQ[p.graphIdx])
		p.graphIdx++
	}
}

// applyGraphEnv — graph.* 事件映射（域中立 C4）+ spine open 折叠。
// session 归属：node_started 按 data.sessionId、node_result 按 g-<nodeId> 推导
// （engine §1 sessionId=g-<nodeId> 契约）；其余=main。
func (p *projector) applyGraphEnv(env *graphEnvelope) {
	data := env.Data
	str := func(k string) string { s, _ := data[k].(string); return s }
	ev := &model.Event{Type: env.Type}
	if !env.graphTime().IsZero() {
		p.curTs = env.Time // 事件发生时刻优先（graph.log time 字段恒在——engine 发射契约）
		defer func() { p.curTs = "" }()
	}
	switch env.Type {
	case "graph.node_declared":
		var from []string
		if fa, ok := data["from"].([]any); ok {
			for _, f := range fa {
				if fs, ok := f.(string); ok {
					from = append(from, fs)
				}
			}
		}
		ev.GraphNodeDeclared = &model.GraphNodeDeclared{
			NodeID: str("nodeId"), Type: str("type"), Intent: str("intent"),
			DeclaredBy: str("declaredBy"), From: from,
		}
		p.graphOpenAdd(str("nodeId"))
		p.emit(ev)
		p.emitSpine() // open 增→脊柱快照（§1 spine 折叠）
	case "graph.node_started":
		ev.Session = str("sessionId")
		ev.GraphNodeStarted = &model.GraphNodeStarted{
			NodeID: str("nodeId"), SessionID: str("sessionId"), TurnRef: str("turnRef")}
		p.emit(ev)
	case "graph.node_result":
		nodeID := str("nodeId")
		ev.Session = "g-" + nodeID
		res := &model.GraphNodeResult{
			NodeID: nodeID, Outcome: str("outcome"), ResultRef: str("resultRef")}
		if ue, ok := data["usageEstimate"].(map[string]any); ok {
			if turns, ok := ue["turns"].(float64); ok {
				res.UsageEstimate = &model.GraphUsageEstimate{Turns: turns}
			}
		}
		ev.GraphNodeResult = res
		p.emit(ev)
		switch res.Outcome { // 折叠规则与 engine GraphState 一致（ADR-001 跨仓契约）
		case "completed", "failed", "rejected":
			p.graphOpenRemove(nodeID)
			p.emitSpine() // open 减→脊柱快照
		case "indeterminate":
			// 留 open（待重派，D1）
		default:
			// R121 P3：rogue outcome 破坏 schema SSOT（档案可验证性）——告警留痕
			//（事件仍如实入档：中立日志记录引擎实发，不篡改）。
			p.stats.Warnings = append(p.stats.Warnings,
				fmt.Sprintf("graph.node_result outcome %q 不在枚举（engine 契约漂移？）", res.Outcome))
		}
	case "graph.node_claimed", "graph.node_released", "graph.node_lease_expired",
		"graph.node_reopened", "graph.human_message":
		// v0.5 认领层+人机共编辑（§2.2/§4）：五事件载荷全量搬运（谁认领/谁说
		// 什么——审计可重建）；open 折叠：reopened 回 declared→open 加回（幂等），
		// 其余不改 open。emit 先于 spine 快照（UpdatedAtSeq 含本事件——B13 顺序
		// 与 declared 路径一致）。
		audit := &model.GraphAudit{NodeID: str("nodeId")}
		if h, ok := data["holder"].(string); ok {
			audit.Holder = h
		}
		if lu, ok := data["leaseUntil"].(string); ok {
			audit.LeaseUntil = lu
		}
		audit.Reason = str("reason")
		audit.By = str("by")
		audit.Actor = str("actor")
		audit.Digest = str("digest")
		if b, ok := data["bytes"].(float64); ok {
			audit.Bytes = int(b)
		}
		ev.GraphAudit = audit
		if audit.NodeID != "" && env.Type == "graph.node_reopened" {
			p.graphOpenAdd(audit.NodeID)
		}
		p.emit(ev)
		if audit.NodeID != "" && env.Type == "graph.node_reopened" {
			p.emitSpine()
		}
	case "graph.plan_triggered":
		ev.GraphPlanTriggered = &model.GraphPlanTriggered{Trigger: str("trigger"), Detail: str("detail")}
		p.emit(ev)
	case "graph.paused":
		var open []string
		if oa, ok := data["openNodeIds"].([]any); ok {
			for _, o := range oa {
				if os, ok := o.(string); ok {
					open = append(open, os)
				}
			}
		}
		ev.GraphPaused = &model.GraphPaused{Reason: str("reason"), OpenNodeIDs: open}
		p.emit(ev)
	case "graph.converged":
		rem, _ := data["openRemaining"].(float64)
		ev.GraphConverged = &model.GraphConverged{Summary: str("summary"), OpenRemaining: int(rem)}
		p.emit(ev)
	default:
		// 未知 graph.* 类型：忽略（前向兼容——schema 演进期消费面兜底）
	}
}

// graphOpenAdd / graphOpenRemove — open 集有序折叠（声明序）。
func (p *projector) graphOpenAdd(nodeID string) {
	if nodeID == "" {
		return
	}
	for _, id := range p.graphOpen {
		if id == nodeID {
			return
		}
	}
	p.graphOpen = append(p.graphOpen, nodeID)
}

func (p *projector) graphOpenRemove(nodeID string) {
	out := p.graphOpen[:0]
	for _, id := range p.graphOpen {
		if id != nodeID {
			out = append(out, id)
		}
	}
	p.graphOpen = out
}

func truncateForWarn(s string) string {
	if len(s) > 80 {
		s = s[:80]
	}
	return strings.TrimSpace(s)
}
