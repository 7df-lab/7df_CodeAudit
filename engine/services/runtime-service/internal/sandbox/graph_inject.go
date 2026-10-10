// graph_inject.go — 图注入注册表（v0.5 人机共编辑 b 档，graph-engine-v0.5-blackboard-hoc.md §4）。
// RunGraph 开启图会话时注册闭包（持有 gs/emit/st），服务层 RPC 调用入口注入人消息；
// 沙箱回收/图收束时注销。与 PauseGate 的 GateFor/OnFrozen 同构（任务级注册表，
// 引擎单写者纪律不变——注入只追加 user 消息，不做结果回收）。
package sandbox

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// InjectorFn — 注入执行体：向指定节点会话追加 user 消息（bridge 排队语义=回合
// 边界生效，不打断当回合）。返回错误=校验失败（节点非 running/图已收束）。
type InjectorFn func(nodeID, message string) error

// ClaimFn/ReleaseFn — 认领层 REST 执行体（§2.1）：human 占住节点（调度跳过）/
// 释放回 open。
type ClaimFn func(nodeID string, ttlS int) (leaseUntil string, err error)
type ReleaseFn func(nodeID string) error

// graphOps — 任务级操作集（注入+认领；RunGraph 注册，收束注销）。
type graphOps struct {
	inject  InjectorFn
	claim   ClaimFn
	release ReleaseFn
}

// graphInjectRegistry — 任务级注册表（进程内；图执行生命周期=注册窗口）。
var graphInjectRegistry = struct {
	mu sync.Mutex
	m  map[string]*graphOps
}{m: map[string]*graphOps{}}

// registerGraphOps — 注册操作集并返回释放函数（RunGraph defer 释放）。
func registerGraphOps(taskID string, ops *graphOps) (release func()) {
	graphInjectRegistry.mu.Lock()
	defer graphInjectRegistry.mu.Unlock()
	graphInjectRegistry.m[taskID] = ops
	return func() {
		graphInjectRegistry.mu.Lock()
		defer graphInjectRegistry.mu.Unlock()
		delete(graphInjectRegistry.m, taskID)
	}
}

// lookupGraphOps — 注册表查询（取指针前持锁；nil=任务无活跃图执行）。
func lookupGraphOps(taskID string) *graphOps {
	graphInjectRegistry.mu.Lock()
	defer graphInjectRegistry.mu.Unlock()
	return graphInjectRegistry.m[taskID]
}

// InjectGraphNode — 服务层入口：任务无活跃图=错误（诚实拒绝，不静默丢弃）。
func InjectGraphNode(taskID, nodeID, message string) error {
	ops := lookupGraphOps(taskID)
	if ops == nil || ops.inject == nil {
		return fmt.Errorf("任务 %s 无活跃图执行——注入不可达（非图任务/已收束/沙箱已回收）", taskID)
	}
	return ops.inject(nodeID, message)
}

// ClaimGraphNode — human 认领（declared 节点；TTL 秒，0=缺省 1800）。
func ClaimGraphNode(taskID, nodeID string, ttlS int) (leaseUntil string, err error) {
	ops := lookupGraphOps(taskID)
	if ops == nil || ops.claim == nil {
		return "", fmt.Errorf("任务 %s 无活跃图执行", taskID)
	}
	return ops.claim(nodeID, ttlS)
}

// ReleaseGraphNode — human 释放（holder 清空→调度恢复）。
func ReleaseGraphNode(taskID, nodeID string) error {
	ops := lookupGraphOps(taskID)
	if ops == nil || ops.release == nil {
		return fmt.Errorf("任务 %s 无活跃图执行", taskID)
	}
	return ops.release(nodeID)
}

// GraphInjectActive — 任务图执行中探测（reopen 校验用：活跃图拒绝重开）。
func GraphInjectActive(taskID string) bool {
	graphInjectRegistry.mu.Lock()
	defer graphInjectRegistry.mu.Unlock()
	_, ok := graphInjectRegistry.m[taskID]
	return ok
}

// BuildGraphReopened — 回放校验节点终态后构造 reopened 事件行（JSONL，seq 接续
// 档案现状）。调用方负责追加落盘（writeGraph 同款 fsync 纪律）。校验失败=节点
// 不存在/非终态/回放 fatal（R116 撕裂容忍照旧）——诚实拒绝不静默。
func BuildGraphReopened(logReader io.Reader, nodeID, reason string) ([]byte, error) {
	st, err := ReplayGraphLog(logReader)
	if err != nil {
		return nil, err
	}
	n := st.nodes[nodeID]
	if n == nil {
		return nil, fmt.Errorf("节点 %q 不存在", nodeID)
	}
	if !n.Status.terminal() {
		return nil, fmt.Errorf("节点 %q 状态 %s 非终态——重开仅限终态节点", nodeID, n.Status)
	}
	env := graphEnvelope{
		Type: evNodeReopened, Seq: st.lastSeq + 1,
		Time: time.Now().UTC().Format(time.RFC3339Nano),
		Data: map[string]any{"nodeId": nodeID, "reason": reason, "by": "human"},
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
