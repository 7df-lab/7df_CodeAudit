// 项目列表组件回归（14号 P0 页面）：渲染 + 创建流行为（ADR-203 fakeGateway 测试台）
// ADR-203：弹窗上传入口保留并改造为零落盘直传——
// uploadArchive(ADR-200 file_id 契约) → 项目 config.upload_file_id → 任务启动时后端拉包。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import ProjectsPage from '../pages/ProjectsPage';
import ProjectNewPage from '../pages/ProjectNewPage';
import { SessionProvider } from '../auth/session';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

let configFail = false;

const gateway = useFakeGateway({
  'GET /v1/projects': () => ({
    // ADR-164: 列表走服务端游标翻页——响应含 pagination.total；p-up 为上传型项目（无 repo_url）；
    // p-e2e/p-bare=2026-10-08 报障夹具：无名上传（API/e2e 路径不写名）与全无源码两种失真态
    projects: [
      { project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null },
      { project_id: 'p-up', name: '上传型', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null },
      { project_id: 'p-e2e', name: 'E2E无名上传', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_SAST_ONLY', created_at: null },
      { project_id: 'p-bare', name: '裸项目', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_SAST_ONLY', created_at: null },
    ],
    pagination: { next_cursor: '', has_next: false, total: 4 },
  }),
  'GET /v1/projects/p-up/config': () => ({ project_id: 'p-up', config: { upload_file_id: 'file-9', upload_file_name: 'code9.zip' } }),
  'GET /v1/projects/p-e2e/config': () => ({ project_id: 'p-e2e', config: { upload_file_id: 'file-x' } }),
  'GET /v1/projects/p-bare/config': () => ({ project_id: 'p-bare', config: {} }),
  'POST /v1/uploads/archive': () => ({ upload_id: 'up-1', file_id: 'file-1', file_path: 'uploads/up-1/src.zip', size_bytes: 3 }),
  'POST /v1/projects': () => ({ project_id: 'p-new' }),
  // 交互审计 Top4-③：源码关联失败可注入（此前整体 onError 报"创建失败"，重试=重复建项目）
  'PUT /v1/projects/:projectId/config': () => {
    if (configFail) throw httpError(500, { error: 'storage down' });
    return { project_id: 'p-new', config: {} };
  },
  'POST /v1/tasks': () => ({ task_id: 't-new' }),
  'POST /v1/tasks/:taskId/start': () => ({}),
});

function renderWith() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionProvider>
        <MemoryRouter initialEntries={['/projects']}>
          <ProjectsPage />
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

async function openModal() {
  fireEvent.click(await screen.findByText('新建项目'));
  const nameInput = (await screen.findByText('名称')).closest('.ant-form-item')!.querySelector('input')!;
  fireEvent.change(nameInput, { target: { value: 'P2' } });
  expect(document.querySelector('.ant-modal form')).toBeTruthy();
}

async function submitModal() {
  fireEvent.submit(document.querySelector('.ant-modal form')!);
  await waitFor(() =>
    expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/projects')).toBe(true),
  );
}

describe('ProjectsPage', () => {
  it('渲染项目行并映射默认模式中文（P4 展示即数据）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    // 两行项目（仓库型+上传型）同为 AI_ONLY → 模式映射出现两次
    expect(screen.getAllByText('模式B 纯AI').length).toBe(2);
  });

  it('源码列三态（2026-10-08 报障收口）：有名=文件名 / 无名=统一短文案 / 无源码=如实未配置', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('上传型')).toBeTruthy());
    // ①有名（web 创建/换源码路径写入）→ 显示文件名（悬浮见 file_id）
    await waitFor(() => expect(screen.getByText('code9.zip')).toBeTruthy());
    expect(screen.getByText('code9.zip').closest('span')?.getAttribute('title')).toBe('file-9');
    // ②无名上传（API/e2e 路径不写名、原名服务端无档）→ 统一短文案，悬浮见 file_id 溯源
    await waitFor(() => expect(screen.getByText('压缩包（名称未记录）')).toBeTruthy());
    expect(screen.getByText('压缩包（名称未记录）').closest('span')?.getAttribute('title')).toBe('file-x');
    // ③全无源码 → 如实"未配置源码"；泛称"上传压缩包"从列表绝迹（此前无源码项目被假称有上传件）
    await waitFor(() => expect(screen.getByText('未配置源码')).toBeTruthy());
    expect(screen.queryByText('上传压缩包')).toBeNull();
  });

  it('ADR-203: 上传压缩包创建项目——file_id 落项目 config，自动建任务 config 留空', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();

    // 弹窗上传：jsdom 构造 File 走真实 axios FormData 序列化（fakeGateway ctx.raw 接住）
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    expect(fileInput).toBeTruthy();
    fireEvent.change(fileInput, {
      target: { files: [new File(['zip'], 'src.zip', { type: 'application/zip' })] },
    });
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.url === '/v1/uploads/archive')).toBe(true),
    );

    await submitModal();
    // file_id 落项目 config（ADR-203 后端兜底档的数据锚点）
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'PUT' && r.url === '/v1/projects/p-new/config')).toBe(true),
    );
    const configPut = gateway.requests.find((r) => r.method === 'PUT' && r.url === '/v1/projects/p-new/config');
    // proto L1158 双层包装: UpdateProjectConfigRequest{config: ProjectConfig{project_id, config}}
    const writtenConfig = (configPut?.body as { config: { config: Record<string, string> } }).config.config;
    expect(writtenConfig.upload_file_id).toBe('file-1');
    // 2026-09-09 用户指令: 原始文件名一并落 config（项目页/详情展示用）
    expect(writtenConfig.upload_file_name).toBe('src.zip');
    // 自动建任务：config 留空——源码来源由 task-service 启动时从项目配置解析
    const taskPost = gateway.requests.find((r) => r.url === '/v1/tasks' && r.method === 'POST');
    expect(taskPost?.body).toMatchObject({
      project_id: 'p-new',
      scan_mode: 'SCAN_MODE_PARALLEL',
      sast_tools: ['opengrep'],
      config: {},
    });
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks/t-new/start')).toBe(true),
    );
  });

  it('交互审计 Top4-③: 项目已建但源码关联失败——如实警告并跳详情，不误报"创建失败"不自动建任务', async () => {
    configFail = true;
    try {
      renderWith();
      await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
      await openModal();
      const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
      fireEvent.change(fileInput, {
        target: { files: [new File(['zip'], 'src.zip', { type: 'application/zip' })] },
      });
      await waitFor(() =>
        expect(gateway.requests.some((r) => r.url === '/v1/uploads/archive')).toBe(true),
      );
      await submitModal();
      // 部分成功如实呈现：警告而非错误，自动建任务被跳过（源码未关联，启动必败）
      expect(await screen.findByText(/项目已创建，但压缩包关联失败/)).toBeTruthy();
      await waitFor(() =>
        expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks')).toBe(false));
    } finally {
      configFail = false;
    }
  });

  // 复审修正（R）：dict 补 SCAN_MODE_UNSPECIFIED 展示键后，新建入口过滤谓词没同步——
  // 可选"未指定"→ MODE_SPECS 缺项 → sast_tools:[] → P-26 同型任务必 FAILED 复活
  it('R: 新建项目模式下拉不含"未指定"（UNSPECIFIED 不进新建入口）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    fireEvent.mouseDown(document.querySelector('.ant-modal .ant-select-selector')!);
    await waitFor(() => expect(document.body.querySelector('.ant-select-item-option')).toBeTruthy());
    const titles = [...document.body.querySelectorAll('.ant-select-item-option')].map((el) => el.getAttribute('title'));
    expect(titles).not.toContain('未指定');
    expect(titles).toContain('模式D AI增强SAST'); // 正常模式不受牵连
  });

  // B5(P3-i)：accept 只约束文件选择器，拖拽可绕过——非压缩包扩展名本地拦截不发请求
  it('B5: 扩展名预检——非 zip/tar.gz 不发起上传请求', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    fireEvent.change(fileInput, { target: { files: [new File(['x'], 'evil.exe', { type: 'application/octet-stream' })] } });
    await screen.findByText(/仅支持 zip\/tar\.gz/);
    expect(gateway.requests.some((r) => r.url === '/v1/uploads/archive')).toBe(false);
  });

  it('100MB 本地预检：超限文件不发起上传请求（2026-09-08 限额批次）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    const big = new File(['zip'], 'big.zip', { type: 'application/zip' });
    Object.defineProperty(big, 'size', { value: 101 * 1024 * 1024 }); // jsdom 免真分配 100MB
    fireEvent.change(fileInput, { target: { files: [big] } });
    // B5 扩展名预检用例先行且共用 antd message 单例——此处 findAll（锁意不变：提示在）
    expect((await screen.findAllByText(/仅支持 zip\/tar\.gz，≤100MB/)).length).toBeGreaterThan(0);
    expect(gateway.requests.some((r) => r.url === '/v1/uploads/archive')).toBe(false);
  });

  // B5-P1-3：模式D（AI_ENHANCED_SAST）需要 SAST 工具——
  // 旧 NEEDS_TOOLS 集合漏了它，自动任务 sast_tools:[] 被 sast-adapter RunMultipleScans
  // 以 "tool_ids is required" 400 拒绝→runModeDEnhanced 判全失败→任务必 FAILED。
  // 修复后与 TaskNewPage MODE_SPECS.needsSastTools 单一事实源对齐。
  it('B5-P1-3: 模式D 自动建任务带 SAST 工具（NEEDS_TOOLS 漏 SCAN_MODE_AI_ENHANCED_SAST 回归锁）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    // 弹窗内唯一下拉=默认扫描模式；antd Select 以 mouseDown 展开选项层（ReportsTasksPaging 同法，
    // 须落在 .ant-select-selector 上——rc-select 监听在该元素，事件不下传）
    fireEvent.mouseDown(document.querySelector('.ant-modal .ant-select-selector')!);
    const opt = await waitFor(() => {
      const el = document.body.querySelector<HTMLElement>('.ant-select-item-option[title="模式D AI增强SAST"]');
      expect(el).toBeTruthy();
      return el!;
    });
    fireEvent.click(opt);
    fireEvent.change(screen.getByPlaceholderText('https://git.example.com/team/repo.git'),
      { target: { value: 'https://git.example.com/team/repo4.git' } });
    await submitModal();
    const taskPost = gateway.requests.find((r) => r.url === '/v1/tasks' && r.method === 'POST');
    expect(taskPost?.body).toMatchObject({
      scan_mode: 'SCAN_MODE_AI_ENHANCED_SAST',
      sast_tools: ['opengrep'],
    });
  });

  it('ADR-203: 仓库通道回归——不传包时 repo_url 可独立建项目（自动 clone）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    const repoInput = screen.getByPlaceholderText('https://git.example.com/team/repo.git');
    fireEvent.change(repoInput, { target: { value: 'https://git.example.com/team/repo2.git' } });
    await submitModal();
    const projPost = gateway.requests.find((r) => r.url === '/v1/projects' && r.method === 'POST');
    // createProject 走 proto L844 包装 {project: payload}
    expect((projPost?.body as { project: { repo_url?: string } }).project?.repo_url).toBe('https://git.example.com/team/repo2.git');
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks/t-new/start')).toBe(true),
    );
  });

  // P1-4：自动链显式化——"创建后立即按默认模式扫描"默认勾选
  // ，取消则只建项目
  it('P1-4: 取消"创建后立即扫描"→ 只建项目，不 POST /v1/tasks', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    // 弹窗内唯一复选框=auto_scan（默认勾选 → 取消）
    fireEvent.click(screen.getByRole('checkbox'));
    fireEvent.change(screen.getByPlaceholderText('https://git.example.com/team/repo.git'),
      { target: { value: 'https://git.example.com/team/repo5.git' } });
    await submitModal();
    // antd message 单例跨用例残留（B5 扩展名用例同款先例）——findAll 容忍多命中
    expect((await screen.findAllByText('项目已创建')).length).toBeGreaterThan(0);
    expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks')).toBe(false);
  });

  it('ADR-203: 上传与仓库都默认——mutation 层拦截，不发创建请求', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    fireEvent.submit(document.querySelector('.ant-modal form')!);
    await waitFor(() => expect(screen.getByText(/请上传代码压缩包或填写仓库地址/)).toBeTruthy());
    expect(gateway.requests.some((r) => r.url === '/v1/projects' && r.method === 'POST')).toBe(false);
  });

  // B4-2（审计修复）：上传→取消→再建项目——取消必须清空上传态。此前 onCancel 只关弹窗，
  // 旧 file_id 残留：再建项目时 config 被写入上一个已取消项目的上传件（新项目源码指向错包）。
  it('B4-2: 上传→取消→再建项目——取消清空上传态，新建链路 payload 无 upload_file_id', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    // 上传 src.zip（file-1）
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    fireEvent.change(fileInput, { target: { files: [new File(['zip'], 'src.zip', { type: 'application/zip' })] } });
    await waitFor(() => expect(gateway.requests.some((r) => r.url === '/v1/uploads/archive')).toBe(true));
    await waitFor(() => expect(screen.getByText('src.zip')).toBeTruthy()); // 上传件在弹窗 fileList 可见

    // 取消弹窗 → 上传态清空。注：测试树无 ConfigProvider zh_CN，antd Modal 底座按钮为
    // 英文 Cancel/OK——按 footer 首按钮定位（antd 约定第一个即取消），不受 locale 影响。
    const cancelBtn = await waitFor(() => {
      const btn = document.querySelector<HTMLButtonElement>('.ant-modal-footer .ant-btn');
      expect(btn).toBeTruthy();
      return btn!;
    });
    fireEvent.click(cancelBtn);

    // 再开弹窗：上传态已清——仓库地址栏 extra 回到"不填则必须上传代码压缩包"
    // （uploadFileId=null 的派生文案；残留时显示"已上传压缩包——此栏可留空…"）。
    // 不直接断言 src.zip 文本消失：Upload 列表项离场动画的幽灵节点在 jsdom 不消散，文本断言不稳。
    fireEvent.click(screen.getByRole('button', { name: '新建项目' })); // 弹窗标题同名——按按钮角色定位
    await waitFor(() => expect(screen.getByText('不填则必须上传代码压缩包')).toBeTruthy());
    const nameInput = (await screen.findByText('名称')).closest('.ant-form-item')!.querySelector('input')!;
    fireEvent.change(nameInput, { target: { value: 'P3' } });
    fireEvent.change(screen.getByPlaceholderText('https://git.example.com/team/repo.git'),
      { target: { value: 'https://git.example.com/team/repo3.git' } });
    fireEvent.submit(document.querySelector('.ant-modal form')!);
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/projects')).toBe(true),
    );
    // 全链路 payload 不得出现 upload_file_id（残留时它会进 PUT config）
    expect(gateway.requests.some((r) => JSON.stringify(r.body ?? {}).includes('upload_file_id'))).toBe(false);
    // 也没有旧包触发的 config 写入（上传态已清，uploadFileId=null 分支不进）
    expect(gateway.requests.some((r) => r.method === 'PUT' && /\/v1\/projects\/.+\/config$/.test(r.url))).toBe(false);
  });
});

// ?new=1 旧深链——流程统一批
// 改为兼容重定向 /projects/new 薄壳（与 /tasks/new 同构的入口范式）
describe('ProjectsPage（?new=1 旧深链兼容重定向）', () => {
  it('带 ?new=1 进入 → 重定向 /projects/new 薄壳（弹窗自动打开）', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <SessionProvider>
          <MemoryRouter initialEntries={['/projects?new=1']}>
            <Routes>
              <Route path="/projects" element={<ProjectsPage />} />
              <Route path="/projects/new" element={<ProjectNewPage />} />
            </Routes>
          </MemoryRouter>
        </SessionProvider>
      </QueryClientProvider>,
    );
    // 薄壳页弹窗自动打开；auto_scan 复选框文案佐证落点在新版 ProjectCreateModal
    await waitFor(() => expect(screen.getByRole('dialog')).toBeTruthy());
    expect(await screen.findByText(/创建后立即按默认模式扫描并启动/)).toBeTruthy();
  });

  it('不带参数 → 弹窗不自动打开', () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <SessionProvider>
          <MemoryRouter initialEntries={['/projects']}>
            <ProjectsPage />
          </MemoryRouter>
        </SessionProvider>
      </QueryClientProvider>,
    );
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});
