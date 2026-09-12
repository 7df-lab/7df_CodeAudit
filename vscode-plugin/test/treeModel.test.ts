import * as assert from 'assert';
import { buildTree, findingDescription, findingLabel, pickFindingAtLine, rollbackPickItems } from '../src/treeModel';
import type { UnifiedFinding } from '../src/types';

function f(id: string, over: Partial<UnifiedFinding> = {}): UnifiedFinding {
  return {
    finding_id: id, task_id: 't', project_id: 'p', source_tool: 'semgrep', source_rule_id: 'r',
    cwe_id: 'CWE-79', title: `T-${id}`, description: '', severity: 'SEVERITY_HIGH', confidence: 1,
    ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '', diff_patch: '',
    location: { file_path: 'src/a.ts', start_line: 1 }, dedup_group: '', is_unique: true, ...over,
  };
}

describe('treeModel', () => {
  it('文件分组 + 条目挂到对应文件；无位置发现归入“(无位置)”组', () => {
    const nodes = buildTree([f('a'), f('b', { location: { file_path: 'src/b.ts', start_line: 2 } }), f('c', { location: null })]);
    const files = nodes.filter((n) => n.kind === 'file');
    assert.deepStrictEqual(files.map((n) => (n as { path: string }).path), ['src/a.ts', 'src/b.ts', '(无位置)']);
    const aChildren = nodes.filter((n) => n.kind === 'finding' && n.parentPath === 'src/a.ts');
    assert.strictEqual(aChildren.length, 1);
  });

  it('文件节点按路径排序；组内按严重级降序', () => {
    const nodes = buildTree([
      f('low', { severity: 'SEVERITY_LOW' }),
      f('crit', { severity: 'SEVERITY_CRITICAL' }),
    ]);
    const children = nodes.filter((n) => n.kind === 'finding').map((n) => n.finding.finding_id);
    assert.deepStrictEqual(children, ['crit', 'low']);
  });

  it('findingLabel/Description 展示严重级、CWE、工具与 AI 结论', () => {
    const x = f('x', { ai_verdict: 'AI_VERDICT_CONFIRMED' });
    assert.strictEqual(findingLabel(x), '高危 [CWE-79] T-x');
    assert.strictEqual(findingDescription(x), 'semgrep · AI:AI_VERDICT_CONFIRMED');
  });

  describe('pickFindingAtLine（QuickFix 反向映射，回归锁：绝不退化为同文件首条）', () => {
    const findings = [
      f('first', { location: { file_path: 'vuln.py', start_line: 7, end_line: 8 } }),
      f('second', { location: { file_path: 'vuln.py', start_line: 20, end_line: 22 } }),
      f('other-file', { location: { file_path: 'other.py', start_line: 7 } }),
    ];

    it('诊断起始行精确匹配对应发现（0-based 诊断行 == start_line-1）', () => {
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 6)?.finding_id, 'first');
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 19)?.finding_id, 'second');
    });

    it('行落在发现区间内（多行块中间）也命中该发现', () => {
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 21)?.finding_id, 'second');
    });

    it('同文件但行不属任何发现 → undefined（不允许错配到别的发现）', () => {
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 10), undefined);
    });

    it('不同文件 / 反斜杠路径归一后匹配', () => {
      assert.strictEqual(pickFindingAtLine(findings, 'other.py', 6)?.finding_id, 'other-file');
      assert.strictEqual(pickFindingAtLine(findings, 'other.py', 100), undefined);
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py'.replace(/\//g, '\\'), 6)?.finding_id, 'first');
    });

    it('无位置发现不参与行匹配（不遮蔽同文件有位置发现）', () => {
      const withNull = [...findings, f('noloc', { location: null })];
      assert.strictEqual(pickFindingAtLine(withNull, 'vuln.py', 6)?.finding_id, 'first');
    });

    it('校准行号（trackedLines）：行漂移后按校准行命中，原始行失配不误配（回归锁：行号校准贯通 QuickFix）', () => {
      // 修复在 first 上方插入 3 行：诊断迁移到校准行（first 7→10、second 20→23，1-based），
      // 反查必须与诊断同源用校准行——原始行号已漂移，不得再命中（否则灯泡消失/错配）
      const tracked = { first: 10, second: 23 };
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 9, tracked)?.finding_id, 'first', '校准起点精确命中');
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 10, tracked)?.finding_id, 'first', '校准区间命中（跨度平移）');
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 22, tracked)?.finding_id, 'second', 'second 校准区间 [22,24] 命中');
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 6, tracked), undefined, '原始行 6（=7-1）已漂移，不得命中 first');
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 19, tracked), undefined, '原始起点 19 已漂移，不得命中 second');
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 21, tracked), undefined, '原始区间行 21 已漂移，不得误配');
      // 不传校准表：保持扫描原始行号口径（兼容旧调用方）
      assert.strictEqual(pickFindingAtLine(findings, 'vuln.py', 6)?.finding_id, 'first');
    });
  });

  describe('rollbackPickItems（回滚 QuickPick 候选：applied ∩ 当前发现集）', () => {
    it('只列已应用的发现；label 带文件:行号与严重级，trackedLines 行号优先于扫描原始行号', () => {
      const findings = [
        f('fixed-1', { location: { file_path: 'vuln.py', start_line: 7 } }),
        f('unfixed', { location: { file_path: 'vuln.py', start_line: 20 } }),
      ];
      const items = rollbackPickItems(findings, new Set(['fixed-1']), { 'fixed-1': 9 });
      assert.strictEqual(items.length, 1);
      assert.strictEqual(items[0].label, 'vuln.py:9 · 高危');
      assert.ok(items[0].description.includes('T-fixed-1'));
      assert.ok(items[0].description.includes('已修复（可回滚）'));
      assert.strictEqual(items[0].finding.finding_id, 'fixed-1');
    });

    it('无跟踪行号时回退扫描原始行号；AI 结论进 description', () => {
      const items = rollbackPickItems(
        [f('fx', { location: { file_path: 'v.py', start_line: 3 }, ai_verdict: 'AI_VERDICT_CONFIRMED' })],
        new Set(['fx']),
      );
      assert.strictEqual(items[0].label, 'v.py:3 · 高危');
      assert.ok(items[0].description.includes('AI:AI_VERDICT_CONFIRMED'));
    });

    it('applied 集为空 / 发现集为空 → 无候选', () => {
      assert.strictEqual(rollbackPickItems([], new Set(['x'])).length, 0);
      assert.strictEqual(rollbackPickItems([f('x')], new Set()).length, 0);
    });
  });
});
