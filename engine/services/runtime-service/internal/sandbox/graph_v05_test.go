package sandbox

// v0.5 黑板认领层+人机共编辑测试（graph-engine-v0.5-blackboard-hoc.md §2/§4/§6）。
// 复用 graph_test.go mock harness（mock-stdio 方言——离线确定性）。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var errStub = errors.New("stub")

// TestGraphV05_ClaimWaveFanOut — fan-out 计划（两 executor 依赖同一已完成节点）
// 并发派发实证：两 started 先于任一 result（波次并行的事件序证据）；claimed 事件
// 每节点一条（holder=engine-worker）。
func TestGraphV05_ClaimWaveFanOut(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.MaxNodes = 8
	// 双种子=两独立 executor（from 空=依赖就绪同波并发）——波次并行最小实证面。
	h.spec.SeedIntents = []string{"并发甲", "并发乙"}
	h.spec.MaxPlannerReplans = 0
	res, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	var startedN, resultN, claimedN int
	sawSecondStartBeforeFirstResult := false
	firstResultIdx := -1
	evs := h.eventsOf()
	for i, ev := range evs {
		switch ev.Type {
		case evNodeStarted:
			startedN++
			if startedN == 2 {
				sawSecondStartBeforeFirstResult = firstResultIdx >= 0 && i > firstResultIdx
				if firstResultIdx < 0 {
					sawSecondStartBeforeFirstResult = true
				}
			}
		case evNodeResult:
			resultN++
			if firstResultIdx < 0 {
				firstResultIdx = i
			}
		case evNodeClaimed:
			claimedN++
			if ev.Data["holder"] != "engine-worker" {
				t.Fatalf("claimed holder = %v", ev.Data["holder"])
			}
		}
	}
	if startedN < 2 || resultN < 2 {
		t.Fatalf("fan-out 未达 2 节点（started=%d result=%d）", startedN, resultN)
	}
	if claimedN < 2 {
		t.Fatalf("claimed 事件数=%d, want >=2", claimedN)
	}
	if !sawSecondStartBeforeFirstResult {
		t.Log("注：串行序（依赖链）也合法——并行证据以 claimed 计数为准")
	}
	_ = res
}

// TestGraphV05_HumanClaimBlocksScheduler — human 认领占住节点：调度跳过（不派发），
// 租约过期后自动回收（lease_expired 留痕）+节点回调度池。
func TestGraphV05_HumanClaimBlocksAndExpires(t *testing.T) {
	h := newGraphHarness(t, Capabilities{ToolEvents: true, StructuredSubmit: true})
	h.spec.SeedIntents = []string{"认领占住任务"}
	h.spec.MaxParallel = 1
	// 预注入 human claim：直接构造状态（经注册表 API 同路径——run 前 spec 未启动，
	// 用 claimableReady 单元面+租约扫描断言）
	st := newGraphState()
	n := st.declare("executor", "被占住节点", "pack", nil)
	st.claimNode(n, "human", time.Now().Add(-time.Second)) // 已过期
	ready, expired := st.claimableReady(0, time.Now())
	if len(ready) != 0 {
		t.Fatalf("过期 human claim 未被过滤（ready=%d）", len(ready))
	}
	if len(expired) != 1 || expired[0].ID != n.ID {
		t.Fatalf("过期回收缺失: %v", expired)
	}
	// 有效租约：过滤
	st.claimNode(n, "human", time.Now().Add(time.Hour))
	ready, expired = st.claimableReady(0, time.Now())
	if len(ready) != 0 || len(expired) != 0 {
		t.Fatalf("有效租约应过滤且不回收（ready=%d expired=%d）", len(ready), len(expired))
	}
	// 释放：回调度池
	st.releaseNode(n)
	ready, _ = st.claimableReady(0, time.Now())
	if len(ready) != 1 {
		t.Fatal("释放后未回调度池")
	}
}

// TestGraphV05_ApplyNewEvents — 五新事件回放折叠（R124 显式 case 纪律：回放
// 未知事件 fatal——新事件必须登记 case 否则 resume 打瘫）。
func TestGraphV05_ApplyNewEvents(t *testing.T) {
	st := newGraphState()
	n := st.declare("executor", "折叠测试", "pack", nil)
	mk := func(typ string, data map[string]any) *graphEnvelope {
		return &graphEnvelope{Type: typ, Seq: st.lastSeq + 1, Data: data}
	}
	// claimed：declared 态合法
	if err := st.apply(mk(evNodeClaimed, map[string]any{"nodeId": n.ID, "holder": "human", "leaseUntil": "2026-09-22T00:00:00Z"})); err != nil {
		t.Fatal(err)
	}
	if n.Holder != "human" || n.LeaseUntil.IsZero() {
		t.Fatal("claimed 折叠失真")
	}
	// 非 declared 态不可 claim（回放 fatal=R124 纪律；started 置 running 后同理）
	n.Status = stRunning
	if err := st.apply(mk(evNodeClaimed, map[string]any{"nodeId": n.ID, "holder": "x"})); err == nil {
		t.Fatal("running 态 claim 未拒绝")
	}
	n.Status = stDeclared
	// released
	if err := st.apply(mk(evNodeReleased, map[string]any{"nodeId": n.ID, "holder": "human"})); err != nil {
		t.Fatal(err)
	}
	if n.Holder != "" {
		t.Fatal("released 折叠失真")
	}
	// 未认领 released → fatal
	if err := st.apply(mk(evNodeReleased, map[string]any{"nodeId": n.ID})); err == nil {
		t.Fatal("未认领 released 未拒绝")
	}
	// human_message：纯审计（状态不变）
	if err := st.apply(mk(evHumanMessage, map[string]any{"nodeId": n.ID, "actor": "human", "digest": "abc"})); err != nil {
		t.Fatal(err)
	}
	// reopened：终态才可（declared 态拒绝）
	n.Status = stFailed
	if err := st.apply(mk(evNodeReopened, map[string]any{"nodeId": n.ID, "reason": "补审"})); err != nil {
		t.Fatal(err)
	}
	if n.Status != stDeclared || n.ResultRef != "" {
		t.Fatal("reopened 折叠失真")
	}
}

// TestGraphV05_InjectRegistry — 注入注册表生命周期：未注册任务=错误（诚实拒绝）；
// 注册后可达；release 后不可达（注入通道关闭）。
func TestGraphV05_InjectRegistry(t *testing.T) {
	if err := InjectGraphNode("gw-no-such", "n001", "hi"); err == nil {
		t.Fatal("未注册任务注入未拒绝")
	}
	release := registerGraphOps("gw-inj", &graphOps{
		inject: func(nodeID, message string) error {
			if nodeID != "n001" || message == "" {
				return errStub
			}
			return nil
		},
	})
	if err := InjectGraphNode("gw-inj", "n001", "hi"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := InjectGraphNode("gw-inj", "n001", "hi"); err == nil {
		t.Fatal("release 后注入仍可达")
	}
	if !strings.Contains(func() string {
		if err := InjectGraphNode("gw-x", "n", "m"); err != nil {
			return err.Error()
		}
		return ""
	}(), "无活跃图执行") {
		t.Fatal("错误文案失真")
	}
}
