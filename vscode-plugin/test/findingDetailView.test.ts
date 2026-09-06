import * as assert from 'assert';
import { renderFindingDetailHtml, type FindingDetailData } from '../src/findingDetailView';
import type { UnifiedFinding } from '../src/types';

function finding(over: Partial<UnifiedFinding> = {}): UnifiedFinding {
  return {
    finding_id: 'f1', task_id: 't', project_id: 'p', source_tool: 'ai_agent', source_rule_id: 'R1',
    cwe_id: 'CWE-89', title: 'SQL 注入<script>', description: '拼接 <SQL>', severity: 'SEVERITY_HIGH',
    confidence: 0.9, ai_verdict: 'AI_VERDICT_LIKELY_TRUE', ai_confidence: 0.8, ai_reasoning: '推理',
    ai_fix_suggestion: '参数化', diff_patch: '*** Begin Patch\n*** Update File: a.py\n@@\n-bad\n+good',
    location: { file_path: 'a.py', start_line: 7, end_line: 9 }, dedup_group: '', is_unique: true,
    ...over,
  } as UnifiedFinding;
}

describe('findingDetailView', () => {
  it('完整渲染：严重级/标题/位置/CWE/AI 结论/建议/补丁，HTML 元字符转义', () => {
    const html = renderFindingDetailHtml({ finding: finding(), fixed: false });
    assert.ok(html.includes('高危'));
    assert.ok(html.includes('SQL 注入&lt;script&gt;'), '标题必须转义');
    assert.ok(html.includes('a.py:7-9'));
    assert.ok(html.includes('CWE-89'));
    assert.ok(html.includes('AI 判定：大概率真实'));
    assert.ok(html.includes('拼接 &lt;SQL&gt;'), '描述必须转义');
    assert.ok(html.includes('+good'));
    assert.ok(html.includes('AI 修复此漏洞'));
    assert.ok(!html.includes('回滚此修复'), '未修复状态不显示回滚按钮');
  });

  it('fixed 状态：按钮切换为回滚 + ✔ 已修复徽章', () => {
    const html = renderFindingDetailHtml({ finding: finding(), fixed: true });
    assert.ok(html.includes('回滚此修复'));
    assert.ok(html.includes('已修复（可回滚）'));
    assert.ok(!html.includes('AI 修复此漏洞'));
  });

  it('空态：无发现时给操作指引而非空白', () => {
    const html = renderFindingDetailHtml({ finding: null, fixed: false } satisfies FindingDetailData);
    assert.ok(html.includes('点击任意漏洞'));
    assert.ok(!html.includes('AI 修复此漏洞'));
  });

  it('无补丁/无建议降级：不出现空区块', () => {
    const html = renderFindingDetailHtml({ finding: finding({ diff_patch: '', ai_fix_suggestion: '' }), fixed: false });
    assert.ok(!html.includes('修复补丁'), '无补丁不渲染补丁区');
    assert.ok(html.includes('（平台未产出修复建议）'));
  });
});
