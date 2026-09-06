// 任务快照订阅流回归（ADR-189）：订阅即时收到首帧（含订阅位之后的日志增量）/
// 写路径信号即时唤醒推增量（日志追加/阶段流转）/终态 settled 终帧后关流/游标
// 增量语义与 GetTaskLogs 一致（不重发已见条目）。
package service

import (
	"context"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
)

// fakeSnapshotStream — StreamTaskSnapshotServer 假实现（捕获推帧；ctx 可取消）。
type fakeSnapshotStream struct {
	pb.TaskService_StreamTaskSnapshotServer
	frames chan *pb.TaskSnapshotDelta
	ctx    context.Context
	cancel context.CancelFunc
}

func newFakeSnapshotStream() *fakeSnapshotStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeSnapshotStream{frames: make(chan *pb.TaskSnapshotDelta, 32), ctx: ctx, cancel: cancel}
}

func (f *fakeSnapshotStream) Send(d *pb.TaskSnapshotDelta) error {
	f.frames <- d
	return nil
}
func (f *fakeSnapshotStream) Context() context.Context { return f.ctx }

func waitDelta(t *testing.T, f *fakeSnapshotStream, timeout time.Duration) *pb.TaskSnapshotDelta {
	t.Helper()
	select {
	case d := <-f.frames:
		return d
	case <-time.After(timeout):
		t.Fatal("stream frame not delivered in time")
		return nil
	}
}

func expectNoDelta(t *testing.T, f *fakeSnapshotStream, quiet time.Duration) {
	t.Helper()
	select {
	case d := <-f.frames:
		t.Fatalf("unexpected frame: task=%s logs=%d settled=%v", d.GetTask().GetTaskId(), len(d.GetLogs().GetLogs()), d.GetSettled())
	case <-time.After(quiet):
	}
}

func TestStreamTaskSnapshot_FirstFrameThenPush(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	task := createTask(t, s, "stream-1", pb.ScanMode_SCAN_MODE_SAST_ONLY)
	// 订阅位之前的日志：先追加两条（直接经幂等 RPC）
	for i := 0; i < 2; i++ {
		if _, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
			Metadata: &pb.RequestMetadata{RequestId: task.GetTaskId() + "-pre-" + string(rune('a'+i))},
			TaskId:   task.GetTaskId(), Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO, Source: "test", Message: "pre log",
		}); err != nil {
			t.Fatalf("AppendTaskLog pre: %v", err)
		}
	}

	fs := newFakeSnapshotStream()
	errCh := make(chan error, 1)
	go func() { errCh <- s.StreamTaskSnapshot(&pb.StreamTaskSnapshotRequest{TaskId: task.GetTaskId()}, fs) }()

	// 首帧必推：当前任务态 + 订阅位之后的全部日志
	first := waitDelta(t, fs, 2*time.Second)
	if first.GetTask().GetTaskId() != task.GetTaskId() || len(first.GetLogs().GetLogs()) != 2 {
		t.Fatalf("first frame: task=%s logs=%d", first.GetTask().GetTaskId(), len(first.GetLogs().GetLogs()))
	}
	if first.GetProgress() == nil || first.GetProgress().GetTaskId() != task.GetTaskId() {
		t.Fatalf("first frame progress missing")
	}
	expectNoDelta(t, fs, 2200*time.Millisecond) // 无变化不推（覆盖 2s 兜底 tick 一轮）

	// 写路径信号即时唤醒：追加日志 → 增量帧恰含新条目（不重发已见）
	start := time.Now()
	if _, err := s.AppendTaskLog(ctx, &pb.AppendTaskLogRequest{
		Metadata: &pb.RequestMetadata{RequestId: task.GetTaskId() + "-post"},
		TaskId:   task.GetTaskId(), Level: pb.TaskLogLevel_TASK_LOG_LEVEL_INFO, Source: "test", Message: "post log",
	}); err != nil {
		t.Fatalf("AppendTaskLog post: %v", err)
	}
	second := waitDelta(t, fs, 2*time.Second)
	if wake := time.Since(start); wake > time.Second {
		t.Fatalf("notify wake too slow: %v", wake)
	}
	if len(second.GetLogs().GetLogs()) != 1 || second.GetLogs().GetLogs()[0].GetMessage() != "post log" {
		t.Fatalf("incremental frame must carry only the new entry: %d", len(second.GetLogs().GetLogs()))
	}
	fs.cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("StreamTaskSnapshot returned error: %v", err)
	}
}

func TestStreamTaskSnapshot_SettleOnTerminal(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	task := createTask(t, s, "stream-2", pb.ScanMode_SCAN_MODE_SAST_ONLY)

	fs := newFakeSnapshotStream()
	errCh := make(chan error, 1)
	go func() { errCh <- s.StreamTaskSnapshot(&pb.StreamTaskSnapshotRequest{TaskId: task.GetTaskId()}, fs) }()
	waitDelta(t, fs, 2*time.Second) // 首帧

	// 置 RUNNING（绕开编排协程——本测试聚焦订阅流，不测状态机）后走真实 CompleteTask
	s.mu.Lock()
	s.tasks[task.GetTaskId()].Status = pb.TaskStatus_TASK_STATUS_RUNNING
	s.mu.Unlock()
	if _, err := s.CompleteTask(ctx, &pb.CompleteTaskRequest{TaskId: task.GetTaskId()}); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	last := waitDelta(t, fs, 2*time.Second)
	if !last.GetSettled() || last.GetTask().GetStatus() != pb.TaskStatus_TASK_STATUS_COMPLETED {
		t.Fatalf("terminal frame must be settled+COMPLETED: settled=%v status=%s",
			last.GetSettled(), last.GetTask().GetStatus())
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("stream must close cleanly after settle: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not return after settled frame")
	}
}

func TestStreamTaskSnapshot_UnknownTask(t *testing.T) {
	s := newSvc(t)
	fs := newFakeSnapshotStream()
	err := s.StreamTaskSnapshot(&pb.StreamTaskSnapshotRequest{TaskId: "nope"}, fs)
	if err == nil {
		t.Fatal("unknown task must error")
	}
}
