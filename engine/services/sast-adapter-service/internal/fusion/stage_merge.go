package fusion

import (
	"context"
	"fmt"
	"strings"

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

// normalizeRelPath — 比对用路径归一化：统一分隔符、去前导 "./" 与 "/"。
func normalizeRelPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	return strings.TrimPrefix(p, "/")
}

// sameVulnFile — R38 路径后缀对齐（2026-09-09 用户报障"同一文件同一行的漏洞显示成两个
// 风险"）：SAST（opengrep）产出沙箱绝对路径，AI（ai_agent）结论引用裸文件名/相对路径，
// 精确字符串比对永不命中 → 同文件同行漏洞 SAST+AI 各出一条。AI 路径与 SAST 路径相等、
// 或是 SAST 路径的尾部（"/"+ai 结尾）即视为同一文件。
func sameVulnFile(sastPath, aiPath string) bool {
	s, a := normalizeRelPath(sastPath), normalizeRelPath(aiPath)
	return s == a || strings.HasSuffix(s, "/"+a)
}

// Execute merges SAST and AI findings into groups.
// 依据: codeaudit_common.proto L463-L468 (MergeGroup)
// 依据: codeaudit_common.proto L84-L86 (matched_findings/dedup_group)
func (s *MergeStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	groups := make([]*pb.MergeGroup, 0)
	groupCounter := 0
	mergedIDs := make(map[string]bool)

	for _, aiF := range input.FilteredAI {
		loc := aiF.GetLocation()

		// 同文件同位置匹配（04 §3.2 阶段3对齐逻辑；R38 路径后缀对齐，行号要求 start 相等）
		sastMatches := make([]*pb.UnifiedFinding, 0)
		for _, sf := range input.FilteredSAST {
			sloc := sf.GetLocation()
			if sloc.GetStartLine() != loc.GetStartLine() {
				continue
			}
			if sameVulnFile(sloc.GetFilePath(), loc.GetFilePath()) {
				sastMatches = append(sastMatches, sf)
			}
		}

		if len(sastMatches) > 0 {
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
