// 纯逻辑：UnifiedFinding → VS Code Diagnostic 描述（不 import vscode，便于单测）。
// 依据: proto LocationInfo（file_path/start_line/end_line）与 Severity 枚举 L119-126。
import type { UnifiedFinding } from './types';

// 诊断严重级：proto Severity 枚举名 → 0-3 标度（Error/Warning/Information 的语义级）
export const SEVERITY_RANK: Record<string, number> = {
  SEVERITY_CRITICAL: 3,
  SEVERITY_HIGH: 3,
  SEVERITY_MEDIUM: 2,
  SEVERITY_LOW: 1,
  SEVERITY_INFO: 0,
  SEVERITY_UNSPECIFIED: 1,
};

export const SEVERITY_LABEL: Record<string, string> = {
  SEVERITY_CRITICAL: '严重',
  SEVERITY_HIGH: '高危',
  SEVERITY_MEDIUM: '中危',
  SEVERITY_LOW: '低危',
  SEVERITY_INFO: '提示',
  SEVERITY_UNSPECIFIED: '未知',
};

export interface MappedDiagnostic {
  filePath: string; // 仓库相对路径（/ 分隔）
  line: number; // 0-based 起始行
  endLine: number; // 0-based 结束行（含）
  severityRank: number;
  message: string;
  code: string; // CWE / 规则 ID，显示为诊断 code
  source: string; // 来源工具
}

export function severityRank(severity: string): number {
  return SEVERITY_RANK[severity] ?? 1;
}

/**
 * lineOverride：修复/回滚后的跟踪行号（1-based，trackedLines），缺省用发现
 * 原始 location。行跨度保持原始相对跨度（修复后内容长度未知，保跨度而非塌缩单行）。
 */
export function mapFinding(f: UnifiedFinding, lineOverride?: number): MappedDiagnostic | null {
  const loc = f.location;
  if (!loc?.file_path || !loc.start_line) return null; // 无精确定位的发现进 TreeView，不进 Problems
  const start0 = Math.max(1, Math.floor(loc.start_line)) - 1; // proto 1-based → vscode 0-based
  const end0 = Math.max(start0, Math.floor(loc.end_line ?? loc.start_line) - 1);
  const start = lineOverride !== undefined ? Math.max(0, Math.floor(lineOverride) - 1) : start0;
  const end = start === start0 ? end0 : Math.max(start, start + (end0 - start0));
  const label = SEVERITY_LABEL[f.severity] ?? f.severity;
  const parts = [
    `[${f.source_tool}] ${f.title || `${loc.file_path}:${loc.start_line}`}`, // 平台 title 可能为空，文件:行号兜底
    f.cwe_id ? `CWE: ${f.cwe_id}` : '',
    f.description ? `\n${f.description}` : '',
  ].filter(Boolean);
  return {
    filePath: loc.file_path.replace(/\\/g, '/'),
    line: start,
    endLine: end,
    severityRank: severityRank(f.severity),
    message: `${parts.join(' | ')}（${label}${f.ai_verdict ? ` · AI:${f.ai_verdict}` : ''}）`,
    code: f.cwe_id || f.source_rule_id || f.finding_id,
    source: 'CodeAudit',
  };
}

export function groupFindingsByFile(findings: UnifiedFinding[]): Map<string, UnifiedFinding[]> {
  const byFile = new Map<string, UnifiedFinding[]>();
  for (const f of findings) {
    const key = f.location?.file_path?.replace(/\\/g, '/') ?? '';
    if (!key) continue;
    const list = byFile.get(key);
    if (list) list.push(f);
    else byFile.set(key, [f]);
  }
  for (const list of byFile.values()) {
    list.sort((a, b) => severityRank(b.severity) - severityRank(a.severity) || a.title.localeCompare(b.title));
  }
  return byFile;
}
