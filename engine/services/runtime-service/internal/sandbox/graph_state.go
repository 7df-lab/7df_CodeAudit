// 图状态表与回放重建（切片 5+6 TP18-T3，ADR-249）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §3——图状态=graph.log
// 的物化视图（A1 投影、只增不改），可回放重建（对账器/换手恢复路径共用）；open 集
// 折叠规则与 baton 投影一致（ADR-249）：declared 入 open、node_result 终态
// （completed|failed|rejected）出 open、indeterminate 留 open（待重派，D1 没跑完）。
// PG 表 task_graph_nodes 延后至出现查询消费者（消融记录见 ADR-249）——回放重建器
// 接口已钉住物化形状。
package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"time"
)

// nodeStatus — 图状态表 status 列（§3 枚举；indeterminate 不是表状态——
// node_result(indeterminate) 后节点回 open（status=declared，attempt+1））。
type nodeStatus string

const (
	stDeclared  nodeStatus = "declared"
	stRunning   nodeStatus = "running"
	stCompleted nodeStatus = "completed"
	stFailed    nodeStatus = "failed"
	stRejected  nodeStatus = "rejected"
)

func (s nodeStatus) terminal() bool {
	return s == stCompleted || s == stFailed || s == stRejected
}

// graphNode — 状态表行（§3 列集）。
type graphNode struct {
	ID          string
	Type        string // planner|executor|verifier（B3）
	Intent      string
	Status      nodeStatus
	DeclaredBy  string // pack|planner 节点 id（§2）
	From        []string
	ResultRef   string // 提交通道引用或文本摘要（≤200 字）
	Attempts    int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time

	// completedOrd — completed 时点的声明序快照（新态势判定"上次 planner 派发后
	// 新增 completed"的比较基准，M2：不依赖 findings 通道）。
	completedOrd int

	// v0.5 认领层（graph-engine-v0.5-blackboard-hoc.md §2）：holder 空=未认领。
	// 引擎 worker 波次认领（holder=engine-worker）与 human claim（holder=human，
	// 带租约）共用字段；租约过期由调度扫描回收（lease_expired 留痕）。
	Holder     string
	LeaseUntil time.Time
}

// SessionID — 节点会话（§1：sessionId=g-<nodeId>，bridge 惰性建独立 agent+session 对）。
func (n *graphNode) SessionID() string { return "g-" + n.ID }

// graphState — 物化视图（进程内权威形态；重建=ReplayGraphLog）。
// v0.5 mu：注入闭包（RPC goroutine）与主循环并发读节点——读锁保护；写路径
// （apply/claim/collect）仍在主循环单 goroutine（波次边界同步，免写锁竞争）。
type graphState struct {
	mu          sync.Mutex
	nodes       map[string]*graphNode
	order       []string // 声明序（open 调度取最早——§4 步骤 1）
	nodeSeq     int      // nodeId 分配计数（n001 起单调，§2）
	lastSeq     int      // graph.log 已见最大事件 seq（emit 接续）
	plannerRuns int      // 非 initial 的 plan_triggered 计数（max_planner_replans 预算）
	converged   bool     // graph.converged 已见（回放终局判定）
	lines       int      // 已处理行数（错误定位）

	// R116（切片 5+6 复查 P2）：pause 时已消费的新态势水位（evPaused 快照）——
	// resume 后以快照值恢复 plannerMark，否则"最后终态之后、排空检查之前"冻结的
	// 一次 new_evidence 重规划被吞（水位被当前值重置）。
	pausedMark     int
	hasPausedMark  bool
	droppedLines   int      // 回放容忍丢弃的撕裂/坏行数（R116：诚实计数）
	recoveredNodes []string // 回放尾孤儿归回 declared 的节点（R116：可观测）
}

func newGraphState() *graphState {
	return &graphState{nodes: map[string]*graphNode{}}
}

// nextNodeID — 分配下一节点号（n001 起单调；回放后接续）。
func (st *graphState) nextNodeID() string {
	st.nodeSeq++
	return fmt.Sprintf("n%03d", st.nodeSeq)
}

// declare — 声明节点（写路径唯一入口；封顶执法在调用方 GraphRunner——D1 拒绝
// 声明+留痕不静默截断）。
func (st *graphState) declare(typ, intent, declaredBy string, from []string) *graphNode {
	id := st.nextNodeID()
	now := time.Now().UTC()
	n := &graphNode{ID: id, Type: typ, Intent: intent, Status: stDeclared,
		DeclaredBy: declaredBy, From: append([]string(nil), from...), CreatedAt: now, UpdatedAt: now}
	st.nodes[id] = n
	st.order = append(st.order, id)
	return n
}

// nextOpen — 最早已声明 open 节点（§4 步骤 1；nil=open 排空）。
func (st *graphState) nextOpen() *graphNode {
	for _, id := range st.order {
		n := st.nodes[id]
		if n.Status == stDeclared {
			// Cooldown 死码清除：
			// 原 `&& !n.Cooldown` 过滤为死码——rejected 已是终态不在 declared
			// 集，过滤永不生效；intent 级冷却窗（§4 设计）从未实现，随批裁决删除
			// 死码（若未来需要，按 intent 维度另设计）。
			return n
		}
	}
	return nil
}

// claimableReady — v0.5 波次组装：
// 依赖（from 集）全部 terminal 的 declared 节点，按声明序产出；跳过：
// ①已认领（holder 非空）②human claim 租约未过期（guard 栏）。
// maxParallel<=0=无护栏（动态派发）；>0=截断。返回前先回收过期 human claim
// （lease_expired 事件由调用方经返回值发射——状态机纯函数，事件在 graph.go）。
func (st *graphState) claimableReady(maxParallel int, now time.Time) (ready []*graphNode, expired []*graphNode) {
	// 过期扫描：human claim 租约到期→回收（引擎单调钟判定，事件由调用方写）。
	for _, id := range st.order {
		n := st.nodes[id]
		if n.Holder != "" && !n.LeaseUntil.IsZero() && now.After(n.LeaseUntil) {
			expired = append(expired, n)
		}
	}
	for _, id := range st.order {
		n := st.nodes[id]
		if n.Status != stDeclared || n.Holder != "" {
			continue
		}
		readyDeps := true
		for _, dep := range n.From {
			if d := st.nodes[dep]; d == nil || !d.Status.terminal() {
				readyDeps = false
				break
			}
		}
		if !readyDeps {
			continue
		}
		ready = append(ready, n)
		if maxParallel > 0 && len(ready) >= maxParallel {
			break
		}
	}
	return ready, expired
}

// claimNode — 调度/人认领（写路径：holder+租约）。
func (st *graphState) claimNode(n *graphNode, holder string, leaseUntil time.Time) {
	n.Holder = holder
	n.LeaseUntil = leaseUntil
}

// releaseNode — 释放（holder 清空）。
func (st *graphState) releaseNode(n *graphNode) {
	n.Holder = ""
	n.LeaseUntil = time.Time{}
}

// reopenNode — 终态重开（§2.1：回 declared，attempt 保留）。
func (st *graphState) reopenNode(n *graphNode) {
	if n.Status.terminal() {
		n.Status = stDeclared
		n.ResultRef = ""
		n.UpdatedAt = time.Now().UTC()
	}
}

// openIDs — open 集快照（graph.paused 定格用）。
func (st *graphState) openIDs() []string {
	var out []string
	for _, id := range st.order {
		if n := st.nodes[id]; n.Status == stDeclared {
			out = append(out, id)
		}
	}
	return out
}

// counts — 终局统计（RunGraphTaskResponse 面）。
func (st *graphState) counts() (total, completed, failed, rejected, open int) {
	for _, n := range st.nodes {
		total++
		switch n.Status {
		case stCompleted:
			completed++
		case stFailed:
			failed++
		case stRejected:
			rejected++
		case stDeclared:
			open++
		}
	}
	return
}

// hasFailureHistory — R122 graph-reseed 种子挂接判定（⚑1）：rejected/failed
// 终态、indeterminate 重派史（Attempts>0）、回放恢复孤儿任一=存在失败史。
func (st *graphState) hasFailureHistory() bool {
	if len(st.recoveredNodes) > 0 {
		return true
	}
	for _, n := range st.nodes {
		if n.Status == stFailed || n.Status == stRejected || n.Attempts > 0 {
			return true
		}
	}
	return false
}

// replayCompletedSince — 上次 planner 派发后新增的 completed 数（新态势判定 M2：
// 图自身状态事件，不依赖 findings 通道）。
func (st *graphState) replayCompletedSince(mark int) int {
	n := 0
	for _, node := range st.nodes {
		if node.Status == stCompleted && node.completedOrd > mark {
			n++
		}
	}
	return n
}

// apply — 单事件折叠（回放与实时共用同一状态机——A1 投影一致性）。
func (st *graphState) apply(env *graphEnvelope) error {
	st.lines++
	if env.Seq <= 0 {
		return fmt.Errorf("graph.log 行 %d: seq 非法（%d）", st.lines, env.Seq)
	}
	if env.Seq > st.lastSeq {
		st.lastSeq = env.Seq
	}
	data := env.Data
	nodeOf := func() (*graphNode, error) {
		id, _ := data["nodeId"].(string)
		n := st.nodes[id]
		if n == nil {
			return nil, fmt.Errorf("graph.log 行 %d: 节点 %q 未声明先引用", st.lines, id)
		}
		return n, nil
	}
	switch env.Type {
	case evNodeClaimed:
		n, aerr := nodeOf()
		if aerr != nil {
			return aerr
		}
		if n.Status != stDeclared {
			return fmt.Errorf("graph.log 行 %d: 节点 %q 非 declared 态认领（%s）", st.lines, n.ID, n.Status)
		}
		h, _ := data["holder"].(string)
		n.Holder = h
		if lu, ok := data["leaseUntil"].(string); ok && lu != "" {
			if t, perr := time.Parse(time.RFC3339, lu); perr == nil {
				n.LeaseUntil = t
			}
		}
	case evNodeReleased:
		n, aerr := nodeOf()
		if aerr != nil {
			return aerr
		}
		if n.Holder == "" {
			return fmt.Errorf("graph.log 行 %d: 节点 %q 未认领却 released", st.lines, n.ID)
		}
		n.Holder = ""
		n.LeaseUntil = time.Time{}
	case evNodeLeaseExpired:
		n, aerr := nodeOf()
		if aerr != nil {
			return aerr
		}
		n.Holder = ""
		n.LeaseUntil = time.Time{}
	case evNodeReopened:
		// 终态重开（§2.1）：回 declared+attempt 保留（v0 重派语义泛化——重派计数
		// 独立于重开）。R125 终态再 declared 留痕纪律的 v0.5 正道（显式事件替代
		// 容忍面）。planner 水位不受影响（reopened 非 completed）。
		n, aerr := nodeOf()
		if aerr != nil {
			return aerr
		}
		if !n.Status.terminal() && n.Status != stDeclared {
			return fmt.Errorf("graph.log 行 %d: 节点 %q 状态 %s 不可重开", st.lines, n.ID, n.Status)
		}
		if n.Status.terminal() {
			n.Status = stDeclared
			n.ResultRef = ""
			n.UpdatedAt = time.Now().UTC()
		}
	case evHumanMessage:
		// 纯审计事件（§4）：不改节点状态——回放可重建"人说了什么"（消息全文
		// blob 化引用，digest 留痕）。R124 显式 case 纪律：新事件必须登记。
		if _, ok := data["nodeId"].(string); !ok {
			return fmt.Errorf("graph.log 行 %d: human_message 缺 nodeId", st.lines)
		}
	case evNodeDeclared:
		id, _ := data["nodeId"].(string)
		typ, _ := data["type"].(string)
		intent, _ := data["intent"].(string)
		declaredBy, _ := data["declaredBy"].(string)
		fromAny, _ := data["from"].([]any)
		from := make([]string, 0, len(fromAny))
		for _, f := range fromAny {
			if s, ok := f.(string); ok {
				from = append(from, s)
			}
		}
		if id == "" || typ == "" || intent == "" {
			return fmt.Errorf("graph.log 行 %d: node_declared 缺字段", st.lines)
		}
		if ex := st.nodes[id]; ex != nil {
			// R116 顺手批：重复 declared 幂等忽略（污染档容忍面）——覆盖会清
			// Attempts/状态并使 order 重复 id（openIDs 重复产出）；与 baton
			// graphOpenAdd 去重对齐。语义冲突（已终态再 declared）仍 fatal。
			if ex.Status == stDeclared {
				return nil
			}
			return fmt.Errorf("graph.log 行 %d: 节点 %q 已终态再 declared", st.lines, id)
		}
		n := &graphNode{ID: id, Type: typ, Intent: intent, Status: stDeclared, DeclaredBy: declaredBy,
			From: from, CreatedAt: parseGraphTime(data["declaredAt"]), UpdatedAt: time.Now().UTC()}
		st.nodes[id] = n
		st.order = append(st.order, id)
		if seq := nodeSeqOf(id); seq > st.nodeSeq {
			st.nodeSeq = seq // 回放接续编号
		}
	case evNodeStarted:
		n, err := nodeOf()
		if err != nil {
			return err
		}
		n.Status = stRunning
		n.UpdatedAt = time.Now().UTC()
	case evNodeResult:
		n, err := nodeOf()
		if err != nil {
			return err
		}
		outcome, _ := data["outcome"].(string)
		if ref, ok := data["resultRef"].(string); ok {
			n.ResultRef = ref
		}
		n.UpdatedAt = time.Now().UTC()
		switch outcome {
		case outcomeCompleted:
			n.Status = stCompleted
			n.CompletedAt = time.Now().UTC()
			n.completedOrd = len(st.order) // 声明序粗粒度单调（新态势判定用）
		case outcomeFailed:
			n.Status = stFailed
			n.CompletedAt = time.Now().UTC()
		case outcomeRejected:
			n.Status = stRejected
			n.CompletedAt = time.Now().UTC()
		case outcomeIndeterminate:
			n.Attempts++
			n.Status = stDeclared // 回 open 重派（attempt 已计；上限执法在 runner）
		default:
			return fmt.Errorf("graph.log 行 %d: outcome %q 不在枚举", st.lines, outcome)
		}
	case evPlanTriggered:
		trigger, _ := data["trigger"].(string)
		if trigger != triggerInitial {
			st.plannerRuns++
		}
	case evPaused:
		// 定格事件：状态面无折叠（open 集由既有节点状态推导）；R116 捕获已消费
		// 新态势水位（resume 恢复 plannerMark 用；多次 pause 取最后一次）。
		if v, ok := data["plannerMark"].(float64); ok {
			st.pausedMark = int(v)
			st.hasPausedMark = true
		}
	case evNodeSeeded:
		// R124：种子摘要事件显式登记（R116 起未知类型 fatal，新事件必须显式
		// case——否则含种子行的档 resume 即 fatal，§1"审计/回放可重建"落空）。
		// 审计摘要面：digest/lines 不参与状态折叠，只校验引用完整性（未声明先
		// 引用=结构性污染，与 node_started 同纪律）。
		if _, err := nodeOf(); err != nil {
			return err
		}
	case evConverged:
		st.converged = true
	default:
		return fmt.Errorf("graph.log 行 %d: 未知事件类型 %q", st.lines, env.Type)
	}
	return nil
}

// parseGraphTime — RFC3339 宽松解析（失败=零值，不阻塞折叠）。
func parseGraphTime(v any) time.Time {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// nodeSeqOf — "n001" → 1（非形态返回 0）。
func nodeSeqOf(id string) int {
	// R116 顺手批：原 len==4 钉死 n001..n999——四位编号（n0007 形态，max_nodes>999
	// 即触发）回放编号接续断裂，declare 重新从低号分配撞已存在 id。
	if len(id) < 2 || id[0] != 'n' {
		return 0
	}
	seq, err := strconv.Atoi(id[1:])
	if err != nil {
		return 0
	}
	return seq
}

// ReplayGraphLog — 回放折叠重建（A1：log 是唯一持久真相；resume/对账共用）。
// R116 撕裂容忍（对齐 baton 同档纪律）：JSON 解析失败（写侧崩溃尾行）丢弃+计数，
// 不再 fatal——一次坏行打瘫 resume=自动重试死路；语义折叠错误（未声明先引用/
// 非法 seq/outcome 非枚举/终态再 declared）保持 fatal（结构性污染不容忍，丢了
// 会静默分叉）。回放尾将 started-无-result 的 running 孤儿归回 declared 重派
// （单写者假设下回放时必无真 running；不归回则 nextOpen/counts 均不可见=吞节点
// 假收敛），Attempts++ 使 crash-loop 仍受 max_attempts 封顶。
func ReplayGraphLog(r io.Reader) (*graphState, error) {
	st := newGraphState()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env graphEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			st.droppedLines++
			log.Printf("[graph-replay] 行 %d 解析失败丢弃（撕裂/坏行，累计 %d）: %v",
				st.lines+1, st.droppedLines, err)
			continue
		}
		if err := st.apply(&env); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	st.recoverRunningOrphans()
	return st, nil
}

// recoverRunningOrphans — R116 P1：started-无-result 的 running 孤儿归回 declared
// （重派；Attempts++ 保留 crash-loop 封顶执法）。
func (st *graphState) recoverRunningOrphans() {
	for _, id := range st.order {
		n := st.nodes[id]
		if n.Status != stRunning {
			continue
		}
		n.Status = stDeclared
		n.Attempts++
		n.UpdatedAt = time.Now().UTC()
		st.recoveredNodes = append(st.recoveredNodes, id)
		log.Printf("[graph-replay] running 孤儿归回 declared（重派，attempts=%d）: %s", n.Attempts, id)
	}
}
