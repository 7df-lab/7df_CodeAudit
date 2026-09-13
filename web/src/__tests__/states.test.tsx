// （2026-09-13）回归：
//  1. 状态体系组件（PageLoading/EmptyState/QueryError）——替换全站四种并存加载形态的统一件；
//  2. FusionView/ReviewView 查询失败显性化——此前无错误分支，失败永远停在加载文案
//    （与 FindingDetailBody (P3-k) 同病）。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import FusionView from '../pages/views/FusionView';
import ReviewView from '../pages/views/ReviewView';
import { EmptyState, PageLoading, QueryError } from '../components/states';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行，仅 HTTP 层伪造；未建模路由响亮失败
const routes: Record<string, unknown> = {
  'GET /v1/findings': () => httpError(500, { error: 'demo backend down' }),
};
useFakeGateway(routes);

function withProviders(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('状态体系组件（states.tsx）', () => {
  it('PageLoading 渲染 Spin + 可选说明文案', () => {
    const { container } = render(<PageLoading tip="会话恢复中…" />);
    expect(container.querySelector('.ant-spin')).toBeTruthy();
    expect(screen.getByText('会话恢复中…')).toBeTruthy();
  });
  it('EmptyState 渲染说明与行动指引（空屏是行动邀请）', () => {
    render(<EmptyState description="暂无项目" action={<a href="#x">新建项目</a>} />);
    expect(screen.getByText('暂无项目')).toBeTruthy();
    expect(screen.getByText('新建项目')).toBeTruthy();
  });
  it('QueryError 展示原因与重试出口', () => {
    const onRetry = () => {};
    render(<QueryError error={new Error('boom')} onRetry={onRetry} />);
    expect(screen.getByText('加载失败')).toBeTruthy();
    expect(screen.getByText(/boom/)).toBeTruthy();
    expect(screen.getByRole('button', { name: /重\s*试/ })).toBeTruthy();
  });
});

describe('视图查询失败显性化', () => {
  it('FusionView 500 → QueryError + 重试，非"加载中…"死态', async () => {
    withProviders(<FusionView taskId="t1" />);
    await waitFor(() => expect(screen.getByText('加载失败')).toBeTruthy());
    expect(screen.queryByText('加载中…')).toBeNull();
    expect(screen.getByRole('button', { name: /重\s*试/ })).toBeTruthy();
  });
  it('ReviewView 500 → QueryError + 重试，非"加载中…"死态', async () => {
    withProviders(<ReviewView taskId="t1" />);
    await waitFor(() => expect(screen.getByText('加载失败')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /重\s*试/ }));
    // 重试仍失败（同一路由表），错误持续可见——不死循环白屏
    await waitFor(() => expect(screen.getByText('加载失败')).toBeTruthy());
  });
});
