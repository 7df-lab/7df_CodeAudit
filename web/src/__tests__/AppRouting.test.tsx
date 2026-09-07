// 路由与全局壳契约（docs/internal-interfaces.md §8 [I-80..I-84]）。
// 此前 App.tsx 守卫链零直测——未登录跳转 / must_change_password 锁死 / RequireAdmin / 404 兜底
// 都是用户可见行为（ADR-147/205 历史修复点），在此行为级锁定。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from '../App';
import { SessionProvider } from '../auth/session';
import { TOKEN_KEY, clearSession } from '../api/client';
import { useFakeGateway } from '../testsupport/fakeGateway';

let me: Record<string, unknown> = { user_id: 'u-1', username: 'alice', email: 'a@x', role: 'ROLE_ADMIN' };
const routes: Record<string, unknown> = {
  'GET /v1/users/me': () => me,
  'GET /v1/notifications': () => ({
    notifications: [
      { notification_id: 'n-1', user_id: 'u-1', title: 't1', body: 'b1', read: false, created_at: null },
      { notification_id: 'n-2', user_id: 'u-1', title: 't2', body: 'b2', read: false, created_at: null },
      { notification_id: 'n-3', user_id: 'u-1', title: 't3', body: 'b3', read: true, created_at: null },
    ],
  }),
  'GET /v1/projects': { projects: [], pagination: { next_cursor: '', has_next: false, total: 0 } },
  'GET /v1/users': {
    users: [
      { user_id: 'u-1', username: 'alice', email: 'a@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_ADMIN', created_at: null },
      { user_id: 'u-2', username: 'dev1', email: 'd@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_DEVELOPER', created_at: null },
    ],
    pagination: { total: 2 },
  },
};
useFakeGateway(routes);

function renderApp(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionProvider>
        <MemoryRouter initialEntries={[path]}>
          <App />
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  clearSession();
  me = { user_id: 'u-1', username: 'alice', email: 'a@x', role: 'ROLE_ADMIN' };
  // 带token进站必经 bootRefresh（裸 fetch）——默认成功
  vi.stubGlobal('fetch', vi.fn(async () =>
    new Response(JSON.stringify({ access_token: 'acc-boot', refresh_token: 'ref-boot', expires_in_s: 1800 }),
      { status: 200, headers: { 'Content-Type': 'application/json' } })));
});
afterEach(() => { vi.unstubAllGlobals(); });

describe('I-81 Shell 守卫链', () => {
  it('未登录访问受保护路由 → 跳 /login（登录卡可见）', async () => {
    renderApp('/projects');
    expect(await screen.findByText('CodeAudit 控制台')).toBeTruthy();
  });

  it('must_change_password=true → 锁死 /change-password（访问 /projects 也被重定向，ADR-205）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    me = { ...me, must_change_password: true };
    renderApp('/projects');
    // 菜单按钮与卡片标题同为"修改密码"——断言强改密页说明文案（该页独有）
    expect(await screen.findByText(/必须设置新密码后才能继续使用/)).toBeTruthy();
    expect((await screen.findAllByText('修改密码')).length).toBeGreaterThanOrEqual(1);
  });

  it('已登录正常进站 → 受保护路由可达（项目页；菜单与页标题同文案取 all）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/projects');
    expect((await screen.findAllByText('项目')).length).toBeGreaterThanOrEqual(1);
  });
});

describe('I-82 RequireAdmin + I-83 未读角标', () => {
  it('ROLE_ADMIN 访问 /admin/users → 用户列表加载（dev1 行可见）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/admin/users');
    expect(await screen.findByText('dev1')).toBeTruthy();
  });

  it('非 admin 访问 /admin/users → 403 页（后端 requireAdmin 为最终防线，前端仅体验层）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    me = { ...me, role: 'ROLE_DEVELOPER' };
    renderApp('/admin/users');
    expect(await screen.findByText('权限不足')).toBeTruthy();
  });

  it('未读角标 = notifications 未读计数（2 条未读 → 通知（2 未读），60s 兜底轮询的初始拉取）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/projects');
    expect(await screen.findByText('通知（2 未读）')).toBeTruthy();
  });
});

describe('I-80 路由兜底', () => {
  it('未知路由 → 404 页（不静默重定向）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/definitely/not/exist');
    expect(await screen.findByText(/页面不存在/)).toBeTruthy();
  });
});
