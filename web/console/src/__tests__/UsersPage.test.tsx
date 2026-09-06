// 用户管理页回归（Q2a：只读+更新）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import UsersPage from '../pages/admin/UsersPage';

const { getMock, putCalls, putMock } = vi.hoisted(() => {
  const putCalls: [string, unknown][] = [];
  return {
    putCalls,
    getMock: vi.fn(async (url: string) => {
      if (url === '/v1/users/u-1') {
        return { data: { user_id: 'u-1', username: 'admin', email: 'a@x', state: 'USER_STATE_ACTIVE', created_at: null } };
      }
      if (url === '/v1/users/u-1/permissions') {
        return { data: { permissions: ['task:create'] } };
      }
      throw new Error('unexpected ' + url);
    }),
    putMock: vi.fn(async (url: string, body?: unknown) => {
      putCalls.push([url, body]);
      return { data: {} };
    }),
  };
});

vi.mock('../api/client', () => ({
  default: { get: getMock, put: putMock },
  api: { get: getMock, put: putMock },
}));
vi.mock('../auth/session', () => ({ useSession: () => ({ user: { user_id: 'u-1', username: 'admin', email: '' } }) }));

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/admin/users']}>
        <UsersPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('UsersPage（Q2a 只读+更新）', () => {
  it('展示用户资料与权限标签', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    expect(screen.getByText('task:create')).toBeTruthy();
  });
  it('状态选择即触发 UpdateUser（PUT 携带全量 user）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    fireEvent.mouseDown(screen.getByRole('combobox'));
    const opt = await screen.findByText('停用');
    fireEvent.click(opt);
    await waitFor(() => expect(putCalls.length).toBe(1));
    const [url, body] = putCalls[0];
    expect(url).toBe('/v1/users/u-1');
    expect((body as { user: { state: string } }).user.state).toBe('USER_STATE_INACTIVE');
  });
});
