// 注册页回归（V2.1 ADR-205）：注册即登录 + 错误语义如实呈现
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import RegisterPage from '../pages/RegisterPage';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

// useFakeGateway 需在用例前注册（beforeEach 语义）——handler 表模块级可变
const routes: Record<string, unknown> = {
  'POST /v1/auth/register': { access_token: 'acc-1', refresh_token: 'ref-1', expires_in_s: 1800 },
};
const gateway = useFakeGateway(routes);

const registerFn = vi.fn().mockResolvedValue(undefined);
vi.mock('../auth/session', () => ({ useCanWrite: () => true,
  useSession: () => ({
    user: null,
    booting: false,
    register: registerFn,
    login: vi.fn(),
    logout: vi.fn(),
    refreshUser: vi.fn().mockResolvedValue(undefined),
  }),
}));

function renderPage() {
  const qc = new QueryClient();
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/register']}>
        <RegisterPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function fillAndSubmit(invite = 'CODEAUDIT-TEST-1') {
  fireEvent.change(screen.getByLabelText('用户名'), { target: { value: 'newbie' } });
  fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: 'n@x.io' } });
  fireEvent.change(screen.getByLabelText(/^密码$/), { target: { value: 'passw0rd-X' } });
  fireEvent.change(screen.getByLabelText('确认密码'), { target: { value: 'passw0rd-X' } });
  // V2.2 (ADR-255)：invitation 档邀请码必填——policy 查询经 fakeGateway 无路由
  // （404→retry 耗尽→保守 invitation 形态），栏位在且必填
  fireEvent.change(screen.getByLabelText('邀请码'), { target: { value: invite } });
  fireEvent.click(screen.getByRole('button', { name: '注册并登录' }));
}

// 注册页经 session.register 走业务层，错误映射测试通过 mock reject 注入 axios 形状错误
function axiosLike(status: number, body: { error: string }) {
  return Object.assign(new Error(`Request failed with status code ${status}`), {
    isAxiosError: true,
    response: { status, data: body, headers: {}, config: {}, statusText: String(status) },
  });
}

describe('RegisterPage（ADR-205）', () => {
  it('弱密码被前端校验拦截（不发起请求）', async () => {
    renderPage();
    fireEvent.change(screen.getByLabelText('用户名'), { target: { value: 'newbie' } });
    fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: 'n@x.io' } });
    fireEvent.change(screen.getByLabelText(/^密码$/), { target: { value: 'onlyletters' } });
    fireEvent.change(screen.getByLabelText('确认密码'), { target: { value: 'onlyletters' } });
    fireEvent.click(screen.getByRole('button', { name: '注册并登录' }));
    await waitFor(() => expect(screen.getByText('须同时包含字母与数字')).toBeTruthy());
    expect(registerFn).not.toHaveBeenCalled();
  });

  it('成功路径走 register（注册即登录）', async () => {
    renderPage();
    fillAndSubmit();
    await waitFor(() => expect(registerFn).toHaveBeenCalledWith('newbie', 'n@x.io', 'passw0rd-X', 'CODEAUDIT-TEST-1'));
  });

  it('用户名冲突（409）如实呈现', async () => {
    registerFn.mockRejectedValueOnce(axiosLike(409, { error: 'username or email already exists' }));
    renderPage();
    fillAndSubmit();
    await waitFor(() => expect(screen.getByText('注册失败：用户名或邮箱已被使用')).toBeTruthy());
  });

  it('邀请码无效（409 内 invite 语义）如实呈现', async () => {
    registerFn.mockRejectedValueOnce(axiosLike(409, { error: 'invalid or missing invite code' }));
    renderPage();
    fillAndSubmit();
    await waitFor(() => expect(screen.getByText('注册失败：邀请码无效或注册未开放')).toBeTruthy());
  });
});

// ---- V2.2 (ADR-255, Q10)：注册页随 registration-policy 档位自适应 ----
describe('RegisterPage 策略自适应（V2.2 ADR-255）', () => {
  it('invitation 档：邀请码栏必填呈现（动态文案替代静态提示）', async () => {
    routes['GET /v1/auth/registration-policy'] = { mode: 'invitation' };
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MemoryRouter><RegisterPage /></MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByText('邀请码')).toBeInTheDocument();
    expect(screen.getByText('请向管理员索取')).toBeInTheDocument();
    // 误导性静态文案不再存在
    expect(screen.queryByText('开放注册模式下可留空')).not.toBeInTheDocument();
  });

  it('open 档：邀请码栏隐藏', async () => {
    routes['GET /v1/auth/registration-policy'] = { mode: 'open' };
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MemoryRouter><RegisterPage /></MemoryRouter>
      </QueryClientProvider>,
    );
    await screen.findByText('用户名');
    // 初始骨架=保守 invitation（policy 未达先渲染邀请码栏），档位到达后隐藏
    await waitFor(() => expect(screen.queryByLabelText('邀请码')).not.toBeInTheDocument());
  });

  it('disabled 档：表单不渲染，如实说明+返回登录', async () => {
    routes['GET /v1/auth/registration-policy'] = { mode: 'disabled' };
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MemoryRouter><RegisterPage /></MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByText('注册未开放')).toBeInTheDocument();
    expect(screen.getByText('返回登录')).toBeInTheDocument();
    expect(screen.queryByText('用户名')).not.toBeInTheDocument();
    // 档位复原，不泄漏到后续用例
    routes['GET /v1/auth/registration-policy'] = { mode: 'invitation' };
  });
});
