// 首页总览（2026-09-13）：态势聚合的纯客户端渲染——统计带取数口径与
// 任务页统计一致（近 200 条一次性查询）；状态分布零计数分组不渲染；空数据有行动出口。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import DashboardPage from '../pages/dashboard/DashboardPage';
import { SessionProvider } from '../auth/session';
import { useFakeGateway } from '../testsupport/fakeGateway';

vi.stubGlobal('fetch', vi.fn(async () =>
  new Response(JSON.stringify({ access_token: 'acc', refresh_token: 'ref', expires_in_s: 1800 }),
    { status: 200, headers: { 'Content-Type': 'application/json' } })));

const TASK = (over: Record<string, unknown>) => ({
  task_id: 'gw-t00000a1f', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL',
  status: 'TASK_STATUS_COMPLETED', created_at: '2026-09-12T10:00:00Z', updated_at: '2026-09-12T11:00:00Z',
  ...over,
});

let tasks: unknown[] = [];
let reports: unknown[] = [];
let notifications: unknown[] = [];
const routes: Record<string, unknown> = {
  'GET /v1/projects': () => ({ projects: [{ project_id: 'p1', name: '支付网关', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null }], pagination: { next_cursor: '', has_next: false, total: 1 } }),
  'GET /v1/tasks': () => ({ tasks, pagination: { next_cursor: '', has_next: false, total: tasks.length } }),
  'GET /v1/reports': () => ({ reports, pagination: { next_cursor: '', has_next: false, total: reports.length } }),
  'GET /v1/notifications': () => ({ notifications }),
};
useFakeGateway(routes);

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionProvider>
        <MemoryRouter initialEntries={['/']}>
          <DashboardPage />
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  tasks = [
    TASK({ task_id: 'gw-t00000a1f', status: 'TASK_STATUS_COMPLETED' }),
    TASK({ task_id: 'gw-t00000b2e', status: 'TASK_STATUS_RUNNING' }),
    TASK({ task_id: 'gw-t00000c3d', status: 'TASK_STATUS_FAILED' }),
  ];
  reports = [{ report_id: 'rpt-demo-0001-json-format-aaaaaaaaaaaaaaaa', task_id: 'gw-t00000a1f', format: 'REPORT_FORMAT_JSON', url: '', generated_at: '2026-09-12T10:42:00Z' }];
  notifications = [{ notification_id: 'n-1', title: '任务已完成', read: false, created_at: '2026-09-12T10:41:00Z' }];
});

describe('DashboardPage（总览）', () => {
  it('统计带：项目/任务总数/进行中/已完成/失败/报告 六项渲染（数据到达后）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByText('3').length).toBeGreaterThan(0)); // 任务总数 3
    for (const t of ['项目', '任务总数', '进行中', '已完成', '失败', '报告']) {
      expect(screen.getAllByText(t).length).toBeGreaterThan(0);
    }
  });
  it('状态分布只渲染非零分组（失败/进行中/完成三组，无"已取消"组）', async () => {
    renderPage();
    await screen.findByText('#000a1f'); // 先等任务数据落位（统计带标题恒在，不能作就绪信号）
    expect(screen.getAllByText('已完成').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('执行中').length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText('失败').length).toBeGreaterThanOrEqual(1);
    expect(screen.queryByText('已取消')).toBeNull();
  });
  it('最近任务表：短 ID 等宽链接 + 项目名（非 ID）+ 状态签', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('#000a1f')).toBeTruthy());
    expect(screen.getAllByText('支付网关').length).toBeGreaterThanOrEqual(1);
  });
  it('最近报告：mono 截断 ID 链接到报告中心（task 深链）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/rpt-demo-0001/)).toBeTruthy());
    const link = screen.getByText(/rpt-demo-0001/).closest('a');
    expect(link?.getAttribute('href')).toBe('/reports?task=gw-t00000a1f');
  });
  it('空数据：状态分布给"创建第一个任务"行动出口', async () => {
    tasks = []; reports = []; notifications = [];
    renderPage();
    expect(await screen.findByText('创建第一个任务')).toBeTruthy();
  });
});
