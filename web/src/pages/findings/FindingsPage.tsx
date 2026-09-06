// 发现列表（14号 §3.3 ③，P0）：GET /v1/findings?task_id=（游标分页）+ 行内快捷 triage
import dayjs from 'dayjs';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Select, Space, Table, Tag, Tooltip, Typography, message } from 'antd';
import { useState } from 'react';
import { api } from '../../api/client';
import FindingDetailBody, { extractDataflowTrace, isAIReasoning, VERDICT_COLOR } from './FindingDetailBody';
import type { PaginationResponse, UnifiedFinding } from '../../api/types';
import { AI_VERDICT, SEVERITY, zh } from '../../dict';

const SEVERITY_COLOR: Record<string, string> = {
  SEVERITY_CRITICAL: 'volcano', SEVERITY_HIGH: 'red', SEVERITY_MEDIUM: 'orange', SEVERITY_LOW: 'gold',
};

export default function FindingsPage({ taskId }: { taskId: string }) {
  const qc = useQueryClient();
  const [cursor, setCursor] = useState('');
  const [verdictFilter, setVerdictFilter] = useState<string>('');
  const [localFilter, setLocalFilter] = useState<'all' | 'reviewed' | 'unreviewed'>('all');

  const { data, isLoading } = useQuery({
    // 结论筛选是纯客户端行为（filter 不进 queryKey）——服务端 ListFindings 未接线
    // filter（result-service repo.List 第 4 参硬编码空串），proto FilterRequest 只认
    // conditions 形状且网关 DiscardUnknown 静默丢弃未知字段，发 {filter:{ai_verdict}}
    // 等于没发。已判定/未判定分组同为客户端过滤。
    queryKey: ['findings', taskId, cursor],
    queryFn: async () =>
      (await api.get('/v1/findings', {
        params: {
          task_id: taskId,
          pagination: { page_size: 100, cursor },
        },
      })).data as { findings: UnifiedFinding[]; pagination: PaginationResponse },
  });

  const quickTriage = useMutation({
    mutationFn: async ({ id, verdict }: { id: string; verdict: string }) =>
      api.put(`/v1/findings/${id}/verdict`, { verdict, reasoning: 'console quick triage' }),
    onSuccess: () => {
      message.success('结论已回写（finding.verdict.updated 事件链由后端触发）');
      qc.invalidateQueries({ queryKey: ['findings', taskId] });
    },
  });

  const columns = [
    { title: '发现', dataIndex: 'title' }, // ADR-150: 审核功能内嵌行展开，不再跳独立页
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
          </Space>
        );
      },
    },
    {
      title: '位置', dataIndex: 'location',
      render: (loc: UnifiedFinding['location']) => (loc ? `${loc.file_path}:${loc.start_line}` : '—'),
    },
    // ADR-153 方案A: V1 契约 AI/人工共用 ai_verdict（proto L78/L1240），列头如实标注并悬停说明
    // 人类需求（ADR-167 补遗）：AI 结论文本直接进当前结论列并标明 AI 输出——
    // 创建期 AI 链路的 reasoning 带 [DSH-sandbox]/[LLM:] 前缀，以此标注；两行预览+悬停全文
    {
      title: (
        <Tooltip title="V1 契约：AI 判定与人工裁决共用同一结论字段（后写者生效）。判定来源区分待 proto V2.1（ADR-153）。">
          当前结论
        </Tooltip>
      ),
      dataIndex: 'ai_verdict',
      render: (v: string, rec: UnifiedFinding) => {
        const aiText = isAIReasoning(rec.ai_reasoning) ? rec.ai_reasoning : '';
        return (
          <div style={{ maxWidth: 340 }}>
            <Space size={4} wrap>
              <Tag color={VERDICT_COLOR[v]}>{zh(AI_VERDICT, v)}</Tag>
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
          <Button size="small" type="primary"
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
  const rows = (data?.findings ?? []).filter((f) => {
    const reviewed = f.ai_verdict && f.ai_verdict !== 'AI_VERDICT_UNSPECIFIED';
    if (localFilter === 'reviewed') return reviewed;
    if (localFilter === 'unreviewed') return !reviewed;
    if (verdictFilter) return f.ai_verdict === verdictFilter;
    return true;
  });

  return (
    <div>
      <style>{'.finding-reviewed { background: rgba(82,196,26,0.06); }'}</style>
      <Space style={{ marginBottom: 12 }}>
        <Typography.Text>结论筛选：</Typography.Text>
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
      </Space>
      <Table
        rowKey="finding_id"
        loading={isLoading}
        dataSource={rows}
        columns={columns}
        pagination={false}
        virtual={rows.length > 50}
        scroll={rows.length > 50 ? { y: 480, x: 'max-content' } : undefined}
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
      {data?.pagination?.has_next && (
        <Button style={{ marginTop: 12 }} onClick={() => setCursor(data.pagination.next_cursor)}>
          加载更多
        </Button>
      )}
    </div>
  );
}
