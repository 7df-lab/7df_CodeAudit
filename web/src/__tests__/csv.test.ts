// buildFindingsCsv 纯函数回归（2026-09-09 对标竞品 CSV 导出）
import { describe, expect, it } from 'vitest';
import { buildFindingsCsv } from '../findings/csv';
import type { UnifiedFinding } from '../api/types';

function row(over: Partial<UnifiedFinding>): UnifiedFinding {
  return {
    finding_id: 'f-1', title: 'SQL 注入', severity: 'SEVERITY_HIGH', cwe_id: 'CWE-89',
    source_tool: 'bandit', location: { file_path: 'src/app.py', start_line: 88 },
    ai_verdict: 'AI_VERDICT_FALSE_POSITIVE', ai_confidence: 0.8, created_at: '2026-09-09T02:00:00Z',
    ...over,
  } as UnifiedFinding;
}

describe('buildFindingsCsv', () => {
  it('BOM 头 + 表头 + 基础行（严重程度/结论映射中文）', () => {
    const csv = buildFindingsCsv([row({})]);
    expect(csv.startsWith('\uFEFF')).toBe(true);
    const lines = csv.slice(1).split('\r\n');
    // ADR-225: 表头新增"继承来源"列（末位）
    expect(lines[0]).toBe('缺陷ID,缺陷名称,严重程度,CWE,来源工具,文件路径,行号,结论,置信度,创建时间,继承来源');
    expect(lines[1]).toContain('f-1,SQL 注入,高危,CWE-89,bandit,src/app.py,88,误报');
  });

  it('ADR-225 继承来源列：继承项带基线任务号，实扫项为空', () => {
    const csv = buildFindingsCsv([
      row({ finding_id: 't2-inh-1', inherited_from_task_id: 't1' }),
      row({ finding_id: 't2-bandit-1' }),
    ]);
    const lines = csv.slice(1).split('\r\n');
    const cells1 = lines[1].split(',');
    const cells2 = lines[2].split(',');
    expect(cells1[cells1.length - 1]).toBe('t1'); // 继承项末列=基线任务号
    expect(cells2[cells2.length - 1]).toBe(''); // 实扫项末列=空
  });

  it('RFC 4180 转义：逗号/引号/换行字段双引号包裹并翻倍引号', () => {
    const csv = buildFindingsCsv([
      row({ title: '有,逗号', description: 'x' }),
      row({ finding_id: 'f-2', title: '引号"与\n换行' }),
    ]);
    const lines = csv.slice(1).split('\r\n');
    expect(lines[1]).toContain('"有,逗号"');
    expect(lines[2]).toContain('"引号""与');
    expect(lines[2]).toContain('换行"');
  });

  it('空位置/未判定如实输出空字段', () => {
    const csv = buildFindingsCsv([row({ location: undefined, ai_verdict: 'AI_VERDICT_UNSPECIFIED' })]);
    const lines = csv.slice(1).split('\r\n');
    const cells = lines[1].split(',');
    expect(cells[5]).toBe(''); // 文件路径空
    expect(cells[6]).toBe(''); // 行号空
    expect(lines[1]).toContain('未判定');
  });

  // （审计修复）：CSV 公式注入中和——= / + / - / @ 开头字段前置单引号，
  // Excel/LibreOffice/WPS 打开时不按公式求值（=cmd|DDE、@SUM 等执行面）。
  it('B4-3: =/+/-/@ 开头字段前置单引号；正常字段不受影响', () => {
    const csv = buildFindingsCsv([
      row({ title: '=SUM(A1:A9)' }),
      row({ finding_id: 'f-plus', title: '+cmd|/C calc' }),
      row({ finding_id: 'f-minus', title: '-2+3+cmd' }),
      row({ finding_id: 'f-at', title: '@SUM(A1)' }),
    ]);
    const lines = csv.slice(1).split('\r\n');
    expect(lines[1]).toContain("'=SUM(A1:A9)");
    expect(lines[2]).toContain("'+cmd|/C calc");
    expect(lines[3]).toContain("'-2+3+cmd");
    expect(lines[4]).toContain("'@SUM(A1)");
    // 对照：正常字段无前置引号（首格缺陷ID 即普通文本）
    expect(lines[1].startsWith('f-1,')).toBe(true);
  });

  it('B4-3: 中和与 RFC4180 转义叠加——危险前缀+特殊字符字段为 "\'=…" 包裹形态', () => {
    const csv = buildFindingsCsv([row({ title: '=HYPERLINK("http://x", "y")' })]);
    const lines = csv.slice(1).split('\r\n');
    // 前置单引号在双引号包裹内侧（Excel 读到 '= 开头即按文本处理）
    expect(lines[1]).toContain('"\'=HYPERLINK(""http://x"", ""y"")"');
  });

  it('B5: 前导 TAB/CR 公式注入中和（OWASP 清单扩展，=cmd 前置制表符绕过防护）', () => {
    const csv = buildFindingsCsv([row({ title: '\t=cmd|/C calc' }), row({ title: '\r@SUM(A1)' })]);
    const lines = csv.slice(1).split('\r\n');
    expect(lines[1]).toContain("'\t=cmd|/C calc");
    expect(lines[2]).toContain("'\r@SUM(A1)");
  });
});
