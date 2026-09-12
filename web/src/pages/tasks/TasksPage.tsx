// 任务列表（14号 §3.2）：GET /v1/tasks 标准服务端翻页（ADR-164；03 §5 游标+total）
// ADR-160: 项目/模式筛选——契约 L1108-1112 的 project_id/filter 字段真实生效（服务端过滤）
// 2026-09-09 对标竞品: 顶部任务统计卡带 + 行内 Stage 进度点 + 每任务发现数（总/高危）
import dayjs from 'dayjs';
import { useQuery } from '@tanstack/react-query';
import { Button, Card, Select, Space, Statistic, Table, Tag, Typography } from 'antd';
import { Link, useNavigate } from 'react-router-dom';
import { useState } from 'react';
import { api, getProjects } from '../../api/client';
import type { PaginationResponse, ScanTask, TaskStage } from '../../api/types';
import { SCAN_MODE, STAGE_STATUS, STAGE_TYPE, TASK_STATUS, zh } from '../../dict';
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

// 2026-09-09 对标竞品: Stage 进度点（行内迷你阶段进度，悬停看阶段名与状态）
const STAGE_DOT_COLOR: Record<string, string> = {
  STAGE_STATUS_COMPLETED: '#52c41a',
  STAGE_STATUS_RUNNING: '#1677ff',
  STAGE_STATUS_FAILED: '#ff4d4f',
};

function StageDots({ stages }: { stages: TaskStage[] | undefined }) {
  if (!stages?.length) return <Typography.Text type="secondary">—</Typography.Text>;
  return (
    <Space size={4} wrap>
      {stages.map((s) => (
        <span
          key={s.stage_id}
          title={`${zh(STAGE_TYPE, s.type)} · ${zh(STAGE_STATUS, s.status)}`}
          style={{
            display: 'inline-block', width: 8, height: 8, borderRadius: '50%',
            background: STAGE_DOT_COLOR[s.status] ?? '#d9d9d9',
          }}
        />
      ))}
    </Space>
  );
}

// 2026-09-09 对标竞品"漏洞数量"列: 每任务发现数（总数 + 高危及以上彩签）。
// 行级懒加载查询（每页 ≤20 个、staleTime 缓存；发现按任务隔离查询是现有契约面）。
// B4-3（审计修复）：带 pagination:{page_size:100}（契约形状分页缺省会命中服务端极小缺省页，
// >缺省条数任务发现数被截成缺省值）；计数优先消费 pagination.total（服务端权威总数），
// 缺失回退已加载行数（历史载荷兼容）。
function FindingCountCell({ taskId }: { taskId: string }) {
  const { data } = useQuery({
    queryKey: ['findings-count', taskId],
    queryFn: async () => (await api.get('/v1/findings', {
      params: { task_id: taskId, pagination: { page_size: 100 } },
    })).data as {
      findings: { severity: string }[];
      pagination?: { total?: number };
    },
    staleTime: 60_000,
  });
  if (!data) return <Typography.Text type="secondary">…</Typography.Text>;
  const total = data.pagination?.total ?? data.findings.length;
  if (total === 0) return <Typography.Text type="secondary">0</Typography.Text>;
  const high = data.findings.filter(
    (f) => f.severity === 'SEVERITY_CRITICAL' || f.severity === 'SEVERITY_HIGH',
  ).length;
  return (
    <Space size={4} style={{ whiteSpace: 'nowrap' }}>
      <span>总 {total}</span>
      {high > 0 && <Tag color="red" style={{ marginRight: 0 }}>高危 {high}</Tag>}
    </Space>
  );
}

export default function TasksPage() {
  const navigate = useNavigate();
  // ADR-160: 项目/模式筛选；ADR-164: 服务端游标翻页（offset 游标+total），改筛选回第一页
  const [projectFilter, setProjectFilter] = useState<string>('');
  const [modeFilter, setModeFilter] = useState<string>('');
  const [page, setPage] = useState(1);
  const PAGE_SIZE = 20;

  const { data: projects } = useQuery({
    queryKey: ['projects'],
    queryFn: () => getProjects(),
  });
  // 2026-09-09 用户指令"任务页应显示项目名称而不是项目ID"：task_id→项目名索引。
  // 独立 key（不与筛选下拉的 ['projects'] 混用 queryFn 形状）；page_size 拉大提升覆盖，
  // 索引未命中的项目（超出首页范围）如实回落显示 ID。
  const { data: projectsIndex } = useQuery({
    queryKey: ['projects-index'],
    queryFn: () => getProjects({ page_size: 200 }),
    staleTime: 60_000,
  });
  const projectName = (pid: string) =>
    projectsIndex?.projects.find((p) => p.project_id === pid)?.name ?? '';

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

  // 2026-09-09 对标竞品统计卡带: 任务域聚合（近 200 条内），全部来自既有一次性查询
  const { data: tasksStats } = useQuery({
    queryKey: ['tasks-stats'],
    queryFn: async () => (await api.get('/v1/tasks', {
      params: { pagination: { page_size: 200, cursor: '0' } },
    })).data as { tasks: ScanTask[] },
    staleTime: 30_000,
  });
  const stat = (pred: (t: ScanTask) => boolean) => (tasksStats?.tasks ?? []).filter(pred).length;

  const columns = [
    // 2026-09-12 用户指令"主要目的是看任务"：任务列改为弹性列（唯一不设宽度的列，
    // 吸收全部剩余宽度——此前项目/报告两列平分剩余空间，宽屏下项目列空占过大）。
    // 短 ID 下加创建时间副标，让加宽后的列有真实信息量。
    // 2026-09-09 对标竞品短 ID: gw- 前缀+24 hex 全量展示无信息量, 取尾部 6 位短码, 悬停全 ID
    {
      title: '任务', dataIndex: 'task_id',
      render: (v: string, rec: ScanTask) => (
        <div>
          <Link to={`/tasks/${v}`} title={v}>#{v.slice(-6)}</Link>
          {rec.created_at && (
            <Typography.Text type="secondary" style={{ display: 'block', fontSize: 12 }}>
              创建于 {dayjs(rec.created_at).format('MM-DD HH:mm')}
            </Typography.Text>
          )}
        </div>
      ),
    },
    {
      // 2026-09-09 用户指令: 项目列显示名称（悬停见 ID；索引未命中回落 ID，链接到项目详情）。
      // 2026-09-12 定宽 150+省略号：项目是辅助信息，不再吃弹性空间
      title: '项目', dataIndex: 'project_id', width: 150, ellipsis: true,
      render: (v: string) => {
        const name = projectName(v);
        return <Link to={`/projects/${v}`} title={v}>{name || v}</Link>;
      },
    },
    { title: '阶段', dataIndex: 'stages', width: 90, render: (_: unknown, rec: ScanTask) => <StageDots stages={rec.stages} /> },
    {
      title: '发现', dataIndex: 'task_id', width: 110,
      render: (v: string) => <FindingCountCell taskId={v} />,
    },
    { title: '模式', dataIndex: 'scan_mode', width: 90, render: (s: string) => <Tag>{zh(SCAN_MODE, s)}</Tag> },
    { title: '状态', dataIndex: 'status', width: 90, render: (s: string) => <Tag color={STATUS_COLOR[s]}>{zh(TASK_STATUS, s)}</Tag> },
    { title: '更新时间', dataIndex: 'updated_at', width: 150, render: (v: string | null) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '—') },
    {
      // 任务↔报告双向导航（对称列）：真实计数——有报告才可点（此前无报告也显示链接，误导）
      title: '报告', dataIndex: 'task_id', width: 100,
      render: (v: string) => {
        const n = reportIndex?.[v] ?? 0;
        return n > 0 ? <Link to={`/reports?task=${v}`}>{n} 份报告</Link> : <Typography.Text type="secondary">—</Typography.Text>;
      },
    },
  ];

  return (
    <div>
      {/* 2026-09-09 对标竞品统计卡带: 任务域概览（数值来自近 200 条一次性聚合） */}
      <Card size="small" style={{ marginBottom: 12 }}>
        <div style={{ display: 'flex', justifyContent: 'space-around', flexWrap: 'wrap', gap: 8 }}>
          <Statistic title="任务总数" value={(tasksStats?.tasks ?? []).length} />
          <Statistic title="已完成" value={stat((t) => t.status === 'TASK_STATUS_COMPLETED')} valueStyle={{ color: '#52c41a' }} />
          <Statistic title="进行中" value={stat((t) => !isTerminal(t.status))} valueStyle={{ color: '#1677ff' }} />
          <Statistic title="失败" value={stat((t) => t.status === 'TASK_STATUS_FAILED' || t.status === 'TASK_STATUS_DEAD')} valueStyle={{ color: '#ff4d4f' }} />
          <Statistic title="已完成占比" value={(() => {
            const all = tasksStats?.tasks ?? [];
            if (all.length === 0) return '—';
            const done = all.filter((t) => t.status === 'TASK_STATUS_COMPLETED').length;
            return `${Math.round((done / all.length) * 100)}%`;
          })()} />
        </div>
      </Card>
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
          进行中的任务在详情页自动刷新进度（10s）
        </Typography.Text>
      )}
    </div>
  );
}
