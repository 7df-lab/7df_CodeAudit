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

const NOTIF_ONE = [
  { notification_id: 'n-1', user_id: 'u-1', title: '任务完成', body: 't1 已完成', read: false, created_at: null },
];
// 具名路由表：条数切换用例需按需覆写 notifications 载荷（finally 还原——共享对象
// 的覆写不还原会殃及同文件后续用例，B3AuditFixes defaultReportsRoute 同款教训）
const routes: Record<string, unknown> = {
  'GET /v1/reports': {
    reports: [{ report_id: 'r-1', task_id: 't1', format: 'REPORT_FORMAT_JSON', url: '', generated_at: '2026-08-29T00:00:00Z' }], // B5-P1-1：真实网关形状=protojson 枚举名（transcode.go 无 UseEnumNumbers，非数值）
  },
  'GET /v1/notifications': {
    notifications: NOTIF_ONE,
  },
  'POST /v1/notifications/:notificationId/read': {},
  'POST /v1/notifications/read-all': { marked: 1 },
};
const gateway = useFakeGateway(routes);

vi.mock('../auth/session', () => ({ useCanWrite: () => true,
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

  it('一键全部已读（E-31a：POST /v1/notifications/read-all，未读=0 时禁用）', async () => {
    withProviders(<NotificationsPage />);
    await waitFor(() => expect(screen.getByText('任务完成')).toBeTruthy());
    const btn = screen.getByRole('button', { name: '全部已读' }) as HTMLButtonElement;
    expect(btn.disabled).toBe(false); // fake 列表含 1 条未读
    fireEvent.click(btn);
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/notifications/read-all')).toBe(true),
    );
  });

  it('分页条数可调（2026-10-08 报障"20条/页不能调整"）：选择器在位，切 50/页全量重切片', async () => {
    const many = Array.from({ length: 25 }, (_, i) => ({
      notification_id: `n-${i}`, user_id: 'u-1', title: `批量通知${String(i).padStart(2, '0')}`,
      body: 'b', read: true, created_at: null,
    }));
    routes['GET /v1/notifications'] = { notifications: many };
    try {
      withProviders(<NotificationsPage />);
      await waitFor(() => expect(screen.getByText('批量通知00')).toBeTruthy());
      // 默认 20/页：首页 20 条，第 21 条在次页不可见
      expect(screen.queryByText('批量通知20')).toBeNull();
      // 条数选择器在位（simple 分页无此件——回归锁）
      const changer = document.querySelector('.ant-pagination-options-size-changer .ant-select-selector') as HTMLElement;
      expect(changer).toBeTruthy();
      // 切 50/页 → 单页容纳全部 25 条
      fireEvent.mouseDown(changer);
      const opt = await waitFor(() =>
        document.body.querySelector<HTMLElement>('.ant-select-item-option[title*="50"]'),
      );
      fireEvent.click(opt!);
      await waitFor(() => expect(screen.getByText('批量通知24')).toBeTruthy());
    } finally {
      routes['GET /v1/notifications'] = { notifications: NOTIF_ONE };
    }
  });
});
