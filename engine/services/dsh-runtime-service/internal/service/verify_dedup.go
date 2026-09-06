// verify_dedup — 沙箱验证前同文件同段去重（ADR-186，人类反馈 2026-09-03：
// "跨工具重复发现多次进沙箱的问题应该在进沙箱前判断是否已发送过同文件的同段代码"）。
//
// 语义：VerifySASTResults 送沙箱前，把指向同一段代码的发现归并为一组——
// 一组只发一轮沙箱 prompt，判定结果广播回组内全部发现（每条 VerifiedFinding 的
// reasoning 加「同段判定（与 <代表id> 共用）」前缀，审计可追溯）。
// 与既有 dedupKey（file:start_line:cwe，ai_engine.go）互补：dedupKey 要求同行同
// CWE（融合去重口径），过严无法吸收跨工具行漂移；本分组按行区间扫并 + 容差半径。
// 契约不变：响应仍对每个输入 finding_id 返回一条 VerifiedFinding。
package service

import (
	"fmt"
	"sort"

	pb "github.com/codeaudit/proto-gen"
)

// segmentRadiusLines — 同段归并容差半径（行）。跨工具对同一语句起算点的行漂移
// 通常 ≤2 行（AST 起点口径差异），ADR-186 实现级决策（E2-3 路线：实现细节不入设计文档，
// 记 decisions.md；非 07 矩阵指标类数值）。
const segmentRadiusLines = 2

// segmentGroup — 一次沙箱轮次的验证单元：同文件同段的全部发现。
type segmentGroup struct {
	Rep     *pb.UnifiedFinding   // 代表条目：severity 最高，同级取行号小者
	Members []*pb.UnifiedFinding // 组内全部发现（含代表），判定广播对象
}

// groupSegments — 同文件同段归并：按文件分桶 → 桶内按 [start,end]（end 缺省=start）
// 扫并——相邻区间在 ±segmentRadiusLines 容差内重叠即归同段。文件路径为空的发现
// 不与任何发现归并（无定位信息，各自独立验证）。
func groupSegments(findings []*pb.UnifiedFinding) []segmentGroup {
	byFile := map[string][]*pb.UnifiedFinding{}
	var fileOrder []string
	for _, f := range findings {
		fp := f.GetLocation().GetFilePath()
		if fp == "" {
			continue // 无文件定位：不参与归并（下方单独成组）
		}
		if _, seen := byFile[fp]; !seen {
			fileOrder = append(fileOrder, fp)
		}
		byFile[fp] = append(byFile[fp], f)
	}

	groups := make([]segmentGroup, 0, len(findings))
	for _, f := range findings {
		if f.GetLocation().GetFilePath() == "" {
			groups = append(groups, segmentGroup{Rep: f, Members: []*pb.UnifiedFinding{f}})
		}
	}
	for _, fp := range fileOrder {
		list := byFile[fp]
		sort.SliceStable(list, func(i, j int) bool {
			siLo, siHi := segRange(list[i])
			sjLo, sjHi := segRange(list[j])
			if siLo != sjLo {
				return siLo < sjLo
			}
			return siHi < sjHi
		})
		var cur []*pb.UnifiedFinding
		var curEnd int32
		flush := func() {
			if len(cur) == 0 {
				return
			}
			groups = append(groups, segmentGroup{Rep: pickRepresentative(cur), Members: cur})
		}
		for _, f := range list {
			s, e := segRange(f)
			if len(cur) == 0 {
				cur = []*pb.UnifiedFinding{f}
				curEnd = e
				continue
			}
			if s <= curEnd+segmentRadiusLines { // 容差内重叠 → 同段
				cur = append(cur, f)
				if e > curEnd {
					curEnd = e
				}
				continue
			}
			flush()
			cur = []*pb.UnifiedFinding{f}
			curEnd = e
		}
		flush()
	}
	return groups
}

// segRange — 发现的行区间 [start,end]；end 缺省（0 或 <start）按 start 计。
func segRange(f *pb.UnifiedFinding) (int32, int32) {
	s := f.GetLocation().GetStartLine()
	e := f.GetLocation().GetEndLine()
	if e < s {
		e = s
	}
	return s, e
}

// severityRank — severity 高低序（代表选择用）：CRITICAL>HIGH>MEDIUM>LOW>INFO>UNSPECIFIED。
// 不依赖 proto 数值序（契约未承诺枚举值大小与语义排序一致）。
func severityRank(s pb.Severity) int {
	switch s {
	case pb.Severity_SEVERITY_CRITICAL:
		return 5
	case pb.Severity_SEVERITY_HIGH:
		return 4
	case pb.Severity_SEVERITY_MEDIUM:
		return 3
	case pb.Severity_SEVERITY_LOW:
		return 2
	case pb.Severity_SEVERITY_INFO:
		return 1
	default:
		return 0
	}
}

// pickRepresentative — 组代表：severity 最高；同级取行号小者；再同级取输入序首位（稳定）。
func pickRepresentative(members []*pb.UnifiedFinding) *pb.UnifiedFinding {
	rep := members[0]
	for _, f := range members[1:] {
		hr, rf := severityRank(f.GetSeverity()), severityRank(rep.GetSeverity())
		if hr > rf || (hr == rf && f.GetLocation().GetStartLine() < rep.GetLocation().GetStartLine()) {
			rep = f
		}
	}
	return rep
}

// broadcastGroupVerdict — 一组一轮沙箱的判定广播：代表条目的 VerifiedFinding 为基准，
// 复制给组内其余成员（verdict/confidence 同源；reasoning 加共用判定前缀溯源）。
// 返回顺序与 members 一致（契约：每个输入 finding_id 恰一条）。
func broadcastGroupVerdict(base *pb.VerifiedFinding, rep *pb.UnifiedFinding, members []*pb.UnifiedFinding) []*pb.VerifiedFinding {
	out := make([]*pb.VerifiedFinding, 0, len(members))
	for _, m := range members {
		if m.GetFindingId() == rep.GetFindingId() {
			out = append(out, base)
			continue
		}
		out = append(out, &pb.VerifiedFinding{
			OriginalFindingId: m.GetFindingId(),
			Verdict:           base.GetVerdict(),
			Confidence:        base.GetConfidence(),
			Reasoning: fmt.Sprintf("同段判定（与 %s 共用一轮沙箱，ADR-186）: %s",
				rep.GetFindingId(), base.GetReasoning()),
		})
	}
	return out
}
