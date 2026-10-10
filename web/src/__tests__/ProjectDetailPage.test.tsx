// 项目详情组件回归（14号 §3.2 P0 + ADR-181 关联任务）：项目信息/关联任务表/空态。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import ProjectDetailPage from '../pages/ProjectDetailPage';
import { SessionProvider } from '../auth/session';
import { httpError, useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行，仅 HTTP 层伪造。
// ADR-181 断言（详情页必须带 project_id 过滤）从 mock 内部断言改为 handler 内守卫——
// 缺过滤直接抛错，查询失败即页面空态，行为级响亮失败。
// 建任务引导：tasksOverride 供空态用例注入空任务列表（用后还原）。
// 源码补配：projectOverride/configOverride 注入无来源项目
// （原夹具 p1 自带仓库+上传件，无法覆盖"未配置"分支）。
let tasksOverride: unknown = null;
let projectOverride: unknown = null;
let configOverride: unknown = null;
let configPutFail = false; // 两步写半程失败注入（仓库已存/清指针失败）
let configGetFail = false; // 评审 P3a：config GET 5xx 注入（不永挂加载中/不谎报未配置）
const gateway = useFakeGateway({
  'GET /v1/projects/:projectId': () => projectOverride ?? { project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', created_at: '2026-09-02T00:00:00Z' },
  'GET /v1/projects/:projectId/config': () => {
    if (configGetFail) throw httpError(500, { error: 'config service down' });
    return configOverride ?? { project_id: 'p1', config: { upload_file_id: 'file-1', upload_file_name: 'src.zip' } };
  },
  // 源码补配双通道（ProjectSourceModal）：仓库档=整对象 PUT / 上传档=config 整 map PUT
  'PUT /v1/projects/:projectId': (ctx: HandlerCtx) => (ctx.body as { project: unknown }).project,
  'PUT /v1/projects/:projectId/config': (ctx: HandlerCtx) => {
    if (configPutFail) throw httpError(500, { error: 'storage down' });
    return (ctx.body as { config: { project_id: string; config: Record<string, string> } }).config;
  },
  'DELETE /v1/projects/:projectId': {},
  // 入口统一向导：详情页按钮深链 /tasks/new?project_id= → 向导挂载
  // 需要 projects 列表（深链预选要命中列表项）与 agents 清单（常开查询）
  'GET /v1/projects': () => ({
    projects: [
      { project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', default_scan_mode: '', created_at: null },
    ],
    pagination: { next_cursor: '', has_next: false, total: 1 },
  }),
  'GET /v1/agents': () => ({ agents: [
    { id: 'dsh', dialect: 'dsh-sse', is_default: true, tool_events: true, structured_submit: true, interaction: false, session_resume: false, conclude_semantics: true },
  ] }),
  'GET /v1/tasks': (ctx: HandlerCtx) => {
    if (ctx.query.get('project_id') !== 'p1') {
      throw new Error('ADR-181: 关联任务查询必须携带 project_id 过滤');
    }
    if (tasksOverride) return tasksOverride; // 建任务引导用例（2026-09-11 报障）：空列表可注入
    return {
      tasks: [
        { task_id: 'gw-1', project_id: 'p1', scan_mode: 'SCAN_MODE_TRADITIONAL_FIRST', sast_tools: [], status: 'TASK_STATUS_COMPLETED', stages: [], created_at: '2026-09-01T10:00:00Z', updated_at: null, error_message: '', retry_count: 0 },
        { task_id: 'gw-2', project_id: 'p1', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], created_at: '2026-09-02T00:00:00Z', updated_at: null, error_message: '', retry_count: 0 },
      ],
    };
  },
});

function renderWith() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionProvider>
        <MemoryRouter initialEntries={['/projects/p1']}>
          <Routes>
            <Route path="/projects/:id" element={<ProjectDetailPage />} />
          </Routes>
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

describe('ProjectDetailPage（ADR-181 关联任务）', () => {
  it('渲染项目信息与关联任务表（含 project_id 过滤请求与状态映射）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('关联任务（2）')).toBeTruthy());
    expect(screen.getByText('gw-1')).toBeTruthy();
    expect(screen.getByText('gw-2')).toBeTruthy();
    expect(screen.getByText('旧·SAST→AI增强')).toBeTruthy(); // ADR-182 弃用项历史兼容展示
    expect(screen.getByText('已完成')).toBeTruthy();
    expect(screen.getByText('执行中')).toBeTruthy();
  });

  it('E-17: 删除项目经 Popconfirm 确认 → DELETE /v1/projects/p1', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('关联任务（2）')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '删除项目' }));
    const ok = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-popconfirm-buttons .ant-btn-primary'),
    );
    expect(ok).toBeTruthy();
    fireEvent.click(ok!);
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'DELETE' && r.url === '/v1/projects/p1')).toBe(true),
    );
  });

  it('源码来源显示压缩包原始文件名（2026-09-09 用户指令）', async () => {
    renderWith();
    expect(await screen.findByText('上传压缩包（src.zip）')).toBeTruthy();
  });

  it('无名上传 → 统一短文案（2026-10-08 报障：旧注"创建于文件名记录启用前"与实证不符——API/e2e 路径不写名，与时间无关）', async () => {
    configOverride = { project_id: 'p1', config: { upload_file_id: 'file-1' } };
    try {
      renderWith();
      expect(await screen.findByText('上传压缩包（名称未记录）')).toBeTruthy();
      expect(screen.queryByText(/创建于文件名记录启用前/)).toBeNull();
    } finally {
      configOverride = null;
    }
  });

  // 创建任务=向导悬浮窗：跳转 /tasks/new 会断回程
  // （薄壳关闭落任务列表而非来源详情页）——本页作为向导第三宿主原地弹出，preset 预选
  // 本项目，取消回原地；内容=同一向导（单一 UI 单一事实源）
  it('创建扫描任务=向导悬浮窗：原地弹出+预选本项目+取消回详情页（不跳转）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('关联任务（2）')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '创建扫描任务' }));
    // 向导原地弹出（详情页内容仍在视口）且本项目经 preset 预选
    expect(await screen.findByText('新建扫描任务')).toBeTruthy();
    expect(screen.getByText('关联任务（2）')).toBeTruthy();
    await waitFor(() =>
      expect(document.body.querySelector('.ant-select-selection-item')?.textContent).toBe('Demo (p1)'),
    );
    // 取消回原地：弹窗走隐藏（非卸载）路径，断言不可见而非不存在（TasksPage 同款先例）；
    // 详情页仍在视口（不落在任务列表）
    fireEvent.click(screen.getByRole('button', { name: /取\s*消/ }));
    const wizardTitle = screen.getByText('新建扫描任务');
    await waitFor(() => expect(wizardTitle).not.toBeVisible());
    expect(screen.getByText('关联任务（2）')).toBeTruthy();
  });

  // （快捷弹窗的 P0-2 默认模式跟随与重开重置两用例随弹窗退役删除——同能力由向导侧
  //   TaskNewPage.test 的 P0-2 模式跟随用例锁定）

  it('无来源项目：入口=配置源码 + 创建任务跳向导（无来源拦截由向导参数步承接，向导测试域已锁）', async () => {
    projectOverride = { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', created_at: null };
    configOverride = { project_id: 'p1', config: {} };
    try {
      renderWith();
      expect(await screen.findByRole('button', { name: '配置源码' })).toBeTruthy();
      fireEvent.click(screen.getByRole('button', { name: '创建扫描任务' }));
      expect(await screen.findByText('新建扫描任务')).toBeTruthy(); // 向导悬浮窗原地弹出（无来源拦截在参数步，向导测试域已锁）
    } finally {
      projectOverride = null;
      configOverride = null;
    }
  });

  it('空任务列表空态（2026-09-11 报障）：直链任务向导并深链预选本项目（/tasks/new?project_id=p1）', async () => {
    tasksOverride = { tasks: [] };
    try {
      renderWith();
      const link = await screen.findByRole('link', { name: '前往任务向导创建' });
      expect(link.getAttribute('href')).toBe('/tasks/new?project_id=p1');
      // 旧文案不再出现（原"可在任务向导中选择本项目创建"无出路）
      expect(screen.queryByText(/可在任务向导中选择本项目创建/)).toBeNull();
    } finally {
      tasksOverride = null;
    }
  });

  // 2026-10-03 用户报障收口：项目配置卡恢复编辑面（此前纯只读——就地创建的无来源
  // 项目死路，任务启动必败 project_path 未配置）
  it('源码补配·仓库档：UpdateProject 整对象回写 + 清陈旧上传指针（解析链上传优先，不清则换仓库永不被读到）', async () => {
    renderWith();
    fireEvent.click(await screen.findByRole('button', { name: '更换源码' }));
    const input = await screen.findByPlaceholderText('https://git.example.com/team/repo.git');
    fireEvent.change(input, { target: { value: 'https://git.new/team/repo.git' } });
    fireEvent.click(document.body.querySelector<HTMLElement>('.ant-modal-footer .ant-btn-primary')!);
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'PUT' && r.url === '/v1/projects/p1')).toBe(true),
    );
    const proj = (gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/projects/p1')?.body as { project: Record<string, unknown> }).project;
    // 整体替换语义锚：GET 读回的字段必须全量随行（漏带即被服务端清零）
    expect(proj).toMatchObject({ project_id: 'p1', name: 'Demo', default_branch: 'main', repo_url: 'https://git.new/team/repo.git' });
    expect(proj.created_at).toBe('2026-09-02T00:00:00Z');
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'PUT' && r.url === '/v1/projects/p1/config')).toBe(true),
    );
    const written = (gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/projects/p1/config')?.body as { config: { config: Record<string, string> } }).config.config;
    expect(written.upload_file_id).toBeUndefined();
    expect(written.upload_file_name).toBeUndefined();
  });

  it('config GET 5xx：源码描述如实报"加载失败"，不永挂加载中、不谎报未配置（评审 P3a）', async () => {
    projectOverride = { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', created_at: null };
    configGetFail = true;
    try {
      renderWith();
      expect(await screen.findByText('源码配置加载失败（状态未知）')).toBeTruthy();
      expect(screen.queryByText('未配置（上传压缩包或填写仓库地址）')).toBeNull();
      expect(screen.queryByText('加载中…')).toBeNull();
      // 未知态按钮回落"配置源码"（弹窗自带 GET，任何态打开都安全）
      expect(screen.getByRole('button', { name: '配置源码' })).toBeTruthy();
    } finally {
      projectOverride = null;
      configGetFail = false;
    }
  });

  it('源码补配·两步写半程失败：仓库已存但清指针失败——如实报半程状态，不报成功不静默（评审 c1）', async () => {
    configPutFail = true;
    try {
      renderWith();
      fireEvent.click(await screen.findByRole('button', { name: '更换源码' }));
      const input = await screen.findByPlaceholderText('https://git.example.com/team/repo.git');
      fireEvent.change(input, { target: { value: 'https://git.new/team/repo.git' } });
      fireEvent.click(document.body.querySelector<HTMLElement>('.ant-modal-footer .ant-btn-primary')!);
      // 两步都真的发出（仓库档保存面完整执行）
      await waitFor(() =>
        expect(gateway.requests.some((r) => r.method === 'PUT' && r.url === '/v1/projects/p1')).toBe(true),
      );
      await waitFor(() =>
        expect(gateway.requests.some((r) => r.method === 'PUT' && r.url === '/v1/projects/p1/config')).toBe(true),
      );
      // 半程状态如实透出（前缀"源码配置失败：" + 半程说明），弹窗保持打开可重试
      expect(await screen.findByText(/仓库地址已保存，但清理旧上传件失败/)).toBeTruthy();
    } finally {
      configPutFail = false;
    }
  });

  // （原「无来源项目：建任务弹窗亮警告直达补配」用例随快捷弹窗退役——无来源拦截
  //   迁往向导参数步，由 TaskNewPage.test 的警告/直达补配用例锁定；本页留存配置入口用例见上）
});
