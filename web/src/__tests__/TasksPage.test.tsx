// 任务列表↔报告对称列回归（ADR-142 补全）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import TasksPage from '../pages/tasks/TasksPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

const routes: Record<string, unknown> = {
  'GET /v1/reports': () => ({ reports: [{ task_id: 't-9' }, { task_id: 't-9' }] }),
  'GET /v1/tasks': () => ({
    tasks: [{
      task_id: 't-9', project_id: 'p1', status: 'TASK_STATUS_COMPLETED',
      updated_at: '2026-08-30T00:00:00Z',
      stages: [
        { stage_id: 's1', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_COMPLETED' },
        { stage_id: 's2', type: 'STAGE_TYPE_RESULT_FUSION', status: 'STAGE_STATUS_COMPLETED' },
      ],
    }],
    pagination: { next_cursor: '', has_next: false, total: 1 },
  }),
  'GET /v1/findings': () => ({
    findings: [
      { severity: 'SEVERITY_CRITICAL' },
      { severity: 'SEVERITY_HIGH' },
      { severity: 'SEVERITY_LOW' },
    ],
  }),
  // 2026-09-09 用户指令: 项目列显示名称——索引来自 /v1/projects（筛选下拉同源）
  'GET /v1/projects': () => ({
    projects: [{ project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null }],
    pagination: { next_cursor: '', has_next: false, total: 1 },
  }),
};
const gateway = useFakeGateway(routes);

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks']}>
        <TasksPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('TasksPage（任务↔报告对称）', () => {
  it('报告列链接指向 /reports?task=<id>（与报告中心任务列对称）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('#t-9')).toBeTruthy()); // 任务列短码（尾部 6 位）
    // 真实计数：该任务有 2 份报告 → 链接文本含数量
    await waitFor(() => expect(screen.getByText('2 份报告')).toBeTruthy());
    const link = screen.getByText('2 份报告').closest('a');
    expect(link?.getAttribute('href')).toBe('/reports?task=t-9');
  });

  it('项目列显示项目名称而非 ID（2026-09-09 用户指令；链接到项目详情，悬停见 ID）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    const nameLink = await screen.findByText('Demo');
    expect(nameLink.closest('a')?.getAttribute('href')).toBe('/projects/p1');
    expect(nameLink.getAttribute('title')).toBe('p1');
  });

  it('统计卡带（2026-09-09 对标竞品）：任务总数/已完成聚合渲染', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('任务总数')).toBeTruthy());
    expect(screen.getByText('已完成')).toBeTruthy();
    expect(screen.getByText('已完成占比')).toBeTruthy();
  });

  it('Stage 进度点 + 发现数列（总 N/高危彩签，对标竞品）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('总 3')).toBeTruthy());
    expect(screen.getByText('高危 2')).toBeTruthy();
    // 阶段点: 该任务 2 个已完成阶段 → 2 个绿点（title 含阶段中文名）
    const dots = document.querySelectorAll("span[title*='SAST 扫描']");
    expect(dots.length).toBeGreaterThan(0);
  });
});

// B4-3（审计修复）：发现数单元格——带 pagination:{page_size:100}（契约形状分页缺省命中
// 服务端极小缺省页，>缺省条数任务的发现数被截断）；计数优先消费 pagination.total（服务端
// 权威总数），缺失回退已加载行数（上一用例的无 pagination 载荷即回退路径）。
describe('TasksPage FindingCountCell 分页形状与 total 消费（B4-3）', () => {
  it('请求带 page_size:100；显示服务端 pagination.total（157）而非已加载行数（2）', async () => {
    routes['GET /v1/findings'] = () => ({
      findings: [
        { severity: 'SEVERITY_CRITICAL' },
        { severity: 'SEVERITY_HIGH' },
      ],
      pagination: { next_cursor: '', has_next: false, total: 157 },
    });
    renderPage();
    await waitFor(() => expect(screen.getByText('总 157')).toBeTruthy());
    expect(screen.getByText('高危 2')).toBeTruthy(); // 高危计数仍按已加载行计算（单元格页内口径）
    const cellGet = gateway.requests.find((r) => r.method === 'GET' && r.url === '/v1/findings')!;
    expect(cellGet.query).toContain(encodeURIComponent('"page_size":100'));
    expect(cellGet.query).toContain('task_id=t-9');
  });
});
