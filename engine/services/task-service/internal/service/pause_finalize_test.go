package service

// S2 发现③：pause 落在编排收束
// 之后、终态盖章之前——原 runOrchestration 的"非 RUNNING 即放弃"早退把 PAUSED 与
// 被取消同吞，编排成果丢弃、协程退出，resume 后无人重推 finalize=任务永 RUNNING
// （超时对账因日志 mtime 活跃不触发，需手动 cancel 兜底）。修复=编排协程驻留等
// resume 唤醒后补推收尾。

import (
	"context"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"

	"github.com/codeaudit/services/task-service/internal/orchestrator"
)

// pauseBeforeFinalizeOrch — Execute 成功收束前把任务转 PAUSED（模拟 pause 落点：
// AI 回合已全部跑完、编排 Execute 即将返回——与 gw-ced673c9 场景同时序）。
type pauseBeforeFinalizeOrch struct {
	svc    *TaskServiceImpl
	taskID string
	called int
}

func (f *pauseBeforeFinalizeOrch) Execute(ctx context.Context, r orchestrator.RunRequest) (map[string]interface{}, error) {
	f.called++
	if _, err := f.svc.PauseTask(ctx, &pb.PauseTaskRequest{TaskId: f.taskID}); err != nil {
		return nil, err
	}
	return map[string]interface{}{"graph_nodes_total": 7}, nil // 编排成功（成果不得丢弃；白名单键）
}

func TestRunOrchestration_PausedBeforeFinalize_ResumeCompletes(t *testing.T) {
	s := newSvc(t)
	task := createTask(t, s, "req-pf-1", pb.ScanMode_SCAN_MODE_SAST_ONLY)
	s.mu.Lock()
	cur0, ok := s.tasks[task.GetTaskId()]
	if !ok {
		s.mu.Unlock()
		t.Fatal("任务未入映射表")
	}
	if terr := s.transitionLocked(cur0, pb.TaskStatus_TASK_STATUS_RUNNING, "start-test"); terr != nil {
		s.mu.Unlock()
		t.Fatalf("transition RUNNING: %v", terr)
	}
	s.mu.Unlock()

	fake := &pauseBeforeFinalizeOrch{svc: s, taskID: task.GetTaskId()}
	s.orch = fake
	entry := &cancelEntry{cancel: func() {}}
	r := orchestrator.RunRequest{TaskID: task.GetTaskId(), RequestID: "req-pf-1"}
	done := make(chan struct{})
	go func() {
		s.runOrchestration(context.Background(), entry, fake, r, func(eventKey, msg string) {})
		close(done)
	}()

	// 驻留生效判定：resume 信号已注册（编排协程活着、在等唤醒——修复前此刻协程
	// 已退出，done 立即关闭）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		_, waiting := s.resumes[task.GetTaskId()]
		s.mu.RUnlock()
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatal("编排协程在 PAUSED 收尾窗口即退出（驻留缺失=任务将永 RUNNING）")
		case <-time.After(10 * time.Millisecond):
		}
	}
	s.mu.RLock()
	_, waiting := s.resumes[task.GetTaskId()]
	s.mu.RUnlock()
	if !waiting {
		t.Fatal("驻留等待未注册")
	}
	if cur := s.tasks[task.GetTaskId()]; cur.GetStatus() != pb.TaskStatus_TASK_STATUS_PAUSED {
		t.Fatalf("窗口期任务应 PAUSED: %v", cur.GetStatus())
	}

	// resume → 唤醒 → 补推收尾（COMPLETED + summary 入 config）
	if _, err := s.ResumeTask(context.Background(), &pb.ResumeTaskRequest{TaskId: task.GetTaskId()}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("resume 后编排协程未完成收尾")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur := s.tasks[task.GetTaskId()]
	if cur.GetStatus() != pb.TaskStatus_TASK_STATUS_COMPLETED {
		t.Fatalf("resume 后应补推 COMPLETED（修复前=永 RUNNING）: %v", cur.GetStatus())
	}
	if cur.GetConfig()["graph_nodes_total"] != "7" {
		t.Fatalf("编排成果未随补推落 config（summary 丢弃）: %v", cur.GetConfig())
	}
	if fake.called != 1 {
		t.Fatalf("补推不应重入编排: called=%d", fake.called)
	}
}
