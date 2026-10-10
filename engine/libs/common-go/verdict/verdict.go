// Package verdict — 判定两轴（VerdictDirection × VerdictTier）的派生语义单源。
//
// 依据/动机：原 AIVerdict 七值枚举把
// 真伪方向/确信档位/处置指令三个维度压平，且三个消费点（发现页 reviewed 线、
// 报告摘要桶、模式D 融合桶）各自切出不同的"有无结论"二分线。重构后 proto 只
// 承载两轴组合（见 proto 枚举区注），全部派生谓词收敛到本包；web 侧
// dict/verdict 模块是本包的展示层镜像（口径变更两处同改）。
package verdict

import pb "github.com/codeaudit/proto-gen"

// NormalizeTier — 读侧防御：不变式"tier 仅在 direction 已判定时有意义"被脏数据
// 违反时（档位有值而方向未判定），档位回落 UNSPECIFIED——全部谓词按规范形计算。
func NormalizeTier(d pb.VerdictDirection, t pb.VerdictTier) pb.VerdictTier {
	if !HasConclusion(d) {
		return pb.VerdictTier_VERDICT_TIER_UNSPECIFIED
	}
	return t
}

// HasConclusion — 是否已有真伪结论（方向已判定）。档位不影响本谓词：
// (TRUE, LIKELY) 也是"有结论（可能为真）"，与"未判定"（AI 未覆盖/降级/拒判）
// 是不同状态。
func HasConclusion(d pb.VerdictDirection) bool {
	return d != pb.VerdictDirection_VERDICT_DIRECTION_UNSPECIFIED
}

// NeedsReview — 是否需要人工复核（处置派生，承接原 NEEDS_MANUAL 的职责）：
// 未判定（原 UNSPECIFIED/UNCERTAIN/NEEDS_MANUAL 三态归并——"为什么没结论"
// 属 provenance，在 ai_reasoning 与日志，不占判定位），或档位=可能（自报未验证）。
func NeedsReview(d pb.VerdictDirection, t pb.VerdictTier) bool {
	return !HasConclusion(d) || NormalizeTier(d, t) == pb.VerdictTier_VERDICT_TIER_LIKELY
}

// IsConfirmedTrue — 报告桶"确认为真"：判真且确定档（可进整改工单；
// 可能档不进——那是要复核的，不是要整改的）。
func IsConfirmedTrue(d pb.VerdictDirection, t pb.VerdictTier) bool {
	return d == pb.VerdictDirection_VERDICT_DIRECTION_TRUE && NormalizeTier(d, t) == pb.VerdictTier_VERDICT_TIER_DEFINITE
}

// IsConfirmedFalse — 报告桶"误报"：判假且确定档（可否决关闭）。
func IsConfirmedFalse(d pb.VerdictDirection, t pb.VerdictTier) bool {
	return d == pb.VerdictDirection_VERDICT_DIRECTION_FALSE && NormalizeTier(d, t) == pb.VerdictTier_VERDICT_TIER_DEFINITE
}
