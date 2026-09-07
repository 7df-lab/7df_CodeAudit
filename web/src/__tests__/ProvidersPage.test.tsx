// 推理 Provider 管理页回归（ADR-217）：路由卡+列表 CRUD+在用删除保护+切路由验证回执
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import ProvidersPage from '../pages/admin/ProvidersPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

const routes: Record<string, unknown> = {
  'GET /v1/inference/providers': {
    providers: [
      { name: 'prov-a', type: 'openai', config: { base_url: 'https://x/v1' } },
      { name: 'prov-b', type: 'anthropic', config: {} },
    ],
  },
  'GET /v1/inference/route': { provider: 'prov-a', model: 'm-1', version: '4' },
  'POST /v1/inference/providers': (ctx: { body: { name: string } }) => ({ name: ctx.body.name, created: true }),
  'PUT /v1/inference/providers/:name': { name: 'prov-a', created: false },
  'DELETE /v1/inference/providers/:name': { deleted: true },
  'PUT /v1/inference/route': {
    provider: 'prov-b', model: 'm-9', version: '5',
    validation_performed: true,
    validated_endpoints: [{ url: 'https://gw/v1', protocol: 'https' }],
  },
};
const gateway = useFakeGateway(routes);

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
      {/* 生产装配在 main.tsx 注入 zhCN（Modal/Popconfirm 默认按钮文案随语言）——测试同构 */}
      <ConfigProvider locale={zhCN}>
        <MemoryRouter initialEntries={['/admin/providers']}>
          <ProvidersPage />
        </MemoryRouter>
      </ConfigProvider>
    </QueryClientProvider>,
  );
}

describe('ProvidersPage（ADR-217）', () => {
  it('路由卡显示当前 provider/model/version，列表含"当前使用"标记', async () => {
    renderPage();
    // prov-a 出现在路由卡与表格行两处
    await waitFor(() => expect(screen.getAllByText('prov-a').length).toBeGreaterThanOrEqual(2));
    expect(screen.getByText('m-1')).toBeTruthy();
    expect(screen.getByText('prov-b')).toBeTruthy();
    expect(screen.getByText('当前使用')).toBeTruthy();
    expect(screen.getByText(/base_url=https:\/\/x\/v1/)).toBeTruthy();
    // 两个 GET 都打到真实 client 端点
    expect(gateway.requests.some((r) => r.method === 'GET' && r.url === '/v1/inference/providers')).toBe(true);
    expect(gateway.requests.some((r) => r.method === 'GET' && r.url === '/v1/inference/route')).toBe(true);
  });

  it('新建：KV 行收拢为 map，POST 体带 credentials/config 且不含空键', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));

    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'prov-c' } });
    const typeInput = screen.getByLabelText('类型');
    fireEvent.change(typeInput, { target: { value: 'zhipu' } });
    // 预置行：credentials[api_key] / config[base_url]
    const pw = screen.getByPlaceholderText('凭据值（如 sk-…）') as HTMLInputElement;
    fireEvent.change(pw, { target: { value: 'sk-test' } });
    const cfg = screen.getByPlaceholderText('配置值') as HTMLInputElement;
    fireEvent.change(cfg, { target: { value: 'https://z/v1' } });

    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    await waitFor(() => {
      const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/inference/providers');
      expect(post).toBeTruthy();
      expect(post!.body).toEqual({
        name: 'prov-c',
        type: 'zhipu',
        credentials: { api_key: 'sk-test' },
        config: { base_url: 'https://z/v1' },
      });
    });
  });

  it('编辑：走 PUT /providers/{name}（路径名权威），凭据留空=空 map', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByText('prov-b').length).toBeGreaterThan(0));
    const editBtn = screen.getAllByRole('button', { name: /编\s*辑/ })[1]; // prov-b 行
    fireEvent.click(editBtn);
    await waitFor(() => expect(screen.getByDisplayValue('prov-b')).toBeTruthy());
    // 名称输入禁用（名称不可改）
    expect((screen.getByDisplayValue('prov-b') as HTMLInputElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    await waitFor(() => {
      const put = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/inference/providers/prov-b');
      expect(put).toBeTruthy();
      expect((put!.body as { type: string }).type).toBe('anthropic');
      expect((put!.body as { credentials: Record<string, string> }).credentials).toEqual({});
    });
  });

  it('删除保护：在用 provider 禁用，未用 provider 走 DELETE', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByRole('button', { name: /删\s*除/ }).length).toBe(2));
    const delButtons = screen.getAllByRole('button', { name: /删\s*除/ });
    // prov-a 在用 → 禁用；prov-b 可删
    expect((delButtons[0] as HTMLButtonElement).disabled).toBe(true);
    expect((delButtons[1] as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(delButtons[1]);
    fireEvent.click(screen.getAllByRole('button', { name: /确\s*定/ })[0]); // Popconfirm 确认
    await waitFor(() => {
      expect(gateway.requests.some((r) => r.method === 'DELETE' && r.url === '/v1/inference/providers/prov-b')).toBe(true);
    });
  });

  it('切路由：验证开关默认开（no_verify=false），提交 provider/model', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /切\s*换\s*路\s*由/ }));
    // antd Select：点开选 prov-b
    fireEvent.mouseDown(screen.getByLabelText('Provider').closest('.ant-select')!.querySelector('.ant-select-selector')!);
    await waitFor(() => expect(screen.getByTitle('prov-b（anthropic）')).toBeTruthy());
    fireEvent.click(screen.getByTitle('prov-b（anthropic）'));
    fireEvent.change(screen.getByLabelText('模型'), { target: { value: 'glm-5.3-flash' } });
    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    await waitFor(() => {
      const put = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/inference/route');
      expect(put).toBeTruthy();
      expect(put!.body).toEqual({ provider: 'prov-b', model: 'glm-5.3-flash', no_verify: false });
    });
  });

  it('非管理员：页内 403 拦截，不发任何请求', async () => {
    session.user = { user_id: 'u-2', username: 'dev', email: '', role: 'ROLE_DEVELOPER', must_change_password: false };
    try {
      renderPage();
      expect(screen.getByText(/403：仅管理员可访问推理 Provider 管理/)).toBeTruthy();
      expect(gateway.requests.length).toBe(0);
    } finally {
      session.user = { user_id: 'u-1', username: 'admin', email: '', role: 'ROLE_ADMIN', must_change_password: false };
    }
  });
});
