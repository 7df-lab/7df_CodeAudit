package service

// 图任务阶段模板裁剪测试：execution=graph 的任务
// 时间线只含 graph+report——载体模式 B 的"AI 推理"占位阶段在图路径无实际工作，
// PENDING 到收尾被 finalize 盖章成 1 秒假阶段（生产 gw-5ec1376 实证误导）。

import (
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

// TestRegisterStages_GraphTemplateClip — 图任务模板：graph→report 两阶段，
// 无"AI 推理"/"代码分析"占位；Metadata 就地初始化（ADR-212① 锚点同款）。
func TestRegisterStages_GraphTemplateClip(t *testing.T) {
	s := newSvc(t)
	s.mu.Lock()
	task := &pb.ScanTask{TaskId: "t-graph-tpl", Status: pb.TaskStatus_TASK_STATUS_CREATED,
		ScanMode: pb.ScanMode_SCAN_MODE_AI_ONLY, // 载体模式（图任务恒 AI_ONLY）
		Config:   map[string]string{"execution": "graph", "pack": "audit-graph"}}
	s.registerStagesLocked(task)
	s.mu.Unlock()
	want := []struct {
		id  string
		typ pb.StageType
	}{
		{"graph", pb.StageType_STAGE_TYPE_GRAPH},
		{"report", pb.StageType_STAGE_TYPE_REPORT_GENERATION},
	}
	got := task.GetStages()
	if len(got) != len(want) {
		t.Fatalf("stages want %v, got %d 项", want, len(got))
	}
	for i, w := range want {
		if got[i].GetStageId() != w.id || got[i].GetType() != w.typ {
			t.Fatalf("stage[%d] = {%s %s}, want {%s %s}", i,
				got[i].GetStageId(), got[i].GetType(), w.id, w.typ)
		}
		if got[i].GetMetadata() == nil {
			t.Fatalf("stage %s 无 Metadata map（ADR-212① nil-map panic 锚）", got[i].GetStageId())
		}
		if got[i].GetStatus() != pb.StageStatus_STAGE_STATUS_PENDING {
			t.Fatalf("stage %s 初始态非 PENDING", got[i].GetStageId())
		}
	}
}

// TestRegisterStages_NonGraphUnchanged — 非 graph 任务模板不受裁剪影响
// （模式B 仍含 analyze/ai 占位——session 路径的真实阶段）。
func TestRegisterStages_NonGraphUnchanged(t *testing.T) {
	s := newSvc(t)
	s.mu.Lock()
	task := &pb.ScanTask{TaskId: "t-b", Status: pb.TaskStatus_TASK_STATUS_CREATED,
		ScanMode: pb.ScanMode_SCAN_MODE_AI_ONLY, // 无 execution 键=session 路径
		Config:   map[string]string{}}
	s.registerStagesLocked(task)
	s.mu.Unlock()
	want := []string{"analyze", "ai", "report"}
	got := task.GetStages()
	if len(got) != len(want) {
		t.Fatalf("stages want %v, got %d 项", want, len(got))
	}
	for i := range want {
		if got[i].GetStageId() != want[i] {
			t.Fatalf("stage[%d] = %s, want %s", i, got[i].GetStageId(), want[i])
		}
	}
}
