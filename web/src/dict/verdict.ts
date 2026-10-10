// 判定两轴展示层单源——engine
// libs/common-go/verdict 的 web 镜像：口径变更两处同改（proto 枚举区注）。
// 两轴组合出五态；派生谓词 hasConclusion/needsReview 与引擎同义：
//   HasConclusion = direction != UNSPECIFIED（有方向即有结论，档位不算）
//   NeedsReview   = 未判定 或 档位=可能（原 NEEDS_MANUAL 的处置职责——
//                   "为什么没结论"属 provenance，在 ai_reasoning）
// 兼容：stateFromLegacyVerdict 把旧七值枚举串（存量报告 JSON/旧缓存）映射到五态。
export type VerdictState = 'TRUE_DEFINITE' | 'FALSE_DEFINITE' | 'TRUE_LIKELY' | 'FALSE_LIKELY' | 'UNDETERMINED';

// 五态中文标签（与 engine HTML 报告 verdictLabelZh 同口径）
export const VERDICT_STATE_LABEL: Record<VerdictState, string> = {
  TRUE_DEFINITE: '确认为真',
  FALSE_DEFINITE: '误报',
  TRUE_LIKELY: '可能为真',
  FALSE_LIKELY: '可能误报',
  UNDETERMINED: '未判定',
};

// 色彩语义承旧 VERDICT_COLOR（全站"红=危险"对齐）：真漏洞=红系、降噪=灰、待定=橙
export const VERDICT_STATE_COLOR: Record<VerdictState, string> = {
  TRUE_DEFINITE: 'error',
  TRUE_LIKELY: 'warning',
  FALSE_DEFINITE: 'default',
  FALSE_LIKELY: 'default',
  UNDETERMINED: 'default',
};

type Axes = { verdict_direction?: string | null; verdict_tier?: string | null };

// 档位防御性归一（镜像 engine NormalizeTier）：方向未判定时档位无意义
export function normalizeTier(f: Axes): string {
  if (!hasConclusion(f.verdict_direction)) return 'VERDICT_TIER_UNSPECIFIED';
  return f.verdict_tier || 'VERDICT_TIER_UNSPECIFIED';
}

export function hasConclusion(direction?: string | null): boolean {
  return !!direction && direction !== 'VERDICT_DIRECTION_UNSPECIFIED';
}

export function needsReview(f: Axes): boolean {
  return !hasConclusion(f.verdict_direction) || normalizeTier(f) === 'VERDICT_TIER_LIKELY';
}

// 两轴 → 五态（读侧防御：方向未判定一律 UNDETERMINED，脏档位不外显）
export function verdictState(f: Axes): VerdictState {
  const d = f.verdict_direction || 'VERDICT_DIRECTION_UNSPECIFIED';
  const t = normalizeTier(f);
  if (d === 'VERDICT_DIRECTION_TRUE') return t === 'VERDICT_TIER_DEFINITE' ? 'TRUE_DEFINITE' : 'TRUE_LIKELY';
  if (d === 'VERDICT_DIRECTION_FALSE') return t === 'VERDICT_TIER_DEFINITE' ? 'FALSE_DEFINITE' : 'FALSE_LIKELY';
  return 'UNDETERMINED';
}

// 旧七值枚举串 → 五态（存量报告 JSON 兼容；CONFIRMED 为历史写径遗留别名，随
// 引擎迁移同义归"确认为真"）
export function stateFromLegacyVerdict(v?: string | null): VerdictState {
  switch (v) {
    case 'AI_VERDICT_TRUE_POSITIVE':
    case 'AI_VERDICT_CONFIRMED':
      return 'TRUE_DEFINITE';
    case 'AI_VERDICT_FALSE_POSITIVE':
      return 'FALSE_DEFINITE';
    case 'AI_VERDICT_LIKELY_TRUE':
      return 'TRUE_LIKELY';
    case 'AI_VERDICT_LIKELY_FALSE':
      return 'FALSE_LIKELY';
    default: // UNSPECIFIED / UNCERTAIN / NEEDS_MANUAL / 空
      return 'UNDETERMINED';
  }
}
