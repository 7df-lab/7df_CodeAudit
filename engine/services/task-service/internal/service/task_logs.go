// 任务执行日志（ADR-167）：内存按任务环形缓存，AppendTaskLog 幂等追加（R4），
// GetTaskLogs 按 log_id 游标增量拉取（GUI 执行日志面板数据源）。
// 内存口径与任务存储一致（演示部署；重启清空，ScanTask 同为内存态）。
package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"log"
)

// logCapPerTask — 单任务日志条目上限（环形丢弃最旧；实现细节，ADR-167）。
const logCapPerTask = 500

// AppendTaskLog — 追加一条任务日志。幂等：同 request_id 回放原条目（03 §2 三态）。
func (s *TaskServiceImpl) AppendTaskLog(ctx context.Context, req *pb.AppendTaskLogRequest) (*pb.AppendTaskLogResponse, error) {
	if req.GetMetadata().GetRequestId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RequestMetadata.request_id is required (R4)")
	}
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	if req.GetMessage() == "" {
		return nil, status.Error(codes.InvalidArgument, "message is required")
	}

	if _, ok := s.taskByIDRW(req.GetTaskId()); !ok { // HA P1-c 读回落（他副本任务的追加面）
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.logIdem[req.GetMetadata().GetRequestId()]; ok {
		if e, ok := s.logByID(req.GetTaskId(), id); ok {
			return &pb.AppendTaskLogResponse{Entry: e}, nil // 同键回放
		}
		// R50: 幂等键在而原条目已被环形丢弃——回空壳回执（原 log_id），不再回落追加新条目
		return &pb.AppendTaskLogResponse{Entry: &pb.TaskLogEntry{LogId: id, TaskId: req.GetTaskId()}}, nil
	}
	// P2-4（R139）：跨副本/重启重放——本副本 logIdem 无此键时查 PG（request_id 落
	// PG+回放查接线；此前 insertLog 的 requestID 恒空串=唯一索引永不命中、
	// logByRequest 零调用，副本 B 重放同键会重复追加一条日志）。
	if s.pgStore != nil {
		if prior, lerr := s.pgStore.logByRequest(req.GetMetadata().GetRequestId()); lerr == nil && prior != nil {
			s.logIdem[req.GetMetadata().GetRequestId()] = prior.GetLogId()
			return &pb.AppendTaskLogResponse{Entry: prior}, nil
		}
	}
	entry := s.appendLogLocked(req.GetTaskId(), req.GetLevel(), req.GetSource(), req.GetMessage(), req.GetMetadata().GetRequestId())
	s.logIdem[req.GetMetadata().GetRequestId()] = entry.GetLogId()
	return &pb.AppendTaskLogResponse{Entry: entry}, nil
}

// appendLogLocked — 持写锁前提下的追加核心（状态机流转/内部事件复用；调用方持锁）。
// requestID：仅外部 AppendTaskLog 幂等面传实值；内部事件传 ""（无幂等键，
// PG ON CONFLICT DO NOTHING 对空串视为可重复）。
func (s *TaskServiceImpl) appendLogLocked(taskID string, level pb.TaskLogLevel, source, msg, requestID string) *pb.TaskLogEntry {
	s.logSeq++
	entry := &pb.TaskLogEntry{
		LogId:   strconv.FormatInt(s.logSeq, 10),
		TaskId:  taskID,
		TsMs:    time.Now().UnixMilli(),
		Level:   level,
		Source:  source,
		Message: msg,
	}
	logs := s.logs[taskID]
	logs = append(logs, entry)
	if len(logs) > logCapPerTask { // 环形丢弃最旧（log_id 单调性不受影响）
		logs = logs[len(logs)-logCapPerTask:]
	}
	s.logs[taskID] = logs
	s.hub.notify(taskID)  // ADR-189：订阅流即时唤醒（非阻塞信号）
	if s.pgStore != nil { // HA P1-c 日志双写（best-effort）
		if err := s.pgStore.insertLog(entry, requestID); err != nil {
			log.Printf("[task %s] 日志 PG 双写失败（本副本可见）: %v", taskID, err)
		}
	}
	return entry
}

// GetTaskLogs — 按 after_log_id 游标增量返回（log_id 升序）；limit<=0 用服务端默认。
func (s *TaskServiceImpl) GetTaskLogs(ctx context.Context, req *pb.GetTaskLogsRequest) (*pb.GetTaskLogsResponse, error) {
	if req.GetTaskId() == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id is required")
	}
	after := int64(-1)
	if req.GetAfterLogId() != "" {
		v, err := strconv.ParseInt(req.GetAfterLogId(), 10, 64)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("after_log_id: %v", err))
		}
		after = v
	}
	if _, ok := s.taskByIDRW(req.GetTaskId()); !ok { // HA P1-c 读回落
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.GetTaskId())
	}
	s.mu.RLock()
	_, buffered := s.logs[req.GetTaskId()]
	s.mu.RUnlock()
	if !buffered && s.pgStore != nil { // 内存无环形缓存（他副本任务/重启后）——PG 尾查回落
		logs, err := s.pgStore.tailLogs(req.GetTaskId(), after, req.GetLimit())
		if err != nil {
			log.Printf("[task %s] PG 日志回落失败: %v", req.GetTaskId(), err)
		} else if logs != nil {
			return &pb.GetTaskLogsResponse{Logs: logs}, nil
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &pb.GetTaskLogsResponse{Logs: s.logsAfterLocked(req.GetTaskId(), after, int(req.GetLimit()))}, nil
}

// logsAfterLocked — 游标增量读核心（ADR-189 起 StreamTaskSnapshot 与本 RPC 共用）。
// 调用方持 s.mu 读锁；limit<=0 用服务端默认。
func (s *TaskServiceImpl) logsAfterLocked(taskID string, after int64, limit int) []*pb.TaskLogEntry {
	if limit <= 0 {
		limit = logCapPerTask
	}
	logs := s.logs[taskID]
	out := make([]*pb.TaskLogEntry, 0, len(logs))
	for _, e := range logs {
		id, _ := strconv.ParseInt(e.GetLogId(), 10, 64)
		if id <= after {
			continue
		}
		out = append(out, e)
	}
	if len(out) > limit { // 只丢最旧的超出部分（GUI 轮询游标语义）
		out = out[len(out)-limit:]
	}
	return out
}

// logByID — 任务内按 log_id 查找（幂等回放用）。
func (s *TaskServiceImpl) logByID(taskID, logID string) (*pb.TaskLogEntry, bool) {
	for _, e := range s.logs[taskID] {
		if e.GetLogId() == logID {
			return e, true
		}
	}
	return nil, false
}

// emitTaskLog — 服务内部便捷上报（吞错误：日志通道故障不得影响流水线主链路，
// 失败留服务端 stdout——与 log.Printf 同为可观测通道）。
func (s *TaskServiceImpl) emitTaskLog(taskID string, level pb.TaskLogLevel, source, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: fmt.Sprintf("%s-log-%d", taskID, time.Now().UnixNano())},
		TaskId:   taskID, Level: level, Source: source, Message: msg,
	})
	if err != nil {
		fmt.Printf("[task][%s] emitTaskLog(%s) failed: %v\n", taskID, source, err)
	}
}
