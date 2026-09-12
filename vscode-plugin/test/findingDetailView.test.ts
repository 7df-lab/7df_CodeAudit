import * as assert from 'assert';
import { renderFindingDetailHtml, VERDICT_LABEL, type FindingDetailData } from '../src/findingDetailView';
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
    assert.ok(html.includes('可能为真'), 'AI 结论文案对齐 web 权威（可能为真）');
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

  it('CSP nonce 化（§14 纵深）：script-src 走 nonce 且无 unsafe-inline；页内 script 携带同值 nonce', () => {
    const html = renderFindingDetailHtml({ finding: finding(), fixed: false });
    const meta = html.match(/<meta http-equiv="Content-Security-Policy" content="([^"]*)">/);
    assert.ok(meta, 'CSP meta 必须存在');
    const csp = meta[1] as string;
    assert.ok(csp.includes("default-src 'none'"), 'default-src 不弱化');
    const scriptDir = csp.split(';').map((d) => d.trim()).find((d) => d.startsWith('script-src')) as string;
    assert.match(scriptDir, /^script-src 'nonce-[0-9a-f]{32}'$/, 'script-src 必须为 nonce 形态');
    assert.ok(!scriptDir.includes('unsafe-inline'), 'script-src 不得再含 unsafe-inline');
    const nonce = (scriptDir.match(/'nonce-([0-9a-f]{32})'/) as RegExpMatchArray)[1];
    assert.ok(html.includes(`<script nonce="${nonce}">`), '页内 inline script 必须携带 CSP 同值 nonce');
    // 随机性：两次渲染 nonce 不同（不可预测，防猜测绕过）
    const html2 = renderFindingDetailHtml({ finding: finding(), fixed: false });
    const nonce2 = (html2.match(/'nonce-([0-9a-f]{32})'/) as RegExpMatchArray)[1];
    assert.notStrictEqual(nonce, nonce2, '两次渲染 nonce 必须不同');
  });

  it('VERDICT_LABEL 键集 golden：与 proto AIVerdict 七枚举全等（漂移即红，B2-4；伞仓 parity 闸门同口径）', () => {
    assert.deepStrictEqual(Object.keys(VERDICT_LABEL), [
      'AI_VERDICT_UNSPECIFIED',
      'AI_VERDICT_TRUE_POSITIVE',
      'AI_VERDICT_FALSE_POSITIVE',
      'AI_VERDICT_LIKELY_TRUE',
      'AI_VERDICT_LIKELY_FALSE',
      'AI_VERDICT_UNCERTAIN',
      'AI_VERDICT_NEEDS_MANUAL',
    ]);
    // 文案照抄 web/src/dict/index.ts AI_VERDICT（web 为文案权威）；死键 AI_VERDICT_TRUE/FALSE 已删
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_UNSPECIFIED, '未判定');
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_TRUE_POSITIVE, '确认为真');
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_FALSE_POSITIVE, '误报');
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_LIKELY_TRUE, '可能为真');
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_LIKELY_FALSE, '可能误报');
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_UNCERTAIN, '不确定');
    assert.strictEqual(VERDICT_LABEL.AI_VERDICT_NEEDS_MANUAL, '需人工复核');
  });
});
