// 任务详情嵌入回归（ADR-142 补全）：发现/融合 Tabs 内嵌，不再有独立"查看发现"按钮
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import TaskDetailPage from '../pages/tasks/TaskDetailPage';

const { getMock } = vi.hoisted(() => ({
  getMock: vi.fn(async (url: string): Promise<{ data: unknown }> => {
    // ADR-170: 详情页改用聚合快照单口轮询
    if (url === '/v1/tasks/t-1/snapshot') {
      return { data: {
        task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_TRADITIONAL_FIRST',
          sast_tools: ['bandit'], status: 'TASK_STATUS_COMPLETED', stages: [], retry_count: 0 },
        progress: { task_id: 't-1', status: 'TASK_STATUS_COMPLETED', overall_percent: 100, stages: [] },
        logs: { logs: [{ log_id: '1', task_id: 't-1', ts_ms: 1756630000000,
          level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '状态流转 TASK_STATUS_CREATED → TASK_STATUS_PENDING（submit）' }] },
        ai: { chunk: '', next_cursor: '0', complete: true, total_bytes: '0' },
      } };
    }
    if (url === '/v1/reports') {
      return { data: { reports: [] } };
    }
    if (url.startsWith('/v1/findings')) {
      return { data: { findings: [], pagination: { next_cursor: '', has_next: false, total: 0 } } };
    }
    throw new Error('unexpected ' + url);
  }),
}));

vi.mock('../api/client', () => ({
  default: { get: getMock },
  api: { get: getMock },
  pollIntervalMs: () => 3000,
  getAccessToken: () => '', // ADR-172: 详情页 WS 客户端读取；测试环境空 token 连接失败即回退轮询
}));

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks/t-1']}>
        <TaskDetailPage taskId="t-1" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('TaskDetailPage（发现/融合内嵌）', () => {
  it('COMPLETED 任务渲染发现与融合 Tabs（模式B）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
    await waitFor(() => expect(screen.getAllByText('发现').length).toBeGreaterThan(0));
    expect(screen.getAllByText('融合视图').length).toBeGreaterThan(0);
    // 无独立"查看发现"按钮（已内嵌）
    expect(screen.queryByRole('button', { name: '查看发现' })).toBeNull();
  });
});
