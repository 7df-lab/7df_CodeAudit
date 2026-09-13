// 任务列表↔报告对称列回归（ADR-142 补全）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import TasksPage from '../pages/tasks/TasksPage';
import { useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

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

// （审计修复）：发现数单元格——带 pagination:{page_size:100}（契约形状分页缺省命中
// 服务端极小缺省页，>缺省条数任务的发现数被截断）；计数优先消费 pagination.total（服务端
// 权威总数），缺失回退已加载行数（上一用例的无 pagination 载荷即回退路径）。
describe('TasksPage FindingCountCell 分页形状与 total 消费', () => {
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

// B5-P1-2（web-audit-2026-09-12）：任务列表末页空表——task_service.go:1107 只在有下页时
// 才填 Pagination，末页响应经网关 protojson EmitUnpopulated 发 "pagination":null；
// 旧代码 `total ?? 0` 塌成 0 → antd Table 判 `rows.length < total` 为假走本地切片
// slice(20,40) → 末页空表+分页器消失（>20 任务必现）。修复=末页按页位推导 total 兜底。
describe('B5-P1-2: 末页 pagination=null 不塌空表', () => {
  const mkTask = (n: number) => ({
    task_id: `task-${String(n).padStart(6, '0')}`, project_id: 'p1',
    scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [], status: 'TASK_STATUS_COMPLETED',
    stages: [], created_at: null, updated_at: null, error_message: '', retry_count: 0,
  });
  it('21 个任务翻到末页：第 21 条可见，分页器不塌缩', async () => {
    routes['GET /v1/tasks'] = (ctx: HandlerCtx) => {
      const pag = JSON.parse(ctx.query.get('pagination') ?? '{"cursor":"0"}');
      if (pag.page_size === 200) return { tasks: [] }; // 统计卡聚合查询
      if ((pag.cursor ?? '0') === '0') {
        return {
          tasks: Array.from({ length: 20 }, (_, i) => mkTask(i + 1)),
          pagination: { next_cursor: '20', has_next: true, total: 21 },
        };
      }
      // 末页真实形态：无 pagination 键（服务端 unset message 字段）
      return { tasks: [mkTask(21)] };
    };
    renderPage();
    await waitFor(() => expect(screen.getByText('#000001')).toBeTruthy());
    fireEvent.click(document.querySelector('.ant-pagination-next')!);
    await waitFor(() => expect(screen.getByText('#000021')).toBeTruthy());
    // 分页器未塌缩：仍可看到 2 个页码项
    expect(document.querySelectorAll('.ant-pagination-item').length).toBe(2);
  });
});
