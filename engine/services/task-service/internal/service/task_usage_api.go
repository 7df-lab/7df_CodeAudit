// Package service — GetTaskUsage 计量查询面（ADR-247 切片3后半）。
// task_usage 表按 task 聚合：totals+分模型明细。tenant/user 取 envelope 打标
// （TaskOrigin 贯通成果；缺失=空串不虚构）。total 口径=计费输入（input+
// cache_read+cache_write）+output——reasoning 通常已含于 output，不重复计入；
// 表中 total_tokens 列保留上游原始观测（D2 忠实存档），API 聚合一律自算。
package service

import (
	"context"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetTaskUsage — per-task usage 聚合查询。无 PG 档返回空聚合（任务存在时；
// 诚实降级同 R-31 内存档语义）；任务不存在 NotFound。
func (s *TaskServiceImpl) GetTaskUsage(ctx context.Context, req *pb.GetTaskUsageRequest) (*pb.TaskUsageSummary, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	s.mu.RLock()
	_, ok := s.tasks[req.GetTaskId()]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	sum := &pb.TaskUsageSummary{TaskId: req.GetTaskId(), Totals: &pb.TaskModelUsage{}}
	if s.pgStore == nil {
		return sum, nil // 内存档：无归集落点，空聚合即真实状态
	}
	rows, err := s.pgStore.queryUsageRows(req.GetTaskId())
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "usage store: %v", err)
	}
	byModel := map[string]*pb.TaskModelUsage{}
	var order []string
	for _, r := range rows {
		sum.Tenant, sum.User = r.Tenant, r.User // envelope 打标（行间一致；末行兜底）
		m, ok := byModel[r.Model]
		if !ok {
			m = &pb.TaskModelUsage{Model: r.Model}
			byModel[r.Model] = m
			order = append(order, r.Model)
		}
		acc(m, r)
		acc(sum.Totals, r)
	}
	for _, k := range order { // 分组序稳定（map 遍历乱序，聚合面不引入非确定性）
		sum.Models = append(sum.Models, byModel[k])
	}
	return sum, nil
}

// acc — 单事件累加进聚合桶；billing 合计=计费输入（in+cacheR+cacheW）+out。
func acc(m *pb.TaskModelUsage, r usageRow) {
	m.Calls++
	m.InputTokens += r.In
	m.OutputTokens += r.Out
	m.CacheReadTokens += r.CacheR
	m.CacheWriteTokens += r.CacheW
	m.ReasoningTokens += r.Reason
	m.TotalTokens += r.In + r.Out + r.CacheR + r.CacheW
}
