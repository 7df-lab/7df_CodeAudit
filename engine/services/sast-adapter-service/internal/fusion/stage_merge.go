package fusion

import (
	"context"
	"fmt"

	pb "github.com/codeaudit/proto-gen"
)

// MergeStage merges SAST and AI findings.
// 依据: 04 §3.2 阶段5 第二步（合并）
type MergeStage struct{}

func NewMergeStage() *MergeStage {
	return &MergeStage{}
}

func (s *MergeStage) Name() string {
	return "merge"
}

// Execute merges SAST and AI findings into groups.
// 依据: codeaudit_common.proto L463-L468 (MergeGroup)
// 依据: codeaudit_common.proto L84-L86 (matched_findings/dedup_group)
func (s *MergeStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	// Build location index for SAST findings
	// 依据: 同文件同位置匹配（04 §3.2 阶段3对齐逻辑）
	type locationKey struct {
		FilePath  string
		StartLine int32
		EndLine   int32
	}

	sastIndex := make(map[locationKey][]*pb.UnifiedFinding)
	for _, f := range input.FilteredSAST {
		loc := f.GetLocation()
		key := locationKey{
			FilePath:  loc.GetFilePath(),
			StartLine: loc.GetStartLine(),
			EndLine:   loc.GetEndLine(),
		}
		sastIndex[key] = append(sastIndex[key], f)
	}

	// Match AI findings to SAST findings by location
	groups := make([]*pb.MergeGroup, 0)
	groupCounter := 0
	mergedIDs := make(map[string]bool)

	for _, aiF := range input.FilteredAI {
		loc := aiF.GetLocation()
		key := locationKey{
			FilePath:  loc.GetFilePath(),
			StartLine: loc.GetStartLine(),
			EndLine:   loc.GetEndLine(),
		}

		if sastMatches, ok := sastIndex[key]; ok && len(sastMatches) > 0 {
			// Found matches - create merge group
			groupCounter++
			groupFindingIDs := make([]string, 0, len(sastMatches)+1)
			groupFindingIDs = append(groupFindingIDs, aiF.GetFindingId())
			for _, sf := range sastMatches {
				groupFindingIDs = append(groupFindingIDs, sf.GetFindingId())
			}

			// 依据: codeaudit_common.proto L463-L468 MergeGroup
			groups = append(groups, &pb.MergeGroup{
				GroupId:          fmt.Sprintf("group_%d", groupCounter),
				MergedFindingIds: groupFindingIDs,
				PrimaryFindingId: sastMatches[0].GetFindingId(), // SAST作为主发现
				MergeReason:      "same_location_match",
			})

			// Update matched_findings on SAST findings
			// 依据: proto L84 matched_findings 字段
			for _, sf := range sastMatches {
				sf.MatchedFindings = append(sf.GetMatchedFindings(), aiF.GetFindingId())
				mergedIDs[sf.GetFindingId()] = true
			}
			aiF.MatchedFindings = append(aiF.GetMatchedFindings(), sastMatches[0].GetFindingId())
			mergedIDs[aiF.GetFindingId()] = true
		}
	}

	// Mark non-merged findings as unique
	// 依据: proto L85 is_unique 字段
	for _, f := range input.FilteredSAST {
		if !mergedIDs[f.GetFindingId()] {
			f.IsUnique = true
		}
	}
	for _, f := range input.FilteredAI {
		if !mergedIDs[f.GetFindingId()] {
			f.IsUnique = true
		}
	}

	input.Groups = groups
	input.Metrics.MergedCount = int32(len(groups))

	return input, nil
}
