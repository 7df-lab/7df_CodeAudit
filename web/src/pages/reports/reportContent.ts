// 报告正文解析（报告阅读视图 /reports/:id 专用）：把 E-29① getReportContent 取回的
// 正文文本解析为可渲染结构。独立纯函数文件——形状防御与降级分支在此直测（jsdom 零网关），
// 页面只管渲染。实证形状：JSON 报告 = {findings[],
// summary{total_findings,true_positives,false_positives,not_reviewed}, generated_at,
// task_id, template}；findings[].source_raw 为**明文 JSON 字符串**（与发现域 API 的
// base64 口径不同），内含 {code, context:{lines,start_line,end_line}, line}。
// 历史档（REPORT_FORMAT_UNSPECIFIED）实证同为该 JSON 形状——分型以内容嗅探为准，
// 不信列表行的 format 标签。
import type { VerdictState } from '../../dict/verdict';
import { stateFromLegacyVerdict, verdictState as axesState } from '../../dict/verdict';

export interface ReportCodeEvidence {
  matchedLine: number;          // 报告标记的问题行（0=未记录）
  text: string;                 // 匹配行原文（无上下文窗口时的展示底稿）
  context?: { startLine: number; lines: string[] }; // ±N 行窗口（有则按窗口渲染）
}
export interface ReportFinding {
  findingId: string;
  title: string;
  severity: string;   // SEVERITY_* 枚举名
  verdictState: VerdictState; // 五态组合（新报告=两轴组合；旧报告=legacy 七值映射）
  file: string;
  line: number;
  cwe: string;
  ruleId: string;
  evidence?: ReportCodeEvidence;
}
export interface ReportDocument {
  taskId: string;
  generatedAt: string | null;
  summary: { total: number; truePositives: number; falsePositives: number; notReviewed: number };
  findings: ReportFinding[];
}
export type ParsedReport =
  | { kind: 'json'; doc: ReportDocument }
  | { kind: 'html' }    // HTML 报告——本页不直染（脚本面收口），引导沙箱窗渲染
  | { kind: 'text' }    // CSV 等其他文本——原文卡直显
  | { kind: 'empty' };  // 无正文（生成失败/被清除）——引导重新生成

// 展示序=危险度降序（与光谱带左起一致）；UNSPECIFIED 归最末（未知不冒充无害）
export const SEVERITY_LADDER = [
  'SEVERITY_CRITICAL', 'SEVERITY_HIGH', 'SEVERITY_MEDIUM', 'SEVERITY_LOW', 'SEVERITY_INFO',
] as const;

const asString = (v: unknown): string => (typeof v === 'string' ? v : '');
const asNumber = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

// source_raw → 代码证据：明文 JSON 字符串。两代形状兼容——新档 {code, context{lines,
// start_line,end_line}, line}；老档 code 自带 "5     cur.execute(...)" 行号前缀文本
// （无 context）——原样展示，不臆造行号。
function parseEvidence(raw: string, hintLine: number): ReportCodeEvidence | undefined {
  if (!raw) return undefined;
  let obj: unknown;
  try {
    obj = JSON.parse(raw);
  } catch {
    return undefined; // 非结构化透传（纯文本片段等）——如实"未携带代码片段"
  }
  const o = obj as Record<string, unknown>;
  const code = asString(o.code).replace(/\r\n/g, '\n').replace(/\n$/, '');
  if (!code) return undefined;
  const ctx = o.context as Record<string, unknown> | undefined;
  const lines = Array.isArray(ctx?.lines) ? (ctx!.lines as unknown[]).map(asString) : undefined;
  const startLine = asNumber(ctx?.start_line);
  const evidence: ReportCodeEvidence = {
    matchedLine: asNumber(o.line) || hintLine,
    text: code,
    ...(lines && lines.length > 0 && startLine > 0 ? { context: { startLine, lines } } : {}),
  };
  return evidence;
}

function parseFinding(v: unknown): ReportFinding | null {
  const o = v as Record<string, unknown> | null;
  if (!o || typeof o !== 'object') return null;
  const id = asString(o.finding_id);
  if (!id) return null; // 无 id 无法锚定（展开态/锚点跳转），整条不采
  return {
    findingId: id,
    title: asString(o.title) || '（报告未提供标题）',
    severity: asString(o.severity) || 'SEVERITY_UNSPECIFIED',
    // 两轴字段在=新报告直接组合；否则按 legacy 七值串映射（存量报告兼容）
    verdictState: o.verdict_direction ? axesState({ verdict_direction: asString(o.verdict_direction), verdict_tier: asString(o.verdict_tier) }) : stateFromLegacyVerdict(asString(o.verdict)),
    file: asString(o.file),
    line: asNumber(o.line),
    cwe: asString(o.cwe),
    ruleId: asString(o.rule_id),
    evidence: parseEvidence(asString(o.source_raw), asNumber(o.line)),
  };
}

export function parseReportContent(content: string): ParsedReport {
  const text = (content ?? '').trim();
  if (!text) return { kind: 'empty' };
  if (text.startsWith('<')) return { kind: 'html' };
  let obj: unknown;
  try {
    obj = JSON.parse(text);
  } catch {
    return { kind: 'text' };
  }
  const o = obj as Record<string, unknown> | null;
  if (!o || typeof o !== 'object' || !Array.isArray(o.findings)) {
    // JSON 但非报告形状（网关错误载荷等）——按原文文本处理，不冒充解析成功
    return { kind: 'text' };
  }
  const s = (o.summary ?? {}) as Record<string, unknown>;
  const findings = (o.findings as unknown[])
    .map(parseFinding)
    .filter((f): f is ReportFinding => f !== null);
  return {
    kind: 'json',
    doc: {
      taskId: asString(o.task_id),
      generatedAt: asString(o.generated_at) || null,
      summary: {
        total: asNumber(s.total_findings),
        truePositives: asNumber(s.true_positives),
        falsePositives: asNumber(s.false_positives),
        notReviewed: asNumber(s.not_reviewed),
      },
      findings,
    },
  };
}

// 分级桶键：非阶梯枚举值（UNSPECIFIED/空/未来新增）一律归"未知"末位桶
export function severityBucket(severity: string): string {
  return (SEVERITY_LADDER as readonly string[]).includes(severity) ? severity : 'SEVERITY_UNSPECIFIED';
}

// 光谱带/过滤共用的分级计数：阶梯五级 + 未知（UNSPECIFIED/空）归末位桶
export interface SeverityCount { key: string; count: number }
export function countBySeverity(findings: ReportFinding[]): SeverityCount[] {
  const counts = new Map<string, number>();
  for (const key of [...SEVERITY_LADDER, 'SEVERITY_UNSPECIFIED']) counts.set(key, 0);
  for (const f of findings) {
    const key = counts.has(f.severity) ? f.severity : 'SEVERITY_UNSPECIFIED';
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  return [...counts.entries()].map(([key, count]) => ({ key, count }));
}
