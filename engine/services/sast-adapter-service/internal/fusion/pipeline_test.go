package fusion_test

import (
	"context"
	"testing"

	pb "github.com/codeaudit/proto-gen"
	"github.com/codeaudit/services/sast-adapter-service/internal/fusion"
)

// TestFusionPipeline_Basic 测试基本融合流程
// 依据: 04 §3.2 阶段5 融合五阶段
func TestFusionPipeline_Basic(t *testing.T) {
	pipeline := fusion.NewFusionPipeline()

	// 创建测试数据
	sastFindings := []*pb.UnifiedFinding{
		{
			FindingId:  "sast-1",
			TaskId:     "task-1",
			SourceTool: "semgrep",
			Location: &pb.LocationInfo{
				FilePath:  "test.py",
				StartLine: 10,
			},
			Severity:   pb.Severity_SEVERITY_HIGH,
			Confidence: 0.8,
		},
	}

	aiFindings := []*pb.UnifiedFinding{
		{
			FindingId:  "ai-1",
			TaskId:     "task-1",
			SourceTool: "ai_agent",
			Location: &pb.LocationInfo{
				FilePath:  "test.py",
				StartLine: 10,
			},
			Severity:   pb.Severity_SEVERITY_HIGH,
			Confidence: 0.9,
		},
	}

	req := &pb.FuseResultsRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-1",
		},
		TaskId:         "task-1",
		SastFindingIds: []string{"sast-1"},
		AiFindingIds:   []string{"ai-1"},
	}

	// 执行融合
	result, err := pipeline.Execute(context.Background(), req, sastFindings, aiFindings)
	if err != nil {
		t.Fatalf("pipeline.Execute failed: %v", err)
	}

	// 验证结果
	if result == nil {
		t.Fatal("result should not be nil")
	}

	// 依据: codeaudit_common.proto L437-L444 FusionResult
	// ADR-133: 同位置 SAST+AI 合并后真去重 → 仅保留 primary（SAST），total=1
	if result.GetTotalCount() != 1 {
		t.Errorf("expected total_count=1 (dedup keeps SAST primary), got %d", result.GetTotalCount())
	}

	if result.GetMetrics() == nil {
		t.Error("metrics should not be nil")
	}

	if result.GetReport() == nil {
		t.Error("report should not be nil")
	}

	// ADR-133: dedup_group 组内共享（proto L86）+ 主发现保留
	if result.GetMetrics().GetMergedCount() != 1 {
		t.Errorf("merged_count = %d, want 1", result.GetMetrics().GetMergedCount())
	}
	if len(result.GetReport().GetMergeGroups()) == 1 {
		primaryID := result.GetReport().GetMergeGroups()[0].GetPrimaryFindingId()
		if primaryID != "sast-1" {
			t.Errorf("primary = %q, want sast-1 (SAST 为主发现)", primaryID)
		}
	}
}

// TestFusionPipeline_FilterFalsePositive 测试误报过滤
// 依据: 04 §3.2 阶段5 第一步（过滤误报）
func TestFusionPipeline_FilterFalsePositive(t *testing.T) {
	pipeline := fusion.NewFusionPipeline()

	// 创建被AI判定为误报的SAST结果
	sastFindings := []*pb.UnifiedFinding{
		{
			FindingId:  "sast-1",
			TaskId:     "task-1",
			SourceTool: "semgrep",
			Location: &pb.LocationInfo{
				FilePath:  "test.py",
				StartLine: 10,
			},
			Severity:     pb.Severity_SEVERITY_LOW,
			Confidence:   0.5,
			AiVerdict:    pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE, // AI判定为误报
			AiConfidence: 0.9,                                    // 高置信度
		},
	}

	aiFindings := []*pb.UnifiedFinding{
		{
			FindingId:  "ai-1",
			TaskId:     "task-1",
			SourceTool: "ai_agent",
			Location: &pb.LocationInfo{
				FilePath:  "other.py",
				StartLine: 20,
			},
			Severity:   pb.Severity_SEVERITY_HIGH,
			Confidence: 0.95,
		},
	}

	req := &pb.FuseResultsRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-2",
		},
		TaskId:         "task-1",
		SastFindingIds: []string{"sast-1"},
		AiFindingIds:   []string{"ai-1"},
	}

	result, err := pipeline.Execute(context.Background(), req, sastFindings, aiFindings)
	if err != nil {
		t.Fatalf("pipeline.Execute failed: %v", err)
	}

	// 验证误报被过滤
	if result.GetRemovedFalsePositives() != 1 {
		t.Errorf("expected removed_false_positives=1, got %d", result.GetRemovedFalsePositives())
	}

	if result.GetMetrics().GetRemovedFalsePositives() != 1 {
		t.Errorf("expected metrics.removed_false_positives=1, got %d", result.GetMetrics().GetRemovedFalsePositives())
	}
}

// TestFusionPipeline_MergeByLocation 测试按位置合并
// 依据: 04 §3.2 阶段5 第二步（合并）
func TestFusionPipeline_MergeByLocation(t *testing.T) {
	pipeline := fusion.NewFusionPipeline()

	// 同位置的SAST和AI结果
	sastFindings := []*pb.UnifiedFinding{
		{
			FindingId:  "sast-1",
			TaskId:     "task-1",
			SourceTool: "semgrep",
			Location: &pb.LocationInfo{
				FilePath:  "test.py",
				StartLine: 10,
				EndLine:   15,
			},
			Severity:   pb.Severity_SEVERITY_HIGH,
			Confidence: 0.8,
		},
	}

	aiFindings := []*pb.UnifiedFinding{
		{
			FindingId:  "ai-1",
			TaskId:     "task-1",
			SourceTool: "ai_agent",
			Location: &pb.LocationInfo{
				FilePath:  "test.py",
				StartLine: 10,
				EndLine:   15,
			},
			Severity:   pb.Severity_SEVERITY_HIGH,
			Confidence: 0.9,
		},
	}

	req := &pb.FuseResultsRequest{
		Metadata: &pb.RequestMetadata{
			RequestId: "req-3",
		},
		TaskId:         "task-1",
		SastFindingIds: []string{"sast-1"},
		AiFindingIds:   []string{"ai-1"},
	}

	result, err := pipeline.Execute(context.Background(), req, sastFindings, aiFindings)
	if err != nil {
		t.Fatalf("pipeline.Execute failed: %v", err)
	}

	// 验证合并组
	if result.GetReport() == nil {
		t.Fatal("report should not be nil")
	}

	if len(result.GetReport().GetMergeGroups()) != 1 {
		t.Errorf("expected 1 merge group, got %d", len(result.GetReport().GetMergeGroups()))
	}

	if result.GetMetrics().GetMergedCount() != 1 {
		t.Errorf("expected merged_count=1, got %d", result.GetMetrics().GetMergedCount())
	}
}
