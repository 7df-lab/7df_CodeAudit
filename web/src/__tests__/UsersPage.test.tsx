// 用户管理页回归（V2.1 ADR-205：列表+建号+停启用+重置密码）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import UsersPage from '../pages/admin/UsersPage';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

// useFakeGateway 必须在用例执行前注册（beforeEach 语义）——handler 表模块级可变，
// 各用例按需覆写（test-gates §8：只在传输层造假，未建模路由响亮失败）。
const routes: Record<string, unknown> = {
  'GET /v1/users': {
    users: [
      { user_id: 'u-1', username: 'admin', email: 'a@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_ADMIN', must_change_password: false, created_at: null },
      { user_id: 'u-2', username: 'dev1', email: 'd@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_DEVELOPER', must_change_password: true, created_at: null },
    ],
    pagination: { total: 2 },
  },
  'POST /v1/users': { user_id: 'u-new', username: 'newbie' },
  'PUT /v1/users/:userId': {},
  'POST /v1/users/:userId/password:reset': { temporary_password: 'temp-ab12CD34', must_change_password: true },
};
const gateway = useFakeGateway(routes);

// vi.mock 工厂提升早于模块体——用 vi.hoisted 持有可变会话状态
const session = vi.hoisted(() => ({
  user: { user_id: 'u-1', username: 'admin', email: '', role: 'ROLE_ADMIN', must_change_password: false },
}));
vi.mock('../auth/session', () => ({
  useSession: () => ({ user: session.user }),
}));

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

describe('UsersPage（V2.1 列表）', () => {
  it('列表展示用户/角色/状态/待改密标记', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    expect(screen.getByText('dev1')).toBeTruthy();
    expect(screen.getByText('管理员')).toBeTruthy();
    expect(screen.getByText('首登须改密')).toBeTruthy();
    expect(screen.getByText('共 2 个用户')).toBeTruthy();
    expect(gateway.requests.some((r) => r.method === 'GET' && r.url === '/v1/users')).toBe(true);
  });

  it('搜索触发 username_contains 透传', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    const box = screen.getByPlaceholderText('按用户名搜索');
    fireEvent.change(box, { target: { value: 'dev' } });
    // antd Input.Search 无 <form>，回车触发 onSearch
    fireEvent.keyDown(box, { key: 'Enter', keyCode: 13 });
    await waitFor(() => {
      const last = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/users').pop();
      expect(last?.query).toContain('username_contains');
    });
  });

  it('停用即 PUT 全量 user（USER_STATE_INACTIVE）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    // antd 两字按钮自动插空格（停用→停 用），用正则匹配
    const buttons = screen.getAllByRole('button', { name: /停\s*用/ });
    // u-1 是自己 → 按钮禁用；u-2 可停用
    const active = buttons.find((b) => !(b as HTMLButtonElement).disabled)!;
    fireEvent.click(active);
    await waitFor(() => expect(gateway.requests.some((r) => r.method === 'PUT')).toBe(true));
    const put = gateway.requests.find((r) => r.method === 'PUT')!;
    expect(put.url).toBe('/v1/users/u-2');
    expect((put.body as { user: { state: string } }).user.state).toBe('USER_STATE_INACTIVE');
  });

  it('重置密码弹一次性临时密码窗（仅一次显示）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    fireEvent.click(screen.getAllByRole('button', { name: '重置密码' })[0]);
    await waitFor(() => expect(screen.getByText('temp-ab12CD34')).toBeTruthy());
    expect(screen.getByText(/仅此一次显示/)).toBeTruthy();
    const req = gateway.requests.find((r) => r.url.includes('password:reset'))!;
    expect(req.url).toBe('/v1/users/u-1/password:reset');
  });

  it('新建用户 POST /v1/users（角色缺省 DEVELOPER）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('admin')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '新建用户' }));
    await waitFor(() => expect(document.querySelector('.ant-modal-footer .ant-btn-primary')).toBeTruthy());
    fireEvent.change(screen.getByLabelText('用户名'), { target: { value: 'newbie' } });
    fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: 'n@x.io' } });
    fireEvent.change(screen.getByLabelText('初始密码'), { target: { value: 'start1234' } });
    // Modal footer 主按钮（antd 两字按钮插空格导致可访问名不稳定，用 footer 选择器）
    fireEvent.click(document.querySelector('.ant-modal-footer .ant-btn-primary') as HTMLButtonElement);
    await waitFor(() => expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/users')).toBe(true));
    const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/users')!;
    expect((post.body as { role?: string }).role).toBe('ROLE_DEVELOPER');
  });

  it('非 ROLE_ADMIN 渲染 403 文案且不拉列表', async () => {
    session.user = { user_id: 'u-9', username: 'dev', email: '', role: 'ROLE_DEVELOPER', must_change_password: false };
    const before = gateway.requests.length;
    renderPage();
    await waitFor(() => expect(screen.getByText(/仅管理员可访问/)).toBeTruthy());
    expect(gateway.requests.length).toBe(before); // enabled=false：不发列表请求
    session.user = { user_id: 'u-1', username: 'admin', email: '', role: 'ROLE_ADMIN', must_change_password: false };
  });

  it('列表 403 时如实显示加载失败', async () => {
    routes['GET /v1/users'] = () => httpError(403, { error: 'admin role required' });
    renderPage();
    await waitFor(() => expect(screen.getByText(/加载失败/)).toBeTruthy());
    delete routes['GET /v1/users'];
    routes['GET /v1/users'] = {
      users: [
        { user_id: 'u-1', username: 'admin', email: 'a@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_ADMIN', must_change_password: false, created_at: null },
        { user_id: 'u-2', username: 'dev1', email: 'd@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_DEVELOPER', must_change_password: true, created_at: null },
      ],
      pagination: { total: 2 },
    };
  });
});
