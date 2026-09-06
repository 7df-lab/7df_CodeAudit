package fusion

import (
	"context"

	pb "github.com/codeaudit/proto-gen"
)

// ConfidenceFusionStage fuses confidence scores.
// 依据: 04 §3.2 阶段5 第五步（置信度融合）
type ConfidenceFusionStage struct{}

func NewConfidenceFusionStage() *ConfidenceFusionStage {
	return &ConfidenceFusionStage{}
}

func (s *ConfidenceFusionStage) Name() string {
	return "confidence_fusion"
}

// Execute fuses confidence scores for merged findings.
// 依据: codeaudit_common.proto L72 confidence 字段
func (s *ConfidenceFusionStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	// For merged findings, fuse confidence scores
	for _, group := range input.Groups {
		if len(group.GetMergedFindingIds()) < 2 {
			continue
		}

		// Find primary finding
		var primary *pb.UnifiedFinding
		for _, f := range input.FusedFindings {
			if f.GetFindingId() == group.GetPrimaryFindingId() {
				primary = f
				break
			}
		}

		if primary == nil {
			continue
		}

		// Calculate fused confidence: weighted average
		// 依据: 04 §3.2 阶段5 置信度融合算法
		totalConfidence := float32(0)
		count := int32(0)

		for _, fid := range group.GetMergedFindingIds() {
			for _, f := range input.FusedFindings {
				if f.GetFindingId() == fid {
					totalConfidence += f.GetConfidence()
					count++
					break
				}
			}
		}

		if count > 0 {
			// Weighted average with boost for multiple confirmations
			// 依据: 04 §3.2 多工具确认提升置信度
			avgConfidence := totalConfidence / float32(count)
			boost := float32(1.0)
			if count > 1 {
				boost = 1.0 + 0.1*float32(count-1) // 每多一个确认+10%
			}

			fusedConfidence := avgConfidence * boost
			if fusedConfidence > 1.0 {
				fusedConfidence = 1.0
			}

			// Update primary finding's confidence
			primary.Confidence = fusedConfidence
		}
	}

	return input, nil
}
