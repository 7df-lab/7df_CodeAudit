// 项目列表组件回归（14号 P0 页面）：空态与数据渲染
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import ProjectsPage from '../pages/ProjectsPage';
import { SessionProvider } from '../auth/session';

vi.mock('../api/client', async (importOriginal) => {
  const real = await importOriginal<typeof import('../api/client')>();
  return { ...real, readRefreshToken: () => null, api: { get: vi.fn(async (url: string) => {
    if (url === '/v1/projects') {
      // ADR-164: 列表走服务端游标翻页——响应含 pagination.total
      return { data: { projects: [{ project_id: 'p1', name: 'Demo', repo_url: 'https://x', default_branch: 'main', default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: null }], pagination: { next_cursor: '', has_next: false, total: 1 } } };
    }
    throw new Error('unexpected ' + url);
  }) } };
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

describe('ProjectsPage', () => {
  it('渲染项目行并映射默认模式中文（P4 展示即数据）', async () => {
    renderWith();
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    expect(screen.getByText('模式A 纯AI')).toBeTruthy();
  });
});
