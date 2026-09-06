// 任务详情嵌入回归（ADR-142 补全）：发现/融合 Tabs 内嵌，不再有独立"查看发现"按钮
// 修复回归：WS 卸载关闭 / 重新生成报告失效报告卡片数据源
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import TaskDetailPage from '../pages/tasks/TaskDetailPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行（含 ADR-170 轮询口），仅 HTTP 层伪造。
// pollIntervalMs/getAccessToken 用真实实现（无 token 时 WS 连接失败自然回退轮询，无需 stub）。
const gateway = useFakeGateway({
  // ADR-170: 详情页改用聚合快照单口轮询
  'GET /v1/tasks/:taskId/snapshot': () => ({
    task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_TRADITIONAL_FIRST',
      sast_tools: ['bandit'], status: 'TASK_STATUS_COMPLETED', stages: [], retry_count: 0 },
    progress: { task_id: 't-1', status: 'TASK_STATUS_COMPLETED', overall_percent: 100, stages: [] },
    logs: { logs: [{ log_id: '1', task_id: 't-1', ts_ms: 1756630000000,
      level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '状态流转 TASK_STATUS_CREATED → TASK_STATUS_PENDING（submit）' }] },
    ai: { chunk: '', next_cursor: '0', complete: true, total_bytes: '0' },
  }),
  'GET /v1/reports': () => ({ reports: [{ report_id: 'r-1', task_id: 't-1', format: 3, url: '', generated_at: null }] }),
  'GET /v1/findings': () => ({ findings: [], pagination: { next_cursor: '', has_next: false, total: 0 } }),
  'POST /v1/tasks/:taskId/report': () => ({ report_id: 'r-new' }),
});

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

  it('修复回归：卸载时关闭 WebSocket 推流（此前清理只置标志不 close，连接泄漏持续收帧）', async () => {
    const created: { url: string; closed: boolean; onclose: (() => void) | null }[] = [];
    class FakeWS {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      url: string;
      closed = false;
      onopen: (() => void) | null = null;
      onmessage: ((ev: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(url: string) {
        this.url = url;
        created.push(this);
      }
      close() {
        this.closed = true;
        this.onclose?.();
      }
    }
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS;
    try {
      const { unmount } = renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(created.length).toBeGreaterThan(0);
      expect(created[0].url).toContain('/v1/tasks/t-1/ws');
      expect(created[0].closed).toBe(false);
      unmount();
      expect(created[0].closed).toBe(true);
    } finally {
      delete (globalThis as { WebSocket?: unknown }).WebSocket;
    }
  });

  it('修复回归：重新生成报告后失效报告卡片数据源（GET /v1/reports 重新拉取，卡片摘要随新报告刷新）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
    await waitFor(() => expect(screen.getByRole('button', { name: '重新生成报告' })).toBeTruthy());
    const getsBefore = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/reports').length;
    fireEvent.click(screen.getByRole('button', { name: '重新生成报告' }));
    const ok = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-popconfirm-buttons .ant-btn-primary'),
    );
    expect(ok).toBeTruthy();
    fireEvent.click(ok!);
    await waitFor(() => {
      const getsAfter = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/reports').length;
      expect(getsAfter).toBeGreaterThan(getsBefore);
    });
    expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks/t-1/report')).toBe(true);
  });
});
