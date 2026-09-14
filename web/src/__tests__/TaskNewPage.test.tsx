// T2 完成标准：04 §3 五模式向导分支覆盖（ADR-186）
// ADR-203: 迁移到 fakeGateway（axios adapter 层）——api/client 真实代码全量执行
// （类型化端点/拦截器链），handler 返回响应，未建模路由响亮失败。
// 此前 vi.mock 整模块曾长期掩盖 mock 缺 api 具名导出（queryFn 抛错被 react-query 吞，
// 数据从未加载仍全绿）。请求体矩阵收窄为"config 无任务级源码键"
// （upload_file_id/project_path 档随任务级源码覆盖一并退役），在本文件锁定。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
// 2026-09-14 弹窗化重构：向导主体=TaskNewModal（TasksPage 按钮直开，与新建项目 Modal 同构）；
// 默认导出为 /tasks/new 薄壳宿主页。测试直接渲染 Modal（portal 挂 body，screen 查询兼容）。
import { TaskNewModal, DEFAULT_SCAN_MODE, MODE_SPECS } from '../pages/tasks/TaskNewPage';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

// 向导共用的最小网关模型（文件级注册，beforeEach 对全部用例生效；请求日志按用例隔离）
// 任务向导不再承载任务级源码覆盖——p1 无来源(警告)/p2 仓库/p3 上传包
// 建任务引导（2026-09-11 报障修复）：projects 载荷/故障可按用例注入（defaultProjects 轮换）
const defaultProjects = {
  projects: [
    { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
    { project_id: 'p2', name: 'RepoDemo', repo_url: 'https://git.example.com/team/repo.git', default_branch: 'main', default_scan_mode: '', created_at: null },
    { project_id: 'p3', name: 'ZipDemo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
  ],
  pagination: { next_cursor: '', has_next: false, total: 3 },
};
let projectsPayload: unknown = defaultProjects;
let projectsFail = false;
const gateway = useFakeGateway({
  'GET /v1/projects': () => {
    if (projectsFail) throw httpError(500, { error: 'project service down' });
    return projectsPayload;
  },
  // proto L845: GetProject 返回裸 Project；repo_url 为空 → 非仓库模式（源码走项目 config 上传件）
  'GET /v1/projects/p1': { project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
  'GET /v1/projects/p1/config': { project_id: 'p1', config: {} },
  'GET /v1/projects/p2': { project_id: 'p2', name: 'RepoDemo', repo_url: 'https://git.example.com/team/repo.git', default_branch: 'main', default_scan_mode: '', created_at: null },
  'GET /v1/projects/p2/config': { project_id: 'p2', config: {} },
  'GET /v1/projects/p3': { project_id: 'p3', name: 'ZipDemo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null },
  'GET /v1/projects/p3/config': { project_id: 'p3', config: { upload_file_id: 'fid-9', upload_file_name: 'src.zip' } },
  'GET /v1/tools': {
    tools: [
      { tool_id: 'bandit', name: 'bandit', supported_languages: ['python'], output_format: 'bandit', valid: true, errors: [] },
      { tool_id: 'codeql', name: 'codeql (parser only; no executor mapping)', supported_languages: [], output_format: 'json', valid: false, errors: ['no executor mapping'] },
    ],
  },
  'POST /v1/tasks': { task_id: 't-new-1' },
  // 就地创建项目（第 1 步闭环出路）：proto 裸 Project 回执
  'POST /v1/projects': (ctx: { body: { project: { name: string } } }) => ({
    project_id: 'p-new', name: ctx.body.project.name, repo_url: '',
    default_branch: 'main', default_scan_mode: '', created_at: null,
  }),
});

function renderWizard(initialEntry = '/tasks/new') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <TaskNewModal open onClose={() => {}} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('MODE_SPECS（ADR-186 五模式分支单一来源）', () => {
  it('A 纯SAST/B 纯AI/C 融合/D AI增强/E 对比：仅 B 无工具；旧 D 审核配置保留在弃用项', () => {
    expect(MODE_SPECS.SCAN_MODE_SAST_ONLY).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_AI_ONLY).toMatchObject({ needsSastTools: false, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_PARALLEL).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_AI_ENHANCED_SAST).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_COMPARE).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_SAST_REVIEW).toMatchObject({ needsSastTools: true, needsReviewConfig: true, deprecated: true });
  });
  it('五新模式齐备 + 两弃用项；默认推荐模式C', () => {
    expect(Object.keys(MODE_SPECS)).toHaveLength(7);
    expect(DEFAULT_SCAN_MODE).toBe('SCAN_MODE_PARALLEL');
  });
});

describe('TaskNewPage 向导', () => {
  it('渲染向导骨架（项目选择器+四步步骤条）', async () => {
    renderWizard();
    await screen.findByText('选择项目');
    // 步骤条四步
    for (const t of ['项目', '模式', '参数', '确认']) {
      expect(screen.getAllByText(t).length).toBeGreaterThan(0);
    }
    // 下一步按钮初始禁用（未选项目）
    const next = screen.getByRole('button', { name: '下一步' });
    expect(next).toHaveProperty('disabled', true);
    // ADR-203 数据到达性：列表真经类型化端点加载（假网关下占位符与数据并存）
    fireEvent.mouseDown(screen.getByRole('combobox'));
    expect(await screen.findByText('Demo (p1)')).toBeTruthy();
  });

  // ADR-154 回归（行为级锁）：确认页只随第4步出现；创建按钮不在向导前段渲染。
  // （.agent/evidence/gui-audit/ 为一次性 GUI 取证脚本，非常设回归套件；
  //   行为锁由本文件 vitest 用例承担——ADR-203 mock 纪律）
  it('ADR-154: 确认页按钮不前漏（步骤边界正确）', async () => {
    renderWizard();
    await screen.findByText('选择项目');
    expect(screen.queryByRole('button', { name: '创建任务' })).toBeNull();
  });

  // 无项目时的第 1 步闭环：就地创建项目 → POST /v1/projects 并自动选用（下一步解禁）
  it('未选项目：就地创建项目 → POST /v1/projects 并自动选用', async () => {
    renderWizard();
    await screen.findByText('选择项目');
    const next = screen.getByRole('button', { name: '下一步' });
    expect(next).toHaveProperty('disabled', true); // 未选项目仍禁用（project_id 必填）
    fireEvent.change(screen.getByPlaceholderText('或输入新项目名称，就地创建'), { target: { value: '现场新建' } });
    fireEvent.click(screen.getByRole('button', { name: '创建并选用' }));
    await waitFor(() => expect(next).toHaveProperty('disabled', false)); // 创建成功即选用
    const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/projects');
    expect(post).toBeTruthy();
    expect(post!.body).toEqual({
      project: { name: '现场新建', default_branch: 'main', default_scan_mode: 'SCAN_MODE_PARALLEL' },
    });
  });
});

// 复审修正（R）：dict 补 SCAN_MODE_UNSPECIFIED 展示键后，向导模式过滤谓词
// （!MODE_SPECS[value]?.deprecated）对不在 MODE_SPECS 的 UNSPECIFIED 判 undefined 不过滤——
// 可选"未指定"→ needsSastTools undefined → sast_tools:[] → P-26 同型任务必 FAILED 复活
describe('R: 模式入口过滤（dict 补 UNSPECIFIED 键不泄漏进向导）', () => {
  it('第1步模式单选不含"未指定"（UNSPECIFIED 不进新建入口）', async () => {
    renderWizard();
    await screen.findByText(/新建扫描任务/); // 骨架就绪（projects 查询已回）
    fireEvent.mouseDown(screen.getByRole('combobox'));
    fireEvent.click(await screen.findByText('Demo (p1)', {}, { timeout: 5000 }));
    fireEvent.click(screen.getByRole('button', { name: '下一步' })); // step0 → 1
    await screen.findByText(/模式B 纯AI/);
    expect(screen.queryByText(/未指定/)).toBeNull();
    expect(screen.getByText(/模式D AI增强SAST/)).toBeTruthy(); // 正常模式不受牵连
  });
});

describe('TaskNewPage 项目级源码来源（项目层级决定源码仓库）', () => {
  // 走到参数步：选项目 → 模式B 纯AI（needsSastTools=false，绕开工具多选）
  async function gotoParamsStep(projectLabel: string) {
    fireEvent.mouseDown(screen.getByRole('combobox'));
    fireEvent.click(await screen.findByText(projectLabel));
    fireEvent.click(screen.getByRole('button', { name: '下一步' })); // step0 → 1
    fireEvent.click(await screen.findByText(/模式B 纯AI/));
    fireEvent.click(screen.getByRole('button', { name: '下一步' })); // step1 → 2
    // 2026-09-14 视觉重设计后"源码来源"在参数步 Alert 与常驻任务简报栏两处出现——
    // 等待意图不变（该信息已渲染），findAllByText 容忍多元素
    await screen.findAllByText(/源码来源|未配置源码来源/);
  }

  // ADR-203 资金流：关掉自动启动（start 链路属 stateMachine 测试域），触发创建并捕获请求体
  async function createAndCaptureTaskBody(): Promise<Record<string, unknown>> {
    fireEvent.click(screen.getByRole('button', { name: '下一步：确认' }));
    fireEvent.click(await screen.findByText(/创建后立即启动/)); // 关闭自动启动
    fireEvent.click(await screen.findByRole('button', { name: '创建任务' }));
    await screen.findByText('任务已创建');
    const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/tasks');
    expect(post).toBeTruthy();
    return post!.body as Record<string, unknown>;
  }

  it('向导不提供任务级源码覆盖：无上传控件、无路径输入（原 ADR-202 档退役）', async () => {
    const { container } = renderWizard();
    await gotoParamsStep('Demo (p1)');
    expect(container.querySelector('input[type="file"]')).toBeNull();
    expect(screen.queryByPlaceholderText('/path/to/project')).toBeNull();
  });

  it('无来源项目：参数步与确认页如实警告（启动将失败）', async () => {
    renderWizard();
    await gotoParamsStep('Demo (p1)');
    expect(screen.getByText(/该项目未配置源码来源/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: '下一步：确认' }));
    expect(await screen.findByText(/未配置——启动将失败/)).toBeTruthy();
  });

  it('仓库项目：源码来源只读展示仓库地址（自动拉取）', async () => {
    renderWizard();
    await gotoParamsStep('RepoDemo (p2)');
    expect(screen.getByText(/源码来源（项目级）：仓库自动拉取（https:\/\/git\.example\.com\/team\/repo\.git）/)).toBeTruthy();
  });

  it('上传型项目：源码来源显示压缩包原始文件名（config.upload_file_name）', async () => {
    renderWizard();
    await gotoParamsStep('ZipDemo (p3)');
    expect(screen.getByText(/源码来源（项目级）：项目压缩包：src\.zip/)).toBeTruthy();
  });

  it('ADR-202/200 请求体矩阵收窄：config 不再携带任务级源码键（恒无 upload_file_id/project_path）', async () => {
    renderWizard();
    await gotoParamsStep('Demo (p1)');
    const body = (await createAndCaptureTaskBody()) as { sast_tools: string[]; config: Record<string, string> };
    expect(body.config).toEqual({}); // 模式B 无审核键 → 空 config；源码键已退役
    expect(body.sast_tools).toEqual([]); // 模式B 无工具
    expect('upload_file_id' in body.config).toBe(false);
    expect('project_path' in body.config).toBe(false);
  });
});

// 建任务引导（2026-09-11 用户报障修复）：加载失败显性化+重试 / 空列表引导 / 深链预选
describe('TaskNewPage 建任务引导（2026-09-11 报障修复）', () => {
  it('空列表：下拉空态引导"前往项目页创建"（Link → /projects）', async () => {
    projectsPayload = { projects: [], pagination: { next_cursor: '', has_next: false, total: 0 } };
    try {
      renderWizard();
      await screen.findByText('选择项目');
      fireEvent.mouseDown(screen.getByRole('combobox'));
      const link = await screen.findByRole('link', { name: '前往项目页创建' });
      expect(link.getAttribute('href')).toBe('/projects');
    } finally {
      projectsPayload = defaultProjects;
    }
  });

  it('项目列表加载失败：Alert 显性化 + 重试按钮（refetch 打到真实端点）', async () => {
    projectsFail = true;
    try {
      renderWizard();
      expect(await screen.findByText('项目列表加载失败')).toBeTruthy();
      const gets = () => gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/projects').length;
      const before = gets();
      fireEvent.click(screen.getByRole('button', { name: /重\s*试/ }));
      await waitFor(() => expect(gets()).toBeGreaterThan(before));
    } finally {
      projectsFail = false;
    }
  });

  it('?project_id= 深链预选：命中列表项 → 预选 RepoDemo(p2)，下一步可点', async () => {
    renderWizard('/tasks/new?project_id=p2');
    await screen.findByText('选择项目');
    // 预选后 Select 渲染选中项标签（selection-item；antd input.value 恒空），下一步解锁
    await waitFor(() =>
      expect(document.body.querySelector('.ant-select-selection-item')?.textContent).toBe('RepoDemo (p2)'),
    );
    expect(screen.getByRole('button', { name: '下一步' })).toHaveProperty('disabled', false);
  });

  it('?project_id= 未命中列表项 → 保持未选（下一步禁用，不猜 ID）', async () => {
    renderWizard('/tasks/new?project_id=p-absent');
    await screen.findByText('选择项目');
    // 等列表真实加载完成后再断言未预选（无选中项标签渲染）
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'GET' && r.url === '/v1/projects')).toBe(true),
    );
    expect(document.body.querySelector('.ant-select-selection-item')).toBeNull();
    expect(screen.getByRole('button', { name: '下一步' })).toHaveProperty('disabled', true);
  });
});

// 任务简报栏（2026-09-14 弹窗化重构中移除：Modal 内双栏过挤，简报定稿并入确认步，
// 由"源码来源/引擎编排"等确认步断言覆盖）——原侧栏用例随形态一并退役。
describe('TaskNewPage 创建向导弹窗形态（2026-09-14 二期）', () => {
  it('ADR-154 行为锁在弹窗形态下仍成立：前 3 步不渲染"创建任务"按钮（footer 集中）', async () => {
    renderWizard();
    await screen.findByText('选择项目');
    expect(screen.queryByRole('button', { name: '创建任务' })).toBeNull();
    // antd 两字按钮自动插空格（"取 消"），正则容忍
    expect(screen.getByRole('button', { name: /取\s*消/ })).toBeTruthy(); // footer 常驻取消
  });
});
