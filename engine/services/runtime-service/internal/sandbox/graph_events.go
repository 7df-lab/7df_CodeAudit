// graph.* 事件族与发射器（切片 5+6 TP18-T3，ADR-249）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §2（域中立 schema，
// P0 样本校准信封 {type,seq,time,data}）——全族字段只用控制面词表（C4）：
// 禁业务 schema 词（intent 是一句话自然语言，类型枚举外无自由类型字段）。
// node_result 的 outcome 三分（completed|failed|rejected|indeterminate）——D1
// 拒答≠失败≠没跑完。
package sandbox

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// graph.* 事件类型常量（§2 六事件）。
const (
	evNodeDeclared  = "graph.node_declared"
	evNodeStarted   = "graph.node_started"
	evNodeResult    = "graph.node_result"
	evPlanTriggered = "graph.plan_triggered"
	evPaused        = "graph.paused"
	evConverged     = "graph.converged"
	evNodeSeeded    = "graph.node_seeded" // R122 graph-reseed：重派种子摘要（digest+行数）

	// v0.5 黑板认领层+人机共编辑（graph-engine-v0.5-blackboard-hoc.md §2.2/§4）。
	// 折叠规则三实现同批同步（R125 纪律：engine apply/baton fold/web 投影）。
	evNodeClaimed      = "graph.node_claimed"       // {nodeId, holder, leaseUntil}
	evNodeReleased     = "graph.node_released"      // {nodeId, holder, reason}
	evNodeLeaseExpired = "graph.node_lease_expired" // {nodeId, holder}
	evNodeReopened     = "graph.node_reopened"      // {nodeId, reason, by}
	evHumanMessage     = "graph.human_message"      // {nodeId, actor, digest, bytes}
)

// node_result outcome 枚举（§2；D1 分道）。
const (
	outcomeCompleted     = "completed"
	outcomeFailed        = "failed"
	outcomeRejected      = "rejected"
	outcomeIndeterminate = "indeterminate"
)

// plan_triggered trigger 枚举（§2）。
const (
	triggerInitial     = "initial"
	triggerNewEvidence = "new_evidence"
	triggerOpenDrained = "open_drained"
)

// graphEnvelope — graph.log JSONL 信封（P0 实测形状；seq 全档单调，time=发射时刻）。
type graphEnvelope struct {
	Type string         `json:"type"`
	Seq  int            `json:"seq"`
	Time string         `json:"time"` // RFC3339Nano
	Data map[string]any `json:"data"`
}

// graphEmitter — 事件发射器：seq 单调分配，JSONL 行经 out 外流（service 层落
// <task>.graph.log；留存通道纪律同 .sse.log——不影响执行主链路，out 为空=丢弃）。
type graphEmitter struct {
	mu  sync.Mutex
	seq int
	out func([]byte)
	now func() time.Time
}

func newGraphEmitter(startSeq int, out func([]byte)) *graphEmitter {
	if out == nil {
		out = func([]byte) {} // nil 安全：落盘未配置≠执行失败
	}
	return &graphEmitter{seq: startSeq, out: out, now: time.Now}
}

// emit — 发射一帧事件（data 字段域中立；错误只可能来自 marshal，防御性返回）。
func (e *graphEmitter) emit(kind string, data map[string]any) error {
	// R116 顺手批：marshal+落盘全程持锁（原版锁外写盘，并发发射可 seq 乱序落盘；
	// 当前调用面串行=潜伏缺陷，锁内串行化消除竞态前提）。
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	env := graphEnvelope{Type: kind, Seq: e.seq, Time: e.now().UTC().Format(time.RFC3339Nano), Data: data}
	b, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("graph event marshal %s: %w", kind, err)
	}
	b = append(b, '\n')
	e.out(b)
	return nil
}

// lastSeq — 已发射的最大 seq（回放续写接续用）。
func (e *graphEmitter) lastSeq() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}
