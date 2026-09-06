// T4 回归：报告中心 + 通知中心
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import ReportsPage from '../pages/reports/ReportsPage';
import NotificationsPage from '../pages/notifications/NotificationsPage';

const { getMock, postCalls, postMock } = vi.hoisted(() => {
  const postCalls: [string, unknown][] = [];
  return {
    postCalls,
    getMock: vi.fn(),
    postMock: vi.fn(async (url: string) => {
      postCalls.push([url, null]);
      return { data: {} };
    }),
  };
});
getMock.mockImplementation(async (url: string) => {
  if (url === '/v1/reports') {
    return { data: { reports: [{ report_id: 'r-1', task_id: 't1', format: 3, url: 'report://x', generated_at: '2026-08-29T00:00:00Z' }] } };
  }
  if (url === '/v1/notifications') {
    return { data: { notifications: [
      { notification_id: 'n-1', user_id: 'u-1', title: '任务完成', body: 't1 已完成', read: false, created_at: null },
    ] } };
  }
  throw new Error('unexpected ' + url);
});
vi.mock('../api/client', () => ({ api: { get: getMock, post: postMock } }));
vi.mock('../auth/session', () => ({ useSession: () => ({ user: { user_id: 'u-1', username: 'alice', email: '' } }) }));

function withProviders(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('ReportsPage（14号 §3.3 ⑧）', () => {
  it('格式枚举映射 + 下载按钮存在（流聚合由 T0 路由承载）', async () => {
    withProviders(<ReportsPage />);
    await waitFor(() => expect(screen.getByText('r-1')).toBeTruthy());
    expect(screen.getByText('JSON')).toBeTruthy();
    expect(screen.getByRole('button', { name: /下\s*载/ })).toBeTruthy();
  });
});

describe('NotificationsPage', () => {
  it('未读徽标 + 标记已读调用', async () => {
    withProviders(<NotificationsPage />);
    await waitFor(() => expect(screen.getByText('任务完成')).toBeTruthy());
    fireEvent.click(screen.getByText('标记已读'));
    await waitFor(() => expect(postCalls.some(([u]) => u === '/v1/notifications/n-1/read')).toBe(true));
  });
});
