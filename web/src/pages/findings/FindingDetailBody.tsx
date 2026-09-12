// 发现详情（14号 §3.3 ④，P0 triage 工作台）：字段全部溯源 proto UnifiedFinding（P4）
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Descriptions, Input, Select, Space, Spin, Tag, Tooltip, Typography, message } from 'antd';
import { useEffect, useMemo, useRef, useState } from 'react';
import { api, getSourceFile } from '../../api/client';
import type { UnifiedFinding } from '../../api/types';
import { AI_VERDICT, SEVERITY, zh } from '../../dict';
import { baseName, parseChain } from '../../findings/chainParser';

// 上下文窗口（ADR-143）：适配器在扫描时捕获匹配点 ±10 行（真实文件内容）。
interface CodeContextWindow { start_line: number; end_line: number; lines: string[] }

// ADR-152: 结论配色（判定可见性；单一来源，列表页与详情页共用）
export const VERDICT_COLOR: Record<string, string> = {
  AI_VERDICT_TRUE_POSITIVE: 'green',
  AI_VERDICT_LIKELY_TRUE: 'cyan',
  AI_VERDICT_FALSE_POSITIVE: 'red',
  AI_VERDICT_LIKELY_FALSE: 'volcano',
  AI_VERDICT_NEEDS_MANUAL: 'orange',
  AI_VERDICT_UNCERTAIN: 'default',
};

// ADR-144: taint 规则命中标记（rule id 由规则文件 id 派生）
function isTaintRule(ruleId: string | undefined): boolean {
  return !!ruleId && ruleId.includes('taint');
}

// 机器写入的 reasoning 前缀约定（引擎创建期写入；人工裁决 RPC 无此前缀）：
//   [DSH-sandbox] = 沙箱内 DSH 语义分析（ADR-140/166）
//   [LLM:<model>] = 服务端 LLM 逐条审查（llm_review）
//   [降级]        = 系统降级标记（规则兜底/沙箱不可用，非 AI 语义判定、非人工）
export function isAIReasoning(reasoning: string | undefined): boolean {
  return !!reasoning && (reasoning.startsWith('[DSH-sandbox]') || /^\[LLM:[^\]]+\]/.test(reasoning));
}

// [降级] 前缀 = 系统自动写入的处置标记（规则引擎兜底等）——此前被"有理由=人工"启发式
// 误标为人工（2026-09-09 用户报障），识别为第三类机器写入。
export function isSystemReasoning(reasoning: string | undefined): boolean {
  return !!reasoning && reasoning.startsWith('[降级]');
}

// base64 → UTF-8 文本。atob 产出的是"每字符一字节"的 Latin-1 串，源码中文
// （UTF-8 多字节序列）会被拆成乱码（2026-08-30 会话#41：GUI 截图发现），必须经字节层解码。
function base64ToUtf8(b64: string): string {
  const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
  return new TextDecoder('utf-8').decode(bytes);
}

// ---- 变量级污点链路（ADR-158: OpenGrep dataflow_trace → proto FlowRole 语义渲染）----

export interface TaintStep { path?: string; line?: number; content: string }
export interface DataflowTrace { source?: TaintStep; propagation: TaintStep[]; sink?: TaintStep }

// OpenGrep 的位置元组编码：["CliLoc", [{path,start,end}, "代码文本"]]——防御式解析
function parseCliLoc(v: unknown): TaintStep | null {
  if (Array.isArray(v) && v.length >= 2 && Array.isArray(v[1])) {
    const loc = v[1][0] as Record<string, unknown> | undefined;
    const text = typeof v[1][1] === 'string' ? v[1][1] : '';
    const start = loc && typeof loc === 'object'
      ? (loc as Record<string, unknown>).start as Record<string, unknown> | undefined
      : undefined;
    return {
      path: loc && typeof loc === 'object' ? String((loc as Record<string, unknown>).path ?? '') : undefined,
      line: start ? Number(start.line) || undefined : undefined,
      content: text,
    };
  }
  return null;
}

// 从 source_raw（per-finding JSON, ADR-141/158 透传）提取 dataflow_trace
export function extractDataflowTrace(rawB64: string | undefined): DataflowTrace | null {
  if (!rawB64) return null;
  let obj: Record<string, unknown>;
  try {
    obj = JSON.parse(base64ToUtf8(rawB64)) as Record<string, unknown>;
  } catch {
    return null;
  }
  const t = obj.dataflow_trace;
  if (!t || typeof t !== 'object') return null;
  const tt = t as Record<string, unknown>;
  const propagation: TaintStep[] = [];
  if (Array.isArray(tt.intermediate_vars)) {
    for (const iv of tt.intermediate_vars) {
      const o = iv as Record<string, unknown>;
      const loc = o?.location as Record<string, unknown> | undefined;
      const start = loc ? (loc.start as Record<string, unknown> | undefined) : undefined;
      propagation.push({
        path: loc ? String(loc.path ?? '') : undefined,
        line: start ? Number(start.line) || undefined : undefined,
        content: String(o?.content ?? ''),
      });
    }
  }
  const source = parseCliLoc(tt.taint_source);
  const sink = parseCliLoc(tt.taint_sink);
  if (!source && !sink && propagation.length === 0) return null;
  return { source: source ?? undefined, propagation, sink: sink ?? undefined };
}

// 代码上下文提取（ADR-141/143）
function extractCodeContext(rawB64: string | undefined, hintFile?: string, hintLine?: number): {
  text: string; kind: string; context?: CodeContextWindow; matchedLine?: number;
} | null {
  if (!rawB64) return null;
  let raw: string;
  try {
    raw = base64ToUtf8(rawB64);
  } catch {
    return null;
  }
  let obj: unknown;
  try {
    obj = JSON.parse(raw);
  } catch {
    return raw ? { text: raw, kind: 'raw' } : null;
  }
  const o = obj as Record<string, unknown>;
  const ctxOf = (v: unknown): CodeContextWindow | undefined => {
    const c = v as Record<string, unknown> | undefined;
    if (c && Array.isArray(c.lines) && typeof c.start_line === 'number') {
      return { start_line: c.start_line, end_line: Number(c.end_line), lines: c.lines as string[] };
    }
    return undefined;
  };
  if (typeof o.code === 'string' && o.code.trim()) {
    return { text: o.code, kind: '匹配行', context: ctxOf(o.context), matchedLine: Number(o.line) || hintLine };
  }
  const results = o.results as Array<Record<string, unknown>> | undefined;
  if (Array.isArray(results) && results.length > 0) {
    const match = results.find((r: Record<string, unknown>) => {
      const rf = String(r.filename ?? r.path ?? '');
      const st = r.start as Record<string, unknown> | undefined;
      const rl = Number(r.line_number ?? (st ? st['line'] : 0) ?? 0);
      return hintFile ? rf.endsWith(hintFile.split('/').pop() ?? '') && (hintLine ? rl === hintLine : true) : true;
    }) ?? results[0];
    const extra = match.extra as Record<string, unknown> | undefined;
    const code = (typeof match.code === 'string' && match.code) ||
      (extra && typeof extra.lines === 'string' && extra.lines) || '';
    if (code.trim()) return { text: code, kind: '工具输出中的代码行', matchedLine: hintLine };
  }
  return { text: JSON.stringify(obj, null, 2), kind: 'raw' };
}


export function FindingDetailBody({ findingId }: { findingId: string }) {
const qc = useQueryClient();
  const [verdict, setVerdict] = useState<string>('AI_VERDICT_TRUE_POSITIVE');
  const [reasoning, setReasoning] = useState('');
  // ADR-195: 链路点选态——path 为原文写法（裸文件名/截断路径，服务端 source-file 回退解析）
  const [selHop, setSelHop] = useState<{ path: string; line?: number; endLine?: number } | null>(null);

  const { data: resp } = useQuery({
    queryKey: ['finding', findingId],
    queryFn: async () => (await api.get(`/v1/findings/${findingId}`)).data as { finding: UnifiedFinding },
  });
  const f = resp?.finding;

  // ADR-195: AI 结论 Source→Sink 链路解析（普适解析器，原文顺序 hops）
  const chain = useMemo(() => parseChain(f?.ai_reasoning), [f?.ai_reasoning]);
  const findingFile = f?.location?.file_path ?? null;
  // 复核文件选择器：链路文件 ∪ 漏洞所在文件（基名去重、漏洞文件优先；无链路时仅漏洞文件）
  const fileOptions = useMemo(() => {
    const out: string[] = [];
    const seenBase = new Set<string>();
    for (const p of [findingFile, ...chain.files]) {
      if (!p) continue;
      const b = baseName(p).toLowerCase();
      if (seenBase.has(b)) continue; // AI 写裸文件名与 location 全路径同名 → 同一文件
      seenBase.add(b);
      out.push(p);
    }
    return out;
  }, [findingFile, chain.files]);
  const openPath = selHop?.path ?? findingFile;
  // 链路点选：AI 写的常是裸文件名/截断路径——若选择器里已有同基名条目（如 location
  // 全路径），对齐到该条目，避免 Select 值与选项集脱节
  const selectHop = (h: { path: string; line?: number; endLine?: number }) => {
    const match = fileOptions.find((p) => baseName(p).toLowerCase() === baseName(h.path).toLowerCase());
    setSelHop({ path: match ?? h.path, line: h.line, endLine: h.endLine });
  };

  // ADR-195: 源码全文（endpoint 失败 → 降级回 ADR-143 ±10 行片段并说明原因）
  const srcQuery = useQuery({
    queryKey: ['source-file', f?.task_id, openPath],
    queryFn: () => getSourceFile(f!.task_id, openPath!),
    enabled: !!f?.task_id && !!openPath,
    retry: false,
    staleTime: 5 * 60_000,
  });

  const triage = useMutation({
    mutationFn: async () => api.put(`/v1/findings/${findingId}/verdict`, { verdict, reasoning }),
    onSuccess: () => {
      message.success('结论已回写');
      // ADR-152 补充：同步失效发现列表缓存——否则外层行标签停留在"未判定"
      qc.invalidateQueries({ queryKey: ['findings'] });
      qc.invalidateQueries({ queryKey: ['finding', findingId] });
      // B4-3（审计修复）：本组件还内嵌于 ReviewView 行展开（I-A0），且任务详情 Tabs 的
      // 融合/审核视图读 ai_verdict——裁决成功联动前缀失效，切 Tab/展开不再停留旧结论。
      qc.invalidateQueries({ queryKey: ['fusion-findings'] });
      qc.invalidateQueries({ queryKey: ['review-findings'] });
    },
    onError: (e) => message.error(`回写失败：${(e as Error).message}`),
  });

  if (!f) return <Typography.Text type="secondary">加载中…</Typography.Text>;
  const loc = f.location;
  const codeCtx = extractCodeContext(f.source_raw, loc?.file_path, loc?.start_line);
  const dfTrace = extractDataflowTrace(f.source_raw); // ADR-158: 变量级污点链路（OpenGrep）
  const startLine = loc?.start_line ?? 0;
  // 写入方推断（ADR-167 补遗 + 2026-09-09 三分法）：AI 链路创建期即写 verdict+reasoning
  // （带 [DSH-sandbox]/[LLM:] 前缀），系统降级标记带 [降级] 前缀 → 由此区分；其余有理由的
  // 判定=人工（UpdateVerdict 链路）。
  const verdictSet = !!f.ai_verdict && f.ai_verdict !== 'AI_VERDICT_UNSPECIFIED';
  const isAI = isAIReasoning(f.ai_reasoning);
  const isSystem = !isAI && isSystemReasoning(f.ai_reasoning);
  const isHuman = verdictSet && !!f.ai_reasoning && !isAI && !isSystem;

  return (
    // ADR-151: 展开态铺满表格宽度（与其他内容对齐）；独立页场景同为响应式全宽
    <div style={{ width: '100%' }}>
      <Typography.Title level={3}>{f.title}</Typography.Title>
      <Card style={{ marginBottom: 16 }}>
        <Descriptions column={2} size="small">
          <Descriptions.Item label="严重级"><Tag color="red">{zh(SEVERITY, f.severity)}</Tag></Descriptions.Item>
          <Descriptions.Item label="工具置信度">{f.confidence ?? '—'}</Descriptions.Item>
          <Descriptions.Item label="CWE">{f.cwe_id || '—'}</Descriptions.Item>
          <Descriptions.Item label="规则">{f.source_rule_id ? f.source_rule_id.split('.').pop() : '—'}</Descriptions.Item>
          <Descriptions.Item label="来源工具">{f.source_tool}</Descriptions.Item>
          <Descriptions.Item label="位置">{loc ? `${loc.file_path}:${loc.start_line}` : '—'}</Descriptions.Item>
        </Descriptions>
        <Typography.Paragraph style={{ marginTop: 12, whiteSpace: 'pre-wrap' }}>{f.description}</Typography.Paragraph>
      </Card>

      <Card
        title="代码上下文"
        style={{ marginBottom: 16 }}
        extra={srcQuery.isSuccess ? <Tag color="blue">源码全文</Tag> : codeCtx ? <Tag>{codeCtx.kind}</Tag> : <Tag>工具未提供代码片段</Tag>}
      >
        {/* ADR-195: AI 结论链路（普适解析器产出，按原文顺序；点击=选文件并居中定位行） */}
        {isAI && (
          <div style={{ marginBottom: 12 }} data-testid="chain-hops">
            <Typography.Text type="secondary">
              AI 结论引用的代码位置（点击定位到对应文件）：
            </Typography.Text>
            <div style={{ marginTop: 6, display: 'flex', flexWrap: 'wrap', gap: 6 }}>
              {chain.hops.length === 0 ? (
                <Typography.Text type="secondary">结论未引用其他代码位置，仅可查看漏洞所在文件。</Typography.Text>
              ) : chain.hops.map((h, i) => (
                <Tooltip key={`${h.path}:${h.line}-${h.endLine ?? ''}`} title={h.snippet}>
                  <Button
                    size="small"
                    data-testid={`chain-hop-${i}`}
                    onClick={() => selectHop({ path: h.path, line: h.line, endLine: h.endLine })}
                    style={{
                      padding: '0 8px',
                      borderColor: selHop && selHop.path === h.path && selHop.line === h.line ? '#40a9ff' : undefined,
                    }}
                  >
                    {i + 1}. {h.role === 'source' ? '源 ' : h.role === 'sink' ? '汇 ' : ''}
                    {baseName(h.path)}{h.line ? `:${h.line}${h.endLine ? `-${h.endLine}` : ''}` : ''}
                  </Button>
                </Tooltip>
              ))}
            </div>
          </div>
        )}

        {/* ADR-195: 复核文件选择器（链路文件 ∪ 漏洞文件；无链路时仅漏洞文件） */}
        {openPath && (
          <Space style={{ marginBottom: 8 }} wrap>
            <Typography.Text type="secondary">复核文件：</Typography.Text>
            <Select
              style={{ minWidth: 360 }}
              value={openPath}
              onChange={(p) => setSelHop({ path: p })}
              disabled={fileOptions.length <= 1}
              options={fileOptions.map((p) => ({ value: p, label: baseName(p) }))}
            />
            {fileOptions.length <= 1 && (
              <Typography.Text type="secondary">（仅漏洞所在文件）</Typography.Text>
            )}
            {srcQuery.data && (
              <Typography.Text type="secondary">
                共 {srcQuery.data.total_lines} 行 · {srcQuery.data.bytes}B
              </Typography.Text>
            )}
          </Space>
        )}

        {/* ADR-195: 源码全文滚动视图（保持既有面板尺寸；自动居中到目标行） */}
        {openPath && srcQuery.isLoading && <Spin size="small" style={{ display: 'block', margin: '24px auto' }} />}
        {openPath && srcQuery.isError && (
          <Alert
            style={{ marginBottom: 8 }}
            type="warning"
            showIcon
            message="源码全文不可用，已降级为扫描时捕获的代码片段"
            description={`原因：${((srcQuery.error as Error)?.message ?? '未知').slice(0, 220)}。上传压缩包或仓库拉取方式创建的任务支持全文复核。`}
          />
        )}
        {openPath && !srcQuery.isLoading && (srcQuery.isError || !srcQuery.data) && (
          codeCtx?.context ? (
            <>
              <pre style={{ background: '#0b1021', color: '#d6e2ff', padding: 12, borderRadius: 6, overflowX: 'auto', fontSize: 13, lineHeight: 1.5, margin: 0 }}>
                {codeCtx.context.lines.map((line, i) => {
                  const ln = codeCtx.context!.start_line + i;
                  const isMatch = startLine > 0 && ln >= startLine && ln <= (loc?.end_line && loc.end_line > startLine ? loc.end_line : startLine);
                  return (
                    <div key={ln} style={{ display: 'flex', background: isMatch ? 'rgba(255,208,75,0.12)' : undefined }}>
                      <span style={{ width: 44, textAlign: 'right', marginRight: 12, color: '#5b6b8c', userSelect: 'none' }}>{ln}</span>
                      <span style={{ whiteSpace: 'pre-wrap', color: isMatch ? '#ffd24b' : undefined }}>{line}</span>
                    </div>
                  );
                })}
              </pre>
              <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
                扫描时捕获的上下文片段，高亮行为工具标记位置。
              </Typography.Text>
            </>
          ) : codeCtx ? (
            <pre style={{ background: '#0b1021', color: '#d6e2ff', padding: 12, borderRadius: 6, overflowX: 'auto', fontSize: 13, lineHeight: 1.5 }}>
              {codeCtx.text.split('\n').map((line, i) => (
                <div key={i} style={{ display: 'flex' }}>
                  <span style={{ width: 44, textAlign: 'right', marginRight: 12, color: '#5b6b8c', userSelect: 'none' }}>
                    {startLine > 0 ? startLine + i : i + 1}
                  </span>
                  <span style={{ whiteSpace: 'pre-wrap' }}>{line}</span>
                </div>
              ))}
            </pre>
          ) : (
            <Typography.Text type="secondary">
              该发现未携带代码片段，且当前任务不支持源码全文回查。
            </Typography.Text>
          )
        )}
        {openPath && srcQuery.data && (
          <SourceFileViewer
            lines={srcQuery.data.content.split('\n')}
            findingRange={
              loc && baseName(openPath).toLowerCase() === baseName(loc.file_path).toLowerCase()
                ? { line: startLine, endLine: loc.end_line && loc.end_line > startLine ? loc.end_line : startLine }
                : undefined
            }
            hopRange={selHop?.line ? { line: selHop.line, endLine: selHop.endLine && selHop.endLine > selHop.line ? selHop.endLine : selHop.line } : undefined}
          />
        )}
        {!openPath && (
          <Typography.Text type="secondary">
            该发现未携带代码位置信息，无法定位源码。
          </Typography.Text>
        )}
        {loc && (
          <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
            位置：{loc.file_path}:{loc.start_line}
            {loc.end_line && loc.end_line > startLine ? `-${loc.end_line}` : ''}
          </Typography.Paragraph>
        )}
        {/* ADR-158: OpenGrep dataflow_trace → 变量级链路渲染（proto FlowRole: SOURCE/PROPAGATION/SINK） */}
        {isTaintRule(f.source_rule_id) && dfTrace && (dfTrace.source || dfTrace.sink) && (
          <Alert
            style={{ marginTop: 12 }}
            type="warning"
            showIcon
            message="污点传播链路"
            description={
              <div>
                {dfTrace.source && (
                  <div style={{ marginBottom: 6 }}>
                    <Tag color="volcano">污点来源 SOURCE</Tag>
                    <Typography.Text code>
                      {dfTrace.source.path ? `${dfTrace.source.path.split('/').pop()}:${dfTrace.source.line} — ` : ''}
                      {dfTrace.source.content}
                    </Typography.Text>
                  </div>
                )}
                {dfTrace.propagation.map((p, i) => (
                  <div key={i} style={{ marginBottom: 6 }}>
                    <Tag color="orange">传播 PROPAGATION</Tag>
                    <Typography.Text code>
                      变量 {p.content}
                      {p.path ? `（${p.path.split('/').pop()}:${p.line}）` : ''}
                    </Typography.Text>
                  </div>
                ))}
                {dfTrace.sink && (
                  <div style={{ marginBottom: 6 }}>
                    <Tag color="red">汇点 SINK</Tag>
                    <Typography.Text code>
                      {dfTrace.sink.path ? `${dfTrace.sink.path.split('/').pop()}:${dfTrace.sink.line} — ` : ''}
                      {dfTrace.sink.content}
                    </Typography.Text>
                  </div>
                )}
                <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
                  链路由扫描引擎在扫描时计算得出；跨文件与净化器级别的追踪暂不支持。
                </Typography.Paragraph>
              </div>
            }
          />
        )}
        {/* ADR-144: taint 规则命中但无逐步 trace（历史 semgrep 数据）→ 保留确认级提示 */}
        {isTaintRule(f.source_rule_id) && !(dfTrace && (dfTrace.source || dfTrace.sink)) && (
          <Alert
            style={{ marginTop: 12 }}
            type="warning"
            showIcon
            message="污点传播已确认（Source → Sink 可达）"
            description={
              <div>
                <Typography.Text code>Source: 函数参数（上下文窗口内 def 行）</Typography.Text>
                {' -> '}
                <Typography.Text code>
                  Sink: {loc ? `${loc.file_path.split('/').pop()}:${loc.start_line}` : 'execute()'}
                </Typography.Text>
                <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
                  该发现未携带变量级逐步链路；使用污点规则重新扫描可获得完整链路。
                </Typography.Paragraph>
              </div>
            }
          />
        )}
        {/* ADR-143/159 诚实声明 → 生产化收敛（2026-09-09 用户反馈"不专业"）：非 taint 规则
            的发现不产出数据流，缺席不再用大号 Alert 宣告，降为一行次要说明 */}
        {!isTaintRule(f.source_rule_id) && (
          <Typography.Text type="secondary" style={{ display: 'block', marginTop: 12 }}>
            数据流链路仅由污点类规则扫描产出，该发现未携带链路数据。
          </Typography.Text>
        )}
      </Card>

      {/* ADR-153 方案A（会话#42 人类反馈）+ 2026-09-09 三分法：写入方按 reasoning 前缀推断
          标注——AI（[DSH-sandbox]/[LLM:]）、系统降级（[降级]）、其余有理由=人工（UpdateVerdict
          链路）；无理由的已判定如实标"来源未记录"。判定者字段根治待 proto V2.1（方案B）。 */}
      <Card title="当前结论" style={{ marginBottom: 16 }}>
        <Space direction="vertical" style={{ width: '100%' }}>
          <Space wrap>
            {/* 空值显式归一"未判定"（zh 已无枚举特判，B3-5；展示行为不变） */}
            <Tag color={VERDICT_COLOR[f.ai_verdict]}>{zh(AI_VERDICT, f.ai_verdict || 'AI_VERDICT_UNSPECIFIED')}</Tag>
            {verdictSet && (
              <Tag color={isAI ? 'geekblue' : isSystem ? 'gold' : isHuman ? 'blue' : 'default'}>
                {isAI
                  ? '写入方：AI 分析'
                  : isSystem
                    ? '写入方：系统（自动降级标记）'
                    : isHuman
                      ? '写入方：人工'
                      : '写入方：未记录'}
              </Tag>
            )}
            <Typography.Text type="secondary">置信度 {f.ai_confidence || '—'}</Typography.Text>
          </Space>
          {!verdictSet && (
            <Typography.Text type="secondary">该发现尚未判定。</Typography.Text>
          )}
          {/* AI 结论原文（创建期写入，前缀标来源；人类需求：AI 结论须可见并标明是 AI 输出） */}
          {isAI && (
            <Alert
              type="info"
              showIcon
              message={`AI 分析结论（${f.ai_reasoning?.startsWith('[DSH-sandbox]') ? '沙箱语义分析' : 'LLM 审查'}）`}
              description={<Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_reasoning}</Typography.Text>}
            />
          )}
          {/* 系统降级标记（规则兜底等）：原文展示，说明其非 AI 语义判定亦非人工 */}
          {isSystem && (
            <Alert
              type="warning"
              showIcon
              message="系统自动标记"
              description={<Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_reasoning}</Typography.Text>}
            />
          )}
          {/* P4：裁决理由必须原文展示 */}
          {isHuman && (
            <Alert
              type={f.ai_verdict === 'AI_VERDICT_NEEDS_MANUAL' ? 'warning' : 'info'}
              showIcon
              message="裁决理由"
              description={<Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_reasoning}</Typography.Text>}
            />
          )}
          {f.ai_fix_suggestion && (
            <Card size="small" title="修复建议">
              <Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_fix_suggestion}</Typography.Text>
              {f.ai_fix_suggestion.startsWith('MANUAL_REVIEW_REQUIRED') && (
                <Alert type="warning" showIcon style={{ marginTop: 8 }} message="该条目为“需人工处置”标记，非自动生成的修复方案" />
              )}
            </Card>
          )}
        </Space>
      </Card>

      <Card title="人工裁决">
        <Space direction="vertical" style={{ width: '100%' }}>
          <Select
            style={{ width: 240 }}
            value={verdict}
            onChange={setVerdict}
            options={Object.entries(AI_VERDICT).filter(([k]) => k !== 'AI_VERDICT_UNSPECIFIED').map(([value, label]) => ({ value, label }))}
          />
          <Input.TextArea
            value={reasoning}
            onChange={(e) => setReasoning(e.target.value)}
            placeholder="填写裁决理由（可选，将随结论一并保存）"
          />
          <Button type="primary" loading={triage.isPending} onClick={() => triage.mutate()}>
            提交裁决
          </Button>
        </Space>
      </Card>
    </div>
  );
}

// 源码全文滚动视图（ADR-195）：保持 ±10 行片段时代的面板尺寸（~21 行高），渲染整个
// 文件；挂载/文件/目标行变化时自动滚动到目标行使之处居中（≈前后 10 行可见），
// 用户可滚动自行全面审计。行数护栏：超 2 万行截断显示（2MiB 上限下极端空行文件）。
const MAX_RENDER_LINES = 20000;

function SourceFileViewer({ lines, findingRange, hopRange }: {
  lines: string[];
  findingRange?: { line: number; endLine: number };
  hopRange?: { line: number; endLine: number };
}) {
  const ref = useRef<HTMLDivElement>(null);
  const target = hopRange?.line ?? findingRange?.line;
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (!target) { el.scrollTop = 0; return; }
    // 行号超界钳制到末行（AI 结论引用的行号可能来自源树的另一版本——root_via 披露）
    const clamped = Math.min(target, lines.length || 1);
    const row = el.querySelector<HTMLElement>(`[data-line="${clamped}"]`);
    if (row) el.scrollTop = Math.max(0, row.offsetTop - el.clientHeight / 2 + row.offsetHeight / 2);
  }, [lines, target]);
  const shown = lines.length > MAX_RENDER_LINES ? lines.slice(0, MAX_RENDER_LINES) : lines;
  return (
    <div>
      <div
        ref={ref}
        data-testid="source-viewer"
        style={{ position: 'relative', height: 432, overflow: 'auto', background: '#0b1021', color: '#d6e2ff', padding: 12, borderRadius: 6, fontSize: 13, lineHeight: 1.5 }}
      >
        {shown.map((line, i) => {
          const ln = i + 1;
          const inFinding = !!findingRange && ln >= findingRange.line && ln <= findingRange.endLine;
          const inHop = !!hopRange && ln >= hopRange.line && ln <= hopRange.endLine;
          return (
            <div
              key={ln}
              data-line={ln}
              style={{
                display: 'flex',
                background: inHop ? 'rgba(64,169,255,0.10)' : inFinding ? 'rgba(255,208,75,0.12)' : undefined,
                boxShadow: inHop ? 'inset 2px 0 0 #40a9ff' : undefined,
              }}
            >
              <span style={{ width: 44, flex: '0 0 auto', textAlign: 'right', marginRight: 12, color: '#5b6b8c', userSelect: 'none' }}>{ln}</span>
              <span style={{ whiteSpace: 'pre-wrap', color: inHop ? '#69c0ff' : inFinding ? '#ffd24b' : undefined }}>{line}</span>
            </div>
          );
        })}
      </div>
      <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
        {target
          ? `已居中定位到第 ${target} 行（${hopRange ? '链路引用' : '漏洞位置'}），滚动可审计全文。`
          : '从头展示，滚动可审计全文。'}
        黄色=漏洞所在行，蓝色=链路定位行
        {lines.length > MAX_RENDER_LINES ? `；文件共 ${lines.length} 行，超出渲染护栏仅显示前 ${MAX_RENDER_LINES} 行` : ''}。
      </Typography.Text>
    </div>
  );
}

export default FindingDetailBody;
