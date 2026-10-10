package service

// graph_inject_rpc.go — v0.5 人机共编辑 RPC（graph-engine-v0.5-blackboard-hoc.md §4/
// §2.1）：InjectGraphNodeMessage=对沙箱内 Agent 追加特定消息（bridge 排队→回合边界
// 生效）；ReopenGraphNode=终态节点重开（活跃图拒绝——注册表探测，重开走 graph.log
// append+resume 既有机器）。

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/runtime-service/internal/sandbox"
)

// InjectGraphNodeMessage — 人消息注入（§4 b 档）。校验链：request_id 必填（R4，
// 无幂等缓存——注入非产出性操作，重复注入=人重复操作如实执行）；tasklog/图档
// 存在性=非图任务 404；活跃图注册表=注入通道可达性；节点状态校验在闭包内
// （仅 running）。审计=graph.human_message 事件（engine 侧 emit）。
func (s *RuntimeServiceImpl) InjectGraphNodeMessage(ctx context.Context, req *pb.InjectGraphNodeMessageRequest) (*pb.InjectGraphNodeMessageResponse, error) {
	if req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	taskID := req.GetTaskId()
	if taskID == "" || req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id 与 node_id 必填")
	}
	if req.GetMessage() == "" {
		return nil, status.Error(codes.InvalidArgument, "message 必填（空消息不注入）")
	}
	// 非图任务诚实拒绝（graph.log 不存在=该任务无图执行史）
	if _, err := os.Stat(filepath.Join(interactionDir(), taskID+".graph.log")); err != nil {
		return nil, status.Errorf(codes.NotFound, "任务 %s 无图执行档案——非图任务不可注入", taskID)
	}
	if err := sandbox.InjectGraphNode(taskID, req.GetNodeId(), req.GetMessage()); err != nil {
		// 活跃图不存在（已收束/沙箱回收）=failed_precondition（客户端可修正：
		// 任务已终态，注入通道关闭）
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	log.Printf("[runtime][%s] 人机注入受理：node=%s bytes=%d", taskID, req.GetNodeId(), len(req.GetMessage()))
	return &pb.InjectGraphNodeMessageResponse{Accepted: true, NodeStatus: "running"}, nil
}

// ReopenGraphNode — 终态节点重开（§2.1）。活跃图拒绝（注册表探测=运行中图的重开
// 会破坏波次循环，留 v1）；graph.log 追加 reopened 事件（resume 回放后节点回 open，
// B2 只派 open 既有机器自动重跑）。
func (s *RuntimeServiceImpl) ReopenGraphNode(ctx context.Context, req *pb.ReopenGraphNodeRequest) (*pb.ReopenGraphNodeResponse, error) {
	if req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	taskID, nodeID := req.GetTaskId(), req.GetNodeId()
	if taskID == "" || nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id 与 node_id 必填")
	}
	if sandbox.GraphInjectActive(taskID) {
		return nil, status.Error(codes.FailedPrecondition, "任务图执行中——运行中重开留 v1（先 cancel/等终态）")
	}
	logPath := filepath.Join(interactionDir(), taskID+".graph.log")
	lf, err := os.Open(logPath)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "任务 %s 无图执行档案", taskID)
	}
	line, rerr := sandbox.BuildGraphReopened(lf, nodeID, req.GetReason())
	lf.Close()
	if rerr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "重开拒绝: %v", rerr)
	}
	// 追加落盘（appendGraphFile 纪律同 writeGraph：全量校验+Sync）。
	// aiLogs.mu 直取：appendGraphFile 自带重试锁语义，此处锁外调用。
	s.aiLogs.mu.Lock()
	entry := s.aiLogs.logs[taskID]
	s.aiLogs.mu.Unlock()
	if entry == nil {
		return nil, status.Error(codes.Internal, "交互日志条目不可用——重开未落盘")
	}
	entry.appendGraphFile(logPath, line)
	log.Printf("[runtime][%s] 节点 %s 已重开（reason=%s）——resume 后 B2 只派 open 自动重跑", taskID, nodeID, req.GetReason())
	return &pb.ReopenGraphNodeResponse{Accepted: true, NodeStatus: "declared"}, nil
}

// ClaimGraphNode — human 认领（§2.1）：declared 节点占住（引擎调度跳过），
// TTL 租约到期由调度扫描回收（graph.node_lease_expired 留痕）。
func (s *RuntimeServiceImpl) ClaimGraphNode(ctx context.Context, req *pb.ClaimGraphNodeRequest) (*pb.ClaimGraphNodeResponse, error) {
	if req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	leaseUntil, err := sandbox.ClaimGraphNode(req.GetTaskId(), req.GetNodeId(), int(req.GetTtlS()))
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return &pb.ClaimGraphNodeResponse{Accepted: true, Holder: "human", LeaseUntil: leaseUntil}, nil
}

// ReleaseGraphNode — human 释放（holder 清空→调度恢复派发）。
func (s *RuntimeServiceImpl) ReleaseGraphNode(ctx context.Context, req *pb.ReleaseGraphNodeRequest) (*pb.ReleaseGraphNodeResponse, error) {
	if req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	if err := sandbox.ReleaseGraphNode(req.GetTaskId(), req.GetNodeId()); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return &pb.ReleaseGraphNodeResponse{Accepted: true, NodeStatus: "declared"}, nil
}
