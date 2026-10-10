// 沙箱图执行接线（切片 5+6 TP18-T4，ADR-249）：RunGraphTask RPC → GraphRunner。
// 依据: 主仓库 docs/designs/platform-slice5-6-graph-engine.md §1（task-service 不动、
// runtime 走图执行路径）+§4（冻结/换手/resume）；findings 落库=batchCreateFindingsToResult
// 既有通道（4b 同构，零新提交工具——审计 L1 裁决）。
package service

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// runGraphViaSandbox — 图任务沙箱执行装配：cfg（workspace/network 贯穿同既有路径）
// → aiLog 接线（.sse.log/.graph.log 双落盘）→ resume 回放（B2）→ RunGraph → findings
// 直写 result-service（幂等键随请求）→ 响应只报图自身状态（C4 域中立）。
func runGraphViaSandbox(ctx context.Context, req *pb.RunGraphTaskRequest) (*pb.RunGraphTaskResponse, error) {
	taskID := req.GetTaskId()
	goal := req.GetGoal() // spec 与后续消费共用单读
	cfg, err := sandboxCfg()
	if err != nil {
		return nil, err
	}
	applyWorkspaceOverride(cfg, req.GetTaskOrigin()) // ADR-247/248: per-team workspace+网络分型贯穿
	// TP20 缺损批 B11：图路径任务级 agent 接线——此前不消费（a4a271c
	// 注记），PAUSED 图任务 SetTaskAgent 换笔静默不生效。空=no-op（部署缺省，
	// 既有图任务零变化）；非法 id=诚实失败（同 flat 主回合口径）。
	if err := sandbox.SelectAgent(cfg, req.GetAgent()); err != nil {
		return nil, err
	}
	aiEntry := wireAILog(cfg, taskID)
	r := sandbox.NewManagerRunner(*cfg)
	if aiEntry == nil {
		return nil, sandbox.ErrDisabled
	}

	spec := sandbox.GraphTaskSpec{
		TaskID:       taskID,
		Goal:         goal,
		WorkspaceDir: req.GetProjectPath(),
		OnGraphEvent: aiEntry.writeGraph, // ADR-249：graph.log 唯一持久真相（A1）
	}
	// R122 graph-reseed：节点重派种子切片获取器（失败史存在时派发面调用；
	// BATON_BIN/tasklog 同源派生复用 baton_hook——降级纪律 ⚑2 同钩子）。
	spec.SeedBrief = seedBriefFn(taskID)
	if g := req.GetGraph(); g != nil {
		spec.SeedIntents = g.GetSeedIntents()
		spec.MaxParallel = int(g.GetMaxParallel()) // v0.5 资源护栏（0=动态派发）
		if c := g.GetConstraints(); c != nil {
			spec.MaxNodes = int(c.GetMaxNodes())
			spec.MaxPlannerReplans = int(c.GetMaxPlannerReplans())
			spec.MaxAttemptsPerNode = int(c.GetMaxAttemptsPerNode())
			for _, t := range c.GetAllowedTypes() {
				if spec.AllowedTypes == nil {
					spec.AllowedTypes = map[string]bool{}
				}
				spec.AllowedTypes[t] = true
			}
		}
		if t := g.GetTimeouts(); t != nil {
			spec.PlannerTimeout = time.Duration(t.GetPlannerS()) * time.Second
			spec.ExecutorTimeout = time.Duration(t.GetExecutorS()) * time.Second
			spec.VerifierTimeout = time.Duration(t.GetVerifierS()) * time.Second
		}
	}
	// resume：既有 graph.log 回放重建 open 集（B2 只派 open——已完成节点零重付）。
	if f := aiEntry.graphLogReader(); f != nil {
		defer f.Close()
		spec.ReplayLog = f
		log.Printf("[runtime][%s] graph resume：回放既有 graph.log（B2）", taskID)
	}

	// 冻结回调（B4）：挂起点 baton 重投影（含 -graph 双输入）+checkpoint 基线锚。
	if goal == "" {
		goal = "图任务" // baton -goal 摘要缺省（ADR-237 非空契约）
	}
	releaseFreeze := registerBatonFreeze(batonPen{TaskID: taskID, Entry: aiEntry, Origin: req.GetTaskOrigin(), Agent: req.GetAgent()}, assignmentSummary(goal), req.GetProjectPath()) // B11：图路径 pen 随任务级 agent（a4a271c 缺口闭合）
	defer releaseFreeze()
	defer aiEntry.finish()
	defer batonProjectTurn(batonPen{TaskID: taskID, Entry: aiEntry, Origin: req.GetTaskOrigin(), Agent: req.GetAgent()}, goal) // R1 投影（defer LIFO：先于 finish；B11 pen 随任务级 agent）

	res, gerr := r.RunGraph(ctx, spec)
	if gerr != nil {
		return nil, gerr
	}

	// findings 落库（09 §2；R4 幂等键随请求）：executor 节点经既有 submit_findings
	// 通道提交的发现（渗透报告=结果侧视图过滤，≥LOW 呈现漏洞/INFO 呈现侦察——L1）。
	resp := &pb.RunGraphTaskResponse{
		Converged:      res.Converged,
		Paused:         res.Paused,
		NodesTotal:     int32(res.Total),
		NodesCompleted: int32(res.Completed),
		NodesFailed:    int32(res.Failed),
		NodesRejected:  int32(res.Rejected),
		NodesOpen:      int32(res.Open),
		GraphLogPath:   aiEntry.graphPath,
	}
	if res.Paused {
		return resp, nil // 冻结挂起：findings 不落库（图未定格，换手后 resume 续跑重收）
	}
	// NOSTORE=live 验收旋钮：落库链路=BatchCreateFindingsToResult 既有通道
	//（RunAIAnalysis 同构，非本切片新面），单服务 live 环境无 result-service。
	if len(res.Findings) > 0 && os.Getenv("CODEAUDIT_LIVE_GRAPH_NOSTORE") != "1" {
		unified := mapSandboxFindings(taskID, req.GetProjectPath(), res.Findings)
		if addr := cfgAddr("result", "CODEAUDIT_RESULT_ADDR"); addr != "" {
			if _, serr := batchCreateFindingsToResult(addr, taskID,
				req.GetMetadata().GetRequestId()+"-store", unified); serr != nil {
				return nil, fmt.Errorf("persist %d graph findings to result-service: %w", len(unified), serr)
			}
			log.Printf("[runtime][%s] graph findings=%d persisted", taskID, len(unified))
		}
	}
	return resp, nil
}
