package service

// R116：图任务冻结定格转 PAUSED——此前
// runGraphTask 挂起分支 return nil 被终态迁移当成功→标 COMPLETED（与"按取消/
// 换手链走"语义相悖，用户看到一个已完成但零图痕迹的任务）。

import (
	"context"
	"testing"
	"time"

	pb "github.com/codeaudit/proto-gen"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/codeaudit/services/task-service/internal/orchestrator"
)

type fakePausedOrch struct{ called int }

func (f *fakePausedOrch) Execute(ctx context.Context, r orchestrator.RunRequest) (map[string]interface{}, error) {
	f.called++
	return map[string]interface{}{
		"graph_nodes_total":     3,
		"graph_nodes_completed": 1,
		"graph_paused":          true,
	}, orchestrator.ErrGraphPaused
}

// TestRunOrchestration_GraphPausedTransitionsToPaused：ErrGraphPaused→PAUSED
// （非 COMPLETED、无重试）；graph summary 键入 task.Config（暴露面 v0）；graph
// 阶段留 RUNNING（冻结≠完成）。
func TestRunOrchestration_GraphPausedTransitionsToPaused(t *testing.T) {
	s := newSvc(t)
	task := createTask(t, s, "req-graph-paused", pb.ScanMode_SCAN_MODE_SAST_ONLY)
	s.mu.Lock()
	cur0, ok := s.tasks[task.GetTaskId()] // 活实体（createTask 返回的是 R48 克隆）
	if !ok {
		s.mu.Unlock()
		t.Fatal("任务未入映射表")
	}
	if terr := s.transitionLocked(cur0, pb.TaskStatus_TASK_STATUS_RUNNING, "start-test"); terr != nil {
		s.mu.Unlock()
		t.Fatalf("transition RUNNING: %v", terr)
	}
	st := findOrInsertStageLocked(cur0, "graph")
	if st.Type != stageIDTypeHint["graph"] {
		s.mu.Unlock()
		t.Fatalf("graph 阶段类型提示缺失: %v", st.Type)
	}
	st.Status = pb.StageStatus_STAGE_STATUS_RUNNING
	st.StartedAt = timestamppb.New(time.Now())
	s.mu.Unlock()

	fake := &fakePausedOrch{}
	s.orch = fake
	entry := &cancelEntry{cancel: func() {}}
	r := orchestrator.RunRequest{TaskID: task.GetTaskId(), RequestID: "req-graph-paused"}
	s.runOrchestration(context.Background(), entry, fake, r, func(eventKey, msg string) {})

	s.mu.RLock()
	defer s.mu.RUnlock()
	cur := s.tasks[task.GetTaskId()]
	if cur.GetStatus() != pb.TaskStatus_TASK_STATUS_PAUSED {
		t.Fatalf("图挂起任务应转 PAUSED, got %v（COMPLETED=R116 回归）", cur.GetStatus())
	}
	if cur.GetRetryCount() != 0 {
		t.Fatalf("挂起不应触发重试: retry=%d", cur.GetRetryCount())
	}
	if cur.GetConfig()["graph_nodes_total"] != "3" || cur.GetConfig()["graph_paused"] != "true" {
		t.Fatalf("graph summary 键未入 config: %v", cur.GetConfig())
	}
	for _, stg := range cur.GetStages() {
		if stg.GetStageId() == "graph" && stg.GetStatus() == pb.StageStatus_STAGE_STATUS_COMPLETED {
			t.Fatal("graph 阶段冻结期不应被盖章 COMPLETED")
		}
	}
	if fake.called != 1 {
		t.Fatalf("挂起后不应重入编排: called=%d", fake.called)
	}
}

// TestStageEventStageID_GraphMapped：graph 事件键映射（此前 default 丢弃）。
func TestStageEventStageID_GraphMapped(t *testing.T) {
	if got := stageEventStageID("graph"); got != "graph" {
		t.Fatalf("stageEventStageID(graph)=%q, want graph", got)
	}
}
