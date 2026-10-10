package service

// 图执行 RPC 接线测试（切片 5+6 TP18-T4，ADR-249）：
//   - 请求校验面（R4 幂等键/task_id）——无外部依赖，门禁内恒跑；
//   - 真实沙箱图 e2e（env 门控，默认跳过——DoD §8 验收跑：串行图三类节点+双流
//     投影回放一致+resume 只重跑 open）。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §8。
//
// DoD 验收口径（evidence 注记）：
//   - DoD 1 三类节点"各≥1"：executor=种子（渗透 pack 面）、planner=新态势触发
//     （真模型）；verifier 由 goal 引导 planner 扩图，真模型输出不可控——若本
//     e2e 未出现 verifier 型节点，该形态由 mock 面锁定（TestRunGraph_SerialLifecycle
//     等已含 verifier 分派/回收路径），如实注记不硬凑。
//   - DoD 3（封顶留痕）/DoD 4（rejected）状态机面由单测锁定
//     （TestRunGraph_MaxNodesCap/TestRunGraph_RejectedPath），真沙箱重复注入
//     不增加证明力（同一状态机同一代码路径）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
)

func TestRunGraphTask_RequestValidation(t *testing.T) {
	s := NewRuntimeService()
	// R4: 幂等键缺失
	if _, err := s.RunGraphTask(context.Background(), &pb.RunGraphTaskRequest{TaskId: "t1"}); err == nil ||
		!strings.Contains(err.Error(), "request_id") {
		t.Fatalf("missing metadata must reject, got %v", err)
	}
	// task_id 缺失（metadata 齐）
	if _, err := s.RunGraphTask(context.Background(), &pb.RunGraphTaskRequest{
		Metadata: &pb.RequestMetadata{RequestId: "rg-1"}}); err == nil ||
		!strings.Contains(err.Error(), "task_id") {
		t.Fatalf("missing task_id must reject, got %v", err)
	}
}

// pentestSpec — 渗透 pack 缺省面（configs/packs/pentest.yaml §5 的 proto 形状）。
func pentestSpec(goal string) *pb.GraphSpec {
	return &pb.GraphSpec{
		SeedIntents: []string{
			"回答一个词：本节点是攻击面侦察的第 1 步（无真实目标，仅链路验证）",
			"回答一个词：本节点是攻击面侦察的第 2 步（无真实目标，仅链路验证）",
			"回答一个词：本节点是攻击面侦察的第 3 步（无真实目标，仅链路验证）",
		},
		Constraints: &pb.GraphConstraints{MaxNodes: 12, AllowedTypes: []string{"planner", "executor", "verifier"},
			MaxPlannerReplans: 3, MaxAttemptsPerNode: 2},
		Timeouts: &pb.GraphTimeouts{PlannerS: 120, ExecutorS: 180, VerifierS: 120},
	}
}

// TestLiveGraphRealSandbox — DoD 1：真沙箱串行图 e2e（env 门控）。
// CODEAUDIT_LIVE_GRAPH=1：跑渗透面种子图→收敛→graph.log 事件族+回放重建一致断言。
// goal 显式引导"复核裁决后收敛"——提高 planner 扩 verifier 的概率（不可控面如实注记）。
func TestLiveGraphRealSandbox(t *testing.T) {
	if os.Getenv("CODEAUDIT_LIVE_GRAPH") != "1" {
		t.Skip("live-only: CODEAUDIT_LIVE_GRAPH=1（真实沙箱图执行，DoD 验收归档用）")
	}
	s := NewRuntimeService()
	taskID := envOr("CODEAUDIT_LIVE_GRAPH_TASK", "live-graph-dod1")
	resp, err := s.RunGraphTask(context.Background(), &pb.RunGraphTaskRequest{
		Metadata: &pb.RequestMetadata{RequestId: "rg-live-" + taskID},
		TaskId:   taskID,
		Goal:     "链路验证任务：依次完成三步侦察回答，随后由 planner 复核全部产出（必要时派 verifier 节点裁决）后收敛；无真实目标，禁止外联",
		Graph:    pentestSpec("链路验证"),
	})
	if err != nil {
		t.Fatalf("live graph run: %v", err)
	}
	if !resp.GetConverged() {
		t.Fatalf("graph must converge, got %+v", resp)
	}
	if resp.GetNodesCompleted() == 0 || resp.GetGraphLogPath() == "" {
		t.Fatalf("completed nodes=%d log=%q", resp.GetNodesCompleted(), resp.GetGraphLogPath())
	}
	// DoD 1 回放重建一致：graph.log 重放 counts 与响应一致（节点数/类型/结果）。
	raw, rerr := os.ReadFile(resp.GetGraphLogPath())
	if rerr != nil {
		t.Fatalf("read graph.log: %v", rerr)
	}
	var total, completed int
	types := map[string]int{}
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if ln == "" {
			continue
		}
		var env struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(ln), &env); err != nil {
			t.Fatalf("graph.log 非法行: %v", err)
		}
		switch env.Type {
		case "graph.node_declared":
			total++
			types[env.Data["type"].(string)]++
		case "graph.node_result":
			if env.Data["outcome"] == "completed" {
				completed++
			}
		}
	}
	if total != int(resp.GetNodesTotal()) || completed != int(resp.GetNodesCompleted()) {
		t.Fatalf("replay mismatch: log total/completed=%d/%d resp=%d/%d",
			total, completed, resp.GetNodesTotal(), resp.GetNodesCompleted())
	}
	t.Logf("DoD1 图形状: nodes=%d types=%v（verifier 由 planner 扩图，未出现则由 mock 面锁定）", total, types)
}

// TestLiveGraphResumeAfterKill — DoD 2：崩溃恢复（env 门控）。两阶段：
// 阶段 1 用 20s ctx 中途掐死（等价 kill runtime——graph.log 已持久）；
// 阶段 2 resume 完整跑（回放重建 open 集）→ 断言已完成节点零重复 node_started（B2）。
func TestLiveGraphResumeAfterKill(t *testing.T) {
	if os.Getenv("CODEAUDIT_LIVE_GRAPH") != "1" {
		t.Skip("live-only: CODEAUDIT_LIVE_GRAPH=1（DoD 2 验收归档用）")
	}
	taskID := envOr("CODEAUDIT_LIVE_GRAPH_TASK2", "live-graph-dod2")
	s := NewRuntimeService()
	base := &pb.RunGraphTaskRequest{
		Metadata: &pb.RequestMetadata{RequestId: "rg-live-" + taskID},
		TaskId:   taskID,
		Goal:     "链路验证：三步侦察回答后由 planner 复核收敛；无真实目标，禁止外联",
		Graph:    pentestSpec("链路验证"),
	}
	// 阶段 1：短 ctx 掐死（跑完首个 executor 概率高——180s executor 超时 > 20s 掐点，
	// 挂起/取消返回非 converged 即预期）
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	_, _ = s.RunGraphTask(ctx, base)
	cancel()
	// 阶段 2：resume（同 taskID→graph.log 回放）
	resp, err := s.RunGraphTask(context.Background(), base)
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if !resp.GetConverged() {
		t.Fatalf("resumed graph must converge, got %+v", resp)
	}
	// B2 硬验收：graph.log 内已完成节点不得再次 node_started（零重付）
	raw, rerr := os.ReadFile(resp.GetGraphLogPath())
	if rerr != nil {
		t.Fatalf("read graph.log: %v", rerr)
	}
	started := map[string]int{}
	completedBefore := map[string]bool{}
	// 按 seq 序扫：node_started 计数；completed 的节点其后续 started>1 即违例
	var seqEvents []struct {
		Type string `json:"type"`
		Data struct {
			NodeID  string `json:"nodeId"`
			Outcome string `json:"outcome"`
		} `json:"data"`
	}
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if ln == "" {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				NodeID  string `json:"nodeId"`
				Outcome string `json:"outcome"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("graph.log 非法行: %v", err)
		}
		seqEvents = append(seqEvents, ev)
		if ev.Type == "graph.node_started" {
			if completedBefore[ev.Data.NodeID] {
				t.Fatalf("B2 violation: completed node %s re-dispatched on resume", ev.Data.NodeID)
			}
			started[ev.Data.NodeID]++
		}
		if ev.Type == "graph.node_result" && ev.Data.Outcome == "completed" {
			completedBefore[ev.Data.NodeID] = true
		}
	}
	t.Logf("DoD2 resume: nodes=%d started-distinct=%d zero-repay ✓", resp.GetNodesTotal(), len(started))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestLiveGraphBatonProjection — DoD 1 后半（env 门控）：.graph.log+.sse.log 双流
// 经 baton project -graph 投影 → events.jsonl 含 graph.* 与 spine open 折叠。
func TestLiveGraphBatonProjection(t *testing.T) {
	if os.Getenv("CODEAUDIT_LIVE_GRAPH") != "1" {
		t.Skip("live-only: CODEAUDIT_LIVE_GRAPH=1")
	}
	batonBin := os.Getenv("BATON_BIN")
	if batonBin == "" {
		t.Skip("BATON_BIN=<baton 二进制> 未设置（主仓库 baton 仓 go build 产物）")
	}
	graphLog := os.Getenv("CODEAUDIT_LIVE_GRAPH_LOG")
	sseLog := os.Getenv("CODEAUDIT_LIVE_SSE_LOG")
	if graphLog == "" || sseLog == "" {
		t.Skip("CODEAUDIT_LIVE_GRAPH_LOG/CODEAUDIT_LIVE_SSE_LOG 未设置（DoD1 跑完的产物路径）")
	}
	out := t.TempDir()
	cmd := exec.Command(batonBin, "project", "-sse", sseLog, "-graph", graphLog,
		"-out", out, "-task", "T-live", "-goal", "live 图任务", "-agent", "dsh")
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("baton project: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(filepath.Join(out, ".tasklog", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sawDeclared, sawConverged := false, false
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.Contains(ln, `"graph.node_declared"`) {
			sawDeclared = true
		}
		if strings.Contains(ln, `"graph.converged"`) {
			sawConverged = true
		}
	}
	if !sawDeclared || !sawConverged {
		t.Fatalf("events.jsonl 缺 graph.* 事件（declared=%v converged=%v）", sawDeclared, sawConverged)
	}
	fmt.Println("baton out:", out)
}
