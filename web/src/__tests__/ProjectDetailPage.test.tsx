// 项目详情组件回归（14号 §3.2 P0 + ADR-181 关联任务）：项目信息/关联任务表/空态。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import ProjectDetailPage from '../pages/ProjectDetailPage';
import { SessionProvider } from '../auth/session';
import { useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行，仅 HTTP 层伪造。
// ADR-181 断言（详情页必须带 project_id 过滤）从 mock 内部断言改为 handler 内守卫——
// 缺过滤直接抛错，查询失败即页面空态，行为级响亮失败。
// 建任务引导（2026-09-11 报障）：tasksOverride 供空态用例注入空任务列表（用后还原）。
let tasksOverride: unknown = null;
const gateway = useFakeGateway({
  'GET /v1/projects/:projectId': () => ({ project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', created_at: '2026-09-02T00:00:00Z' }),
  'GET /v1/projects/:projectId/config': () => ({ project_id: 'p1', config: { upload_file_id: 'file-1', upload_file_name: 'src.zip' } }),
  'DELETE /v1/projects/:projectId': {},
  'POST /v1/tasks': () => ({ task_id: 't-quick' }),
  'POST /v1/tasks/:taskId/start': () => ({}),  'GET /v1/tasks': (ctx: HandlerCtx) => {
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

  it('源码来源显示压缩包原始文件名（2026-09-09 用户指令；无名回落 file_id）', async () => {
    renderWith();
    expect(await screen.findByText('上传压缩包（src.zip）')).toBeTruthy();
  });

  it('创建扫描任务快捷入口：POST /v1/tasks config 恒空（源码由项目解析）+ 自动启动', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('关联任务（2）')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '创建扫描任务' }));
    // 弹窗默认 模式C PARALLEL + 立即启动（测试态无 zh_CN ConfigProvider，OK 按钮按 footer 主按钮取）
    const ok = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-modal-footer .ant-btn-primary'),
    );
    expect(ok).toBeTruthy();
    fireEvent.click(ok!);
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks')).toBe(true),
    );
    const post = gateway.requests.find((r) => r.method === 'POST' && r.url === '/v1/tasks');
    expect(post?.body).toMatchObject({
      project_id: 'p1',
      scan_mode: 'SCAN_MODE_PARALLEL',
      sast_tools: ['opengrep'], // 工具模式默认口径（与项目页自动任务一致）
      config: {}, // 2026-09-09: 源码键不落任务级，启动时 task-service 从项目解析
    });
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks/t-quick/start')).toBe(true),
    );
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
});
