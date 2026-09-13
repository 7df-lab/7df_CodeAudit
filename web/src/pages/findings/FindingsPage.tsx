// 发现列表（14号 §3.3 ③，P0）：GET /v1/findings?task_id=（游标分页）+ 行内快捷 triage
import dayjs from 'dayjs';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Button, Input, Select, Space, Table, Tag, Tooltip, Typography, message } from 'antd';
import { DownloadOutlined } from '@ant-design/icons';
import { useState } from 'react';
import { api, errStatus } from '../../api/client';
import FindingDetailBody, { extractDataflowTrace, isAIReasoning } from './FindingDetailBody';
import { buildFindingsCsv, downloadCsv } from '../../findings/csv';
import type { PaginationResponse, UnifiedFinding } from '../../api/types';
import { AI_VERDICT, SEVERITY, zh } from '../../dict';
import { MONO_FONT, SEVERITY_COLOR, VERDICT_COLOR } from '../../dict/tokens';

// AI 降级判定（2026-09-11 用户报障：RuleScan 兜底任务仍 COMPLETED、时间线全绿，用户无从
// 得知 AI 推理未生效）。降级痕迹只在发现级：source_rule_id 带 rulescan-fallback: 前缀，
// 或 ai_reasoning 带 [降级 前缀（系统降级标记，与 FindingDetailBody isSystemReasoning 同约定）。
export function isDegradedFinding(f: { source_rule_id?: string; ai_reasoning?: string }): boolean {
  return (f.source_rule_id ?? '').startsWith('rulescan-fallback:') || (f.ai_reasoning ?? '').startsWith('[降级');
}

export default function FindingsPage({ taskId }: { taskId: string }) {
  const qc = useQueryClient();
  const [verdictFilter, setVerdictFilter] = useState<string>('');
  const [localFilter, setLocalFilter] = useState<'all' | 'reviewed' | 'unreviewed'>('all');
  // 2026-09-09 对标竞品: 缺陷列表按严重程度/文件路径筛选 + CSV 导出
  const [severityFilter, setSeverityFilter] = useState<string>('');
  const [pathFilter, setPathFilter] = useState<string>('');
  // ADR-225 D2 双视图：来源筛选（全部=完整视图缺省 / 新发现 / 继承）——
  // 纯客户端过滤（inherited_from_task_id 空/非空），与结论/严重级筛选同链路
  const [sourceFilter, setSourceFilter] = useState<'all' | 'new' | 'inherited'>('all');

  // （审计修复）：单 cursor useQuery → useInfiniteQuery 无限查询——
  // "加载更多"翻页后前页保留（pages 累积），不再是整表替换。
  // 过滤条件不进 queryKey（审计 复核结论）：结论/严重级/路径/来源筛选全是纯客户端行为
  // （服务端 ListFindings 未接线 filter——result-service repo.List 第 4 参硬编码空串，proto
  // FilterRequest 只认 conditions 形状且网关 DiscardUnknown 静默丢弃未知字段，发 {filter:…}
  // 等于没发）。筛选在渲染层对已累计页过滤，游标分页序列不受影响，无"过滤后翻页错位"面；
  // 若进 queryKey，pathFilter 每次击键都会换 key 重拉全部分页（净回归）。
  // 对照：UsersPage 的 username_contains/state 是真服务端过滤，已在其 queryKey 中。
  const {
    data, isLoading,
    fetchNextPage, hasNextPage, isFetchingNextPage,
  } = useInfiniteQuery({
    queryKey: ['findings', taskId],
    queryFn: async ({ pageParam }) =>
      (await api.get('/v1/findings', {
        params: {
          task_id: taskId,
          pagination: { page_size: 100, cursor: pageParam },
        },
      })).data as { findings: UnifiedFinding[]; pagination: PaginationResponse },
    initialPageParam: '',
    getNextPageParam: (last) => (last.pagination.has_next ? last.pagination.next_cursor : undefined),
  });

  const quickTriage = useMutation({
    mutationFn: async ({ id, verdict }: { id: string; verdict: string }) =>
      api.put(`/v1/findings/${id}/verdict`, { verdict, reasoning: 'console quick triage' }),
    onSuccess: () => {
      message.success('结论已回写');
      qc.invalidateQueries({ queryKey: ['findings', taskId] });
      // （审计修复）：任务详情 Tabs 里本页与融合/审核视图并列——裁决改写 ai_verdict，
      // 融合去重分区/审核视图列读同一字段，不联动失效则切换 Tab 停留旧结论（前缀失效覆盖
      // ['fusion-findings', taskId] / ['review-findings', taskId]）。
      qc.invalidateQueries({ queryKey: ['fusion-findings'] });
      qc.invalidateQueries({ queryKey: ['review-findings'] });
    },
    // （审计修复）：此前失败静默——补页面现有 message.error 通道，携带状态码
    onError: (e) => {
      const status = errStatus(e);
      message.error(`结论回写失败${status ? `（HTTP ${status}）` : ''}：${(e as Error).message}`);
    },
  });

  const columns = [
    // 2026-09-09 GUI 评审: 窄容器（任务详情产出视图）下无宽度约束会把标题挤成竖排单字
    { title: '发现', dataIndex: 'title', width: 220, ellipsis: true }, // ADR-150: 审核功能内嵌行展开，不再跳独立页
    { title: '严重级', dataIndex: 'severity', render: (s: string) => <Tag color={SEVERITY_COLOR[s]}>{zh(SEVERITY, s)}</Tag> },
    { title: 'CWE', dataIndex: 'cwe_id' },
    {
      // ADR-159: 链路可用性可见——真解析 source_raw 判 dataflow_trace（非按工具名猜测），
      // 有变量级污点链路的行给"污点链路"徽标, 用户不必逐个点开试探
      title: '来源',
      dataIndex: 'source_tool',
      render: (v: string, rec: UnifiedFinding) => {
        let hasTrace = false;
        try { hasTrace = !!extractDataflowTrace(rec.source_raw); } catch { hasTrace = false; }
        return (
          <Space size={4}>
            <span>{v}</span>
            {hasTrace && <Tag color="orange" style={{ marginRight: 0 }}>污点链路</Tag>}
            {/* ADR-225: 继承项角标（增量扫描双视图的行级来源标记） */}
            {rec.inherited_from_task_id && <Tag color="purple" style={{ marginRight: 0 }}>继承</Tag>}
          </Space>
        );
      },
    },
    {
      // 2026-09-09 GUI 评审: 容器内绝对路径（/app/data/repos/...）冗长且非用户视角，
      // 显示文件名+行号，完整路径悬停可见
      title: '位置', dataIndex: 'location',
      render: (loc: UnifiedFinding['location']) => {
        if (!loc) return '—';
        const base = loc.file_path.split('/').pop();
        return <Tooltip title={`${loc.file_path}:${loc.start_line}`}><span style={{ fontFamily: MONO_FONT }}>{base}:{loc.start_line}</span></Tooltip>;
      },
    },
    // ADR-153 方案A: V1 契约 AI/人工共用 ai_verdict（proto L78/L1240），列头如实标注并悬停说明
    // 人类需求（ADR-167 补遗）：AI 结论文本直接进当前结论列并标明 AI 输出——
    // 创建期 AI 链路的 reasoning 带 [DSH-sandbox]/[LLM:] 前缀，以此标注；两行预览+悬停全文
    {
      title: (
        <Tooltip title="结论可由 AI 分析产生，也可由人工裁决覆盖（后提交者生效）。">
          当前结论
        </Tooltip>
      ),
      dataIndex: 'ai_verdict',
      render: (v: string, rec: UnifiedFinding) => {
        const aiText = isAIReasoning(rec.ai_reasoning) ? rec.ai_reasoning : '';
        return (
          <div style={{ maxWidth: 340 }}>
            <Space size={4} wrap>
              {/* 空值显式归一"未判定"（zh 已无枚举特判，B3-5；展示行为不变） */}
              <Tag color={VERDICT_COLOR[v]}>{zh(AI_VERDICT, v || 'AI_VERDICT_UNSPECIFIED')}</Tag>
              {aiText && <Tag color="geekblue" style={{ marginRight: 0 }}>AI 输出</Tag>}
            </Space>
            {aiText && (
              <Typography.Paragraph
                ellipsis={{ rows: 2, tooltip: { title: aiText, overlayInnerStyle: { whiteSpace: 'pre-wrap', maxHeight: 320, overflowY: 'auto' } } }}
                style={{ marginBottom: 0, fontSize: 12 }}
                type="secondary"
              >
                {aiText}
              </Typography.Paragraph>
            )}
          </div>
        );
      },
    },
    {
      // ADR-152: 复核状态可见性——判定后行上不止标签变化，还有判定时间
      title: '复核状态',
      dataIndex: 'ai_verdict',
      width: 130,
      render: (v: string, rec: UnifiedFinding) => {
        const reviewed = v && v !== 'AI_VERDICT_UNSPECIFIED';
        return (
          <Space size={2} direction="vertical">
            <Tag color={reviewed ? 'green' : 'default'}>{reviewed ? '已判定' : '未判定'}</Tag>
            {rec.updated_at && (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {dayjs(rec.updated_at).format('MM-DD HH:mm')}
              </Typography.Text>
            )}
          </Space>
        );
      },
    },
    {
      title: '人工复核',
      render: (_: unknown, rec: UnifiedFinding) => (
        <Space>
          {/* 行内快捷 triage 不用 primary：表格里每行一个主色按钮会稀释真正的主操作
              ；与"误报"同级，仅语义文字区分 */}
          <Button size="small"
            onClick={() => quickTriage.mutate({ id: rec.finding_id, verdict: 'AI_VERDICT_TRUE_POSITIVE' })}>
            确认
          </Button>
          <Button size="small"
            onClick={() => quickTriage.mutate({ id: rec.finding_id, verdict: 'AI_VERDICT_FALSE_POSITIVE' })}>
            误报
          </Button>
        </Space>
      ),
    },
  ];

  // ADR-152: 复核可见性客户端过滤（已判定/未判定分组）+ 具体结论精确匹配
  // （修复：此前 onChange 走 else 分支恒 setVerdictFilter('')——下拉选任何具体结论都被清空，
  // 且 filter 参数形状不契约被网关丢弃，结论筛选整条链路从未生效过）
  // pages 累积（flatMap 保序拼接）后客户端过滤（结论分组/严重级/路径/来源）
  const rows = (data?.pages.flatMap((p) => p.findings) ?? []).filter((f) => {
    const reviewed = f.ai_verdict && f.ai_verdict !== 'AI_VERDICT_UNSPECIFIED';
    if (localFilter === 'reviewed') return reviewed;
    if (localFilter === 'unreviewed') return !reviewed;
    if (verdictFilter) return f.ai_verdict === verdictFilter;
    return true;
  }).filter((f) => {
    // 2026-09-09 对标竞品: 严重程度下拉 + 文件路径包含匹配（大小写不敏感）
    if (severityFilter && f.severity !== severityFilter) return false;
    if (pathFilter && !(f.location?.file_path ?? '').toLowerCase().includes(pathFilter.toLowerCase())) return false;
    return true;
  }).filter((f) => {
    // ADR-225: 来源筛选精确对应 inherited_from_task_id 空/非空（A21.1）
    if (sourceFilter === 'new' && f.inherited_from_task_id) return false;
    if (sourceFilter === 'inherited' && !f.inherited_from_task_id) return false;
    return true;
  });

  const exportCsv = () => {
    downloadCsv(`findings-${taskId || 'all'}-${dayjs().format('YYYYMMDD-HHmmss')}.csv`, buildFindingsCsv(rows));
  };

  return (
    <div>
      <style>{'.finding-reviewed { background: rgba(82,196,26,0.06); }'}</style>
      <Space style={{ marginBottom: 12 }} wrap>
        <Typography.Text>结论：</Typography.Text>
        <Select
          style={{ width: 200 }}
          allowClear
          placeholder="全部"
          value={(() => {
            if (localFilter === 'reviewed') return '__reviewed';
            if (localFilter === 'unreviewed') return '__unreviewed';
            return verdictFilter || undefined;
          })()}
          onChange={(v) => {
            if (v === '__reviewed') setLocalFilter('reviewed');
            else if (v === '__unreviewed') setLocalFilter('unreviewed');
            else { setLocalFilter('all'); setVerdictFilter(v ?? ''); } // 具体结论选项此前恒被清空（死控制）
          }}
          options={[...Object.entries(AI_VERDICT).map(([value, label]) => ({ value, label })),
            { value: '__reviewed', label: '已判定（全部类型）' },
            { value: '__unreviewed', label: '未判定' }]}
        />
        <Typography.Text type="secondary">严重程度：</Typography.Text>
        {/*  筛选选项带色阶点——色觉与列上 Tag 同源（SEVERITY_COLOR） */}
        <Select
          style={{ width: 130 }}
          allowClear
          placeholder="全部"
          value={severityFilter || undefined}
          onChange={(v) => setSeverityFilter(v ?? '')}
          options={Object.entries(SEVERITY).map(([value, label]) => ({
            value,
            label: (
              <span>
                <span style={{ display: 'inline-block', width: 8, height: 8, borderRadius: '50%', background: SEVERITY_COLOR[value] ?? '#d9d9d9', marginRight: 6 }} />
                {label}
              </span>
            ),
            // ReactNode label 下 antd 不再自动生成 title——显式给出（测试选择器与无障碍名都依赖它）
            title: label,
          }))}
        />
        <Input
          style={{ width: 180 }}
          allowClear
          placeholder="按文件路径筛选"
          value={pathFilter}
          onChange={(e) => setPathFilter(e.target.value)}
        />
        <Typography.Text type="secondary">来源：</Typography.Text>
        <Select
          style={{ width: 120 }}
          value={sourceFilter}
          onChange={(v) => setSourceFilter(v)}
          options={[
            { value: 'all', label: '全部' },
            { value: 'new', label: '新发现' },
            { value: 'inherited', label: '继承' },
          ]}
        />
        <Button
          icon={<DownloadOutlined />}
          disabled={rows.length === 0}
          onClick={exportCsv}
        >
          导出 CSV
        </Button>
        {/* B3-1：展示服务端全量 total（首页 pagination.total），缺失回退已加载行数 */}
        <Typography.Text type="secondary">共 {data?.pages[0]?.pagination.total ?? rows.length} 条</Typography.Text>
      </Space>
      <Table
        rowKey="finding_id"
        loading={isLoading}
        dataSource={rows}
        columns={columns}
        pagination={false}
        // 2026-09-09 GUI 评审: 任务详情产出视图是窄容器, 无横向滚动会把各列头挤压成
        // 竖排单字——总宽超出即横向滚动, 列头保持可读
        scroll={{ x: 'max-content', ...(rows.length > 50 ? { y: 480 } : {}) }}
        virtual={rows.length > 50}
        rowClassName={(rec) => (rec.ai_verdict && rec.ai_verdict !== 'AI_VERDICT_UNSPECIFIED' ? 'finding-reviewed' : '')}
        expandable={{
          // ADR-150: 行展开=完整审核工作台（代码上下文/当前结论/裁决区），不再跳独立页
          expandedRowRender: (rec: UnifiedFinding) => <FindingDetailBody findingId={rec.finding_id} />,
          rowExpandable: () => true,
          // ADR-151: 默认展开箭头过小不易发现——改为明确的"风险详情"按钮
          expandIcon: (props: import('rc-table/es/interface').RenderExpandIconProps<UnifiedFinding>) => (
            <Button
              size="small"
              type={props.expanded ? 'default' : 'primary'}
              onClick={(e) => props.onExpand(props.record, e)}
            >
              {props.expanded ? '收起' : '风险详情'}
            </Button>
          ),
        }}
        locale={{ emptyText: '暂无发现（任务完成或无命中）' }}
      />
      {hasNextPage && (
        <Button style={{ marginTop: 12 }} loading={isFetchingNextPage} onClick={() => fetchNextPage()}>
          加载更多
        </Button>
      )}
    </div>
  );
}
