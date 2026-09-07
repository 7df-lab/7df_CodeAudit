// 列表翻页与筛选契约（外部 E-18/E-28；ADR-160/164；P-01 回归面）。
// ReportsPage=lastID 不透明游标顺序前进；TasksPage=offset 游标 + project_id/filter 服务端过滤形状。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { beforeEach, describe, expect, it } from 'vitest';
import ReportsPage from '../pages/reports/ReportsPage';
import TasksPage from '../pages/tasks/TasksPage';
import { useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

const REPORT_ROW = { report_id: 'r-1', task_id: 't-1', format: 3, url: '', generated_at: '2026-09-01T00:00:00Z' };
let lastReportsQuery = '';
const routes: Record<string, unknown> = {
  'GET /v1/reports': (ctx: HandlerCtx) => {
    const q = ctx.query.toString();
    if (ctx.query.get('pagination')?.includes('"page_size":100')) {
      return { reports: [] }; // TasksPage 报告索引
    }
    lastReportsQuery = q;
    const cursor = JSON.parse(ctx.query.get('pagination') ?? '{"cursor":""}').cursor ?? '';
    if (cursor === '') return { reports: [REPORT_ROW], pagination: { next_cursor: 'c2', has_next: true } };
    return { reports: [{ ...REPORT_ROW, report_id: `r-${cursor}` }], pagination: { next_cursor: '', has_next: false } };
  },
  'GET /v1/projects': { projects: [{ project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null }], pagination: { next_cursor: '', has_next: false, total: 1 } },
  'GET /v1/tasks': (ctx: HandlerCtx) => {
    lastReportsQuery = `TASKS ${ctx.query.toString()}`;
    return { tasks: [{ task_id: 't-9', project_id: 'p1', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [], status: 'TASK_STATUS_COMPLETED', stages: [], created_at: null, updated_at: null, error_message: '', retry_count: 0 }], pagination: { next_cursor: '', has_next: false, total: 45 } };
  },
};
const gateway = useFakeGateway(routes);

function withProviders(ui: React.ReactElement, initial = ['/reports']) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={initial}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  lastReportsQuery = '';
});

describe('E-28 ReportsPage lastID 游标（顺序前进，无 total 跳页）', () => {
  it('首页 cursor="" → 翻下一页携带 next_cursor=c2', async () => {
    withProviders(<ReportsPage />);
    await waitFor(() => expect(screen.getByText('r-1')).toBeTruthy());
    const next = document.querySelector('.ant-pagination-next button');
    expect(next).toBeTruthy();
    fireEvent.click(next!);
    await waitFor(() => expect(screen.getByText('r-c2')).toBeTruthy());
    expect(lastReportsQuery).toContain(encodeURIComponent('"cursor":"c2"'));
  });

  it('?task=<id> 过滤 → 请求携带 task_id，过滤标签可关闭', async () => {
    withProviders(<ReportsPage />, ['/reports?task=t-1']);
    await waitFor(() => expect(screen.getByText('r-1')).toBeTruthy());
    expect(lastReportsQuery).toContain('task_id=t-1');
    expect(screen.getByText('任务：t-1')).toBeTruthy();
  });
});

describe('E-18 TasksPage offset 游标 + 服务端筛选形状（ADR-160/164）', () => {
  it('首屏 pagination={"page_size":20,"cursor":"0"}；选项目+模式 → project_id 与 filter.conditions 形状', async () => {
    withProviders(<TasksPage />, ['/tasks']);
    await waitFor(() => expect(screen.getByText('t-9')).toBeTruthy());
    const first = gateway.requests.find((r) => r.url === '/v1/tasks')!;
    expect(first.query).toContain(encodeURIComponent('"page_size":20'));
    expect(first.query).toContain(encodeURIComponent('"cursor":"0"'));

    const combos = screen.getAllByRole('combobox');
    fireEvent.mouseDown(combos[0]); // 项目筛选
    fireEvent.click(await screen.findByText('Demo (p1)'));
    fireEvent.mouseDown(combos[1]); // 模式筛选
    const opt = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-select-item-option[title="模式B 纯AI"]'),
    );
    fireEvent.click(opt!);

    await waitFor(() => {
      const last = gateway.requests.filter((r) => r.url === '/v1/tasks').pop()!;
      expect(last.query).toContain('project_id=p1');
      expect(last.query).toContain('SCAN_MODE_AI_ONLY');
      expect(last.query).toContain('FILTER_OPERATOR_EQ');
    });
    // 改筛选回第一页（cursor 重置）
    const last = gateway.requests.filter((r) => r.url === '/v1/tasks').pop()!;
    expect(last.query).toContain(encodeURIComponent('"cursor":"0"'));
  });
});
