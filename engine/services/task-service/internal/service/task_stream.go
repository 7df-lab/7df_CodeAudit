// 任务快照订阅流（ADR-189）：task/进度/日志任一变更即推增量帧——GUI WebSocket
// 近实时化的上游真推送（此前网关只能定时轮询一元 RPC，proto 无流式接口）。
//
// 实现：taskWatchHub 维护每任务的订阅者通知通道集合；写路径在持有 s.mu 的前提下
// 发非阻塞信号（appendLogLocked/transitionLocked 等中心挂点），流处理器被唤醒后
// 按各自游标重读任务态与日志增量（proto.Equal 变化检测）。信号合并（1 缓冲）+
// 2s 安全兜底 tick（防未来新增写点漏挂钩导致静默停推）——唤醒即时性由信号保证，
// 最终一致性由兜底 tick 保证。
package service

import (
	"strconv"
	"sync"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// taskWatchSafetyTick — 流循环兜底唤醒周期（远慢于网关轮询节拍；仅一致性保险）。
const taskWatchSafetyTick = 2 * time.Second

// taskWatchHub — 每任务的通知 hub（信号即"有变化"，订阅者自行读增量）。
type taskWatchHub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]bool
}

func newTaskWatchHub() *taskWatchHub {
	return &taskWatchHub{subs: map[string]map[chan struct{}]bool{}}
}

// subscribe — 注册订阅者，返回通知通道与退订函数。
func (h *taskWatchHub) subscribe(taskID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1) // 1 缓冲：合并连续写，唤醒即重读全量增量，不丢
	h.mu.Lock()
	if h.subs[taskID] == nil {
		h.subs[taskID] = map[chan struct{}]bool{}
	}
	h.subs[taskID][ch] = true
	h.mu.Unlock()
	unsub := func() {
		h.mu.Lock()
		delete(h.subs[taskID], ch)
		h.mu.Unlock()
	}
	return ch, unsub
}

// notify — 非阻塞信号（写路径在 s.mu 写锁内调用；不阻塞主链路）。
func (h *taskWatchHub) notify(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[taskID] {
		select {
		case ch <- struct{}{}:
		default: // 已有挂起信号：订阅者醒来会读到全部增量
		}
	}
}

// notifyLocked — 语义同 notify，标注"调用方持 s.mu 写锁"（读名对齐既有 *Locked 惯例）。
func (s *TaskServiceImpl) notifyLocked(taskID string) {
	s.hub.notify(taskID)
}

// StreamTaskSnapshot — 订阅任务快照增量（ADR-189）。首帧必推（订阅位之后的当前
// 状态）；此后唤醒/兜底 tick 时变化检测，变化才推；终态帧 settled=true 后关流。
// 读 RPC（无幂等键要求，03 §2）。
func (s *TaskServiceImpl) StreamTaskSnapshot(req *pb.StreamTaskSnapshotRequest, stream pb.TaskService_StreamTaskSnapshotServer) error {
	taskID := req.GetTaskId()
	if taskID == "" {
		return status.Error(codes.InvalidArgument, "task_id is required")
	}
	notify, unsub := s.hub.subscribe(taskID)
	defer unsub()

	logsAfter := req.GetLogsAfter()
	after := int64(-1)
	if logsAfter != "" {
		if v, perr := strconv.ParseInt(logsAfter, 10, 64); perr == nil {
			after = v
		}
	}
	var lastTask *pb.ScanTask

	trySend := func() (bool, error) { // 返回 settled
		s.mu.RLock()
		task, ok := s.tasks[taskID]
		if !ok {
			s.mu.RUnlock()
			return false, status.Errorf(codes.NotFound, "task %s not found", taskID)
		}
		tClone := cloneLocked(task)
		logs := &pb.GetTaskLogsResponse{Logs: s.logsAfterLocked(taskID, after, 0)}
		s.mu.RUnlock()

		changed := lastTask == nil || !proto.Equal(tClone, lastTask) || len(logs.GetLogs()) > 0
		settled := s.sm.IsTerminal(tClone.GetStatus())
		if !changed {
			return settled, nil
		}
		if n := len(logs.GetLogs()); n > 0 {
			if id, perr := strconv.ParseInt(logs.Logs[n-1].GetLogId(), 10, 64); perr == nil {
				after = id
			}
			logsAfter = logs.Logs[n-1].GetLogId()
		}
		if err := stream.Send(&pb.TaskSnapshotDelta{
			Task:     tClone,
			Progress: progressOf(tClone),
			Logs:     logs,
			Settled:  settled,
		}); err != nil {
			return settled, err
		}
		lastTask = tClone
		return settled, nil
	}

	settled, err := trySend()
	if err != nil || settled {
		return err
	}
	safety := time.NewTicker(taskWatchSafetyTick)
	defer safety.Stop()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-notify:
		case <-safety.C:
		}
		if settled, err := trySend(); err != nil || settled {
			return err
		}
	}
}
