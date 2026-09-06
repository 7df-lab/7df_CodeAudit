// T2 完成标准：04 §3 四模式向导分支覆盖
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import TaskNewPage, { MODE_SPECS } from '../pages/tasks/TaskNewPage';

vi.mock('../api/client', () => ({
  default: {
    get: vi.fn(async (url: string) => {
      if (url === '/v1/projects') {
        return { data: { projects: [{ project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', default_scan_mode: '', created_at: null }] } };
      }
      if (url === '/v1/tools') {
        return { data: { tools: [
          { tool_id: 'bandit', name: 'bandit', supported_languages: ['python'], output_format: 'bandit', valid: true, errors: [] },
          { tool_id: 'codeql', name: 'codeql (parser only; no executor mapping)', supported_languages: [], output_format: 'json', valid: false, errors: ['no executor mapping'] },
        ] } };
      }
      throw new Error('unexpected ' + url);
    }),
    post: vi.fn(),
  },
}));

function renderWizard() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks/new']}>
        <TaskNewPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('MODE_SPECS（04 §3 分支单一来源）', () => {
  it('模式A 不需要工具；B/C/D 需要；仅 D 需要审核配置', () => {
    expect(MODE_SPECS.SCAN_MODE_AI_ONLY).toMatchObject({ needsSastTools: false, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_TRADITIONAL_FIRST).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_PARALLEL).toMatchObject({ needsSastTools: true, needsReviewConfig: false });
    expect(MODE_SPECS.SCAN_MODE_SAST_REVIEW).toMatchObject({ needsSastTools: true, needsReviewConfig: true });
  });
  it('四模式齐备（04 §3 无第五模式）', () => {
    expect(Object.keys(MODE_SPECS)).toHaveLength(4);
  });
});

describe('TaskNewPage 向导', () => {
  it('渲染向导骨架（项目选择器+四步步骤条）', async () => {
    renderWizard();
    await waitFor(() => expect(screen.getByText('选择项目')).toBeTruthy());
    // 步骤条四步
    for (const t of ['项目', '模式', '参数', '确认']) {
      expect(screen.getAllByText(t).length).toBeGreaterThan(0);
    }
    // 下一步按钮初始禁用（未选项目）
    const next = screen.getByRole('button', { name: '下一步' });
    expect(next).toHaveProperty('disabled', true);
  });

  // ADR-154 回归（行为级锁）：确认页只随第4步出现；创建按钮不在向导前段渲染
  // （完整 step2→state→确认页 链路由 Playwright GUI 回归覆盖，见
  //   evidence/gui-audit/retest_fixed.py）
  it('ADR-154: 确认页按钮不前漏（步骤边界正确）', async () => {
    renderWizard();
    await waitFor(() => expect(screen.getByText('选择项目')).toBeTruthy());
    expect(screen.queryByRole('button', { name: '创建任务' })).toBeNull();
  });
});
