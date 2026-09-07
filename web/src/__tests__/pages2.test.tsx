// T4 回归：报告中心 + 通知中心。
// ADR-203 迁移：此前本文件 vi.mock 整个 ../api/client（测试纪律违例，P-02 假绿根源）——
// 现走 fakeGateway 传输层，client.ts 真实代码（序列化/拦截器）全量执行。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import ReportsPage from '../pages/reports/ReportsPage';
import NotificationsPage from '../pages/notifications/NotificationsPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

const gateway = useFakeGateway({
  'GET /v1/reports': {
    reports: [{ report_id: 'r-1', task_id: 't1', format: 3, url: '', generated_at: '2026-08-29T00:00:00Z' }],
  },
  'GET /v1/notifications': {
    notifications: [
      { notification_id: 'n-1', user_id: 'u-1', title: '任务完成', body: 't1 已完成', read: false, created_at: null },
    ],
  },
  'POST /v1/notifications/:notificationId/read': {},
});

vi.mock('../auth/session', () => ({
  useSession: () => ({ user: { user_id: 'u-1', username: 'alice', email: '', role: 'ROLE_ADMIN' } }),
}));

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
    expect(screen.getByText('JSON')).toBeTruthy(); // format=3 → REPORT_FORMAT 映射
    expect(screen.getByRole('button', { name: /下\s*载/ })).toBeTruthy();
    // E-28：列表请求携带 page_size=20 首页游标（lastID 游标契约）
    const req = gateway.requests.find((r) => r.url === '/v1/reports')!;
    expect(req.query).toContain(encodeURIComponent('"page_size":20'));
  });
});

describe('NotificationsPage', () => {
  it('未读徽标 + 标记已读调用（POST /v1/notifications/:id/read）', async () => {
    withProviders(<NotificationsPage />);
    await waitFor(() => expect(screen.getByText('任务完成')).toBeTruthy());
    fireEvent.click(screen.getByText('标记已读'));
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/notifications/n-1/read')).toBe(true),
    );
  });
});
