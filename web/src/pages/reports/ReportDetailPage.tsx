// 报告阅读视图（/reports/:id，2026-10-08 报告中心人性化批）：把 E-29① 取回的报告
// 正文从"裸 JSON"变成可读的审计文书。三段式——报告头 → 风险概览（审计句+严重度
// 光谱带）→ 发现阅读卡（可展开碳黑代码证据）。分型以内容嗅探为准（reportContent.ts），
// HTML 档不直染（脚本面收口纪律），引导既有沙箱窗渲染。
// 设计令牌纪律（docs/design-tokens.md）：个性只花在光谱带与证据面——severity 实心
// 六级、EVIDENCE 碳黑面板、MONO_FONT 机器标识；其余保持 antd 克制，零图表库。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Drawer, Empty, Space, Tag, Tooltip, Typography, message } from 'antd';
import { useMemo, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import dayjs from 'dayjs';
import { api, errStatus, getReportContent, listAllProjects, listAllTasks, openReportWindow, regenerateReport } from '../../api/client';
import { SEVERITY, zh } from '../../dict';
import { MONO_FONT, SEVERITY_COLOR } from '../../dict/tokens';
import { VERDICT_STATE_COLOR, VERDICT_STATE_LABEL } from '../../dict/verdict';
import { EVIDENCE } from '../../theme/evidence';
import PageHeader from '../../components/PageHeader';
import { EmptyState, PageLoading, QueryError } from '../../components/states';
import { usePageTitle } from '../../hooks/usePageTitle';
import { countBySeverity, parseReportContent, SEVERITY_LADDER, severityBucket, type ReportFinding } from './reportContent';

const shortId = (id: string) => `#${id.slice(-6)}`;
const baseName = (p: string) => p.split('/').pop() || p;

// ---- 签名元素：严重度光谱带 ----
// 全宽比例堆叠条：每段宽度=该级计数占比、色取 severity 实心阶梯（UNSPECIFIED 无色级用
// 中性灰）；段与下方图例均可点选过滤（选中=实色，滤除=半透明）。宽度编码的是真实分布
// 信息，兼作过滤器——不是装饰。零计数级不占段，图例如实标 0 且不可点。
function SeveritySpectrum({ findings, excluded, onToggle }: {
  findings: ReportFinding[];
  excluded: Set<string>;
  onToggle: (key: string) => void;
}) {
  const counts = useMemo(() => countBySeverity(findings), [findings]);
  const total = findings.length;
  const present = counts.filter((c) => c.count > 0);
  return (
    <div data-testid="severity-spectrum">
      <div style={{ display: 'flex', height: 14, borderRadius: 4, overflow: 'hidden', background: '#F0F0F0' }} role="img"
        aria-label={present.map((c) => `${zh(SEVERITY, c.key)} ${c.count}`).join('，')}>
        {present.map((c, idx) => {
          const off = excluded.has(c.key);
          return (
            <button
              key={c.key}
              type="button"
              onClick={() => onToggle(c.key)}
              aria-pressed={!off}
              title={`${zh(SEVERITY, c.key)} ${c.count} 条（点击${off ? '恢复' : '滤出'}）`}
              style={{
                width: total ? `${(c.count / total) * 100}%` : 0,
                minWidth: 6, // 计数≥1 的段至少可点（占比再小也不消失）
                // 段间白缝：高危橙红与中危橙两相邻段色相近，无分隔时边界感知模糊
                // （dev:mock 走查 2026-10-08）——色值不动（severity 阶梯是 SSOT）
                border: 0, borderTop: 0, borderBottom: 0,
                borderLeft: 0,
                borderRight: idx < present.length - 1 ? '2px solid #fff' : 0,
                boxSizing: 'border-box',
                cursor: 'pointer', padding: 0,
                background: SEVERITY_COLOR[c.key] ?? '#BFBFBF',
                opacity: off ? 0.18 : 1,
                transition: 'opacity 120ms',
              }}
            />
          );
        })}
      </div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: '4px 16px', marginTop: 8 }}>
        {counts.map((c) => {
          const selectable = c.count > 0;
          const off = excluded.has(c.key);
          return (
            <button
              key={c.key}
              type="button"
              disabled={!selectable}
              onClick={() => onToggle(c.key)}
              aria-pressed={selectable && !off}
              style={{
                border: 0, background: 'none', padding: 0, cursor: selectable ? 'pointer' : 'default',
                display: 'inline-flex', alignItems: 'center', gap: 6,
                opacity: !selectable ? 0.45 : off ? 0.4 : 1,
              }}
            >
              <span style={{
                width: 10, height: 10, borderRadius: 2, flex: '0 0 auto',
                background: SEVERITY_COLOR[c.key] ?? '#BFBFBF',
                textDecoration: off ? 'line-through' : 'none',
              }} />
              <Typography.Text style={{ fontSize: 13, textDecoration: off ? 'line-through' : 'none' }}>
                {zh(SEVERITY, c.key)} {c.count}
              </Typography.Text>
            </button>
          );
        })}
      </div>
    </div>
  );
}

// 代码证据面板（碳黑证据面，与发现详情/日志面板同一嗓音）：行号+命中行高亮。
// 展示底稿优先 context 窗口（行号真实），无窗口时按 text 逐行展示（老档 code 自带
// 行号前缀，原样直显不臆造行号）。
function EvidencePanel({ finding }: { finding: ReportFinding }) {
  const ev = finding.evidence!;
  const rows = ev.context
    ? ev.context.lines
    : ev.text.split('\n');
  const startLine = ev.context ? ev.context.startLine : 0;
  return (
    <div>
      <pre data-testid="evidence-code" style={{
        background: EVIDENCE.bg, color: EVIDENCE.text, fontFamily: MONO_FONT,
        padding: 12, borderRadius: 6, overflowX: 'auto', fontSize: 13, lineHeight: 1.6, margin: 0,
      }}>
        {rows.map((line, i) => {
          const ln = startLine ? startLine + i : null;
          const isMatch = !!ln && ev.matchedLine > 0 && ln === ev.matchedLine;
          return (
            <div key={i} data-line={ln ?? undefined}
              style={{ display: 'flex', background: isMatch ? EVIDENCE.matchBg : undefined }}>
              <span style={{ width: 44, flex: '0 0 auto', textAlign: 'right', marginRight: 12, color: EVIDENCE.textMuted, userSelect: 'none' }}>
                {ln ?? ''}
              </span>
              <span style={{ whiteSpace: 'pre-wrap', color: isMatch ? EVIDENCE.matchText : undefined }}>{line}</span>
            </div>
          );
        })}
      </pre>
      <Typography.Text type="secondary" style={{ display: 'block', marginTop: 6 }}>
        扫描时捕获的代码片段{ev.matchedLine > 0 ? `，高亮行为报告标记位置（第 ${ev.matchedLine} 行）` : ''}。
      </Typography.Text>
    </div>
  );
}

// 发现阅读卡：title 是长段审计叙述——卡片而非表格（表格会杀掉可读性）；证据默认收起，
// 最危首卡默认展开（进入即见证据面的产品嗓音）。
function FindingCard({ finding, expanded, onToggleExpand }: {
  finding: ReportFinding;
  expanded: boolean;
  onToggleExpand: () => void;
}) {
  const hasEvidence = !!finding.evidence;
  return (
    <Card size="small" style={{ marginBottom: 12 }} data-testid={`finding-card-${finding.findingId}`}>
      <div style={{ display: 'flex', gap: 8, marginBottom: 8, flexWrap: 'wrap', alignItems: 'center' }}>
        <Tag color={SEVERITY_COLOR[finding.severity]} style={{ marginInlineEnd: 0 }}>
          {zh(SEVERITY, finding.severity)}
        </Tag>
        <Tag color={VERDICT_STATE_COLOR[finding.verdictState]} style={{ marginInlineEnd: 0 }}>
          {VERDICT_STATE_LABEL[finding.verdictState]}
        </Tag>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {finding.cwe ? `CWE ${finding.cwe.replace(/^CWE-/, '')}` : 'CWE —'} {/* 数据侧已带 CWE- 前缀，防 CWE CWE-78 冗余 */}
        </Typography.Text>
      </div>
      <Typography.Paragraph style={{ marginBottom: 8, whiteSpace: 'pre-wrap' }}>
        {finding.title}
      </Typography.Paragraph>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <Space size={16} wrap>
          {finding.file ? (
            <Tooltip title={finding.file}>
              <Typography.Text style={{ fontFamily: MONO_FONT, fontSize: 13 }}>
                {baseName(finding.file)}{finding.line > 0 ? `:${finding.line}` : ''}
              </Typography.Text>
            </Tooltip>
          ) : (
            <Typography.Text type="secondary">位置未记录</Typography.Text>
          )}
          {finding.ruleId && (
            <Tooltip title={`规则 ${finding.ruleId}`}>
              <Typography.Text type="secondary" style={{ fontFamily: MONO_FONT, fontSize: 12 }}>
                {finding.ruleId}
              </Typography.Text>
            </Tooltip>
          )}
        </Space>
        {hasEvidence && (
          <Button size="small" type="link" onClick={onToggleExpand} aria-expanded={expanded}>{/* ADR-257 批: 两态统一 link 样式 */}
            {expanded ? '收起代码证据' : '展开代码证据'}
          </Button>
        )}
      </div>
      {hasEvidence && expanded && <div style={{ marginTop: 12 }}><EvidencePanel finding={finding} /></div>}
      {!hasEvidence && (
        <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8, fontSize: 12 }}>
          该发现未携带代码片段。
        </Typography.Text>
      )}
    </Card>
  );
}

export default function ReportDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [excluded, setExcluded] = useState<Set<string>>(new Set());
  const [expandedIds, setExpandedIds] = useState<Set<string> | null>(null); // null=首卡展开态（未手工干预）
  const [rawOpen, setRawOpen] = useState(false);
  usePageTitle(`报告 ${shortId(id)}`);

  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: ['report-content', id],
    queryFn: () => getReportContent(id),
    retry: false,
    staleTime: 60_000,
  });

  // 项目名解析（与报告中心列表/任务页统计共用 ['tasks-all']/['projects-index'] 缓存键——
  // 从列表点进来时零额外请求；深链未命中如实显示任务 ID；listAllTasks 全量翻页，
  // 老任务不再落索引外）
  const { data: tasksIndex } = useQuery({
    queryKey: ['tasks-all'],
    queryFn: listAllTasks,
    staleTime: 60_000,
  });
  const { data: projectsIndex } = useQuery({
    queryKey: ['projects-index'],
    queryFn: async () => ({ projects: await listAllProjects() }),
    staleTime: 60_000,
  });

  const regenerate = useMutation({
    mutationFn: regenerateReport,
    onSuccess: () => {
      message.success('报告生成请求已提交');
      qc.invalidateQueries({ queryKey: ['reports'] });
      qc.invalidateQueries({ queryKey: ['report-content', id] });
    },
    onError: (e) => {
      const status = errStatus(e);
      message.error(`重新生成失败${status ? `（HTTP ${status}）` : ''}：${(e as Error).message}`);
    },
  });

  const parsed = useMemo(() => (data ? parseReportContent(data.content) : null), [data]);

  const download = async () => {
    try {
      const resp = await api.get(`/v1/reports/${id}/download`, { responseType: 'blob' });
      const url = URL.createObjectURL(resp.data as Blob);
      const a = document.createElement('a');
      a.href = url;
      // 扩展名按内容嗅探（本页无列表行 format 枚举；UNSPECIFIED 历史档实证为 JSON）
      const ext = parsed?.kind === 'html' ? 'html' : parsed?.kind === 'text' ? 'txt' : 'json';
      a.download = `${id}.${ext}`;
      a.click();
      URL.revokeObjectURL(url);
    } catch {
      message.error('下载失败（报告可能不存在或后端不可达）');
    }
  };

  if (isLoading) return <PageLoading tip="正在取回报告正文" />;
  if (isError) return <QueryError error={error} onRetry={() => refetch()} />;
  if (!parsed) return <PageLoading />;

  // ---- 非 JSON 分型：诚实降级，不把 HTML/CSV 塞进阅读视图假装解析 ----
  if (parsed.kind === 'empty') {
    return (
      <div>
        <PageHeader title="审计报告" extra={<Button onClick={() => navigate('/reports')}>返回报告中心</Button>} />
        <EmptyState
          description="该报告没有正文（生成失败或已被清除）"
          action={<Button type="primary" onClick={() => navigate('/reports')}>回报告中心重新生成</Button>}
        />
      </div>
    );
  }
  if (parsed.kind === 'html' || parsed.kind === 'text') {
    const isHtml = parsed.kind === 'html';
    return (
      <div>
        <PageHeader
          title={`报告 ${shortId(id)}`}
          extra={(
            <>
              <Button onClick={() => navigate('/reports')}>返回报告中心</Button>
              {isHtml && (
                <Button
                  onClick={() => {
                    const opened = openReportWindow(data!.content, 'text/html');
                    if (!opened) message.warning('弹出窗口被浏览器拦截，请允许弹出窗口后重试');
                  }}
                >
                  在新窗口渲染
                </Button>
              )}
              <Button onClick={download}>下载原文件</Button>
            </>
          )}
        />
        {isHtml ? (
          <Alert
            style={{ marginBottom: 16 }}
            type="info"
            showIcon
            message="该报告是 HTML 渲染档"
            description="HTML 报告为排版成品，不在站内直染（脚本与外链资源统一收口）；原文如下，可用「在新窗口渲染」以沙箱窗查看成品版式。"
          />
        ) : (
          <Alert
            style={{ marginBottom: 16 }}
            type="info"
            showIcon
            message="该报告不是结构化 JSON 档"
            description="原文如下直显；如需图表化阅读视图，请用 JSON 格式重新生成报告。"
          />
        )}
        <Card size="small" title="报告原文">
          <pre data-testid="raw-content" style={{
            whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', fontSize: 13,
            fontFamily: isHtml ? undefined : MONO_FONT, margin: 0, maxHeight: 640, overflowY: 'auto',
          }}>
            {data!.content}
          </pre>
        </Card>
      </div>
    );
  }

  // ---- JSON 阅读视图 ----
  const { doc } = parsed;
  const sevRank = (s: string) => {
    const i = (SEVERITY_LADDER as readonly string[]).indexOf(s);
    return i === -1 ? SEVERITY_LADDER.length : i; // 未知归末位
  };
  const sorted = [...doc.findings].sort((a, b) =>
    sevRank(a.severity) - sevRank(b.severity) || a.line - b.line);
  const visible = excluded.size === 0
    ? sorted
    : sorted.filter((f) => !excluded.has(severityBucket(f.severity)));
  const projectId = tasksIndex?.find((t) => t.task_id === doc.taskId)?.project_id ?? '';
  const projectName = projectId
    ? projectsIndex?.projects.find((p) => p.project_id === projectId)?.name ?? projectId
    : '';
  // 默认展开最危首卡（手工干预后不再自动跟随过滤变化）
  const effectiveExpanded = expandedIds ?? new Set(visible[0] ? [visible[0].findingId] : []);
  const toggleSeverity = (key: string) => setExcluded((prev) => {
    const next = new Set(prev);
    if (next.has(key)) next.delete(key); else next.add(key);
    return next;
  });
  const allExpanded = visible.length > 0 && visible.every((f) => effectiveExpanded.has(f.findingId));

  return (
    <div>
      <PageHeader
        title={<Tooltip title={id}>审计报告 {shortId(id)}</Tooltip>}
        extra={(
          <>
            <Button onClick={() => setRawOpen(true)}>原始 JSON</Button>
            <Button onClick={download}>下载原文件</Button>
            <Button
              disabled={!doc.taskId}
              title={doc.taskId ? undefined : '报告未记录任务 ID，无法重发生成'}
              loading={regenerate.isPending}
              onClick={() => regenerate.mutate(doc.taskId)}
            >
              重新生成
            </Button>
          </>
        )}
      />

      {/* 报告头：一句元信息行（机器标识等宽、人类名称普通嗓音） */}
      <Typography.Paragraph type="secondary" style={{ marginTop: -4, marginBottom: 16 }}>
        任务{' '}
        {doc.taskId
          ? <Link to={`/tasks/${doc.taskId}`} style={{ fontFamily: MONO_FONT }}>{doc.taskId}</Link>
          : '未记录'}
        {projectName && <> · 项目 {projectId ? <Link to={`/projects/${projectId}`}>{projectName}</Link> : projectName}</>}
        {doc.generatedAt && <> · 生成于 {dayjs(doc.generatedAt).format('YYYY-MM-DD HH:mm:ss')}</>}
        {' '}· 共 {doc.findings.length} 条发现
      </Typography.Paragraph>

      {/* 风险概览：复核结论审计句 + 严重度光谱带（签名元素） */}
      <Card size="small" style={{ marginBottom: 16 }} title="风险概览">
        <Typography.Paragraph style={{ marginBottom: 12 }}>
          复核结论——确认真 <Typography.Text strong type="danger">{doc.summary.truePositives}</Typography.Text>
          {' · '}误报 <Typography.Text strong>{doc.summary.falsePositives}</Typography.Text>
          {' · '}待复核 <Typography.Text strong type="warning">{doc.summary.notReviewed}</Typography.Text>
          <Typography.Text type="secondary">（服务端按人工复核口径统计）</Typography.Text>
        </Typography.Paragraph>
        <SeveritySpectrum findings={doc.findings} excluded={excluded} onToggle={toggleSeverity} />
      </Card>

      {/* 发现阅读卡列表 */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
        <Typography.Text strong>
          发现列表{excluded.size > 0 && <Typography.Text type="secondary">（已滤出 {visible.length}/{doc.findings.length} 条）</Typography.Text>}
        </Typography.Text>
        {visible.length > 0 && (
          <Button
            size="small"
            onClick={() => setExpandedIds(allExpanded ? new Set() : new Set(visible.map((f) => f.findingId)))}
          >
            {allExpanded ? '全部收起' : '全部展开'}
          </Button>
        )}
      </div>
      {doc.findings.length === 0 ? (
        <Empty
          image={Empty.PRESENTED_IMAGE_SIMPLE}
          description="本次扫描未产出任何发现——没有可阅读的风险项。"
        />
      ) : visible.length === 0 ? (
        <EmptyState description="全部严重级已被滤出——点击光谱带图例恢复显示" />
      ) : (
        visible.map((f) => (
          <FindingCard
            key={f.findingId}
            finding={f}
            expanded={effectiveExpanded.has(f.findingId)}
            onToggleExpand={() => setExpandedIds((prev) => {
              const base = prev ?? new Set(effectiveExpanded);
              const next = new Set(base);
              if (next.has(f.findingId)) next.delete(f.findingId); else next.add(f.findingId);
              return next;
            })}
          />
        ))
      )}

      <Drawer
        title="原始 JSON"
        placement="right"
        width={520}
        open={rawOpen}
        onClose={() => setRawOpen(false)}
      >
        <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>
          报告正文原文（与下载文件一致），供核对与留档。
        </Typography.Paragraph>
        <pre data-testid="raw-json" style={{
          whiteSpace: 'pre-wrap', overflowWrap: 'anywhere', fontSize: 12.5,
          fontFamily: MONO_FONT, margin: 0,
        }}>
          {data!.content}
        </pre>
      </Drawer>
    </div>
  );
}
