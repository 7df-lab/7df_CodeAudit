// 纯逻辑：findings → 树节点模型（文件分组 → 漏洞条目），供 TreeDataProvider 消费。
import type { UnifiedFinding } from './types';
import { SEVERITY_LABEL, severityRank } from './diagnosticsMapper';

export type TreeNode =
  | { kind: 'file'; path: string; count: number; findings: UnifiedFinding[] }
  | { kind: 'finding'; finding: UnifiedFinding; parentPath: string };

export function buildTree(findings: UnifiedFinding[]): TreeNode[] {
  const byFile = new Map<string, UnifiedFinding[]>();
  const orphans: UnifiedFinding[] = [];
  for (const f of findings) {
    const p = f.location?.file_path?.replace(/\\/g, '/');
    if (p) {
      const list = byFile.get(p);
      if (list) list.push(f);
      else byFile.set(p, [f]);
    } else {
      orphans.push(f);
    }
  }
  const nodes: TreeNode[] = [];
  for (const [path, list] of [...byFile.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
    list.sort((a, b) => severityRank(b.severity) - severityRank(a.severity));
    nodes.push({ kind: 'file', path, count: list.length, findings: list });
    for (const f of list) nodes.push({ kind: 'finding', finding: f, parentPath: path });
  }
  // 无精确定位的发现也可见：归入“(无位置)”组
  if (orphans.length > 0) {
    const label = '(无位置)';
    nodes.push({ kind: 'file', path: label, count: orphans.length, findings: orphans });
    for (const f of orphans) nodes.push({ kind: 'finding', finding: f, parentPath: label });
  }
  return nodes;
}

export function findingLabel(f: UnifiedFinding): string {
  const sev = SEVERITY_LABEL[f.severity] ?? f.severity;
  const cwe = f.cwe_id ? ` [${f.cwe_id}]` : '';
  // 平台 title 可能为空：以 文件:行号 兜底，避免树里出现无名条目
  const title = f.title || `${f.location?.file_path ?? '?'}:${f.location?.start_line ?? '?'}`;
  return `${sev}${cwe} ${title}`;
}

export function findingDescription(f: UnifiedFinding): string {
  return f.source_tool + (f.ai_verdict ? ` · AI:${f.ai_verdict}` : '');
}

/**
 * 按文件相对路径 + 0-based 行号定位发现（QuickFix/诊断反向映射用）。
 * 先精确匹配 start_line（诊断起始行 == start_line-1），再退按 [start_line-1, end_line-1]
 * 区间包含判定；同文件多条发现时绝不退化为"取第一条"——那会把补丁应用到错误的发现上。
 */
export function pickFindingAtLine(
  findings: UnifiedFinding[],
  relPath: string,
  line0: number,
): UnifiedFinding | undefined {
  const norm = relPath.replace(/\\/g, '/');
  const inFile = findings.filter((f) => {
    const p = f.location?.file_path?.replace(/\\/g, '/');
    return p === norm && !!f.location?.start_line;
  });
  return (
    inFile.find((f) => Math.max(0, Math.floor(f.location!.start_line) - 1) === line0)
    ?? inFile.find((f) => {
      const s = Math.max(0, Math.floor(f.location!.start_line) - 1);
      const e = Math.max(s, Math.floor(f.location!.end_line ?? f.location!.start_line) - 1);
      return line0 >= s && line0 <= e;
    })
  );
}

// —— 回滚 QuickPick 候选（命令面板"回滚此漏洞修复"无参入口） ————————————————

export interface RollbackPickItem {
  /** "file.py:7 · 高危"（行号优先取修复后跟踪行号，无则扫描原始行号） */
  label: string;
  /** 标题 + AI 结论 + 已修复标记 */
  description: string;
  finding: UnifiedFinding;
}

/**
 * 已应用修复 ∩ 当前发现集：只列面板上可见且可回滚的条目（登记表里残留但
 * 不在当前结果里的发现无从定位，留在面板的 ✔ 徽章路径处理）。
 */
export function rollbackPickItems(
  findings: UnifiedFinding[],
  appliedFindingIds: Set<string>,
  trackedLines?: Record<string, number>,
): RollbackPickItem[] {
  const items: RollbackPickItem[] = [];
  for (const f of findings) {
    if (!appliedFindingIds.has(f.finding_id)) continue;
    const sev = SEVERITY_LABEL[f.severity] ?? f.severity;
    const line = trackedLines?.[f.finding_id] ?? f.location?.start_line;
    const loc = f.location?.file_path ? `${f.location.file_path}${line ? `:${line}` : ''} · ` : '';
    const verdict = f.ai_verdict ? ` · AI:${f.ai_verdict}` : '';
    items.push({
      label: `${loc}${sev}`,
      description: `${f.title || f.finding_id}${verdict} · 已修复（可回滚）`,
      finding: f,
    });
  }
  return items;
}
