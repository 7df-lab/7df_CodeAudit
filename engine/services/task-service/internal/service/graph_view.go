package service

// graph_view.go — GetTaskGraph（切片5+6 暴露批 P1a，2026-09-20；设计=主仓库
// docs/designs/platform-graph-web-exposure.md §1）：读共享卷
// <interaction_dir>/<taskID>.graph.log（与 runtime ai_interaction_log 同源
// 派生，usage_collector 同款 env/cfg 口径），回放折叠为图执行视图投影。
// 权威源=graph.log（非内存态）——与 resume 同源，崩溃后仍可答（R116 撕裂容忍
// 同纪律：坏行丢弃+日志，不 fatal）。域中立 C4：只透传控制面词表。

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	codeauditcfg "github.com/codeaudit/go-config"
	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// interactionDir — runtime ai_interaction_log 落盘目录同源派生
// （cfg 显式键 > CODEAUDIT_INTERACTION_DIR；usage_collector 同款读取面）。
func interactionDir() string {
	cfg, err := codeauditcfg.Default()
	if err == nil {
		if dir, err := cfg.Str("runtime.sandbox.interaction_dir", "CODEAUDIT_INTERACTION_DIR"); err == nil && dir != "" {
			return dir
		}
	}
	return ""
}

// graphViewEnvelope — graph.log 单行信封（与 runtime graphEnvelope 同源契约
// {type,seq,time,data}；baton graphlog.go 同形消费）。
type graphViewEnvelope struct {
	Type string         `json:"type"`
	Seq  int            `json:"seq"`
	Time string         `json:"time"`
	Data map[string]any `json:"data"`
}

// foldGraphLog — graph.log 回放折叠（与 runtime graph_state.apply 同规则：
// declared 增/终态减/open 留 declared；started→running；坏行丢弃+计数——
// R116 读侧容忍纪律跨服务一致。R125 修复：回放尾 running 孤儿归回 declared
// （权威侧 ReplayGraphLog.recoverRunningOrphans 同规则——单写者假设下视图侧
// running 恒为崩溃孤儿，不归回则端点永显 running）；declared 不再预填 started_at
// （与权威侧一致：started 才有起点）。
func foldGraphLog(lines [][]byte) (*pb.TaskGraph, []string) {
	g := &pb.TaskGraph{}
	var warnings []string
	nodes := map[string]*pb.TaskGraphNode{}
	var order []string
	var lastTime string
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	for _, line := range lines {
		var env graphViewEnvelope
		if err := json.Unmarshal(line, &env); err != nil || env.Type == "" {
			warnings = append(warnings, "坏行丢弃")
			continue
		}
		lastTime = env.Time
		data := env.Data
		switch env.Type {
		case "graph.node_declared":
			id := str(data, "nodeId")
			if id == "" {
				continue
			}
			if ex := nodes[id]; ex != nil {
				// R120 同款容忍面：重复 declared 幂等忽略；语义冲突（已终态再
				// declared）视图侧不 fatal 但留痕（权威侧 fatal，读侧 R116 纪律）。
				if ex.Status != "declared" {
					warnings = append(warnings, "已终态再 declared（视图侧容忍留痕）: "+id)
				}
				continue
			}
			fromAny, _ := data["from"].([]any)
			from := make([]string, 0, len(fromAny))
			for _, f := range fromAny {
				if fs, ok := f.(string); ok {
					from = append(from, fs)
				}
			}
			nodes[id] = &pb.TaskGraphNode{
				Id: id, Type: str(data, "type"), Intent: str(data, "intent"),
				DeclaredBy: str(data, "declaredBy"), From: from,
				Status:      "declared",
				DeclaredSeq: int32(env.Seq), // v1：计划批次归属键（web 按 trigger seq 切分）
			}
			order = append(order, id)
		case "graph.node_started":
			if n := nodes[str(data, "nodeId")]; n != nil {
				n.Status = "running"
				n.StartedAt = env.Time
			}
		case "graph.node_result":
			n := nodes[str(data, "nodeId")]
			if n == nil {
				continue
			}
			outcome := str(data, "outcome")
			n.ResultRef = str(data, "resultRef")
			n.EndedAt = env.Time
			switch outcome {
			case "completed", "failed", "rejected":
				n.Status = outcome
			case "indeterminate":
				n.Attempts++
				n.Status = "declared" // 回 open 重派（D1）
			default:
				warnings = append(warnings, "outcome 非枚举")
			}
		case "graph.plan_triggered":
			g.PlanTriggers = append(g.PlanTriggers, &pb.TaskGraphPlanTrigger{
				Trigger: str(data, "trigger"), Detail: str(data, "detail"),
				At: env.Time, Seq: int32(env.Seq),
			})
		case "graph.node_claimed":
			if n := nodes[str(data, "nodeId")]; n != nil {
				n.Holder = str(data, "holder")
				n.LeaseUntil = str(data, "leaseUntil")
			}
		case "graph.node_released", "graph.node_lease_expired":
			if n := nodes[str(data, "nodeId")]; n != nil {
				n.Holder = ""
				n.LeaseUntil = ""
			}
		case "graph.node_reopened":
			// 视图侧容忍面（R125 纪律）：终态回 declared；已 declared 幂等忽略。
			if n := nodes[str(data, "nodeId")]; n != nil {
				st := n.Status
				if st == "completed" || st == "failed" || st == "rejected" {
					n.Status = "declared"
					n.ResultRef = ""
					n.EndedAt = ""
				}
			}
		case "graph.human_message":
			// 审计事件：无状态折叠（权威侧同口径）——Detail 侧栏经事件流展示。
			_ = str(data, "nodeId")
		case "graph.paused":
			g.Paused = true
			g.PauseReason = str(data, "reason")
		case "graph.converged":
			g.Converged = true
			g.ConvergeSummary = str(data, "summary")
			if v, ok := data["openRemaining"].(float64); ok {
				g.OpenRemaining = int32(v)
			}
		default:
			// 未知类型前向兼容忽略（baton 同口径；graph.node_seeded 属审计摘要
			// 事件，视图面无状态折叠——权威侧 apply 显式 case 见 R124）
		}
	}
	// R125：回放尾 running 孤儿归回 declared 重派（权威侧 recoverRunningOrphans
	// 同规则——视图若不归回，崩溃节点经端点永显 running 且 open 计数丢失）。
	for _, id := range order {
		n := nodes[id]
		if n.Status != "running" {
			continue
		}
		n.Status = "declared"
		n.Attempts++
		warnings = append(warnings, "running 孤儿归回 declared（重派）: "+id)
	}
	for _, id := range order {
		g.Nodes = append(g.Nodes, nodes[id])
		if nodes[id].Status == "declared" {
			g.OpenRemaining++
			g.OpenNodeIds = append(g.OpenNodeIds, id)
		}
	}
	sort.SliceStable(g.PlanTriggers, func(a, b int) bool { return g.PlanTriggers[a].Seq < g.PlanTriggers[b].Seq })
	g.UpdatedAt = lastTime
	return g, warnings
}

// validateTaskIDForGraphPath — 路径消毒：task_id 仅允许 [A-Za-z0-9._-] 且不得含
// ".."（R53 同款白名单；result-service 同名函数的本地副本——internal 包不可跨服务
// 导入。GetTaskGraph 以 task_id 拼共享卷文件名，内部 gRPC 面同样不得穿越）。
func validateTaskIDForGraphPath(taskID string) error {
	if len(taskID) > 128 {
		return status.Error(codes.InvalidArgument, "task_id too long")
	}
	for _, r := range taskID {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return status.Errorf(codes.InvalidArgument, "task_id contains illegal character %q", r)
		}
	}
	if strings.Contains(taskID, "..") {
		return status.Error(codes.InvalidArgument, "task_id must not contain '..'")
	}
	return nil
}

// GetTaskGraph — 图执行视图投影（非图任务/无事件=空 nodes 诚实空态；task_id
// 非法=InvalidArgument 而非空态——穿越面在读写侧同样关门）。
func (s *TaskServiceImpl) GetTaskGraph(ctx context.Context, req *pb.GetTaskGraphRequest) (*pb.TaskGraph, error) {
	g := &pb.TaskGraph{TaskId: req.GetTaskId()}
	if req.GetTaskId() == "" {
		return g, nil
	}
	// R53: task_id 进共享卷文件名——白名单消毒防 ../ 路径穿越（result-service
	// ExportFindings 同面同款，gw- 前缀+受限字符集）。
	if err := validateTaskIDForGraphPath(req.GetTaskId()); err != nil {
		return nil, err
	}
	dir := interactionDir()
	if dir == "" {
		return g, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, req.GetTaskId()+".graph.log"))
	if err != nil {
		return g, nil // 无档=非图任务（诚实空态）
	}
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if ln := bytes.TrimSpace(raw[start:i]); len(ln) > 0 {
				lines = append(lines, ln)
			}
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, bytes.TrimSpace(raw[start:]))
	}
	folded, warnings := foldGraphLog(lines)
	folded.TaskId = req.GetTaskId()
	for _, w := range warnings {
		log.Printf("[task %s] graph-view: %v", req.GetTaskId(), w)
	}
	return folded, nil
}
