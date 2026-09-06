// 发现详情（14号 §3.3 ④，P0 triage 工作台）：字段全部溯源 proto UnifiedFinding（P4）
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Descriptions, Input, Select, Space, Tag, Typography, message } from 'antd';
import { useState } from 'react';
import { api } from '../../api/client';
import type { UnifiedFinding } from '../../api/types';
import { AI_VERDICT, SEVERITY, zh } from '../../dict';

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

// AI 输出的 reasoning 前缀约定（创建期由 AI 链路写入；人工裁决 RPC 无此前缀）：
//   [DSH-sandbox] = 沙箱内 DSH 语义分析（ADR-140/166）
//   [LLM:<model>] = 服务端 LLM 逐条审查（llm_review）
export function isAIReasoning(reasoning: string | undefined): boolean {
  return !!reasoning && (reasoning.startsWith('[DSH-sandbox]') || /^\[LLM:[^\]]+\]/.test(reasoning));
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

  const { data: resp } = useQuery({
    queryKey: ['finding', findingId],
    queryFn: async () => (await api.get(`/v1/findings/${findingId}`)).data as { finding: UnifiedFinding },
  });
  const f = resp?.finding;

  const triage = useMutation({
    mutationFn: async () => api.put(`/v1/findings/${findingId}/verdict`, { verdict, reasoning }),
    onSuccess: () => {
      message.success('结论已回写');
      // ADR-152 补充：同步失效发现列表缓存——否则外层行标签停留在"未判定"
      qc.invalidateQueries({ queryKey: ['findings'] });
      qc.invalidateQueries({ queryKey: ['finding', findingId] });
    },
    onError: (e) => message.error(`回写失败：${(e as Error).message}`),
  });

  if (!f) return <Typography.Text type="secondary">加载中…</Typography.Text>;
  const loc = f.location;
  const codeCtx = extractCodeContext(f.source_raw, loc?.file_path, loc?.start_line);
  const dfTrace = extractDataflowTrace(f.source_raw); // ADR-158: 变量级污点链路（OpenGrep）
  const startLine = loc?.start_line ?? 0;
  // 写入方推断（ADR-167 补遗修正）：AI 链路创建期即写 verdict+reasoning（带
  // [DSH-sandbox]/[LLM:] 前缀）→ 由此区分；其余有理由的判定=人工（UpdateVerdict 链路）。
  const verdictSet = !!f.ai_verdict && f.ai_verdict !== 'AI_VERDICT_UNSPECIFIED';
  const isAI = isAIReasoning(f.ai_reasoning);
  const isHuman = verdictSet && !!f.ai_reasoning && !isAI;

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
        title="代码上下文（复核依据，ADR-141/143）"
        style={{ marginBottom: 16 }}
        extra={codeCtx ? <Tag>{codeCtx.kind}</Tag> : <Tag>工具未提供代码片段</Tag>}
      >
        {codeCtx?.context ? (
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
              匹配点 ±10 行上下文（扫描时真实捕获；高亮行 = 工具标记位置）。
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
            该发现未携带代码片段（V1 口径：仅展示工具已有数据；全文浏览待 storage 文件服务接入，14号 Q5）。
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
            message="污点传播链路（OpenGrep taint 引擎真实计算，非推测）"
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
                  链路由 OpenGrep taint 引擎在扫描时真实计算（同 schema 解析自 source_raw）；
                  跨文件/净化器级明细仍需 CPG 后端（01 §2 引擎层 Joern，尚未接入）。
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
            message="污点传播已确认：引擎判定 source -> sink 可达"
            description={
              <div>
                <Typography.Text code>Source: 函数参数（上下文窗口内 def 行）</Typography.Text>
                {' -> '}
                <Typography.Text code>
                  Sink: {loc ? `${loc.file_path.split('/').pop()}:${loc.start_line}` : 'execute()'}
                </Typography.Text>
                <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
                  该发现未携带变量级逐步链路（历史 semgrep 数据或引擎未导出）；换用 OpenGrep 重新扫描可得完整链路。
                </Typography.Paragraph>
              </div>
            }
          />
        )}
        {/* ADR-143/159 诚实声明：非 taint 规则的发现, 引擎不输出数据流——精确说明而非笼统"不可用" */}
        {!isTaintRule(f.source_rule_id) && (
        <Alert
          style={{ marginTop: 12 }}
          type="info"
          showIcon
          message="Sink 数据流链路：该发现未携带（不推测）"
          description={`该发现来自 ${f.source_tool || '规则匹配类工具'}——模式/规则匹配类扫描不产生污点传播数据，本页不编造链路。需要链路时请用 opengrep 污点规则扫描（命中后此处展示 SOURCE→传播→SINK 变量级链路）；跨文件/净化器级明细仍需 CPG 后端（01 §2 引擎层 Joern，尚未接入）。`}
        />
        )}
      </Card>

      {/* ADR-153 方案A（会话#42 人类反馈）：写入方按实际数据推断标注——
          依据写入链路分析：仅人工裁决 RPC（UpdateVerdict, proto L1240）写 reasoning，
          AI 链路（CreateFinding 创建期 AiVerdict / BatchUpdateVerdict proto L1232）均不写理由。
          "有理由=人工"是推断而非记录，判定者字段根治待 proto V2.1（方案B review_source）。 */}
      <Card title="当前结论" style={{ marginBottom: 16 }}>
        <Space direction="vertical" style={{ width: '100%' }}>
          <Space wrap>
            <Tag color={VERDICT_COLOR[f.ai_verdict]}>{zh(AI_VERDICT, f.ai_verdict)}</Tag>
            {verdictSet && (
              <Tag color={isAI ? 'geekblue' : isHuman ? 'blue' : 'default'}>
                {isAI ? '写入方：AI' : isHuman ? '写入方：人工' : '写入方：AI（或人工未留理由，V1 契约无法区分）'}
              </Tag>
            )}
            <Typography.Text type="secondary">置信度 {f.ai_confidence || '—'}</Typography.Text>
          </Space>
          {!verdictSet && (
            <Typography.Text type="secondary">
              V1 契约说明：AI 判定与人工裁决共用同一结论字段；该发现尚未判定。
            </Typography.Text>
          )}
          {/* AI 结论原文（创建期写入，前缀标来源；人类需求：AI 结论须可见并标明是 AI 输出） */}
          {isAI && (
            <Alert
              type="info"
              showIcon
              message={`AI 结论（原文，${f.ai_reasoning?.startsWith('[DSH-sandbox]') ? '沙箱内 DSH 语义分析' : '服务端 LLM 审查'}产出）`}
              description={<Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_reasoning}</Typography.Text>}
            />
          )}
          {/* P4：人工裁决理由必须原文展示 */}
          {isHuman && (
            <Alert
              type={f.ai_verdict === 'AI_VERDICT_NEEDS_MANUAL' ? 'warning' : 'info'}
              showIcon
              message="人工裁决理由（原文）"
              description={<Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_reasoning}</Typography.Text>}
            />
          )}
          {f.ai_fix_suggestion && (
            <Card size="small" title="修复建议">
              <Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{f.ai_fix_suggestion}</Typography.Text>
              {f.ai_fix_suggestion.startsWith('MANUAL_REVIEW_REQUIRED') && (
                <Alert type="warning" showIcon style={{ marginTop: 8 }} message="该建议为“需人工处置”声明，非自动生成方案" />
              )}
            </Card>
          )}
        </Space>
      </Card>

      <Card title="人工裁决（triage）">
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
            placeholder="裁决理由（写入 finding.reasoning，proto L1240）"
          />
          <Button type="primary" loading={triage.isPending} onClick={() => triage.mutate()}>
            提交裁决
          </Button>
        </Space>
      </Card>
    </div>
  );
}

export default FindingDetailBody;
