// 项目列表组件回归（14号 P0 页面）：渲染 + 创建流行为（ADR-203 fakeGateway 测试台）
// ADR-203（人类 2026-09-05 裁决，推翻方案b移除）：弹窗上传入口保留并改造为零落盘直传——
// uploadArchive(ADR-200 file_id 契约) → 项目 config.upload_file_id → 任务启动时后端拉包。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import ProjectsPage from '../pages/ProjectsPage';
import { SessionProvider } from '../auth/session';
import { useFakeGateway } from '../testsupport/fakeGateway';

const gateway = useFakeGateway({
  'GET /v1/projects': () => ({
    // ADR-164: 列表走服务端游标翻页——响应含 pagination.total；p-up 为上传型项目（无 repo_url）
    projects: [
      { project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null },
      { project_id: 'p-up', name: '上传型', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null },
    ],
    pagination: { next_cursor: '', has_next: false, total: 2 },
  }),
  'GET /v1/projects/p-up/config': () => ({ project_id: 'p-up', config: { upload_file_id: 'file-9', upload_file_name: 'code9.zip' } }),
  'POST /v1/uploads/archive': () => ({ upload_id: 'up-1', file_id: 'file-1', file_path: 'uploads/up-1/src.zip', size_bytes: 3 }),
  'POST /v1/projects': () => ({ project_id: 'p-new' }),
  'PUT /v1/projects/:projectId/config': () => ({ project_id: 'p-new', config: {} }),
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

  it('上传型项目源码列显示压缩包原始文件名（2026-09-09 用户指令；存量无键回落"上传压缩包"）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('上传型')).toBeTruthy());
    // config.upload_file_name 在位 → 显示文件名（悬浮见 file_id）
    await waitFor(() => expect(screen.getByText('code9.zip')).toBeTruthy());
    expect(screen.getByText('code9.zip').closest('span')?.getAttribute('title')).toBe('file-9');
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

  it('100MB 本地预检：超限文件不发起上传请求（2026-09-08 限额批次）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    await openModal();
    const fileInput = document.querySelector<HTMLInputElement>('input[type="file"]')!;
    const big = new File(['zip'], 'big.zip', { type: 'application/zip' });
    Object.defineProperty(big, 'size', { value: 101 * 1024 * 1024 }); // jsdom 免真分配 100MB
    fireEvent.change(fileInput, { target: { files: [big] } });
    await screen.findByText(/仅支持 zip\/tar\.gz，≤100MB/);
    expect(gateway.requests.some((r) => r.url === '/v1/uploads/archive')).toBe(false);
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

  it('ADR-203: 上传与仓库都缺省——mutation 层拦截，不发创建请求', async () => {
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
