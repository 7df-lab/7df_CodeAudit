// 任务列表（14号 §3.2）：GET /v1/tasks 标准服务端翻页（ADR-164；03 §5 游标+total）
// ADR-160: 项目/模式筛选——契约 L1108-1112 的 project_id/filter 字段真实生效（服务端过滤）
import dayjs from 'dayjs';
import { useQuery } from '@tanstack/react-query';
import { Button, Select, Space, Table, Tag, Typography } from 'antd';
import { Link, useNavigate } from 'react-router-dom';
import { useState } from 'react';
import { api } from '../../api/client';
import type { PaginationResponse, ScanTask } from '../../api/types';
import { SCAN_MODE, TASK_STATUS, zh } from '../../dict';
import { isTerminal } from '../../tasks/stateMachine';

const STATUS_COLOR: Record<string, string> = {
  TASK_STATUS_COMPLETED: 'green',
  TASK_STATUS_RUNNING: 'blue',
  TASK_STATUS_FAILED: 'red',
  TASK_STATUS_DEAD: 'red',
  TASK_STATUS_TIMEOUT: 'orange',
  TASK_STATUS_CANCELLED: 'default',
  TASK_STATUS_PENDING: 'gold',
  TASK_STATUS_QUEUED: 'cyan',
  TASK_STATUS_CREATED: 'default',
};

export default function TasksPage() {
  const navigate = useNavigate();
  // ADR-160: 项目/模式筛选；ADR-164: 服务端游标翻页（offset 游标+total），改筛选回第一页
  const [projectFilter, setProjectFilter] = useState<string>('');
  const [modeFilter, setModeFilter] = useState<string>('');
  const [page, setPage] = useState(1);
  const PAGE_SIZE = 20;

  const { data: projects } = useQuery({
    queryKey: ['projects'],
    queryFn: async () => (await api.get('/v1/projects')).data as { projects: { project_id: string; name: string }[] },
  });

  // 任务分页（ADR-142 曾改"加载更多"绕开游标不生效；ADR-155 修复游标序列化后，
  // ADR-164 升级为标准服务端翻页——契约 L1108-1112 + PaginationResponse.total）
  const { data, isLoading } = useQuery({
    queryKey: ['tasks-page', projectFilter, modeFilter, page],
    queryFn: async () => (await api.get('/v1/tasks', {
      params: {
        ...(projectFilter ? { project_id: projectFilter } : {}),
        ...(modeFilter
          ? { filter: { conditions: [{ field: 'scan_mode', operator: 'FILTER_OPERATOR_EQ', value: modeFilter }] } }
          : {}),
        pagination: { page_size: PAGE_SIZE, cursor: String((page - 1) * PAGE_SIZE) },
      },
    })).data as {
      tasks: ScanTask[];
      pagination: PaginationResponse;
    },
  });
  const rows = data?.tasks ?? [];
  const total = data?.pagination?.total ?? 0;

  // 报告索引（ADR-142 对称列真实性）：task_id → 报告数。一次拉取，避免逐任务查询。
  const { data: reportIndex } = useQuery({
    queryKey: ['reports-index'],
    queryFn: async () => {
      const rs = (await api.get('/v1/reports', { params: { pagination: { page_size: 100 } } })).data as {
        reports: { task_id: string }[];
      };
      const m: Record<string, number> = {};
      for (const r of rs.reports) m[r.task_id] = (m[r.task_id] ?? 0) + 1;
      return m;
    },
    staleTime: 30_000,
  });

  const columns = [
    { title: '任务', dataIndex: 'task_id', render: (v: string) => <Link to={`/tasks/${v}`}>{v}</Link> },
    { title: '项目', dataIndex: 'project_id' },
    { title: '模式', dataIndex: 'scan_mode', width: 90, render: (s: string) => <Tag>{zh(SCAN_MODE, s)}</Tag> },
    { title: '状态', dataIndex: 'status', render: (s: string) => <Tag color={STATUS_COLOR[s]}>{zh(TASK_STATUS, s)}</Tag> },
    { title: '更新时间', dataIndex: 'updated_at', render: (v: string | null) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '—') },
    {
      // 任务↔报告双向导航（对称列）：真实计数——有报告才可点（此前无报告也显示链接，误导）
      title: '报告',
      dataIndex: 'task_id',
      render: (v: string) => {
        const n = reportIndex?.[v] ?? 0;
        return n > 0 ? <Link to={`/reports?task=${v}`}>{n} 份报告</Link> : <Typography.Text type="secondary">—</Typography.Text>;
      },
    },
  ];

  return (
    <div>
      <Space style={{ marginBottom: 12, justifyContent: 'space-between', width: '100%' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>任务</Typography.Title>
        <Button type="primary" onClick={() => navigate('/tasks/new')}>新建任务</Button>
      </Space>
      {/* ADR-160: 项目/模式筛选（服务端过滤；后端未实现的字段会诚实报 400，不静默忽略） */}
      <Space style={{ marginBottom: 12 }} wrap>
        <Typography.Text type="secondary">筛选：</Typography.Text>
        <Select
          allowClear placeholder="全部项目" style={{ width: 240 }}
          value={projectFilter || undefined}
          onChange={(v) => { setProjectFilter(v ?? ''); setPage(1); }}
          options={(projects?.projects ?? []).map((p) => ({ value: p.project_id, label: `${p.name} (${p.project_id})` }))}
        />
        <Select
          allowClear placeholder="全部模式" style={{ width: 220 }}
          value={modeFilter || undefined}
          onChange={(v) => { setModeFilter(v ?? ''); setPage(1); }}
          options={Object.entries(SCAN_MODE)
            .filter(([k]) => k !== 'SCAN_MODE_UNSPECIFIED')
            .map(([value, label]) => ({ value, label }))}
        />
      </Space>
      <Table
        rowKey="task_id"
        loading={isLoading}
        dataSource={rows}
        columns={columns}
        pagination={{
          current: page,
          pageSize: PAGE_SIZE,
          total,
          onChange: (p) => setPage(p),
          showSizeChanger: false,
        }}
        locale={{ emptyText: '暂无任务——点右上角"新建任务"' }}
      />
      {rows.length > 0 && !isTerminal(rows[0].status) && (
        <Typography.Text type="secondary" style={{ display: 'block', marginTop: 8 }}>
          进行中的任务在详情页自动刷新进度（3s）
        </Typography.Text>
      )}
    </div>
  );
}
