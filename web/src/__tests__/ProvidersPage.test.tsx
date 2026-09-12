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

  it('新建：KV 行收拢为 map，POST 体带约定大写键 credentials/config（2026-09-11 报障修复）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));

    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'prov-c' } });
    const typeInput = screen.getByLabelText('类型');
    fireEvent.change(typeInput, { target: { value: 'zhipu' } });
    // 预置行：credentials[OPENAI_API_KEY] / config[OPENAI_BASE_URL]（约定大写键，zhipu 属 openai 系）
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
        credentials: { OPENAI_API_KEY: 'sk-test' },
        config: { OPENAI_BASE_URL: 'https://z/v1' },
      });
    });
    // 创建成功 toast 带凭据语义（服务端不回显凭据，用户必须被告知凭据已写入）
    expect(await screen.findByText(/已创建；凭据已写入（服务端不再回显）/)).toBeTruthy();
  });

  it('新建预置大写约定键；type 切 anthropic（行未填值）联动重置为 API_KEY/BASE_URL；已填值行不重置', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));

    const keyInputs = () => screen.getAllByPlaceholderText('键（如 OPENAI_API_KEY）') as HTMLInputElement[];
    // openCreate 时 type 未选 → 预置 openai 系缺省键（凭据行/配置行 placeholder 各自区分）
    expect(keyInputs()[0].value).toBe('OPENAI_API_KEY');
    expect((screen.getByPlaceholderText('键（如 OPENAI_BASE_URL）') as HTMLInputElement).value).toBe('OPENAI_BASE_URL');

    const typeInput = screen.getByLabelText('类型');
    fireEvent.change(typeInput, { target: { value: 'anthropic' } });
    expect(keyInputs()[0].value).toBe('API_KEY');
    expect((screen.getByPlaceholderText('键（如 OPENAI_BASE_URL）') as HTMLInputElement).value).toBe('BASE_URL');

    // 凭据行已填值后切回 openai 系：凭据行保持不重置；配置行仍为未填值 → 联动重置
    const pw = screen.getByPlaceholderText('凭据值（如 sk-…）');
    fireEvent.change(pw, { target: { value: 'sk-1' } });
    fireEvent.change(typeInput, { target: { value: 'zhipu' } });
    expect(keyInputs()[0].value).toBe('API_KEY'); // 值非空 → 不碰
    expect((screen.getByPlaceholderText('键（如 OPENAI_BASE_URL）') as HTMLInputElement).value).toBe('OPENAI_BASE_URL');
  });

  it('type 下拉 anthropic 选项带警示标注（端点键与 openai 系不同；BASE_URL 须 https）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));
    const typeInput = screen.getByLabelText('类型');
    fireEvent.mouseDown(typeInput.closest('.ant-select')!.querySelector('.ant-select-selector')!);
    // ADR-228：https 约束入提示——明文 http BASE_URL 会被网关验证层回落官方端点
    expect(await screen.findByText(/anthropic（注意：端点键为 BASE_URL \/ API_KEY，与 openai 系不同；BASE_URL 须 https）/)).toBeTruthy();
  });

  it('新建凭据全空：拦截提交（校验提示，不发 POST）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'prov-empty' } });
    fireEvent.change(screen.getByLabelText('类型'), { target: { value: 'openai' } });
    // 凭据值/配置值均留空 → 键预置在但值全空
    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    expect(await screen.findByText(/凭据不能为空——新建无凭据的 provider 无法通过任何验证/)).toBeTruthy();
    expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/inference/providers')).toBe(false);
  });

  it('编辑：凭据全空提交先弹确认——取消不提交，确认后 PUT credentials={}（防静默清空）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByText('prov-b').length).toBeGreaterThan(0));
    const editBtn = screen.getAllByRole('button', { name: /编\s*辑/ })[1]; // prov-b 行（anthropic）
    fireEvent.click(editBtn);
    await waitFor(() => expect(screen.getByDisplayValue('prov-b')).toBeTruthy());
    // 名称输入禁用（名称不可改）——原编辑用例回归锁保留
    expect((screen.getByDisplayValue('prov-b') as HTMLInputElement).disabled).toBe(true);
    // anthropic 型 provider 的凭据行按其约定键预置
    expect((screen.getByPlaceholderText('键（如 OPENAI_API_KEY）') as HTMLInputElement).value).toBe('API_KEY');
    // 双 Modal 叠开时 jsdom 的 byRole 查询代价极高（超时根因）——按 Modal 标题定位 footer
    // 按钮（querySelector 语义快照不受影响）：编辑 Modal 确定在其 footer 主按钮。
    const editModalOk = () =>
      document.body.querySelector<HTMLButtonElement>('.ant-modal-footer .ant-btn-primary')!;
    const confirmBtn = (text: string) => {
      const title = [...document.body.querySelectorAll('.ant-modal-title')]
        .find((t) => t.textContent === '确认清除已存凭据？');
      const modal = title?.closest('.ant-modal');
      return [...(modal?.querySelectorAll<HTMLButtonElement>('.ant-modal-footer .ant-btn') ?? [])]
        .find((b) => b.textContent === text);
    };
    fireEvent.click(editModalOk());
    // 防误清确认弹窗出现
    expect(await screen.findByText(/凭据行留空将清除已存凭据（网关不回显凭据，无法恢复）/)).toBeTruthy();
    // 取消 → 不发 PUT，回到表单
    fireEvent.click(confirmBtn('返回表单')!);
    expect(gateway.requests.some((r) => r.method === 'PUT' && r.url === '/v1/inference/providers/prov-b')).toBe(false);
    // 再次提交并确认清除 → PUT credentials={}
    fireEvent.click(editModalOk());
    await waitFor(() => expect(confirmBtn('确认清除')).toBeTruthy());
    fireEvent.click(confirmBtn('确认清除')!);
    await waitFor(() => {
      const put = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/inference/providers/prov-b');
      expect(put).toBeTruthy();
      expect((put!.body as { type: string }).type).toBe('anthropic');
      expect((put!.body as { credentials: Record<string, string> }).credentials).toEqual({});
    });
    // 确认清除的 toast 归因明确
    expect(await screen.findByText(/已更新；已存凭据已清除/)).toBeTruthy();
  });

  it('编辑：凭据非空提交免确认直接 PUT，toast"新凭据已生效"', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByText('prov-b').length).toBeGreaterThan(0));
    fireEvent.click(screen.getAllByRole('button', { name: /编\s*辑/ })[1]);
    await waitFor(() => expect(screen.getByDisplayValue('prov-b')).toBeTruthy());
    const pw = screen.getByPlaceholderText('凭据值（如 sk-…）');
    fireEvent.change(pw, { target: { value: 'sk-new' } });
    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    await waitFor(() => {
      const put = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/inference/providers/prov-b');
      expect(put).toBeTruthy();
      expect((put!.body as { credentials: Record<string, string> }).credentials).toEqual({ API_KEY: 'sk-new' });
    });
    expect(await screen.findByText(/已更新；新凭据已生效/)).toBeTruthy();
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
