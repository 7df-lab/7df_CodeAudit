// 总览（首页仪表盘）：登录后的态势全貌——
// 跨项目的工作该从哪里继续看起。数据全部来自既有契约面（projects/tasks/reports/
// notifications 一次性查询，30s staleTime，无新增轮询）；状态分布用 antd Progress
// 纯组件渲染（控制台不引入图表库）。
import { useQuery } from '@tanstack/react-query';
import { Badge, Button, Card, Col, List, Progress, Row, Statistic, Table, Tag, Typography } from 'antd';
import { Link, useNavigate } from 'react-router-dom';
import dayjs from 'dayjs';
import { api, getProjects } from '../../api/client';
import type { ReportRow, ScanTask } from '../../api/types';
import { REPORT_FORMAT, SCAN_MODE, TASK_STATUS, zh } from '../../dict';
import { STATUS_COLOR, MONO_FONT } from '../../dict/tokens';
import PageHeader from '../../components/PageHeader';
import { EmptyState, PageLoading } from '../../components/states';
import { usePageTitle } from '../../hooks/usePageTitle';
import { useSession } from '../../auth/session';

// 状态分布展示序：进行中 → 出问题的（失败/超时/暂停）→ 完成 → 其它；零计数不显示
const DIST_ORDER: { keys: string[]; color: string }[] = [
  { keys: ['TASK_STATUS_RUNNING', 'TASK_STATUS_QUEUED', 'TASK_STATUS_CREATED'], color: '#3056D3' },
  { keys: ['TASK_STATUS_FAILED', 'TASK_STATUS_DEAD'], color: '#ff4d4f' },
  { keys: ['TASK_STATUS_TIMEOUT', 'TASK_STATUS_PAUSED'], color: '#faad14' },
  { keys: ['TASK_STATUS_COMPLETED'], color: '#52c41a' },
];

function shortId(id: string): string {
  return `#${id.slice(-6)}`;
}

export default function DashboardPage() {
  usePageTitle('总览');
  const navigate = useNavigate();
  const { user } = useSession();

  // 项目计数（分页 total 为权威；列表本身供"最近任务"项目名索引）
  const { data: projResp } = useQuery({
    queryKey: ['projects', 'dash'],
    queryFn: () => getProjects({ page_size: 100, cursor: '0' }),
    staleTime: 60_000,
  });
  const projectName = (pid: string) =>
    projResp?.projects.find((p) => p.project_id === pid)?.name ?? pid;

  // 任务域：近 200 条一次性聚合（同任务页统计口径）
  const { data: taskResp, isLoading: taskLoading } = useQuery({
    queryKey: ['tasks-dash'],
    queryFn: async () => (await api.get('/v1/tasks', {
      params: { pagination: { page_size: 200, cursor: '0' } },
    })).data as { tasks: ScanTask[] },
    staleTime: 30_000,
  });
  const tasks = taskResp?.tasks ?? [];
  const recentTasks = tasks.slice(0, 8);

  // 报告/通知
  const { data: reportResp, isLoading: reportLoading } = useQuery({
    queryKey: ['reports-dash'],
    queryFn: async () => (await api.get('/v1/reports', {
      params: { pagination: { page_size: 100, cursor: '0' } },
    })).data as { reports: ReportRow[]; pagination?: { total?: number } },
    staleTime: 30_000,
  });
  const reports = reportResp?.reports ?? [];
  const { data: notifyResp } = useQuery({
    queryKey: ['notifications', user?.user_id],
    queryFn: async () =>
      (await api.get('/v1/notifications', { params: { user_id: user?.user_id ?? '' } })).data as {
        notifications: { notification_id: string; title: string; read: boolean; created_at: string | null }[];
      },
    enabled: !!user,
  });
  const notifications = notifyResp?.notifications ?? [];
  const unread = notifications.filter((n) => !n.read);
  const unreadCount = unread.length;

  // 统计带
  const running = tasks.filter((t) => !['TASK_STATUS_COMPLETED', 'TASK_STATUS_CANCELLED', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_DEAD', 'TASK_STATUS_FAILED'].includes(t.status)).length;
  const completed = tasks.filter((t) => t.status === 'TASK_STATUS_COMPLETED').length;
  const failed = tasks.filter((t) => t.status === 'TASK_STATUS_FAILED' || t.status === 'TASK_STATUS_DEAD').length;

  // 状态分布（分组计数；组内合计为零的组不渲染）
  const dist = DIST_ORDER.map((g) => ({
    ...g,
    count: tasks.filter((t) => g.keys.includes(t.status)).length,
    label: zh(TASK_STATUS, g.keys[0]),
  })).filter((g) => g.count > 0);

  const statBand = (
    <Card size="small" style={{ marginBottom: 16 }}>
      <div style={{ display: 'flex', justifyContent: 'space-around', flexWrap: 'wrap', gap: 8 }}>
        <Statistic title="项目" value={projResp?.pagination?.total ?? projResp?.projects.length ?? 0} />
        <Statistic title="任务总数" value={tasks.length} />
        <Statistic title="进行中" value={running} valueStyle={{ color: '#3056D3' }} />
        <Statistic title="已完成" value={completed} valueStyle={{ color: '#52c41a' }} />
        <Statistic title="失败" value={failed} valueStyle={{ color: '#ff4d4f' }} />
        <Statistic title="报告" value={reportResp?.pagination?.total ?? reports.length} />
      </div>
    </Card>
  );

  return (
    <div>
      <PageHeader
        title="总览"
        extra={(
          <>
            <Button onClick={() => navigate('/projects?new=1')}>新建项目</Button>
            <Button type="primary" onClick={() => navigate('/tasks/new')}>新建任务</Button>
          </>
        )}
      />
      {statBand}
      <Row gutter={16}>
        <Col xs={24} xl={16}>
          <Card
            size="small"
            title="任务状态分布"
            style={{ marginBottom: 16 }}
            loading={taskLoading}
          >
            {dist.length === 0 ? (
              <EmptyState
                description="还没有任何任务"
                action={<Button type="primary" size="small" onClick={() => navigate('/tasks/new')}>创建第一个任务</Button>}
              />
            ) : (
              dist.map((g) => (
                <div key={g.label} style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 8 }}>
                  <Typography.Text style={{ width: 64, flex: '0 0 auto' }}>{g.label}</Typography.Text>
                  <Progress
                    percent={tasks.length ? Math.round((g.count / tasks.length) * 100) : 0}
                    strokeColor={g.color}
                    size="small"
                    style={{ flex: 1, marginBottom: 0 }}
                    format={() => <span>{g.count}</span>}
                  />
                </div>
              ))
            )}
          </Card>
          <Card size="small" title="最近任务" loading={taskLoading}>
            <Table
              rowKey="task_id"
              size="small"
              dataSource={recentTasks}
              pagination={false}
              locale={{ emptyText: <EmptyState description="暂无任务" action={<Link to="/tasks/new">前往创建</Link>} /> }}
              columns={[
                {
                  title: '任务', dataIndex: 'task_id',
                  render: (v: string) => <Link to={`/tasks/${v}`} title={v} style={{ fontFamily: MONO_FONT }}>{shortId(v)}</Link>,
                },
                {
                  title: '项目', dataIndex: 'project_id', ellipsis: true,
                  render: (v: string) => <Link to={`/projects/${v}`}>{projectName(v)}</Link>,
                },
                { title: '模式', dataIndex: 'scan_mode', width: 150, ellipsis: true, render: (s: string) => zh(SCAN_MODE, s) },
                { title: '状态', dataIndex: 'status', width: 90, render: (s: string) => <Tag color={STATUS_COLOR[s]}>{zh(TASK_STATUS, s)}</Tag> },
                { title: '更新时间', dataIndex: 'updated_at', width: 150, render: (v: string | null) => (v ? dayjs(v).format('MM-DD HH:mm') : '—') },
              ]}
            />
          </Card>
        </Col>
        <Col xs={24} xl={8}>
          <Card
            size="small"
            title="最近报告"
            style={{ marginBottom: 16 }}
            loading={reportLoading}
            extra={<Link to="/reports">全部</Link>}
          >
            {reports.length === 0 ? (
              <EmptyState description="暂无报告（任务完成后由编排器生成）" />
            ) : (
              <List
                size="small"
                dataSource={reports.slice(0, 5)}
                renderItem={(r) => (
                  <List.Item
                    actions={[
                      <Typography.Text key="t" type="secondary" style={{ fontSize: 12 }}>
                        {r.generated_at ? dayjs(r.generated_at).format('MM-DD HH:mm') : '—'}
                      </Typography.Text>,
                    ]}
                  >
                    <Link to={`/reports?task=${r.task_id}`} title={r.report_id} style={{ fontFamily: MONO_FONT, fontSize: 13 }}>
                      {r.report_id.length > 24 ? `${r.report_id.slice(0, 24)}…` : r.report_id}
                    </Link>
                    <Tag style={{ marginLeft: 8 }}>{zh(REPORT_FORMAT, r.format)}</Tag>
                  </List.Item>
                )}
              />
            )}
          </Card>
          <Card
            size="small"
            title={<>未读通知 <Badge count={unreadCount} size="small" showZero={false} style={{ marginLeft: 8 }} /></>}
            extra={<Link to="/notifications">全部</Link>}
          >
            {unread.length === 0 ? (
              <EmptyState description="没有未读通知" />
            ) : (
              <List
                size="small"
                dataSource={unread.slice(0, 3)}
                renderItem={(n) => (
                  <List.Item
                    actions={[
                      <Typography.Text key="t" type="secondary" style={{ fontSize: 12 }}>
                        {n.created_at ? dayjs(n.created_at).format('MM-DD HH:mm') : ''}
                      </Typography.Text>,
                    ]}
                  >
                    <Typography.Text ellipsis style={{ maxWidth: 260 }}>{n.title}</Typography.Text>
                  </List.Item>
                )}
              />
            )}
          </Card>
        </Col>
      </Row>
    </div>
  );
}
