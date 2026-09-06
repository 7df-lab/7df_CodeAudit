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
    // ADR-164: 列表走服务端游标翻页——响应含 pagination.total
    projects: [{ project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null }],
    pagination: { next_cursor: '', has_next: false, total: 1 },
  }),
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
    expect(screen.getByText('模式B 纯AI')).toBeTruthy(); // ADR-182 重排后 AI_ONLY=模式B
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
    expect((configPut?.body as { config: { config: Record<string, string> } }).config.config.upload_file_id).toBe('file-1');
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
});
