import * as assert from 'assert';
import { groupFindingsByFile, mapFinding, severityRank } from '../src/diagnosticsMapper';
import type { UnifiedFinding } from '../src/types';

function finding(over: Partial<UnifiedFinding> = {}): UnifiedFinding {
  return {
    finding_id: 'f1',
    task_id: 't1',
    project_id: 'p1',
    source_tool: 'semgrep',
    source_rule_id: 'r1',
    cwe_id: 'CWE-79',
    title: 'XSS',
    description: '反射型 XSS',
    severity: 'SEVERITY_HIGH',
    confidence: 0.9,
    ai_verdict: 'AI_VERDICT_CONFIRMED',
    ai_confidence: 0.8,
    ai_reasoning: '',
    ai_fix_suggestion: '',
    diff_patch: '',
    location: { file_path: 'src/a.ts', start_line: 10, end_line: 12 },
    dedup_group: '',
    is_unique: true,
    ...over,
  };
}

describe('diagnosticsMapper', () => {
  it('位置映射：proto 1-based → vscode 0-based，含起止行', () => {
    const m = mapFinding(finding());
    assert.strictEqual(m?.filePath, 'src/a.ts');
    assert.strictEqual(m?.line, 9);
    assert.strictEqual(m?.endLine, 11);
  });

  it('severity 枚举名映射到语义级', () => {
    assert.strictEqual(severityRank('SEVERITY_CRITICAL'), 3);
    assert.strictEqual(severityRank('SEVERITY_HIGH'), 3);
    assert.strictEqual(severityRank('SEVERITY_MEDIUM'), 2);
    assert.strictEqual(severityRank('SEVERITY_LOW'), 1);
    assert.strictEqual(severityRank('SEVERITY_INFO'), 0);
    assert.strictEqual(severityRank('SEVERITY_WEIRD'), 1, '未知枚举默认 Information 级');
  });

  it('无精确定位（缺 file_path 或 start_line）→ null，不进 Problems', () => {
    assert.strictEqual(mapFinding(finding({ location: null })), null);
    assert.strictEqual(mapFinding(finding({ location: { file_path: '', start_line: 5 } })), null);
    assert.strictEqual(mapFinding(finding({ location: { file_path: 'a.ts', start_line: 0 } })), null);
  });

  it('消息包含 工具/标题/CWE/AI 结论；code 优先 CWE', () => {
    const m = mapFinding(finding());
    assert.ok(m!.message.includes('[semgrep] XSS'));
    assert.ok(m!.message.includes('CWE-79'));
    assert.ok(m!.message.includes('AI:AI_VERDICT_CONFIRMED'));
    assert.strictEqual(m!.code, 'CWE-79');
  });

  it('groupFindingsByFile：反斜杠归一 + 组内按严重级降序', () => {
    const byFile = groupFindingsByFile([
      finding({ location: { file_path: 'src\\a.ts', start_line: 1 } }),
      finding({ severity: 'SEVERITY_CRITICAL', title: 'B', location: { file_path: 'src/a.ts', start_line: 2 } }),
      finding({ severity: 'SEVERITY_INFO', title: 'C', location: { file_path: 'src/a.ts', start_line: 3 } }),
    ]);
    assert.deepStrictEqual([...byFile.keys()], ['src/a.ts']);
    assert.deepStrictEqual(byFile.get('src/a.ts')!.map((f) => f.title), ['B', 'XSS', 'C']);
  });
});
