package fusion

import (
	"context"

	pb "github.com/codeaudit/proto-gen"
)

// ConflictResolveStage resolves conflicts between SAST and AI findings.
// 依据: 04 §3.2 阶段5 第四步（冲突解决）
// ADR-133 修复：此前 resolution 只记录在 ConflictItem 里不回写字段（"声称解决实为仅报告"）；
// 且 sast/ai 识别按组内下标 [0]/[1] 猜测。现按 source_tool 分类，并把解决策略真实回写：
//
//	severity_mismatch → use_sast_severity（保守策略：采信SAST严重级别，AI侧回写）
//	verdict_mismatch  → use_ai_verdict（AI优先策略，SAST侧回写）
type ConflictResolveStage struct{}

func NewConflictResolveStage() *ConflictResolveStage {
	return &ConflictResolveStage{}
}

func (s *ConflictResolveStage) Name() string {
	return "conflict_resolve"
}

// Execute identifies and resolves conflicts.
// 依据: codeaudit_common.proto L470-L474 (ConflictItem)
func (s *ConflictResolveStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	conflicts := make([]*pb.ConflictItem, 0)

	// byID — 组内成员实体索引
	byID := make(map[string]*pb.UnifiedFinding)
	for _, f := range input.FusedFindings {
		byID[f.GetFindingId()] = f
	}
	// isAI — source_tool=ai_agent 判定（依据: proto L68 source_tool 注释 "ai_agent"）
	isAI := func(f *pb.UnifiedFinding) bool { return f.GetSourceTool() == "ai_agent" }

	for _, group := range input.Groups {
		if len(group.GetMergedFindingIds()) < 2 {
			continue
		}
		var sastFinding, aiFinding *pb.UnifiedFinding
		for _, id := range group.GetMergedFindingIds() {
			f := byID[id]
			if f == nil {
				continue
			}
			if isAI(f) {
				if aiFinding == nil {
					aiFinding = f
				}
			} else if sastFinding == nil {
				sastFinding = f
			}
		}
		if sastFinding == nil || aiFinding == nil {
			continue
		}

		// severity 冲突：保守策略采信 SAST，真实回写 AI 侧 severity
		// 依据: proto L11 Severity 枚举
		if sastFinding.GetSeverity() != aiFinding.GetSeverity() {
			aiFinding.Severity = sastFinding.GetSeverity()
			conflicts = append(conflicts, &pb.ConflictItem{
				FindingIds:   group.GetMergedFindingIds(),
				ConflictType: "severity_mismatch",
				Resolution:   "use_sast_severity",
			})
		}

		// verdict 冲突：AI优先策略，真实回写 SAST 侧 ai_verdict
		if sastFinding.GetAiVerdict() != aiFinding.GetAiVerdict() &&
			sastFinding.GetAiVerdict() != pb.AIVerdict_AI_VERDICT_UNSPECIFIED &&
			aiFinding.GetAiVerdict() != pb.AIVerdict_AI_VERDICT_UNSPECIFIED {
			sastFinding.AiVerdict = aiFinding.GetAiVerdict()
			conflicts = append(conflicts, &pb.ConflictItem{
				FindingIds:   group.GetMergedFindingIds(),
				ConflictType: "verdict_mismatch",
				Resolution:   "use_ai_verdict",
			})
		}
	}

	input.Conflicts = conflicts

	return input, nil
}
