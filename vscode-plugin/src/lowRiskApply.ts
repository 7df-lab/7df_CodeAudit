// 低风险修复批量应用的候选筛选（纯逻辑，可单测）。
// 「CodeAudit: 批量应用低风险修复」命令的候选 = 同时满足：
//   1. 严重级 LOW/INFO；
//   2. AI 置信度 ≥ 0.9；
//   3. 带机器补丁 diff_patch（apply_patch 主路径）——```diff 围栏兜底路径服务旧任务，
//      不参与批量改盘；
//   4. 不在 excludeIds 中：传入登记表全部已知发现（applied + rolledback）——已应用
//      的不重复处理，用户显式回滚过的不被翻案。
// 是否应用、应用哪些由人在 QuickPick 中逐条裁决——插件绝不自动改工作区。
import type { UnifiedFinding } from './types';

export const LOW_RISK_SEVERITIES = new Set(['SEVERITY_LOW', 'SEVERITY_INFO']);
export const LOW_RISK_MIN_AI_CONFIDENCE = 0.9;

export function selectLowRiskFixCandidates(findings: UnifiedFinding[], excludeIds: Set<string>): UnifiedFinding[] {
  return findings.filter(
    (f) =>
      LOW_RISK_SEVERITIES.has(f.severity)
      && f.ai_confidence >= LOW_RISK_MIN_AI_CONFIDENCE
      && !!f.diff_patch
      && !excludeIds.has(f.finding_id),
  );
}
