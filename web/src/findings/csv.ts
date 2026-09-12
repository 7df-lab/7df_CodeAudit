// 发现列表 CSV 导出（2026-09-09，对标竞品"可按条件筛选并导出 CSV 报告"）。
// 纯函数：行 → CSV 文本（含 BOM，Excel 直接打开中文不乱码）；含逗号/引号/换行的
// 字段按 RFC 4180 双引号转义。
// B4-3（审计修复）：CSV 公式注入中和——字段以 =/+/-/@ 开头时前置单引号，
// 防止 Excel/LibreOffice/WPS 打开时按公式求值（=cmd|DDE、@SUM 等执行面）。
import { AI_VERDICT, SEVERITY, zh } from '../dict';
import type { UnifiedFinding } from '../api/types';

const HEADER = '缺陷ID,缺陷名称,严重程度,CWE,来源工具,文件路径,行号,结论,置信度,创建时间,继承来源';

function csvField(v: string | number | null | undefined): string {
  const s = String(v ?? '');
  const guarded = /^[=+\-@]/.test(s) ? `'${s}` : s;
  return /[",\n\r]/.test(guarded) ? `"${guarded.replace(/"/g, '""')}"` : guarded;
}

export function buildFindingsCsv(rows: UnifiedFinding[]): string {
  const lines = [HEADER];
  for (const f of rows) {
    lines.push([
      f.finding_id,
      f.title,
      zh(SEVERITY, f.severity),
      f.cwe_id || '',
      f.source_tool || '',
      f.location?.file_path ?? '',
      f.location?.start_line ?? '',
      f.ai_verdict ? zh(AI_VERDICT, f.ai_verdict) : '未判定',
      f.ai_confidence || '',
      f.created_at ?? '',
      // ADR-225: 继承来源列（空=本任务实扫；非空=继承自该基线任务）
      f.inherited_from_task_id || '',
    ].map(csvField).join(','));
  }
  return '\uFEFF' + lines.join('\r\n');
}

// 浏览器下载（Blob + a[download]，与报告下载同一模式）
export function downloadCsv(filename: string, csv: string): void {
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
