// Package project 实现 DSH 方言投影器：.sse.log 原始帧 → .tasklog/ 中立事件。
// 映射规范 = docs/projection-dsh.md（与 engine sandbox.go 的 sseParser 同源依据，
// 2026-09-17 实读 engine 源码校准：含 0.1.5 session.stream 通道与 assistant/message
// 内嵌 stream 语义，词表报告未覆盖的部分以源码为准）。
package project

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"baton/internal/model"
	"baton/internal/sseframe"
	"baton/internal/store"
)

const mainSession = "main"

type Options struct {
	ACPPath       string
	SSEPath       string // 引擎 data/ai-interaction/<taskID>.sse.log（DSH 方言输入）
	MakaPath      string // Maka RuntimeEventLog JSONL（maka 方言输入；与 SSEPath 互斥）
	GraphPath     string // 图任务第二输入 <taskID>.graph.log（空=非图任务，ADR-001）
	OutRoot       string // 任务工作区根；产物写 <OutRoot>/.tasklog/
	Task          string
	Tenant        string
	User          string
	Goal          string
	Constraints   []string
	Agent         string // pen.agent；默认 dsh
	Model         string // 覆盖 bridge.hello 模型名（可空）
	BlobThreshold int    // 默认 2048 字节
	WorkspaceID   string // 固定 id（测试）；空=随机 uuid
	GitAnchor     string
	Now           time.Time // 时间源；零值=当前时刻
}

type Stats struct {
	Events   int
	ByType   map[string]int
	Blobs    int
	Parked   int // maka 金丝雀 park 行数（>0=投影含不确定窗口，.tasklog/PARKED 已落）
	Warnings []string
}

type sessionState struct {
	turnOpen  bool
	turnNo    int
	textBuf   strings.Builder
	reasonBuf strings.Builder
	callSeq   int
	openCalls []string // 本回合未闭合 tool.call 的 call_id
	denied    map[string]bool
	preset    string // permission/preset 会话级预授权（S1 实证形状 {preset:"danger-full-access"}，粘性挂载）
	pending   []*pendingCall
}

// pendingCall 已构造未发射的 tool.call。DSH 持久层事件序=tool/call →
// approval/asked{callId} → approval/decided → tool/result（agent-loop
// appendToolCall 先于 prepare/审批门，2026-09-19 dsh-runtime 源码实读）——
// 审批对到达时其所属 call 已产生，须缓冲到 result/回合收束才发射（保
// seq=因果序），decided 按 callId 回填挂载。
type pendingCall struct {
	ev     *model.Event
	callID string
	tool   string
	ts     string // 构造时刻（R5 帧时间戳快照——缓冲后发射不得漂移到 flush 帧时刻）
}

// pendingAsk approval/asked 载荷（DSH v1 user-approval 审计对）：
// {id, toolName, callId?, reason?}——callId 是审批与调用的关联键。
type pendingAsk struct {
	toolName string
	callID   string
}

type projector struct {
	opts      Options
	now       time.Time
	pen       model.Pen
	events    []*model.Event
	blobs     *store.BlobStore
	sessions  map[string]*sessionState
	asks      map[string]pendingAsk // approval/asked.id → 问询（decided 按 id 对账回填）
	stats     *Stats
	curTs     string         // 当前帧接收时刻（R5 ": ts" 注释；空=降级投影时刻）
	unknown   map[string]int // 未映射帧/事件名计数（词表白名单外——计收不投影，防零痕迹丢弃）
	wsRewrite bool           // 旧 WORKSPACE.json 损坏——身份重新生成并覆写盘上文件
	// 机械提炼失败要点（graph-reseed v0；扫描完成后填充，最终快照生效）
	spineFacts []model.Fact

	// graph 第二输入（ADR-001）：ts 归并队列 + spine open 折叠（declared 增/终态减/
	// indeterminate 留——与 engine GraphState 折叠规则逐字一致，跨仓契约）。
	graphQ    []*graphEnvelope
	graphIdx  int
	graphOpen []string
}

// Run 执行投影。幂等语义：events.jsonl / spine.json 全量重写，blobs 只增去重，
// WORKSPACE.json 已存在则原样保留（重跑不换身份）。
func Run(opts Options) (*Stats, error) {
	{
		n := 0
		for _, v := range []string{opts.SSEPath, opts.MakaPath, opts.ACPPath} {
			if v != "" {
				n++
			}
		}
		if n != 1 {
			return nil, fmt.Errorf("-sse/-maka/-acp 恰好选一（DSH 原始帧 / Maka RuntimeEventLog / ACP 帧流）")
		}
	}
	var frameSrc io.Reader
	var mms *dialectStats // maka/acp 转译统计（非 nil=方言转译输入）
	if opts.ACPPath != "" {
		if !strings.HasSuffix(opts.ACPPath, ".jsonl") {
			return nil, fmt.Errorf("acp 输入只接受帧流 JSONL（%q）", filepath.Base(opts.ACPPath))
		}
		buf, as, terr := transpileACP(opts.ACPPath)
		if terr != nil {
			return nil, fmt.Errorf("acp 转译失败: %w", terr)
		}
		frameSrc = buf
		mms = as // 统一转译统计（dialectStats=方言转译统计共用型；Parked 金丝雀同链路）
	} else if opts.MakaPath != "" {
		if !strings.HasSuffix(opts.MakaPath, ".jsonl") {
			return nil, fmt.Errorf("maka 输入只接受 RuntimeEventLog JSONL（%q）", filepath.Base(opts.MakaPath))
		}
		buf, ms, terr := transpileMaka(opts.MakaPath)
		if terr != nil {
			return nil, fmt.Errorf("maka 转译失败: %w", terr)
		}
		frameSrc = buf
		mms = ms
	} else {
		if !strings.HasSuffix(opts.SSEPath, ".sse.log") {
			return nil, fmt.Errorf("输入只接受 .sse.log 原始帧（%q）；.ai.log 是人性化纯文本，禁作输入", filepath.Base(opts.SSEPath))
		}
		f, err := os.Open(opts.SSEPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		frameSrc = f
	}
	if opts.Agent == "" {
		opts.Agent = "dsh" // DSH 方言缺省持笔标识；-maka 输入时由 CLI 层给 "maka"
	}
	if opts.MakaPath != "" && opts.Agent == "dsh" {
		opts.Agent = "maka" // B6：maka 档案 pen.agent 缺省对齐方言（他任期告警防误报）
	}
	if opts.ACPPath != "" && opts.Agent == "dsh" {
		opts.Agent = "acp" // B6 同源：acp 档案持笔标识对齐方言
	}
	if opts.BlobThreshold <= 0 {
		opts.BlobThreshold = 2048
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	tasklog := filepath.Join(opts.OutRoot, ".tasklog")
	if err := os.MkdirAll(tasklog, 0o755); err != nil {
		return nil, err
	}
	// 双段语义（TP17-T2）：SSE 派生段重算可重写；尾部 handoff.* 生命周期段是
	// append-only 事实——重投影后原样续写（seq 连续，ts/载荷保历史原值）。
	handoffTail, warns := readHandoffTail(tasklog, opts.Agent)
	blobs, err := store.New(filepath.Join(tasklog, "blobs"))
	if err != nil {
		return nil, err
	}

	p := &projector{
		opts: opts, now: now,
		pen:      model.Pen{Agent: opts.Agent},
		blobs:    blobs,
		sessions: map[string]*sessionState{},
		asks:     map[string]pendingAsk{},
		stats:    &Stats{ByType: map[string]int{}},
		unknown:  map[string]int{},
	}
	p.stats.Warnings = append(p.stats.Warnings, warns...)

	gq, gwarns, gerr := loadGraphEvents(opts.GraphPath) // ADR-001：第二输入（空路径=非图任务）
	p.graphQ = gq
	p.stats.Warnings = append(p.stats.Warnings, gwarns...)
	if gerr != nil {
		return nil, gerr
	}

	ws, err := p.ensureWorkspace(tasklog)
	if err != nil {
		return nil, err
	}

	p.emit(&model.Event{
		Type: "task.created", Goal: opts.Goal, Constraints: opts.Constraints,
		Workspace: &model.WorkspaceRef{WorkspaceID: ws.WorkspaceID, GitAnchor: opts.GitAnchor},
	})
	p.emitSpine()

	if mms != nil {
		if len(mms.Parked) > 0 {
			mms.Warnings = append([]string{
				"金丝雀 PARKED：" + strings.Join(mms.Parked, "；")}, mms.Warnings...)
		}
		p.stats.Warnings = append(p.stats.Warnings, mms.Warnings...)
	}
	err = sseframe.Scan(frameSrc, p.frame)
	if err != nil {
		return nil, fmt.Errorf("SSE 扫描失败: %w", err)
	}
	p.flushGraphRemaining()                 // ADR-001：SSE 段之后的 graph 事件（EOF 归并尾）
	p.curTs = ""                            // EOF 后合成事件（未闭合 tool.result）恒投影时刻——裁决时刻语义，ADR-239
	p.closeAll()                            // EOF 收尾；中断的回合按公理裁决
	p.reportUnknown()                       // 未映射帧/事件名汇总（可观测性：丢弃了什么至少留痕）
	p.spineFacts = extractLessons(p.events) // graph-reseed v0：失败要点→spine.facts（全量重算确定性）
	p.replayHandoffTail(handoffTail)

	if err := p.writeOutputs(tasklog, ws); err != nil {
		return nil, err
	}
	if mms != nil {
		if len(mms.Parked) > 0 {
			// 金丝雀落档（README §5"简报标注不确定窗口"的数据源）：
			// .tasklog/PARKED 一行原因；brief 渲染到即输出不确定窗口段。
			// 原子写（3b14c39 纪律：crash 不留半截标记触发假不确定窗口）。
			if err := store.WriteFileAtomic(filepath.Join(tasklog, "PARKED"),
				[]byte(strings.Join(mms.Parked, "\n")+"\n")); err != nil {
				return nil, fmt.Errorf("PARKED 标记落盘: %w", err)
			}
			p.stats.Parked = len(mms.Parked)
		} else {
			// B3 幂等修复：本轮零 park→清历史标记（重跑修复后输入/换 DSH 输入，
			// 旧 PARKED 残留会让 brief 持续渲染已消失的不确定窗口）。
			_ = os.Remove(filepath.Join(tasklog, "PARKED"))
		}
	}
	p.stats.Blobs = blobs.Count()
	return p.stats, nil
}

// replayHandoffTail — 把旧档案的 handoff 段续写到新事件流末尾（seq 重排连续；
// ts/载荷为历史原值，不经 emit 改写）。
func (p *projector) replayHandoffTail(tail []*model.Event) {
	for _, ev := range tail {
		ev.Seq = len(p.events) + 1
		p.events = append(p.events, ev)
		p.stats.Events++
		p.stats.ByType[ev.Type]++
	}
}

// reportUnknown — 未映射帧/事件名按名排序汇总一行一条（确定性输出）。
// 词表内显式不投影（todo/write、step/* 等，projection-dsh.md 映射表）不计入。
func (p *projector) reportUnknown() {
	if len(p.unknown) == 0 {
		return
	}
	names := make([]string, 0, len(p.unknown))
	for n := range p.unknown {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p.stats.Warnings = append(p.stats.Warnings,
			fmt.Sprintf("未映射帧/事件 %q ×%d（词表白名单外——计收不投影）", n, p.unknown[n]))
	}
}

// knownIgnoredEvent — projection-dsh.md 映射表"v0 不投影"行的显式白名单
// （文档化静默≠零痕迹，白名单外的名字进 unknown 计数）。
func knownIgnoredEvent(et string) bool {
	switch {
	case strings.HasPrefix(et, "step/"), strings.HasPrefix(et, "request/"):
		return true
	case et == "todo/write", et == "session/title", et == "sandbox/mode":
		return true
	}
	return false
}

// readHandoffTail — 读旧 events.jsonl 尾部连续 handoff.* 段。前缀残留 handoff
// （中段混入非 handoff 的脏档案）→ 整段不保留并记 Warning（只保干净尾段，诚实降级）。
// 前缀含他任期（pen.agent ≠ 当前 agent）事件 → 计数告警：重投影将重写该段
// （多任期 append 同档未实现——data-flows.md 流 B 的现状约束，防静默抹史）。
func readHandoffTail(tasklog, curAgent string) ([]*model.Event, []string) {
	b, err := os.ReadFile(filepath.Join(tasklog, "events.jsonl"))
	if err != nil {
		return nil, nil // 首投无旧档案
	}
	var all []model.Event
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		var ev model.Event
		if json.Unmarshal([]byte(ln), &ev) != nil {
			return nil, []string{"旧档案存在非法行——handoff 尾段不保留"}
		}
		all = append(all, ev)
	}
	i := len(all)
	for i > 0 && strings.HasPrefix(all[i-1].Type, "handoff.") {
		i--
	}
	tail := make([]*model.Event, 0, len(all)-i)
	for j := i; j < len(all); j++ {
		tail = append(tail, &all[j])
	}
	foreign := 0
	for j := 0; j < i; j++ {
		if strings.HasPrefix(all[j].Type, "handoff.") {
			return nil, []string{"旧档案中段残留 handoff 事件（脏档案）——尾段不保留"}
		}
		if all[j].Pen.Agent != "" && all[j].Pen.Agent != curAgent {
			foreign++
		}
	}
	var warns []string
	if foreign > 0 {
		warns = append(warns, fmt.Sprintf(
			"旧档案前缀含他任期（pen≠%s）事件 %d 条——重投影将重写该段（多任期 append 未实现）", curAgent, foreign))
	}
	return tail, warns
}

// ---- 事件发射 ----

func (p *projector) emit(ev *model.Event) {
	ev.Seq = len(p.events) + 1
	ev.Ts = p.now.UTC().Format(time.RFC3339)
	if p.curTs != "" { // R5: 帧接收时刻优先（合法性已在 frame 入口校验）
		ev.Ts = p.curTs
	}
	ev.Task = p.opts.Task
	ev.Tenant = p.opts.Tenant
	ev.User = p.opts.User
	if ev.Session == "" {
		ev.Session = mainSession
	}
	ev.Pen = p.pen
	p.events = append(p.events, ev)
	p.stats.Events++
	p.stats.ByType[ev.Type]++
}

func (p *projector) emitSpine() {
	p.emit(&model.Event{Type: "spine.updated", Spine: p.currentSpine()})
}

// currentSpine — 脊柱快照（v0 浅投影：goal/constraints；图任务叠加 open 折叠，
// ADR-001——declared 增/终态减/indeterminate 留）。
func (p *projector) currentSpine() *model.Spine {
	return &model.Spine{
		Task: p.opts.Task, UpdatedAtSeq: len(p.events),
		Goal: p.opts.Goal, Constraints: p.opts.Constraints,
		Facts: append([]model.Fact(nil), p.spineFacts...),
		Open:  append([]string(nil), p.graphOpen...),
	}
}

// ---- 帧分发 ----

func (p *projector) frame(fr sseframe.Frame) error {
	if fr.Ts != "" { // ADR-001：ts 归并——graph 事件在 frameTs 之前（含同刻）先出；
		if ft, err := time.Parse(time.RFC3339Nano, fr.Ts); err == nil {
			p.flushGraphBefore(ft)
		}
	}
	p.curTs = fr.Ts
	if fr.Ts != "" { // R5: 帧时刻合法性是帧级属性——非法即告警并降级（不依赖该帧是否产事件）
		if _, err := time.Parse(time.RFC3339Nano, fr.Ts); err != nil {
			p.warn(fr, fmt.Sprintf("帧时间戳非法 %q——降级投影时刻", fr.Ts))
			p.curTs = ""
		}
	}
	if !sseframe.ValidJSON(fr.Data) {
		p.warn(fr, "撕裂尾行（data JSON 不完整）——丢弃")
		return nil
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(fr.Data), &top); err != nil {
		p.warn(fr, "data 非对象 JSON——跳过")
		return nil
	}
	switch fr.Name {
	case "bridge.hello":
		if m, _ := top["model"].(string); m != "" {
			p.pen.Model = &m // 模型名唯一来源
		}
	case "session.status":
		sid, _ := top["sessionId"].(string)
		if st, _ := top["status"].(string); st == "idle" && sid == mainSession {
			p.closeTurn(mainSession, "stop") // 主会话收束=干净回合闭合
		}
	case "runtime.exit":
		p.emit(&model.Event{Type: "task.status", Status: "FAILED", Reason: fr.Data})
	case "subagent.started":
		p.emit(&model.Event{Type: "task.subagent", Subagent: &model.Subagent{
			ParentSession: str(top["parentSessionId"]), ChildSession: str(top["childSessionId"]), Phase: "started",
		}, Session: str(top["childSessionId"])})
	case "subagent.finished":
		p.emit(&model.Event{Type: "task.subagent", Subagent: &model.Subagent{
			ParentSession: str(top["parentSessionId"]), ChildSession: str(top["childSessionId"]),
			Phase: "finished", Status: str(top["status"]), StopReason: str(top["stopReason"]),
		}, Session: str(top["childSessionId"])})
	case "session.event":
		sid, _ := top["sessionId"].(string)
		if sid == "" {
			sid = mainSession
		}
		ev, _ := top["event"].(map[string]any)
		if ev != nil {
			p.sessionEvent(sid, ev)
		}
	case "session.stream": // 0.1.5 逐字实时通道（ADR-236/R93）：chunk 帧同 assistant/chunk
		sid, _ := top["sessionId"].(string)
		if sid != mainSession {
			return nil // 子会话流静默（与 engine 同口径）
		}
		fm, _ := top["frame"].(map[string]any)
		if fm == nil {
			return nil
		}
		if ft, _ := fm["type"].(string); ft == "chunk" {
			if chunk, _ := fm["chunk"].(map[string]any); chunk != nil {
				p.assistantChunk(sid, chunk)
			}
		}
	default:
		p.unknown[fr.Name]++ // 词表外新帧名——计收不投影（零痕迹丢弃不可观测是缺陷）
	}
	return nil
}

func (p *projector) sessionEvent(sid string, ev map[string]any) {
	et, _ := ev["type"].(string)
	data, _ := ev["data"].(map[string]any)
	switch et {
	case "turn/start":
		st := p.sess(sid)
		st.turnOpen = true
		if sid == mainSession {
			st.turnNo++
		}
	case "user/message":
		text := contentText(data, "content")
		if text != "" {
			p.emit(&model.Event{Type: "model.message", Session: sid,
				Message: &model.Message{Role: "user", Parts: []model.Part{{Kind: "text", Text: text}}}})
		}
	case "assistant/chunk":
		if chunk, _ := data["chunk"].(map[string]any); chunk != nil {
			p.assistantChunk(sid, chunk)
		}
	case "assistant/attempt":
		p.assistantAttempt(sid, data)
	case "assistant/message":
		p.assistantMessage(sid, data)
	case "tool/call":
		p.toolCall(sid, data)
	case "turn/end":
		kind := "stop"
		if reason, _ := data["reason"].(map[string]any); reason != nil {
			if k, _ := reason["kind"].(string); k != "" {
				kind = k
			}
		}
		p.closeTurn(sid, kind)
	case "approval/asked":
		// DSH v1 审计对（user-approval/src/index.ts request()）：asked 载荷
		// {id, toolName, callId?, reason?}。id 是问询标识——decided 按 id 对账；
		// callId 关联所属调用（call 帧已先行到达，见 pendingCall 注记）。
		if id := str(data["id"]); id != "" {
			p.asks[id] = pendingAsk{toolName: str(data["toolName"]), callID: str(data["callId"])}
		} else {
			p.warn(sseframe.Frame{Name: et}, "approval/asked 缺 id——无法与 decided 对账，丢弃")
		}
	case "approval/decided":
		// 同审计对：decided 载荷 {id, outcome}，outcome 枚举 allowed-once |
		// rejected | cancelled | unavailable（cancelled=信号中止，
		// unavailable=应答器缺失/异常 fail-closed——DSH 明示 rogue 值亦归一
		// unavailable）。非 allowed-once 一律 denied：未授权执行。
		p.decideApproval(sid, data)
	case "permission/preset":
		// S1 实证形状 {"preset":"danger-full-access"}（gw-dc46007… 帧 seq0）：
		// 会话级预授权，粘性挂到后续所有 tool.call（approval/decided 到达则压过一次）。
		p.sess(sid).preset = str(data["preset"])
	case "tool/result":
		// S1 实证（gw-dc46007…）：DSH 0.1.5 有显式结果帧——真实输出与 isError
		// 是事实，不能只靠回合收束近似。
		p.toolResult(sid, data)
	case "agent/inbox/spliced":
		// v0 不投影（主会话=注入通道；子任务任务文本经 user/message 已覆盖）
	default:
		if !knownIgnoredEvent(et) {
			p.unknown[et]++ // 词表白名单外——计收不投影
		}
	}
}

func (p *projector) assistantChunk(sid string, chunk map[string]any) {
	st := p.sess(sid)
	switch ct, _ := chunk["type"].(string); ct {
	case "reasoning-delta":
		st.reasonBuf.WriteString(str(chunk["text"]))
	case "text-delta":
		st.textBuf.WriteString(str(chunk["text"]))
	case "finish":
		// S1 实证修正：finish 后紧跟
		// assistant/message（content 权威）——此处发射必与 content 版双记。
		// 缓冲保留：message 携 content 到达即重置；无 message（撕裂/中断）由
		// 回合收束/EOF 兜底发射。
	case "usage":
		if u := p.usage(chunk); u != nil {
			p.emit(&model.Event{Type: "usage.recorded", Usage: u})
		}
	}
}

// assistantAttempt 0.1.5 持久层聚合记录（stream:[records]）展开——
// text-chunks/reasoning-chunks 的 texts 聚合；内嵌 chunk 记录按 assistantChunk 处理。
func (p *projector) assistantAttempt(sid string, data map[string]any) {
	stream, _ := data["stream"].([]any)
	st := p.sess(sid)
	for _, rec := range stream {
		rm, _ := rec.(map[string]any)
		if rm == nil {
			continue
		}
		switch rt, _ := rm["type"].(string); rt {
		case "text-chunks":
			for _, t := range texts(rm) {
				st.textBuf.WriteString(t)
			}
		case "reasoning-chunks":
			for _, t := range texts(rm) {
				st.reasonBuf.WriteString(t)
			}
		case "chunk":
			if chunk, _ := rm["chunk"].(map[string]any); chunk != nil {
				p.assistantChunk(sid, chunk)
			}
		}
	}
}

// assistantMessage 0.1.5 通知面正文唯一形态。message.content 为权威——
// 内嵌 stream 与实时通道是同一正文的重复表达，防双记（对齐 engine 防双渲染语义）。
func (p *projector) assistantMessage(sid string, data map[string]any) {
	msg, _ := data["message"].(map[string]any)
	if msg == nil {
		return
	}
	blocks, _ := msg["content"].([]any)
	var parts []model.Part
	for _, blk := range blocks {
		bm, _ := blk.(map[string]any)
		if bm == nil {
			continue
		}
		switch bt, _ := bm["type"].(string); {
		case bt == "text":
			if t := str(bm["text"]); t != "" {
				parts = append(parts, model.Part{Kind: "text", Text: t})
			}
		case strings.Contains(bt, "reason") || strings.Contains(bt, "think"):
			if t := str(bm["text"]); t != "" {
				parts = append(parts, model.Part{Kind: "reasoning", Text: t})
			}
		}
	}
	st := p.sess(sid)
	if len(parts) == 0 {
		return // 无 content（罕见）：缓冲保留，回合收束兜底发射（S1 防双记同源）
	}
	st.textBuf.Reset()
	st.reasonBuf.Reset()
	p.emit(&model.Event{Type: "model.message", Session: sid,
		Message: &model.Message{Role: "assistant", Parts: parts}})
}

func (p *projector) toolCall(sid string, data map[string]any) {
	st := p.sess(sid)
	st.callSeq++
	name := str(data["name"])
	// S1 实证（gw-dc46007…）：真实帧带桥接 callId（call_…）——保真优先，
	// 合成 id 仅无 callId 时兜底（外部系统按 id 对账的连接键不能丢）。
	callID := str(data["callId"])
	if callID == "" {
		callID = fmt.Sprintf("%s-t%d-c%d", sid, st.turnNo, st.callSeq)
	}
	call := &model.Call{CallID: callID, Tool: name, Via: p.opts.Agent}
	args := []byte(str(data["arguments"]))
	if raw, ok := data["arguments"]; ok && raw != nil {
		if _, isStr := raw.(string); !isStr {
			// arguments 非字符串（上游形状漂移，如直接 JSON 对象）：原值序列化入库，
			// 不静默替换为空串——形状假设未防御是本仓栽过的坑（S1 approval 证伪史）。
			if b, err := json.Marshal(raw); err == nil {
				args = b
				p.warn(sseframe.Frame{Name: "tool/call"}, fmt.Sprintf(
					"arguments 非字符串形状（type=%T）——原值序列化入库", raw))
			}
		}
	}
	if len(args) > p.opts.BlobThreshold || !json.Valid(args) {
		if ref, err := p.blobs.Put(args); err == nil {
			call.ArgsRef = ref
		} else {
			call.Args = json.RawMessage(fmt.Sprintf("%q", string(args))) // 兜底内联转义
		}
	} else {
		call.Args = json.RawMessage(args)
	}
	if st.preset != "" {
		call.Approval = &model.Approval{Decision: "preset", Policy: st.preset}
	}
	st.openCalls = append(st.openCalls, callID)
	// 审计对在其后到达（pendingCall 注记）：缓冲而非即发，decided 回填后由
	// toolResult/回合收束触发发射。
	st.pending = append(st.pending, &pendingCall{
		ev: &model.Event{Type: "tool.call", Session: sid, Call: call}, callID: callID, tool: name,
		ts: p.curTs})
}

// decideApproval 处理 approval/decided {id, outcome}：按 asked.id 对账，把
// 决策挂回所属 pending call（callId 精确匹配优先，缺 callId 时按 toolName
// 匹配最近未决的同名调用）。目标不在缓冲（已发射/序异常）→ warn 丢弃，
// 不猜测挂载。
func (p *projector) decideApproval(sid string, data map[string]any) {
	id := str(data["id"])
	ask, ok := p.asks[id]
	if !ok {
		p.warn(sseframe.Frame{Name: "approval/decided"}, fmt.Sprintf(
			"decided id=%q 无对应 asked（重复/撕裂/乱序）——丢弃", id))
		return
	}
	delete(p.asks, id)
	decision := "denied"
	if str(data["outcome"]) == "allowed-once" {
		decision = "granted"
	}
	st := p.sess(sid)
	target := -1
	for i, pc := range st.pending {
		if ask.callID != "" && pc.callID == ask.callID || ask.callID == "" && pc.tool == ask.toolName {
			target = i
		}
	}
	if target < 0 {
		p.warn(sseframe.Frame{Name: "approval/decided"}, fmt.Sprintf(
			"审批 %q(%s) 的所属调用不在待发缓冲——已发射或乱序，决策丢弃", id, ask.toolName))
		return
	}
	pc := st.pending[target]
	pc.ev.Call.Approval = &model.Approval{Decision: decision}
	if decision == "denied" {
		st.denied[pc.callID] = true
	}
}

// flushPendingTo 发射缓冲中目标调用及其之前的全部（含目标）。
// 持久层 call 按模型序 append：目标 result 到达时，其前的调用审批对必已
// 到齐（DSH scheduler 串行 prepare——前一个审批未决不会启动后续调用）。
func (p *projector) flushPendingTo(sid string, callID string) {
	st := p.sess(sid)
	cut := len(st.pending)
	if callID != "" {
		found := -1
		for i, pc := range st.pending {
			if pc.callID == callID {
				found = i
				break
			}
		}
		if found < 0 {
			// R121 P3：目标不在缓冲（旧 callId 的乱序/重复 result）→ 不裸发其后
			// 仍在等审批的调用（原实现把 cut=len 全部裸发，后到 decided 只能告警
			// 丢弃——因果序破防）。result 事件本身照常投影。
			p.warn(sseframe.Frame{Name: "tool/result", Line: 0},
				"result 对应调用不在待发缓冲（乱序/重复）——缓冲保持原序不裸发")
			return
		}
		cut = found + 1
	}
	saved := p.curTs
	for _, pc := range st.pending[:cut] {
		p.curTs = pc.ts
		p.emit(pc.ev)
	}
	p.curTs = saved
	st.pending = st.pending[cut:]
}

// toolResult — 0.1.5 真实 tool/result 帧（S1 实证 2026-09-17，形状=
// data.message.content[] 块 type=tool-result{toolCallId,isError,content[].text}）。
// 有帧=事实结果：outcome 按 isError completed/error，输出文本内联或 blob，
// 并把调用从 openCalls 摘除——回合收束只兜底真正未闭合的调用。
func (p *projector) toolResult(sid string, data map[string]any) {
	msg, _ := data["message"].(map[string]any)
	if msg == nil {
		return
	}
	st := p.sess(sid)
	blocks, _ := msg["content"].([]any)
	for _, blk := range blocks {
		bm, _ := blk.(map[string]any)
		if bm == nil || bm["type"] != "tool-result" {
			continue
		}
		callID := str(bm["toolCallId"])
		if callID == "" {
			continue
		}
		// 所属 call 及其之前全部先发射（seq=因果序：call 必先于 result）。
		p.flushPendingTo(sid, callID)
		isErr, _ := bm["isError"].(bool)
		out := blocksText(bm["content"])
		res := &model.Result{CallID: callID, Ok: !isErr, Bytes: len(out)}
		if isErr {
			res.Outcome = "error"
		} else {
			res.Outcome = "completed"
		}
		if st.denied[callID] {
			// 审批拒绝事实压过帧级 isError→error：denied 更精确地解释失败
			// 归因（与回合收束兜底同口径；S2 审计对回填引入）。
			res.Outcome, res.Ok = "denied", false
		}
		if len(out) > p.opts.BlobThreshold {
			if ref, err := p.blobs.Put([]byte(out)); err == nil {
				res.ResultRef = ref
			} else {
				res.Result = json.RawMessage(fmt.Sprintf("%q", out))
			}
		} else if out != "" {
			res.Result = json.RawMessage(fmt.Sprintf("%q", out))
		}
		for i, id := range st.openCalls {
			if id == callID {
				st.openCalls = append(st.openCalls[:i], st.openCalls[i+1:]...)
				break
			}
		}
		p.emit(&model.Event{Type: "tool.result", Session: sid, Result: res})
	}
}

// blocksText — content[] 块的 text 拼接（tool-result 输出为 text 块序列）。
func blocksText(v any) string {
	cs, _ := v.([]any)
	var sb strings.Builder
	for _, c := range cs {
		if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
			sb.WriteString(str(cm["text"]))
		}
	}
	return sb.String()
}

// closeTurn 回合闭合。真实 tool/result 帧已闭合的调用在帧处落账（S1 实证）；
// 此处只兜底仍未闭合的调用：干净收束白名单（stop/completed/tool-calls——参数已被
// 桥接消费）→ completed；其余一律 indeterminate（公理：缺失结果≠没跑过）。
// 白名单而非黑名单：engine turnEndReasonValue 枚举=completed|blocked|max-tokens|
// interrupted|aborted|error（payload-validation.ts:768），interrupted/max-tokens/
// blocked 曾被黑名单漏成 completed——把"结果不明"伪证成"已消费"，方向性错误。
func (p *projector) closeTurn(sid string, kind string) {
	st := p.sess(sid)
	if !st.turnOpen && len(st.openCalls) == 0 && len(st.pending) == 0 {
		return
	}
	p.flushPendingTo(sid, "") // 回合收束=审批窗口关闭，剩余缓冲裸发
	p.flushAssistant(sid, nil)
	// R121 注记：正文/待发调用的收束序保留"调用先、正文后"——黄金夹具（真实流
	// 校准）证实 turn 内尾段时序=call(.09) 先于收尾 text(.12)；审查假设的
	// "正文先于调用"场景无实证，两缓冲无交织信息，维持经真实流验证的原序。
	outcome := "indeterminate"
	switch kind {
	case "stop", "completed", "tool-calls":
		outcome = "completed"
	}
	for _, id := range st.openCalls {
		oc := outcome
		ok := oc == "completed"
		if st.denied[id] {
			oc, ok = "denied", false
		}
		p.emit(&model.Event{Type: "tool.result", Session: sid,
			Result: &model.Result{CallID: id, Ok: ok, Outcome: oc}})
	}
	st.openCalls = nil
	st.turnOpen = false
}

// flushAssistant 把缓冲的 delta 聚合提交为一条 assistant 消息（无缓冲则空操作）。
func (p *projector) flushAssistant(sid string, _ []model.Part) {
	st := p.sess(sid)
	var parts []model.Part
	if st.reasonBuf.Len() > 0 {
		parts = append(parts, model.Part{Kind: "reasoning", Text: st.reasonBuf.String()})
	}
	if st.textBuf.Len() > 0 {
		parts = append(parts, model.Part{Kind: "text", Text: st.textBuf.String()})
	}
	st.reasonBuf.Reset()
	st.textBuf.Reset()
	if len(parts) == 0 {
		return
	}
	p.emit(&model.Event{Type: "model.message", Session: sid,
		Message: &model.Message{Role: "assistant", Parts: parts}})
}

// closeAll EOF 收尾：所有仍打开的回合按中断处理（aborted 语义走 closeTurn）。
// 会话按 id 排序遍历——map 迭代序随机会使多会话 EOF 的 events.jsonl 逐字节
// 不可复现（确定性契约：固定时钟下逐字节一致）。
func (p *projector) closeAll() {
	sids := make([]string, 0, len(p.sessions))
	for sid := range p.sessions {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	for _, sid := range sids {
		st := p.sessions[sid]
		if st.turnOpen || len(st.openCalls) > 0 || st.textBuf.Len() > 0 || st.reasonBuf.Len() > 0 {
			p.warn(sseframe.Frame{Name: "eof"}, fmt.Sprintf("会话 %s 在回合中途中断（EOF）", sid))
			p.closeTurn(sid, "aborted")
		}
	}
}

// ---- 载荷小工具 ----

func (p *projector) usage(chunk map[string]any) *model.Usage {
	u, _ := chunk["usage"].(map[string]any)
	if u == nil {
		return nil
	}
	m := "" // bridge.hello 唯一来源；无 hello 时为空串（schema required model——降级记 ""）
	if p.pen.Model != nil {
		m = *p.pen.Model
	}
	return &model.Usage{
		Model:       m,
		InputTokens: num(u["inputTokens"]), OutputTokens: num(u["outputTokens"]),
		CacheReadTokens: num(u["cacheReadTokens"]), CacheWriteTokens: num(u["cacheWriteTokens"]),
		ReasoningTokens: num(u["reasoningTokens"]), TotalTokens: num(u["totalTokens"]),
	}
}

func (p *projector) sess(sid string) *sessionState {
	st := p.sessions[sid]
	if st == nil {
		st = &sessionState{denied: map[string]bool{}}
		p.sessions[sid] = st
	}
	return st
}

func (p *projector) warn(fr sseframe.Frame, msg string) {
	if fr.Line > 0 {
		p.stats.Warnings = append(p.stats.Warnings, fmt.Sprintf("L%d %s: %s", fr.Line, fr.Name, msg))
		return
	}
	p.stats.Warnings = append(p.stats.Warnings, fmt.Sprintf("%s: %s", fr.Name, msg)) // 合成告警无行号（EOF/审批对等）
}

// ---- 输出 ----

func (p *projector) ensureWorkspace(tasklog string) (*model.WorkspaceFile, error) {
	path := filepath.Join(tasklog, "WORKSPACE.json")
	if b, err := os.ReadFile(path); err == nil {
		var ws model.WorkspaceFile
		if json.Unmarshal(b, &ws) == nil && ws.WorkspaceID != "" {
			if ws.Task != "" && ws.Task != p.opts.Task {
				return nil, fmt.Errorf("WORKSPACE.json task=%q 与 -task %q 不符——工作区身份硬闸（重挂前核对）", ws.Task, p.opts.Task)
			}
			return &ws, nil // 重跑保留身份（幂等）
		}
		// 损坏/空身份：告警 + 重新生成并覆写（events 与盘上文件不得分叉引用两个身份）
		p.stats.Warnings = append(p.stats.Warnings, "WORKSPACE.json 损坏或无身份——重新生成工作区身份并覆写")
		p.wsRewrite = true
	}
	id := p.opts.WorkspaceID
	if id == "" {
		id = newUUID()
	}
	ws := &model.WorkspaceFile{
		WorkspaceID: id, Task: p.opts.Task,
		CreatedTs: p.now.UTC().Format(time.RFC3339),
		GitAnchor: p.opts.GitAnchor, Vcs: "git",
	}
	return ws, nil // 写入由 writeOutputs 统一执行
}

func (p *projector) writeOutputs(tasklog string, ws *model.WorkspaceFile) error {
	var sb strings.Builder
	for _, ev := range p.events {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := store.WriteFileAtomic(filepath.Join(tasklog, "events.jsonl"), []byte(sb.String())); err != nil {
		return err
	}
	spine := p.currentSpine() // ADR-001：图任务叠加 open 折叠终态
	spine.UpdatedAtSeq = p.stats.Events
	sb2, err := json.MarshalIndent(spine, "", "  ")
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(filepath.Join(tasklog, "spine.json"), append(sb2, '\n')); err != nil {
		return err
	}
	wsPath := filepath.Join(tasklog, "WORKSPACE.json")
	if _, err := os.Stat(wsPath); os.IsNotExist(err) || p.wsRewrite {
		wb, err := json.MarshalIndent(ws, "", "  ")
		if err != nil {
			return err
		}
		if err := store.WriteFileAtomic(wsPath, append(wb, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// ---- 通用小函数 ----

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func texts(rm map[string]any) []string {
	raw, _ := rm["texts"].([]any)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contentText(data map[string]any, key string) string {
	blocks, _ := data[key].([]any)
	var sb strings.Builder
	for _, blk := range blocks {
		bm, _ := blk.(map[string]any)
		if bm != nil && bm["type"] == "text" {
			sb.WriteString(str(bm["text"]))
		}
	}
	return sb.String()
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano()) // crypto 失败极罕见；保运行
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
