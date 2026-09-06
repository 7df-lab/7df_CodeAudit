import * as assert from 'assert';
import { LOW_RISK_MIN_AI_CONFIDENCE, selectLowRiskFixCandidates } from '../src/lowRiskApply';
import type { UnifiedFinding } from '../src/types';

const finding = (over: Partial<UnifiedFinding>): UnifiedFinding => ({
  finding_id: 'f1',
  task_id: 't',
  project_id: 'p',
  source_tool: 'semgrep',
  source_rule_id: 'r',
  cwe_id: 'CWE-79',
  title: 'T',
  description: '',
  severity: 'SEVERITY_LOW',
  confidence: 0.5,
  ai_verdict: 'AI_VERDICT_TRUE',
  ai_confidence: 0.95,
  ai_reasoning: '',
  ai_fix_suggestion: '',
  diff_patch: '*** Begin Patch\n*** End Patch',
  location: null,
  dedup_group: '',
  is_unique: true,
  ...over,
});

describe('lowRiskApply：批量应用低风险修复的候选筛选', () => {
  it('仅 LOW/INFO + AI置信度≥0.9 + 带机器补丁入选；其余全部排除', () => {
    const fs = [
      finding({ finding_id: 'a' }),                               // 满足
      finding({ finding_id: 'b', severity: 'SEVERITY_MEDIUM' }),  // 级别不符
      finding({ finding_id: 'c', severity: 'SEVERITY_INFO' }),    // 满足
      finding({ finding_id: 'd', ai_confidence: 0.89 }),          // 置信度不足
      finding({ finding_id: 'e', diff_patch: '' }),               // 无机器补丁（兜底路径不批量改盘）
      finding({ finding_id: 'g', severity: 'SEVERITY_CRITICAL', ai_confidence: 1 }), // 高危不批量
    ];
    assert.deepStrictEqual(selectLowRiskFixCandidates(fs, new Set()).map((f) => f.finding_id), ['a', 'c']);
  });

  it('置信度阈值含边界（≥0.9 精确命中）', () => {
    assert.strictEqual(LOW_RISK_MIN_AI_CONFIDENCE, 0.9);
    assert.strictEqual(selectLowRiskFixCandidates([finding({ ai_confidence: 0.9 })], new Set()).length, 1);
    assert.strictEqual(selectLowRiskFixCandidates([finding({ ai_confidence: 0.899 })], new Set()).length, 0);
  });

  it('excludeIds 排除登记表已知发现：已应用不重复处理，用户回滚不被翻案', () => {
    const fs = [finding({ finding_id: 'a' }), finding({ finding_id: 'b' })];
    assert.deepStrictEqual(selectLowRiskFixCandidates(fs, new Set(['a'])).map((f) => f.finding_id), ['b']);
    assert.deepStrictEqual(selectLowRiskFixCandidates(fs, new Set(['a', 'b'])), []);
  });
});
