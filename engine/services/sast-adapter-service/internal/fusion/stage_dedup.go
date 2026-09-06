package fusion

import (
	"context"
	"fmt"

	pb "github.com/codeaudit/proto-gen"
)

// DedupAlignStage deduplicates and aligns findings.
// 依据: 04 §3.2 阶段5 第三步（去重对齐）
// ADR-133 修复：此前本阶段只防 ID 重复，对合并组每条 finding 各发一个不同 dedup_group
// （proto L86 "同一合并组内共享"语义未实现），且不实际去重。现实现为：
//  1. 消费阶段2产出的 MergeGroups，组内所有成员共享同一 dedup_group；
//  2. 每组保留 primary（SAST 主发现），其余成员从融合输出中移除（重复项去重）。
type DedupAlignStage struct{}

func NewDedupAlignStage() *DedupAlignStage {
	return &DedupAlignStage{}
}

func (s *DedupAlignStage) Name() string {
	return "dedup_align"
}

// Execute deduplicates findings and assigns dedup_group.
// 依据: codeaudit_common.proto L86 dedup_group 字段
func (s *DedupAlignStage) Execute(ctx context.Context, input *FusionContext) (*FusionContext, error) {
	// 消费阶段2 Groups：primary 保留并标 dedup_group；其余成员标记移除
	groupOf := make(map[string]string) // finding_id → dedup_group（组内共享）
	remove := make(map[string]bool)    // 非primary重复成员 → 移除
	counter := 0
	for _, g := range input.Groups {
		members := g.GetMergedFindingIds()
		if len(members) < 2 {
			continue
		}
		counter++
		gid := g.GetGroupId()
		if gid == "" {
			gid = fmt.Sprintf("dedup_%d", counter)
		}
		primary := g.GetPrimaryFindingId()
		if primary == "" {
			primary = members[0]
		}
		for _, id := range members {
			if id == primary {
				if groupOf[id] == "" {
					groupOf[id] = gid
				}
				continue
			}
			groupOf[id] = gid
			remove[id] = true
		}
	}

	seen := make(map[string]bool)
	uniqueFindings := make([]*pb.UnifiedFinding, 0, len(input.FilteredSAST)+len(input.FilteredAI))
	appendF := func(f *pb.UnifiedFinding) {
		id := f.GetFindingId()
		if seen[id] || remove[id] {
			return
		}
		seen[id] = true
		// 依据: proto L86 dedup_group 同组共享
		if f.GetDedupGroup() == "" {
			if gid, ok := groupOf[id]; ok {
				f.DedupGroup = gid
			}
		}
		uniqueFindings = append(uniqueFindings, f)
	}

	for _, f := range input.FilteredSAST {
		appendF(f)
	}
	for _, f := range input.FilteredAI {
		appendF(f)
	}

	input.FusedFindings = uniqueFindings

	return input, nil
}
