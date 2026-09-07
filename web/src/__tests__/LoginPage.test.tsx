// 登录页契约（E-01 消费端；ADR-147：booting 防闪烁 + 失败文案如实）。
// LoginPage 经 session.login 走业务层——session mock 换成可控桩，仅本页 UX 锚定。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import LoginPage from '../pages/LoginPage';

const sessionState = vi.hoisted(() => ({
  value: {
    user: null as unknown,
    booting: false,
    login: vi.fn(),
    register: vi.fn(),
    logout: vi.fn(),
    refreshUser: vi.fn(),
  },
}));
vi.mock('../auth/session', () => ({ useSession: () => sessionState.value }));

function renderPage() {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/login']}>
        <LoginPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('LoginPage（E-01 消费端）', () => {
  it('booting → 渲染 Spin（会话恢复防闪烁，不闪登录表单）', () => {
    sessionState.value.booting = true;
    try {
      renderPage();
      expect(document.querySelector('.ant-spin')).toBeTruthy();
      expect(screen.queryByRole('button', { name: /登\s*录/ })).toBeNull();
    } finally {
      sessionState.value.booting = false;
    }
  });

  it('提交 → login(用户名, 密码)', async () => {
    sessionState.value.login.mockResolvedValueOnce(undefined);
    renderPage();
    fireEvent.change(screen.getByLabelText('用户名'), { target: { value: 'alice' } });
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'pw' } });
    fireEvent.click(screen.getByRole('button', { name: /登\s*录/ }));
    await waitFor(() => expect(sessionState.value.login).toHaveBeenCalledWith('alice', 'pw'));
  });

  it('登录失败 → 文案如实（凭证错与服务不可用同话术，不泄露内部细节）', async () => {
    sessionState.value.login.mockRejectedValueOnce(new Error('401'));
    renderPage();
    fireEvent.change(screen.getByLabelText('用户名'), { target: { value: 'alice' } });
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'bad' } });
    fireEvent.click(screen.getByRole('button', { name: /登\s*录/ }));
    await waitFor(() =>
      expect(screen.getByText('登录失败：用户名或密码错误，或服务不可用')).toBeTruthy(),
    );
  });
});
