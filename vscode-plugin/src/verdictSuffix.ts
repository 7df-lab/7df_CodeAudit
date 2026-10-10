// 判定两轴的展示后缀：
// 有结论时附五态中文标签（诊断悬浮/树节点摘要共用），未判定不加后缀。
import { verdictStateOf, VERDICT_LABEL } from './findingDetailView';
import type { UnifiedFinding } from './types';

export function verdictSuffix(f: Pick<UnifiedFinding, 'verdict_direction' | 'verdict_tier'>): string {
  const st = verdictStateOf(f);
  return st === 'UNDETERMINED' ? '' : ` · ${VERDICT_LABEL[st]}`;
}
