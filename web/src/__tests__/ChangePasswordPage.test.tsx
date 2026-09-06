// 强制改密页回归（V2.1 ADR-205）：POST /v1/users/me/password 不携带 user_id（网关从 JWT 注入）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import ChangePasswordPage from '../pages/ChangePasswordPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

const routes: Record<string, unknown> = {
  'POST /v1/users/me/password': {},
};
const gateway = useFakeGateway(routes);

vi.mock('../auth/session', () => ({
  useSession: () => ({
    user: { user_id: 'u-2', username: 'dev1', email: '', role: 'ROLE_DEVELOPER', must_change_password: true },
    refreshUser: vi.fn().mockResolvedValue(undefined),
  }),
}));

function renderPage() {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/change-password']}>
        <ChangePasswordPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('ChangePasswordPage（ADR-205）', () => {
  it('提交 POST /v1/users/me/password（self 语义，body 无 user_id）', async () => {
    renderPage();
    fireEvent.change(screen.getByLabelText('当前密码'), { target: { value: 'temp-ab12' } });
    fireEvent.change(screen.getByLabelText('新密码'), { target: { value: 'newpass99-Y' } });
    fireEvent.change(screen.getByLabelText('确认新密码'), { target: { value: 'newpass99-Y' } });
    fireEvent.click(screen.getByRole('button', { name: '确认修改' }));
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/users/me/password')).toBe(true),
    );
    const post = gateway.requests.find((r) => r.method === 'POST')!;
    const body = post.body as { old_password: string; new_password: string; user_id?: string };
    expect(body.old_password).toBe('temp-ab12');
    expect(body.new_password).toBe('newpass99-Y');
    expect(body.user_id).toBeUndefined();
  });

  it('新密码不含数字被前端校验拦截', async () => {
    renderPage();
    fireEvent.change(screen.getByLabelText('当前密码'), { target: { value: 'temp-ab12' } });
    fireEvent.change(screen.getByLabelText('新密码'), { target: { value: 'onlyletters' } });
    fireEvent.click(screen.getByRole('button', { name: '确认修改' }));
    await waitFor(() => expect(screen.getByText('须同时包含字母与数字')).toBeTruthy());
  });
});
