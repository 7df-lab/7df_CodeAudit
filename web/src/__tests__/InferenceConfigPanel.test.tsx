// 推理配置面板回归（ADR-217；2026-10-05 父含子合并：原独立页收敛为工作区行内抽屉）：
// 路由卡+列表 CRUD+在用删除保护+切路由验证回执+宿主页 403 闸随迁。
// 网关行为契约（2026-09-17 107 实测）锁：类型六选受控、创建后类型/名称锁定、
// anthropic 端点键=ANTHROPIC_BASE_URL、编辑凭据留空=保留（credentials:{}）。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import InferenceConfigPanel from '../pages/admin/InferenceConfigPanel';
import WorkspacesPage from '../pages/admin/WorkspacesPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

const routes: Record<string, unknown> = {
  'GET /v1/workspaces': {
    workspaces: [{ name: 'default', phase: 'PHASE_ACTIVE', labels: {} }],
  },
  'GET /v1/inference/providers': {
    providers: [
      { name: 'prov-a', type: 'openai', config: { OPENAI_BASE_URL: 'https://x/v1' } },
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
vi.mock('../auth/session', () => ({ useCanWrite: () => true,
  useSession: () => ({ user: session.user }),
}));

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      {/* 生产装配在 main.tsx 注入 zhCN（Modal/Popconfirm 默认按钮文案随语言）——测试同构 */}
      <ConfigProvider locale={zhCN}>
        <MemoryRouter initialEntries={['/admin/providers']}>
          <InferenceConfigPanel workspace="" />
        </MemoryRouter>
      </ConfigProvider>
    </QueryClientProvider>,
  );
}

const PW_PLACEHOLDER = 'API Key（留空=保留已存）';

// 宿主页装配（父含子）：工作区页自身不依赖路由器，MemoryRouter 仅为与 renderPage 同构
function renderWorkspaces() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ConfigProvider locale={zhCN}>
        <MemoryRouter initialEntries={['/admin/workspaces']}>
          <WorkspacesPage />
        </MemoryRouter>
      </ConfigProvider>
    </QueryClientProvider>,
  );
}

// antd Select 交互（jsdom）：点开下拉后按 option title 点选
const pickSelect = (label: string, optionTitle: string) => {
  fireEvent.mouseDown(screen.getByLabelText(label).closest('.ant-select')!.querySelector('.ant-select-selector')!);
  fireEvent.click(screen.getByTitle(optionTitle));
};
// 厂商模板 Select 无 name（纯填充器，不挂 label 关联）——按 Form.Item 文本就近定位
const pickTemplate = (optionTitle: string) => {
  fireEvent.mouseDown(screen.getByText('厂商模板').closest('.ant-form-item')!
    .querySelector('.ant-select-selector')!);
  fireEvent.click(screen.getByTitle(optionTitle));
};

describe('InferenceConfigPanel（ADR-217；原 ProvidersPage 主体，2026-10-05 父含子合并）', () => {
  it('路由卡显示当前 provider/model/version，列表含"当前使用"标记', async () => {
    renderPage();
    // prov-a 出现在路由卡与表格行两处
    await waitFor(() => expect(screen.getAllByText('prov-a').length).toBeGreaterThanOrEqual(2));
    expect(screen.getByText('m-1')).toBeTruthy();
    expect(screen.getByText('prov-b')).toBeTruthy();
    expect(screen.getByText('当前使用')).toBeTruthy();
    expect(screen.getByText(/OPENAI_BASE_URL=https:\/\/x\/v1/)).toBeTruthy();
    // 两个 GET 都打到真实 client 端点
    expect(gateway.requests.some((r) => r.method === 'GET' && r.url === '/v1/inference/providers')).toBe(true);
    expect(gateway.requests.some((r) => r.method === 'GET' && r.url === '/v1/inference/route')).toBe(true);
  });

  it('新建：KV 行收拢为 map，POST 体带约定大写键 credentials/config（2026-09-11 报障修复）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));

    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'prov-c' } });
    // 新建默认 type=openai（受控六选；zhipu/deepseek 是厂商不是类型，不在选项里）
    const pw = screen.getByPlaceholderText(PW_PLACEHOLDER) as HTMLInputElement;
    fireEvent.change(pw, { target: { value: 'sk-test' } });
    const cfg = screen.getByPlaceholderText('配置值') as HTMLInputElement;
    fireEvent.change(cfg, { target: { value: 'https://z/v1' } });

    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    await waitFor(() => {
      const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/inference/providers');
      expect(post).toBeTruthy();
      expect(post!.body).toEqual({
        name: 'prov-c',
        type: 'openai',
        credentials: { OPENAI_API_KEY: 'sk-test' },
        config: { OPENAI_BASE_URL: 'https://z/v1' },
      });
    });
    // 创建成功 toast 带凭据语义（服务端不回显凭据，用户必须被告知凭据已写入）
    expect(await screen.findByText(/已创建；凭据已写入（服务端不再回显）/)).toBeTruthy();
  });

  it('类型受控六选（网关 supported 表），无 zhipu/openai-compatible 等失实选项', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));
    fireEvent.mouseDown(screen.getByLabelText('类型').closest('.ant-select')!.querySelector('.ant-select-selector')!);
    // getAllByTitle：选中态展示与下拉选项同名（openai 既在框内也在选项里）
    for (const t of ['openai', 'anthropic', 'nvidia', 'deepinfra', 'google-vertex-ai', 'aws-bedrock']) {
      expect(screen.getAllByTitle(t).length).toBeGreaterThanOrEqual(1);
    }
    expect(screen.queryByTitle('zhipu')).toBeNull();
    expect(screen.queryByTitle('openai-compatible')).toBeNull();
  });

  it('新建预置大写约定键；type 切 anthropic（行未填值）联动重置为 API_KEY/ANTHROPIC_BASE_URL；已填值行不重置', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));

    const keyInputs = () => screen.getAllByPlaceholderText('键（如 OPENAI_API_KEY）') as HTMLInputElement[];
    // openCreate 默认 type=openai → 预置 openai 系约定键
    expect(keyInputs()[0].value).toBe('OPENAI_API_KEY');
    expect((screen.getByPlaceholderText('键（如 OPENAI_BASE_URL）') as HTMLInputElement).value).toBe('OPENAI_BASE_URL');

    pickSelect('类型', 'anthropic');
    expect(keyInputs()[0].value).toBe('API_KEY');
    expect((screen.getByPlaceholderText('键（如 OPENAI_BASE_URL）') as HTMLInputElement).value).toBe('ANTHROPIC_BASE_URL');

    // 凭据行已填值后切回 openai：凭据行保持不重置；配置行仍为未填值 → 联动重置
    const pw = screen.getByPlaceholderText(PW_PLACEHOLDER);
    fireEvent.change(pw, { target: { value: 'sk-1' } });
    pickSelect('类型', 'openai');
    expect(keyInputs()[0].value).toBe('API_KEY'); // 值非空 → 不碰
    expect((screen.getByPlaceholderText('键（如 OPENAI_BASE_URL）') as HTMLInputElement).value).toBe('OPENAI_BASE_URL');
  });

  it('厂商模板：智谱 GLM 一键铺 anthropic 型+ANTHROPIC_BASE_URL 端点+API_KEY 键名', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));
    pickTemplate('智谱 GLM（Anthropic 兼容）');
    expect((screen.getAllByPlaceholderText('键（如 OPENAI_API_KEY）')[0] as HTMLInputElement).value).toBe('API_KEY');
    const cfg = screen.getByPlaceholderText('配置值') as HTMLInputElement;
    expect(cfg.value).toBe('https://open.bigmodel.cn/api/anthropic');
    // 模板铺的类型落在受控 Select 上（显示 anthropic）
    expect(screen.getByLabelText('类型').closest('.ant-select')!.textContent).toContain('anthropic');
  });

  it('厂商模板覆盖全部六型（类型约束由模板层吸收，选型不再分两处）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));
    // 模板下拉：六型各有入口（实测型带端点，未实证型标注端点手填）
    fireEvent.mouseDown(screen.getByText('厂商模板').closest('.ant-form-item')!
      .querySelector('.ant-select-selector')!);
    for (const label of [
      '智谱 GLM（Anthropic 兼容）', 'Anthropic 官方', 'DeepSeek（OpenAI 兼容）', 'OpenAI',
      'NVIDIA NIM（端点手填）', 'DeepInfra（端点手填）',
      'Google Vertex AI（端点手填）', 'AWS Bedrock（端点手填）', '自定义（手填）',
    ]) {
      expect(screen.getByTitle(label)).toBeTruthy();
    }
    // 端点手填型（nvidia）：带出类型与键位、不编造端点
    fireEvent.click(screen.getByTitle('NVIDIA NIM（端点手填）'));
    expect(screen.getByLabelText('类型').closest('.ant-select')!.textContent).toContain('nvidia');
    expect((screen.getByPlaceholderText('配置值') as HTMLInputElement).value).toBe('');
  });

  it('新建凭据全空：拦截提交（校验提示，不发 POST）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('当前使用')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /新\s*建\s*Provider/ }));
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'prov-empty' } });
    // 默认 type=openai 已预置；凭据值/配置值均留空 → 键预置在但值全空
    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    expect(await screen.findByText(/凭据不能为空——新建无凭据的 provider 无法通过任何验证/)).toBeTruthy();
    expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/inference/providers')).toBe(false);
  });

  it('编辑：名称/类型均锁定（网关不可改约束）；凭据全空提交=保留（直接 PUT credentials:{}，无确认弹窗）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByText('prov-b').length).toBeGreaterThan(0));
    const editBtn = screen.getAllByRole('button', { name: /编\s*辑/ })[1]; // prov-b 行（anthropic）
    fireEvent.click(editBtn);
    await waitFor(() => expect(screen.getByDisplayValue('prov-b')).toBeTruthy());
    // 名称输入禁用 + 类型 Select 禁用（网关 "type cannot be changed"）
    expect((screen.getByDisplayValue('prov-b') as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByLabelText('类型').closest('.ant-select')!.className).toContain('ant-select-disabled');
    // anthropic 型 provider 的凭据行按其约定键预置
    expect((screen.getByPlaceholderText('键（如 OPENAI_API_KEY）') as HTMLInputElement).value).toBe('API_KEY');
    // A-6 锚点：编辑面明示凭据合并语义（留空=保留+删键须删除重建），防"留空=清空"失实断言回潮
    expect(screen.getByText(/留空=保留已存，填写=覆盖；删键须删除重建 Provider/)).toBeTruthy();
    // 双 Modal 叠开时 jsdom 的 byRole 查询代价极高——按 footer 主按钮定位（原用例经验）
    const editModalOk = () =>
      document.body.querySelector<HTMLButtonElement>('.ant-modal-footer .ant-btn-primary')!;
    fireEvent.click(editModalOk());
    // 2026-09-17 网关实测：credentials:{} 更新=保留已存——无需确认弹窗直接提交
    await waitFor(() => {
      const put = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/inference/providers/prov-b');
      expect(put).toBeTruthy();
      expect((put!.body as { type: string }).type).toBe('anthropic');
      expect((put!.body as { credentials: Record<string, string> }).credentials).toEqual({});
    });
    expect(await screen.findByText(/已更新；已存凭据保留未变（留空=保留）/)).toBeTruthy();
    // 旧"确认清除"弹窗已随失实语义一并移除
    expect(screen.queryByText(/确认清除已存凭据/)).toBeNull();
  });

  it('编辑：凭据非空提交直接 PUT 覆盖，toast"新凭据已生效"', async () => {
    renderPage();
    await waitFor(() => expect(screen.getAllByText('prov-b').length).toBeGreaterThan(0));
    fireEvent.click(screen.getAllByRole('button', { name: /编\s*辑/ })[1]);
    await waitFor(() => expect(screen.getByDisplayValue('prov-b')).toBeTruthy());
    const pw = screen.getByPlaceholderText(PW_PLACEHOLDER);
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
    fireEvent.change(screen.getByLabelText('模型'), { target: { value: 'glm-4.5' } });
    fireEvent.click(screen.getByRole('button', { name: /确\s*定/ }));
    await waitFor(() => {
      const put = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/inference/route');
      expect(put).toBeTruthy();
      expect(put!.body).toEqual({ provider: 'prov-b', model: 'glm-4.5', no_verify: false });
    });
  });

  // 父含子合并（2026-10-05）：面板不再自带 admin 闸（宿主 WorkspacesPage 双闸：
  // 路由 RequireAdmin + 页内 403 卡），非管理员防线回归锚随迁宿主页。
  it('非管理员：宿主页 403 拦截，面板不挂载、不发任何请求', async () => {
    session.user = { user_id: 'u-2', username: 'dev', email: '', role: 'ROLE_DEVELOPER', must_change_password: false };
    try {
      renderWorkspaces();
      expect(screen.getByText(/403：仅管理员可访问工作区管理/)).toBeTruthy();
      expect(gateway.requests.length).toBe(0);
    } finally {
      session.user = { user_id: 'u-1', username: 'admin', email: '', role: 'ROLE_ADMIN', must_change_password: false };
    }
  });

  it('父含子：行内就绪度点击 → 打开该区推理配置抽屉（default=全局默认，面板挂载可用）', async () => {
    renderWorkspaces();
    // 就绪度徽标：2 个 Provider · 路由 m-1（providers/route 夹具回填）
    const badge = await screen.findByText(/2 个 Provider · 路由 m-1/);
    fireEvent.click(badge.closest('button')!);
    // 抽屉标题带区名；default 行显性化"全局默认"语义
    expect(await screen.findByText('推理配置 · default（全局默认）')).toBeTruthy();
    // 面板真实挂载（路由卡 Descriptions + Provider 表格两处出现 prov-a），非空抽屉
    expect((await screen.findAllByText('prov-a')).length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText(/当前推理路由/)).toBeTruthy();
  });
});
