// 项目详情组件回归（14号 §3.2 P0 + ADR-181 关联任务）：项目信息/关联任务表/空态。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import ProjectDetailPage from '../pages/ProjectDetailPage';
import { SessionProvider } from '../auth/session';
import { useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行，仅 HTTP 层伪造。
// ADR-181 断言（详情页必须带 project_id 过滤）从 mock 内部断言改为 handler 内守卫——
// 缺过滤直接抛错，查询失败即页面空态，行为级响亮失败。
useFakeGateway({
  'GET /v1/projects/:projectId': () => ({ project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', created_at: '2026-09-02T00:00:00Z' }),
  'GET /v1/projects/:projectId/config': () => ({ project_id: 'p1', config: {} }),
  'GET /v1/tasks': (ctx: HandlerCtx) => {
    if (ctx.query.get('project_id') !== 'p1') {
      throw new Error('ADR-181: 关联任务查询必须携带 project_id 过滤');
    }
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
});
