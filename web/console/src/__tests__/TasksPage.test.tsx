// 任务列表↔报告对称列回归（ADR-142 补全）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import TasksPage from '../pages/tasks/TasksPage';

const { getMock } = vi.hoisted(() => ({
  getMock: vi.fn(async (url: string): Promise<{ data: unknown }> => {
    if (url === '/v1/reports') {
      return { data: { reports: [{ task_id: 't-9' }, { task_id: 't-9' }] } };
    }
    return { data: { tasks: [{
      task_id: 't-9', project_id: 'p1', status: 'TASK_STATUS_COMPLETED',
      updated_at: '2026-08-30T00:00:00Z', stages: [],
    }], pagination: { next_cursor: '', has_next: false, total: 1 } } };
  }),
}));

vi.mock('../api/client', () => ({
  default: { get: getMock },
  api: { get: getMock },
}));

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
    await waitFor(() => expect(screen.getByText('t-9')).toBeTruthy());
    // 真实计数：该任务有 2 份报告 → 链接文本含数量
    await waitFor(() => expect(screen.getByText('2 份报告')).toBeTruthy());
    const link = screen.getByText('2 份报告').closest('a');
    expect(link?.getAttribute('href')).toBe('/reports?task=t-9');
  });
});
