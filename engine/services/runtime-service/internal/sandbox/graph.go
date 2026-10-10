// GraphRunner — 图执行运行器（切片 5+6 TP18-T3，ADR-249；v0.5 波次并行+黑板认领层——
// graph-engine-v0.5-blackboard-hoc.md：executor/verifier 依赖就绪即派发，planner 波次边界串行）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §4（主循环/异常路径/
// 冻结换手/resume）+§1（节点=同沙箱独立 session，sessionId=g-<nodeId>）。
// 公理锚定：B3 三权=任务类型（类型化超时/结果契约/并发位 v0 全 1）；B4 冻结定格
// （PauseGate 节点边界挂起+graph.paused）；A4 节点 prompt=有界投影（intent+goal+
// 已完成节点 resultRef 摘要，不喂聊天历史）；D1 rejected 一等+封顶拒绝留痕不静默
// 截断；B2 resume 只派 open（已完成节点零重付）；C4 事件域中立。
//
// 新态势判定（审计 M2 修正的防自激细化，ADR-249）：上次 planner 派发后有**非
// planner 节点**进入终态=新态势（completed→new_evidence；failed/rejected 致 open
// 排空→open_drained）。planner 自身完成不算新证据（否则 planner 完成触发 planner
// 无限自激直到预算耗尽）。
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// graph 缺省档（§5 pack 可覆写；0 值归一到此）。
const (
	graphMaxNodesDefault           = 24
	graphMaxPlannerReplansDefault  = 5
	graphMaxAttemptsPerNodeDefault = 2
	graphPlannerTimeoutDefault     = 180 * time.Second
	graphExecutorTimeoutDefault    = 900 * time.Second
	graphVerifierTimeoutDefault    = 300 * time.Second
	// A4 有界投影上限：已完成节点 resultRef 摘要条数与单条字数（resultRef 本身
	// ≤200 字，§2）——上下文不随图增长。
	graphProjectionMaxNodes = 8
	graphResultRefMaxRunes  = 200
)

// GraphTaskSpec — RunGraph 入参（service 层自 proto RunGraphTaskRequest 装配）。
type GraphTaskSpec struct {
	TaskID                                           string
	Goal                                             string
	WorkspaceDir                                     string // 空=不上传项目（渗透任务目标=授权目标非代码树）
	SeedIntents                                      []string
	MaxNodes                                         int
	MaxParallel                                      int // v0.5 资源护栏（0=动态派发无上限；显式声明才生效——设计档 §2.3）
	AllowedTypes                                     map[string]bool
	MaxPlannerReplans                                int
	MaxAttemptsPerNode                               int
	PlannerTimeout, ExecutorTimeout, VerifierTimeout time.Duration
	ReplayLog                                        io.Reader // resume：既有 <task>.graph.log（nil=新任务，B2）
	OnGraphEvent                                     func([]byte)
	SeedBrief                                        func() string // R122 graph-reseed：节点重派种子切片获取器（nil=禁用；失败史存在才被调用）
}

// GraphRunResult — 图运行收束面（findings 由 executor 经既有 submit_findings
// 通道提交——响应只报图自身状态，域中立 C4）。
type GraphRunResult struct {
	Converged                                bool
	Paused                                   bool
	Findings                                 []Finding
	Total, Completed, Failed, Rejected, Open int
	Turns                                    float64 // 全节点会话轮次合计（usageEstimate 面；精确计量归 C2 批）
}

// graphSession — 图节点派发的方言无关面（v0 两实现：dsh-sse 焦点会话 / mock 进程内）。
type graphSession interface {
	// dispatch — 派发一节点回合（prompt 自含；断流续跑/恢复由实现按方言承接），
	// 返回最终文本、捕获的工具调用与本会话累计轮次。
	dispatch(ctx context.Context, node *graphNode, prompt string) (text string, calls []ToolCall, turns float64, err error)
	close()
	name() string
	turnsTotal() float64
}

// normalize — 0 值归一缺省档（pack 0=缺省注入语义，ADR-249）。
func (s *GraphTaskSpec) normalize() {
	if s.MaxNodes <= 0 {
		s.MaxNodes = graphMaxNodesDefault
	}
	if s.MaxPlannerReplans <= 0 {
		s.MaxPlannerReplans = graphMaxPlannerReplansDefault
	}
	if s.MaxAttemptsPerNode <= 0 {
		s.MaxAttemptsPerNode = graphMaxAttemptsPerNodeDefault
	}
	if s.PlannerTimeout <= 0 {
		s.PlannerTimeout = graphPlannerTimeoutDefault
	}
	if s.ExecutorTimeout <= 0 {
		s.ExecutorTimeout = graphExecutorTimeoutDefault
	}
	if s.VerifierTimeout <= 0 {
		s.VerifierTimeout = graphVerifierTimeoutDefault
	}
	if len(s.AllowedTypes) == 0 {
		s.AllowedTypes = map[string]bool{"planner": true, "executor": true, "verifier": true}
	}
	if strings.TrimSpace(s.Goal) == "" {
		s.Goal = "完成本 pack 声明的负载目标" // A4 goal 面缺省（pack 描述级语义）
	}
}

func (s *GraphTaskSpec) timeoutFor(nodeType string) time.Duration {
	switch nodeType {
	case "planner":
		return s.PlannerTimeout
	case "verifier":
		return s.VerifierTimeout
	default:
		return s.ExecutorTimeout
	}
}

// RunGraph — 图执行主循环（§4）。返回收敛/挂起结果；findings 聚合上浮。
func (r *ManagerRunner) RunGraph(ctx context.Context, spec GraphTaskSpec) (*GraphRunResult, error) {
	if !r.Enabled() {
		return nil, ErrDisabled
	}
	spec.normalize()

	// resume：回放既有 graph.log 重建物化视图（B2：快照为真相，log 是唯一持久面）。
	var st *graphState
	if spec.ReplayLog != nil {
		var err error
		st, err = ReplayGraphLog(spec.ReplayLog)
		if err != nil {
			return nil, fmt.Errorf("graph.log 回放: %w", err)
		}
	} else {
		st = newGraphState()
	}
	if st.converged { // 已收敛的图零重付（B2）
		res := &GraphRunResult{Converged: true}
		st.countsInto(&res.Total, &res.Completed, &res.Failed, &res.Rejected, &res.Open)
		return res, nil
	}
	emit := newGraphEmitter(st.lastSeq, spec.OnGraphEvent)

	gs, err := r.openGraphSession(ctx, &spec)
	if err != nil {
		return nil, err
	}
	defer gs.close()
	defer ClearSession(spec.TaskID) // ADR-200: 会话结束清闸门（防注册表泄漏）

	gate := GateFor(spec.TaskID)
	result := &GraphRunResult{}

	// v0.5 人机共编辑（§4）：注入闭包注册——校验节点 running（open/终态诚实拒绝）
	// 后经会话派发 user 消息（bridge 排队=回合边界生效）；graph.human_message
	// 审计事件落 graph.log（digest 留痕，全文经事件 refs 引用）。
	injectRelease := registerGraphOps(spec.TaskID, &graphOps{
		inject: func(nodeID, message string) error {
			st.mu.Lock()
			n, ok := st.nodes[nodeID]
			if !ok {
				st.mu.Unlock()
				return fmt.Errorf("节点 %q 不存在", nodeID)
			}
			if n.Status != stRunning {
				status := n.Status
				st.mu.Unlock()
				return fmt.Errorf("节点 %q 状态 %s——仅 running 节点可注入（回合边界排队语义）", nodeID, status)
			}
			timeout := spec.timeoutFor(n.Type)
			st.mu.Unlock()
			sum := sha256.Sum256([]byte(message))
			if err := emit.emit(evHumanMessage, map[string]any{
				"nodeId": nodeID, "actor": "human",
				"digest": hex.EncodeToString(sum[:6]), "bytes": len(message)}); err != nil {
				return err
			}
			tctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_, _, _, derr := gs.dispatch(tctx, n, message) // 返回值丢弃：人消息不进结果回收
			return derr
		},
		claim: func(nodeID string, ttlS int) (string, error) {
			st.mu.Lock()
			n, ok := st.nodes[nodeID]
			if !ok {
				st.mu.Unlock()
				return "", fmt.Errorf("节点 %q 不存在", nodeID)
			}
			if n.Status != stDeclared || n.Holder != "" {
				status := n.Status
				st.mu.Unlock()
				return "", fmt.Errorf("节点 %q 状态 %s——仅未认领 declared 节点可 human 认领", nodeID, status)
			}
			if ttlS <= 0 {
				ttlS = 1800
			}
			until := time.Now().Add(time.Duration(ttlS) * time.Second).UTC()
			st.claimNode(n, "human", until)
			st.mu.Unlock()
			lease := until.Format(time.RFC3339)
			_ = emit.emit(evNodeClaimed, map[string]any{"nodeId": nodeID, "holder": "human", "leaseUntil": lease})
			st.mu.Lock()
			_ = st.apply(&graphEnvelope{Type: evNodeClaimed, Seq: emit.lastSeq(),
				Data: map[string]any{"nodeId": nodeID, "holder": "human", "leaseUntil": lease}})
			st.mu.Unlock()
			return lease, nil
		},
		release: func(nodeID string) error {
			st.mu.Lock()
			n, ok := st.nodes[nodeID]
			if !ok {
				st.mu.Unlock()
				return fmt.Errorf("节点 %q 不存在", nodeID)
			}
			if n.Holder == "" {
				st.mu.Unlock()
				return fmt.Errorf("节点 %q 未被认领", nodeID)
			}
			holder := n.Holder
			st.releaseNode(n)
			st.mu.Unlock()
			_ = emit.emit(evNodeReleased, map[string]any{"nodeId": nodeID, "holder": holder, "reason": "human_release"})
			st.mu.Lock()
			_ = st.apply(&graphEnvelope{Type: evNodeReleased, Seq: emit.lastSeq(),
				Data: map[string]any{"nodeId": nodeID, "holder": holder, "reason": "human_release"}})
			st.mu.Unlock()
			return nil
		},
	})
	defer injectRelease()

	// 初始化（§4）：种子展开（type=executor——种子=固定阶段/初始攻击面的执行单元，
	// ADR-249）；无种子则 plan_triggered(initial) 起 planner。
	if len(st.nodes) == 0 {
		if len(spec.SeedIntents) > 0 {
			for _, intent := range spec.SeedIntents {
				n := st.declare("executor", intent, "pack", nil)
				_ = emit.emit(evNodeDeclared, map[string]any{
					"nodeId": n.ID, "type": n.Type, "intent": n.Intent, "declaredBy": "pack", "from": []string{}})
			}
		} else {
			_ = emit.emit(evPlanTriggered, map[string]any{"trigger": triggerInitial, "detail": "无种子：起初始 planner"})
			n := st.declare("planner", "初始规划：依据任务目标与既有授权范围给出执行计划", "pack", nil)
			_ = emit.emit(evNodeDeclared, map[string]any{
				"nodeId": n.ID, "type": n.Type, "intent": n.Intent, "declaredBy": "pack", "from": []string{}})
		}
	}
	// 新态势水位：上次 planner 派发后的非 planner 终态数。R116：回放场景优先取
	// evPaused 快照的已消费水位——若 freeze 落在"最后终态之后、排空检查之前"，
	// 当前值重置会吞掉一次 pending 的 new_evidence 重规划（提前假收敛）；无快照
	// （无 pause 历史）维持原值语义。
	plannerMark := st.nonPlannerTerminals()
	if st.hasPausedMark {
		plannerMark = st.pausedMark
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// B4 冻结闸门：节点边界挂起（当前回合跑完生效）；挂起中 ctx 取消=换手
		// 窗口（沙箱 hold 保活，R4）。
		if err := gate.WaitFrozen(ctx, spec.TaskID); err != nil {
			_ = emit.emit(evPaused, map[string]any{"reason": "task_pause", "openNodeIds": st.openIDs(),
				"plannerMark": plannerMark})
			st.countsInto(&result.Total, &result.Completed, &result.Failed, &result.Rejected, &result.Open)
			result.Paused = true
			result.Turns = gs.turnsTotal()
			return result, nil
		}

		// v0.5 波次组装（graph-engine-v0.5-blackboard-hoc.md §2.3）：依赖就绪即派发。
		// planner 不进波次（全局视野=态势水位判定后才派发，串行语义保留）。
		// code-review 修复：全部 st 读写持 mu（注入/认领/释放 RPC
		// 闭包并发可达——E2 数据竞争）；claim 波内重验 holder（E3 与 human 认领
		// 竞态静默覆写）；node_seeded 先于 started（E10 因果序）；护栏截断移到
		// filterNonPlanner 之后（planner 不占并发名额）。
		st.mu.Lock()
		ready, expired := st.claimableReady(0, time.Now()) // 先全量收集过期项
		for _, en := range expired {                       // human claim 租约回收（引擎单调钟判定，事件留痕）
			emitApply(emit, st, evNodeLeaseExpired, map[string]any{"nodeId": en.ID, "holder": en.Holder})
			st.releaseNode(en)
		}
		ready = claimableCap(filterNonPlanner(ready), spec.MaxParallel)
		// claim 波（E3 防护依赖本锁窗口的连续性：Lock→Unlock 之间 human 认领
		// RPC 无法插入——重验 holder 恒假，勿在窗口内引入阻塞点）。
		for _, n := range ready {
			st.claimNode(n, "engine-worker", time.Time{})
			emitApply(emit, st, evNodeClaimed, map[string]any{"nodeId": n.ID, "holder": "engine-worker"})
		}
		st.mu.Unlock()
		wave := ready // 锁窗口连续 ⇒ ready 全部为 engine-worker 认领态
		if len(wave) > 0 {
			// —— 派发（node_seeded 先于 started——因果序 E10；dispatch 经会话锁
			// 串行化执行体，事件/认领语义不变）——
			type waveOut struct {
				text  string
				calls []ToolCall
				turns float64
				derr  error
			}
			outs := make([]waveOut, len(wave))
			var wg sync.WaitGroup
			for i, n := range wave {
				n := n
				st.mu.Lock()
				seedText := seedTextFor(st, &spec, n, emit)
				emitApply(emit, st, evNodeStarted, map[string]any{
					"nodeId": n.ID, "sessionId": n.SessionID(), "turnRef": n.SessionID()})
				prompt := buildGraphNodePrompt(&spec, n, st, seedText, r.agentCaps().StructuredSubmit)
				st.mu.Unlock()
				wg.Add(1)
				go func(i int, n *graphNode, prompt string) {
					defer wg.Done()
					tctx, ccancel := context.WithTimeout(ctx, spec.timeoutFor(n.Type))
					defer ccancel()
					text, calls, turns, derr := gs.dispatch(tctx, n, prompt)
					outs[i] = waveOut{text: text, calls: calls, turns: turns, derr: derr}
				}(i, n, prompt)
			}
			wg.Wait()
			// —— 收束段：锁只包状态面，网络调用（收笔轮）在锁外（E5 可用性）——
			for i, n := range wave {
				o := outs[i]
				needConclude := o.derr == nil && n.Type == "executor" &&
					r.agentCaps().ConcludeSemantics && !expectedSubmitCaptured(o.calls, false)
				if needConclude {
					r.event("warn", "图节点 %s executor 零提交——收笔注入恰一轮（只定稿不推进，ADR-249 H2）", n.ID)
					cctx, ccancel := context.WithTimeout(ctx, spec.timeoutFor(n.Type))
					cText, cCalls, _, cErr := gs.dispatch(cctx, n, concludePrompt(false))
					ccancel()
					if cErr != nil {
						r.event("warn", "收笔轮失败（保原结果不放大，ADR-246）: %v", cErr)
					} else {
						if cText != "" {
							o.text = cText
						}
						o.calls = append(o.calls, cCalls...)
					}
				}
				st.mu.Lock()
				outcome, resultRef, findings := r.collectGraphNode(&spec, st, n, o.text, o.calls, o.derr)
				result.Findings = append(result.Findings, findings...)
				emitApply(emit, st, evNodeResult, map[string]any{
					"nodeId": n.ID, "outcome": outcome, "resultRef": resultRef,
					"usageEstimate": map[string]any{"turns": o.turns}})
				// E1（code-review）：释放认领必须发 node_released 事件——只改内存
				// 会让 resume 回放残留 holder（indeterminate 重派被过滤=死循环）。
				st.releaseNode(n)
				emitApply(emit, st, evNodeReleased, map[string]any{"nodeId": n.ID,
					"holder": "engine-worker", "reason": "wave_done"})
				r.event("info", "图节点 %s（%s）%s：%s", n.ID, n.Type, outcome, resultRef)
				st.mu.Unlock()
			}
			continue // 波次边界：回到冻结闸门/下一波组装
		}

		node := st.nextOpen()
		if node != nil && node.Holder != "" {
			// human claim 占住（§2.1）：引擎不派发也不收敛——轮询等待租约到期/
			// release 释放（与注入注册表同机制；2s 粒度=与 SSE 轮询同档成本）。
			time.Sleep(2 * time.Second)
			continue
		}
		if node == nil {
			// 新态势判定（M2：图自身状态事件，不依赖 findings 通道）。
			terminals := st.nonPlannerTerminals()
			if terminals > plannerMark && st.plannerRuns < spec.MaxPlannerReplans && spec.AllowedTypes["planner"] {
				if len(st.nodes) >= spec.MaxNodes { // 重规划 planner 声明同受 max_nodes 封顶（图规模上限统一执法，D1）
					_ = emit.emit(evConverged, map[string]any{
						"summary": fmt.Sprintf("max_nodes=%d 已满，重规划拒绝（completed=%d failed=%d rejected=%d）",
							spec.MaxNodes, countBy(st, stCompleted), countBy(st, stFailed), countBy(st, stRejected)),
						"openRemaining": 0})
					st.countsInto(&result.Total, &result.Completed, &result.Failed, &result.Rejected, &result.Open)
					result.Converged = true
					result.Turns = gs.turnsTotal()
					return result, nil
				}
				trigger, detail := st.situation()
				plannerMark = terminals // 水位推进（本次派发消费）
				_ = emit.emit(evPlanTriggered, map[string]any{"trigger": trigger, "detail": detail})
				n := st.declare("planner", "重规划：依据新态势给出后续节点或收敛", "pack", fromRecent(st))
				_ = emit.emit(evNodeDeclared, map[string]any{
					"nodeId": n.ID, "type": n.Type, "intent": n.Intent, "declaredBy": "pack", "from": n.From})
				continue
			}
			// 收敛（§4：converged 收束）。预算耗尽/planner 不在允许集也在此收敛——
			// summary 如实记因（D1 不静默）。
			summary := fmt.Sprintf("open 排空且无新态势：completed=%d failed=%d rejected=%d",
				countBy(st, stCompleted), countBy(st, stFailed), countBy(st, stRejected))
			if terminals > plannerMark {
				summary += fmt.Sprintf("（planner 预算耗尽：%d/%d）", st.plannerRuns, spec.MaxPlannerReplans)
			}
			_ = emit.emit(evConverged, map[string]any{"summary": summary, "openRemaining": 0})
			st.countsInto(&result.Total, &result.Completed, &result.Failed, &result.Rejected, &result.Open)
			result.Converged = true
			result.Turns = gs.turnsTotal()
			return result, nil
		}

		// 步骤 2：派发（node_started 先于 prompt——ts 归并因果安全，§9）。实时折叠与
		// 回放共用同一状态机同序（A1 投影一致性）。
		// R122 graph-reseed：失败史存在 → 取 baton 种子切片注入 prompt（任务内换手
		// ——换壳重派=同 task_id 换持笔人）。获取失败/未配置=无种子派发+WARN（降级
		// 纪律 ⚑2）；干净图不取（⚑1——首批节点 prompt 精瘦）。node_seeded 先于
		// started（因果序：种子先于派发）。
		seedText := ""
		if st.hasFailureHistory() && spec.SeedBrief != nil {
			seedText = strings.TrimSpace(spec.SeedBrief())
			if seedText != "" {
				sum := sha256.Sum256([]byte(seedText))
				_ = emit.emit(evNodeSeeded, map[string]any{"nodeId": node.ID,
					"digest": hex.EncodeToString(sum[:6]),
					"lines":  1 + strings.Count(seedText, "\n")})
			} else {
				log.Printf("[graph] 种子获取为空（brief 失败或空档）——无种子派发 %s", node.ID)
			}
		}

		_ = emit.emit(evNodeStarted, map[string]any{
			"nodeId": node.ID, "sessionId": node.SessionID(), "turnRef": node.SessionID()})
		_ = st.apply(&graphEnvelope{Type: evNodeStarted, Seq: emit.lastSeq(), Data: map[string]any{"nodeId": node.ID}})
		prompt := buildGraphNodePrompt(&spec, node, st, seedText, r.agentCaps().StructuredSubmit)
		tctx, cancel := context.WithTimeout(ctx, spec.timeoutFor(node.Type)) // B3 类型化超时
		text, calls, turns, derr := gs.dispatch(tctx, node, prompt)
		cancel()

		// 类型化收笔（审计 H2）：仅 executor 按提交通道判零提交注入恰一轮
		// （capability 闸门同 Run，ADR-246）；planner（schema 文本）/verifier（裁决
		// 文本）零提交=合法态不注入。
		if derr == nil && node.Type == "executor" && r.agentCaps().ConcludeSemantics && !expectedSubmitCaptured(calls, false) {
			r.event("warn", "图节点 %s executor 零提交——收笔注入恰一轮（只定稿不推进，ADR-249 H2）", node.ID)
			cctx, ccancel := context.WithTimeout(ctx, spec.timeoutFor(node.Type))
			cText, cCalls, _, cErr := gs.dispatch(cctx, node, concludePrompt(false))
			ccancel()
			if cErr != nil {
				r.event("warn", "收笔轮失败（保原结果不放大，ADR-246）: %v", cErr)
			} else {
				if cText != "" {
					text = cText
				}
				calls = append(calls, cCalls...)
			}
		}

		// 步骤 3-4：结果回收（planner 扩图在特判；其余按类型收）。
		outcome, resultRef, findings := r.collectGraphNode(&spec, st, node, text, calls, derr)
		if node.Type == "planner" && outcome == outcomeCompleted {
			// planner 输出解析+校验+封顶（§4；拒绝留痕进 resultRef——D1 不静默截断。
			// 设计 §3"记 plan_triggered 明细"的承载面裁决：伪造触发事件会污染因果序，
			// planner 自身 resultRef 是其产出的忠实位置，ADR-249）。
			resultRef = r.declareFromPlanner(&spec, st, node, text, emit)
		}
		result.Findings = append(result.Findings, findings...)
		_ = emit.emit(evNodeResult, map[string]any{
			"nodeId": node.ID, "outcome": outcome, "resultRef": resultRef,
			"usageEstimate": map[string]any{"turns": turns}})
		// 实时折叠与回放共用同一状态机（A1 投影一致性——DoD 1 回放重建等价的构造性保证）。
		_ = st.apply(&graphEnvelope{Type: evNodeResult, Seq: emit.lastSeq(), Data: map[string]any{
			"nodeId": node.ID, "outcome": outcome, "resultRef": resultRef}})
		r.event("info", "图节点 %s（%s）%s：%s", node.ID, node.Type, outcome, resultRef)
	}
}

// collectGraphNode — 节点结果回收（§4 步骤 3）：outcome 三分（D1）+findings 提取
// （executor 经既有 submit_findings 通道——A184 解析面复用，零新提交工具）。
func (r *ManagerRunner) collectGraphNode(spec *GraphTaskSpec, st *graphState, node *graphNode,
	text string, calls []ToolCall, derr error) (outcome, resultRef string, findings []Finding) {
	if derr != nil {
		// 异常路径（§4）：类型化超时/断流续跑再败/取消——indeterminate，attempt 在
		// apply 折叠时 +1；达上限=终态 failed（不回 open）——杜绝失败无限循环（M1）。
		// open 排空可触发 plan_triggered(open_drained) 由 planner 裁决换路或收敛。
		if attemptExhausted(node, spec) {
			return outcomeFailed, truncateRunes(fmt.Sprintf("尝试上限 %d 内未收敛：%v", spec.MaxAttemptsPerNode, derr), graphResultRefMaxRunes), nil
		}
		return outcomeIndeterminate, truncateRunes(derr.Error(), graphResultRefMaxRunes), nil
	}
	if reason := refusedOf(text); reason != "" {
		// 拒答（D1 一等）：REFUSED 首行契约（节点 prompt 结果契约明示；mock 旋钮
		// 直出同形状）。冷却窗语义（§4）未实现——rejected 终态天然不重派，
		// intent 级冷却如需另设计。
		return outcomeRejected, truncateRunes("拒答："+reason, graphResultRefMaxRunes), nil
	}
	switch node.Type {
	case "executor":
		fs, _, perr := parseAuditResult(text, calls)
		if perr != nil {
			if attemptExhausted(node, spec) {
				return outcomeFailed, truncateRunes("findings 解析失败（达尝试上限）："+perr.Error(), graphResultRefMaxRunes), nil
			}
			return outcomeIndeterminate, truncateRunes("findings 解析失败："+perr.Error(), graphResultRefMaxRunes), nil
		}
		return outcomeCompleted, fmt.Sprintf("submit_findings×%d", len(fs)), fs
	case "verifier":
		return outcomeCompleted, truncateRunes(text, graphResultRefMaxRunes), nil
	default: // planner：解析与声明在调用方特判
		return outcomeCompleted, "intents 待声明", nil
	}
}

// attemptExhausted — 本次尝试后是否达 per-node 上限（审计 M1 活锁防线单点：
// err 分支与解析失败分支共用——两道独立判定会互相兜底，变异逃逸面）。
func attemptExhausted(node *graphNode, spec *GraphTaskSpec) bool {
	return node.Attempts+1 >= spec.MaxAttemptsPerNode
}

// plannerIntents — planner 输出 schema 钉形状（§4：P0 实证 schema 收敛可行）。
// nodeId 由 engine 分配（planner 不管编号）；from 引用既有节点。
type plannerIntents struct {
	Nodes []struct {
		Type   string   `json:"type"`
		Intent string   `json:"intent"`
		From   []string `json:"from"`
	} `json:"nodes"`
}

// declareFromPlanner — planner 输出解析+校验+封顶（§3 写路径执法）：超限拒绝声明
// 并把明细记入返回的 resultRef（D1）。合法声明逐个 node_declared 发射。
func (r *ManagerRunner) declareFromPlanner(spec *GraphTaskSpec, st *graphState, planner *graphNode,
	text string, emit *graphEmitter) string {
	var intents plannerIntents
	if err := json.Unmarshal([]byte(extractJSON(text)), &intents); err != nil {
		return "planner 输出非 schema 形状（intents JSON 解析失败）"
	}
	declared, rejections := 0, []string{}
	for _, it := range intents.Nodes {
		if reason := validateIntent(spec, st, it); reason != "" {
			rejections = append(rejections, reason)
			continue
		}
		n := st.declare(it.Type, it.Intent, planner.ID, it.From)
		_ = emit.emit(evNodeDeclared, map[string]any{
			"nodeId": n.ID, "type": n.Type, "intent": n.Intent, "declaredBy": planner.ID, "from": n.From})
		declared++
	}
	if len(rejections) > 0 { // 留痕（D1：不静默截断）
		r.event("warn", "planner %s 扩图封顶拒绝 %d 项：%s", planner.ID, len(rejections), strings.Join(rejections, "；"))
		return fmt.Sprintf("声明 %d 节点；拒绝 %d 项：%s", declared, len(rejections), strings.Join(rejections, "；"))
	}
	return fmt.Sprintf("声明 %d 节点", declared)
}

// validateIntent — planner 输出单条校验（封顶/枚举/引用存在性；空=合法）。
// v0 不验 DAG 无环——串行调度下环只烧 planner 配额，max_planner_replans 封顶兜底
// （ADR-249 消融记录）。
func validateIntent(spec *GraphTaskSpec, st *graphState, it struct {
	Type   string   `json:"type"`
	Intent string   `json:"intent"`
	From   []string `json:"from"`
}) string {
	if !spec.AllowedTypes[it.Type] {
		return fmt.Sprintf("type %q 不在 allowed_types", it.Type)
	}
	if strings.TrimSpace(it.Intent) == "" {
		return "intent 为空"
	}
	if len(st.nodes) >= spec.MaxNodes {
		return fmt.Sprintf("max_nodes=%d 已满（含已声明）", spec.MaxNodes)
	}
	for _, f := range it.From {
		if st.nodes[f] == nil {
			return fmt.Sprintf("from 引用 %q 不存在", f)
		}
	}
	return ""
}

// extractJSON — 文本中的 JSON 体提取（裸 JSON 或 ```json 围栏；无体=原样返回交
// 解析器报错）。
func extractJSON(text string) string {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "```json"); i >= 0 {
		s = s[i+len("```json"):]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		return strings.TrimSpace(s)
	}
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
		return strings.TrimSpace(s)
	}
	return s
}

// refusedOf — 拒答识别（§4：REFUSED 首行契约，v0 最小识别面，ADR-249）。
func refusedOf(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "REFUSED:"); ok {
			return strings.TrimSpace(rest)
		}
		return "" // 首个非空行非 REFUSED 即正常输出
	}
	return ""
}

// buildGraphNodePrompt — 节点 prompt 组装（A4 有界投影）：intent+goal+已完成节点
// resultRef 摘要（≤8 条×200 字，上下文不随图增长）+类型化结果契约（B3）。
// prompt 自含：换手后任意 harness 可接任意 open 节点（会话无历史依赖，B6 对称性）。
// structured=descriptor capabilities.structured_submit：
// true→submit_findings 工具契约；false→围栏 JSON 契约（平面路径同款——
// parseAuditResult 的 text-fence 降级通道收割,非工具 agent 的图任务由此通路）。
func buildGraphNodePrompt(spec *GraphTaskSpec, node *graphNode, st *graphState, seed string, structured bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# 图节点任务（%s · %s）\n\n## 任务目标\n%s\n\n## 本节点意图\n%s\n",
		node.ID, node.Type, spec.Goal, node.Intent)
	if seed != "" {
		sb.WriteString("\n## 前任失败要点（baton 种子切片——重派前必读，先验证副作用再行动）\n" + seed + "\n")
	}
	if from := st.projectionOf(node); from != "" {
		sb.WriteString("\n## 已确认事实（前序节点结果摘要）\n" + from)
	}
	sb.WriteString("\n## 结果契约\n")
	switch node.Type {
	case "executor":
		if structured {
			sb.WriteString("调用 submit_findings 提交全部发现（侦察观察以 severity=INFO 携带；" +
				"漏洞按严重度分级）。分批提交，单批参数勿过长。\n")
		} else {
			// 围栏示例与平面 assignmentTemplate 同构（字段值语义显式——首版键列表
			// 压缩致模型类型漂移:confidence 写 "HIGH"/start_line 写字符串,实测翻案）
			sb.WriteString("以 ```json 围栏输出全部发现（最后一条消息,正文唯一围栏）:\n" +
				"```json\n{\n  \"findings\": [\n    {\n" +
				"      \"title\": \"简短漏洞标题\",\n" +
				"      \"description\": \"问题与影响说明\",\n" +
				"      \"severity\": \"SEVERITY_CRITICAL|SEVERITY_HIGH|SEVERITY_MEDIUM|SEVERITY_LOW|SEVERITY_INFO\",\n" +
				"      \"cwe_id\": \"CWE-XXX 或空字符串\",\n" +
				"      \"file_path\": \"项目内相对路径\",\n" +
				"      \"start_line\": 行号整数,\n" +
				"      \"confidence\": 0.0到1.0的小数,\n" +
				"      \"reasoning\": \"判定理由（引用代码证据）\",\n" +
				"      \"fix_suggestion\": \"修复说明（无把握可空字符串）\"\n    }\n  ]\n}\n```\n" +
				"（侦察观察以 SEVERITY_INFO 携带；漏洞按严重度分级;零发现输出空数组。）\n")
		}
	case "planner":
		sb.WriteString(`只输出 JSON（无其他正文）：{"nodes":[{"type":"planner|executor|verifier","intent":"一句话","from":["n001"]}]}` +
			"\n依据新态势扩展节点；图已足够则输出 {\"nodes\":[]}。\n")
	case "verifier":
		sb.WriteString("输出裁决文本：结论（确认/证伪/存疑）+依据（引用前序节点结果）+后续建议。\n")
	}
	sb.WriteString("\n若无法执行本节点（授权范围外/材料不足），回复首行 \"REFUSED: <原因>\"——" +
		"拒答不是失败，不会被判失败，冷却后会重新规划。\n")
	return sb.String()
}

// projectionOf — 有界投影体（A4）：最近完成的非自身节点 resultRef，≤8 条。
func (st *graphState) projectionOf(self *graphNode) string {
	var lines []string
	for i := len(st.order) - 1; i >= 0 && len(lines) < graphProjectionMaxNodes; i-- {
		n := st.nodes[st.order[i]]
		if n == nil || n.ID == self.ID || n.ResultRef == "" {
			continue
		}
		switch n.Status {
		case stCompleted, stFailed, stRejected:
			// R122：失败史也进投影（重派/接手必读——rejected/failed 带标记）
		default:
			continue
		}
		marker := ""
		if n.Status == stFailed {
			marker = "[FAILED] "
		} else if n.Status == stRejected {
			marker = "[REJECTED] "
		}
		lines = append(lines, fmt.Sprintf("- [%s %s] %s%s", n.ID, n.Type, marker, truncateRunes(n.ResultRef, graphResultRefMaxRunes)))
	}
	if len(lines) == 0 {
		return ""
	}
	// 逆序收集后恢复声明序（时间线可读）
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n")
}

// ---- graphState 派生查询 ----

// nonPlannerTerminals — 非 planner 节点终态计数（新态势水位，防 planner 自激）。
func (st *graphState) nonPlannerTerminals() int {
	n := 0
	for _, node := range st.nodes {
		if node.Type != "planner" && node.Status.terminal() {
			n++
		}
	}
	return n
}

// situation — 新态势归类（new_evidence|open_drained）与明细（M2；全图口径计数）。
func (st *graphState) situation() (trigger, detail string) {
	var failures, completes int
	for _, node := range st.nodes {
		if node.Type == "planner" || !node.Status.terminal() {
			continue
		}
		switch node.Status {
		case stCompleted:
			completes++
		case stFailed, stRejected:
			failures++
		}
	}
	if failures > 0 {
		return triggerOpenDrained, fmt.Sprintf("失败/拒答 %d 项致 open 排空，请求换路或收敛", failures)
	}
	return triggerNewEvidence, fmt.Sprintf("新增 completed %d 项", completes)
}

func countBy(st *graphState, s nodeStatus) int {
	n := 0
	for _, node := range st.nodes {
		if node.Status == s {
			n++
		}
	}
	return n
}

func (st *graphState) countsInto(total, completed, failed, rejected, open *int) {
	*total, *completed, *failed, *rejected, *open = st.counts()
}

// fromRecent — 重规划 planner 的 from：最近终态节点（认知输入指针）。
// emitApply — 事件发射+实时折叠的同 Data 双写收敛（A1：emit 与 apply 共用同一
// map 引用，envelope 即弃无观察面差异）。仅限持 st.mu 的调用方；锁外 emit 的
// 注入/认领/释放闭包不适用（会拉长临界区）。
func emitApply(emit *graphEmitter, st *graphState, kind string, data map[string]any) {
	_ = emit.emit(kind, data)
	_ = st.apply(&graphEnvelope{Type: kind, Seq: emit.lastSeq(), Data: data})
}

// claimableCap — 护栏截断（v0.5 §2.3：MaxParallel>0 才生效；在 filterNonPlanner
// 之后调用——planner 不占并发名额）。
func claimableCap(ns []*graphNode, maxParallel int) []*graphNode {
	if maxParallel <= 0 || len(ns) <= maxParallel {
		return ns
	}
	return ns[:maxParallel]
}

// filterNonPlanner — 波次只装 executor/verifier（planner 全局视野=波次边界串行
// 派发，态势水位判定不受并行影响）。
func filterNonPlanner(ns []*graphNode) []*graphNode {
	out := make([]*graphNode, 0, len(ns))
	for _, n := range ns {
		if n.Type != "planner" {
			out = append(out, n)
		}
	}
	return out
}

// seedTextFor — R122 graph-reseed 种子切片获取（原主循环内联逻辑抽取——波次
// 派发在 goroutine 内调用；emit 持锁并发安全）。干净图/未配置=空串（⚑2 降级）。
func seedTextFor(st *graphState, spec *GraphTaskSpec, node *graphNode, emit *graphEmitter) string {
	if !st.hasFailureHistory() || spec.SeedBrief == nil {
		return ""
	}
	seedText := strings.TrimSpace(spec.SeedBrief())
	if seedText == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seedText))
	_ = emit.emit(evNodeSeeded, map[string]any{"nodeId": node.ID,
		"digest": hex.EncodeToString(sum[:6]),
		"lines":  1 + strings.Count(seedText, "\n")})
	return seedText
}

func fromRecent(st *graphState) []string {
	for i := len(st.order) - 1; i >= 0; i-- {
		if n := st.nodes[st.order[i]]; n != nil && n.Status.terminal() {
			return []string{n.ID}
		}
	}
	return nil
}

// truncateRunes — 按字符截断（resultRef ≤200 字契约，§2）。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
