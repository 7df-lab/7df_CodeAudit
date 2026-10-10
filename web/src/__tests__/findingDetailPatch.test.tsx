// R97 报障批：修复补丁 web 消费入口——发现详情此前全无 diff_patch 渲染
// （服务端 NormalizeDiffPatch 校验后的补丁只有 vscode-plugin 消费，GUI 用户不可见）。
// 锁定：有补丁→补丁卡+apply_patch 原文+复制按钮；无补丁→不渲染卡片（诚实留白）。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { FindingDetailBody } from '../pages/findings/FindingDetailBody';
import { useFakeGateway } from '../testsupport/fakeGateway';

const PATCH = `*** Begin Patch
*** Update File: app.py
@@     cur = conn.cursor()
-    cur.execute("SELECT * FROM users WHERE id = '%s'" % uid)
+    cur.execute("SELECT * FROM users WHERE id = ?", (uid,))
*** End Patch`;

function finding(overrides: Record<string, unknown>) {
  return {
    finding: {
      finding_id: 'gw-x-sbx-1',
      task_id: 'gw-x',
      project_id: 'p1',
      source_tool: 'ai_agent',
      source_rule_id: 'dsh-headless',
      cwe_id: 'CWE-89',
      title: 'SQL 注入',
      description: 'd',
      severity: 'SEVERITY_CRITICAL',
      confidence: 0.9,
      ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
      ai_confidence: 0.9,
      ai_reasoning: '[DSH-sandbox] r',
      ai_fix_suggestion: '参数化查询',
      dedup_group: '',
      matched_findings: [],
      is_unique: true,
      location: { file_path: 'app.py', start_line: 5 },
      ...overrides,
    },
  };
}

function mount(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('发现详情·修复补丁卡（R97 报障批）', () => {
  // useFakeGateway 须在 describe 层注册（beforeEach 挂 adapter）；用可变 handler 切换响应体
  let respBody: unknown;
  useFakeGateway({ 'GET /v1/findings/:findingId': () => respBody });

  it('有补丁 → 补丁卡渲染 apply_patch 原文与复制按钮', async () => {
    respBody = finding({ diff_patch: PATCH });
    mount(<FindingDetailBody findingId="gw-x-sbx-1" />);
    await waitFor(() => expect(screen.getByText('修复补丁（apply_patch）')).toBeTruthy());
    expect(screen.getByText(/Begin Patch/)).toBeTruthy();
    expect(screen.getByText(/cur\.execute\("SELECT \* FROM users WHERE id = \?"/)).toBeTruthy();
    expect(screen.getByRole('button', { name: /复\s*制/ })).toBeTruthy();
  });

  it('无补丁（空串=未产出/失配丢弃）→ 不渲染补丁卡（诚实留白，不编造占位）', async () => {
    respBody = finding({ diff_patch: '' });
    mount(<FindingDetailBody findingId="gw-x-sbx-1" />);
    await waitFor(() => expect(screen.getByText('SQL 注入')).toBeTruthy());
    expect(screen.queryByText('修复补丁（apply_patch）')).toBeNull();
  });
});
