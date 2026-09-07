package fusion

// ADR-214 回归：conflict/confidence 两阶段此前在 FusedFindings（dedup 后仅剩
// 各组 primary）上建组员索引，AI 成员恒查不到——冲突解决与多源确认加权对
// 合并组从未生效（Conflicts 恒空、boost 恒 1.0，阶段死代码）。修复后索引覆盖
// 全量输入，输出组成不变（AI 成员仍按设计被 dedup 移除，只有裁决/置信度回写）。

import (
	"context"
	"math"
	"testing"

	pb "github.com/codeaudit/proto-gen"
)

// seedGroup — 构造一个 SAST+AI 同位置合并组的后 dedup 状态（FusedFindings 仅 primary）。
func seedGroup() (*FusionContext, *pb.UnifiedFinding) {
	sast := &pb.UnifiedFinding{
		FindingId:  "sast-1",
		SourceTool: "bandit",
		Confidence: 0.85,
		Severity:   pb.Severity_SEVERITY_HIGH,
		AiVerdict:  pb.AIVerdict_AI_VERDICT_TRUE_POSITIVE, // SAST 侧此前的裁决
	}
	ai := &pb.UnifiedFinding{
		FindingId:  "ai-1",
		SourceTool: "ai_agent",
		Confidence: 0.92,
		Severity:   pb.Severity_SEVERITY_MEDIUM, // 与 SAST 相左
		AiVerdict:  pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE,
	}
	ctx := &FusionContext{
		FilteredSAST: []*pb.UnifiedFinding{sast},
		FilteredAI:   []*pb.UnifiedFinding{ai},
		FusedFindings: []*pb.UnifiedFinding{sast}, // dedup 后：仅 primary
		Groups: []*pb.MergeGroup{{
			GroupId:          "group_1",
			MergedFindingIds: []string{"ai-1", "sast-1"},
			PrimaryFindingId: "sast-1",
		}},
	}
	return ctx, sast
}

func TestConflictResolve_SeesAIMember_AndWritesBackVerdict(t *testing.T) {
	input, sast := seedGroup()
	out, err := (&ConflictResolveStage{}).Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(out.Conflicts) == 0 {
		t.Fatalf("verdict mismatch must produce ConflictItem (pre-fix: AI member invisible, Conflicts empty)")
	}
	if sast.GetAiVerdict() != pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE {
		t.Fatalf("use_ai_verdict write-back missing: %v", sast.GetAiVerdict())
	}
}

func TestConfidenceFusion_MultiSourceBoost(t *testing.T) {
	input, sast := seedGroup()
	if _, err := (&ConfidenceFusionStage{}).Execute(context.Background(), input); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// 0.85/0.92 两源确认：avg×1.1 = 0.9735（pre-fix: count 恒 1 → 0.85 原值）
	want := float32(0.85+0.92) / 2 * 1.1
	if math.Abs(float64(sast.GetConfidence()-want)) > 1e-5 {
		t.Fatalf("fused confidence = %v, want %v", sast.GetConfidence(), want)
	}
}

// 全管线口径：组内 SAST+AI 同位置 → primary 获得加权置信度 + 冲突记录，
// AI 成员仍按设计从融合输出移除（04 §3.3 输出口径不变，增益在回写与冲突面）。
func TestPipeline_ConflictAndConfidenceLive(t *testing.T) {
	p := NewFusionPipeline()
	sast := &pb.UnifiedFinding{
		FindingId: "s-1", SourceTool: "bandit", Confidence: 0.8,
		Severity: pb.Severity_SEVERITY_HIGH,
		Location: &pb.LocationInfo{FilePath: "src/a.py", StartLine: 10, EndLine: 20},
	}
	ai := &pb.UnifiedFinding{
		FindingId: "a-1", SourceTool: "ai_agent", Confidence: 0.9,
		Severity: pb.Severity_SEVERITY_MEDIUM, // 与 SAST 分歧 → severity_mismatch
		AiVerdict: pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE,
		Location: &pb.LocationInfo{FilePath: "src/a.py", StartLine: 10, EndLine: 20},
	}
	res, err := p.Execute(context.Background(), &pb.FuseResultsRequest{TaskId: "t"}, []*pb.UnifiedFinding{sast}, []*pb.UnifiedFinding{ai})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.GetReport().GetConflicts()) == 0 {
		t.Fatalf("pipeline Conflicts must be populated (pre-fix: 恒空)")
	}
	if sast.GetConfidence() <= 0.8 {
		t.Fatalf("multi-source boost not applied: %v", sast.GetConfidence())
	}
}
