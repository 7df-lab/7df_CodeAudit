package fusion

import (
	"context"

	pb "github.com/codeaudit/proto-gen"
)

// FalsePositiveFilterStage filters out low-confidence findings.
// 依据: 04 §3.2 阶段5 第一步（过滤误报）
type FalsePositiveFilterStage struct{}

func NewFalsePositiveFilterStage() *FalsePositiveFilterStage {
	return &FalsePositiveFilterStage{}
}

func (s *FalsePositiveFilterStage) Name() string {
	return "false_positive_filter"
}

// Execute filters findings based on AI verdict and confidence.
// 依据: codeaudit_common.proto L78-L81 (ai_verdict/ai_confidence)
func (s *FalsePositiveFilterStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	// 依据: proto L78 AIVerdict 枚举（L123-L129）
	// AI_VERDICT_FALSE_POSITIVE=2 表示AI判定为误报
	filtered := make([]*pb.FilteredItem, 0)
	filteredSAST := make([]*pb.UnifiedFinding, 0)

	for _, f := range input.SASTFindings {
		// 过滤AI判定为误报且置信度高的结果
		// 依据: 07 §10 降级策略（融合失败→输出未融合原始结果）
		if f.GetAiVerdict() == pb.AIVerdict_AI_VERDICT_FALSE_POSITIVE && f.GetAiConfidence() > 0.8 {
			filtered = append(filtered, &pb.FilteredItem{
				FindingId:    f.GetFindingId(),
				FilterReason: "AI verdict: FALSE_POSITIVE with high confidence",
			})
			continue
		}
		filteredSAST = append(filteredSAST, f)
	}

	input.FilteredSAST = filteredSAST
	input.FilteredAI = input.AIFindings // AI结果不过滤
	input.Filtered = filtered
	input.Metrics = &FusionMetrics{
		InputSASTCount:        int32(len(input.SASTFindings)),
		InputAICount:          int32(len(input.AIFindings)),
		RemovedFalsePositives: int32(len(filtered)),
	}

	return input, nil
}
