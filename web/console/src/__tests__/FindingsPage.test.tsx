// T3 回归：发现列表渲染 + 快捷 triage 调用体（PUT verdict）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import FindingsPage from '../pages/findings/FindingsPage';

const { putCalls, putMock, getMock } = vi.hoisted(() => {
  const putCalls: [string, Record<string, string>][] = [];
  const putMock = vi.fn(async (url: string, body?: Record<string, string>) => {
    putCalls.push([url, body ?? {}]);
    return { data: {} };
  });
  const getMock = vi.fn(async (url: string) => {
    if (url === '/v1/findings') {
      return { data: { findings: [{
        finding_id: 'f-1', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
        cwe_id: 'CWE-798', title: 'hardcoded password', severity: 'SEVERITY_HIGH',
        confidence: 0.9, ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
        location: { file_path: 'app.py', start_line: 12 },
      }, {
        // 沙箱 DSH 发现（ADR-167 补遗）：AI 结论须在当前结论列可见并标明 AI 输出
        finding_id: 'f-2', task_id: 't1', source_tool: 'ai_agent', source_rule_id: 'dsh-headless',
        cwe_id: 'CWE-89', title: 'SQL 注入：user_id 直接拼接', severity: 'SEVERITY_CRITICAL',
        confidence: 0.98, ai_verdict: 'AI_VERDICT_LIKELY_TRUE', ai_confidence: 0.98,
        ai_reasoning: '[DSH-sandbox] get_user() 将参数 user_id 未做任何参数化或校验，直接用字符串拼接构造 SQL 后交给 cursor.execute() 执行。',
        location: { file_path: 'app.py', start_line: 16 },
      }], pagination: { next_cursor: '', has_next: false, total: 2 } } };
    }
    throw new Error('unexpected ' + url);
  });
  return { putCalls, putMock, getMock };
});
vi.mock('../api/client', () => ({
  api: { // 页面使用命名导入 { api }
    get: getMock,
    put: putMock,
  },
}));

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks/t1/findings']}>
        <FindingsPage taskId="t1" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('FindingsPage（T3 triage 闭环 UI 侧）', () => {
  it('渲染发现行（severity/CWE 中文映射）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('hardcoded password')).toBeTruthy());
    expect(screen.getByText('高危')).toBeTruthy();
    expect(screen.getByText('需人工复核')).toBeTruthy();
    expect(screen.getByText('app.py:12')).toBeTruthy();
  });
  it('沙箱发现的 AI 结论在当前结论列可见并标明 AI 输出（ADR-167 补遗）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('SQL 注入：user_id 直接拼接')).toBeTruthy());
    expect(screen.getByText('AI 输出')).toBeTruthy(); // AI 来源徽标
    expect(screen.getAllByText(/可能为真|很可能为真/).length).toBeGreaterThan(0); // LIKELY_TRUE 中文映射
    expect(screen.getByText(/\[DSH-sandbox\] get_user/)).toBeTruthy(); // 结论预览（前缀即来源标注）
  });
  it('快捷确认 → PUT verdict 携带 proto 枚举值与 reasoning', async () => {
    renderPage();
    const confirmBtn = await waitFor(() => screen.getAllByRole('button', { name: /确\s*认/ })[0]); // antd 两字按钮插空格；多行取首行
    fireEvent.click(confirmBtn);
    await waitFor(() => expect(putMock).toHaveBeenCalled());
    expect(putCalls[0][0]).toBe('/v1/findings/f-1/verdict');
    expect(putCalls[0][1]).toEqual({ verdict: 'AI_VERDICT_TRUE_POSITIVE', reasoning: 'console quick triage' });
  });
});

// ADR-150: 行展开图标存在（内嵌审核工作台入口）
describe('FindingsPage 展开交互', () => {
  it('每行有展开图标（行展开=审核工作台）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText('hardcoded password')).toBeTruthy());
    // ADR-151: 展开控件=明确"风险详情"按钮（不再是小箭头）
    expect(screen.getAllByRole('button', { name: /风险详情/ })[0]).toBeTruthy();
  });
});

// ADR-159 回归：真解析 source_raw 的 dataflow_trace → "污点链路"徽标（非按工具名猜测）
it('携带 dataflow_trace 的行显示污点链路徽标', async () => {
  const trace = { tool: 'opengrep', taint: true,
    dataflow_trace: { taint_source: ['CliLoc', [{ path: 'app.py', start: { line: 4 }, end: { line: 4 } }], 'src'],
      intermediate_vars: [{ location: { path: 'app.py', start: { line: 4 } }, content: 'q' }],
      taint_sink: ['CliLoc', [{ path: 'app.py', start: { line: 7 }, end: { line: 7 } }, 'sink']] } };
  getMock.mockImplementationOnce(async (url: string) => ({ data: { findings: [{
    finding_id: 'f-og', task_id: 't1', source_tool: 'opengrep',
    source_rule_id: 'auditmind-sql-taint-user-param',
    cwe_id: 'CWE-89', title: 'SQL 注入', severity: 'SEVERITY_HIGH',
    confidence: 0.9, ai_verdict: 'AI_VERDICT_UNSPECIFIED', ai_confidence: 0,
    location: { file_path: 'app.py', start_line: 7 },
    source_raw: btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(trace)))),
  }], pagination: { next_cursor: '', has_next: false, total: 1 } } }));
  renderPage();
  await waitFor(() => expect(screen.getByText('污点链路')).toBeTruthy());
});
