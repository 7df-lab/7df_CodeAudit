// ADR-141 回归：复核页面必须呈现代码上下文（人工复核的最低内容条件）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import FindingDetailPage from '../pages/findings/FindingDetailPage';

// bandit 原始输出（含 code 块）→ protojson bytes → base64
const RAW = JSON.stringify({
  code: 'SECRET = "hunter2"\nTOKEN = "abc123"\n',
  filename: 'app.py', line_number: 12, test_id: 'B105',
});
const SOURCE_RAW = btoa(RAW);

const getMock = vi.fn(async (url: string) => {
  if (url.startsWith('/v1/findings/')) {
    return { data: { finding: {
      finding_id: 'f-1', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
      cwe_id: 'CWE-259', title: 'hardcoded password', description: 'desc',
      severity: 'SEVERITY_MEDIUM', confidence: 0.8,
      ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
      ai_reasoning: '', ai_fix_suggestion: '',
      location: { file_path: 'app.py', start_line: 12 },
      source_raw: SOURCE_RAW,
    } } };
  }
  throw new Error('unexpected ' + url);
});
vi.mock('../api/client', () => ({ api: { get: (...a: unknown[]) => getMock(...(a as [string])) } }));

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/findings/f-1']}>
        <FindingDetailPage findingId="f-1" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('人工复核内容条件（ADR-141）', () => {
  it('代码上下文区块：解码 source_raw、按起始行号标注、含代码行', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/代码上下文/)).toBeTruthy());
    expect(screen.getByText('匹配行')).toBeTruthy();
    expect(screen.getByText('SECRET = "hunter2"')).toBeTruthy();
    expect(screen.getByText('TOKEN = "abc123"')).toBeTruthy();
    // 起始行号=12（location.start_line 起算）
    expect(screen.getByText('12')).toBeTruthy();
    expect(screen.getByText('13')).toBeTruthy();
  });
  it('整份输出形态（bandit results[].code）按 file+line 匹配取代码', async () => {
    const full = JSON.stringify({ errors: [], results: [
      { filename: 'other.py', line_number: 1, code: 'x = 1' },
      { filename: 'app.py', line_number: 12, code: 'SECRET = "hunter2"' },
    ] });
    getMock.mockImplementationOnce(async (): Promise<{ data: { finding: any } }> => ({ data: { finding: {
      finding_id: 'f-3', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
      cwe_id: '', title: 't2', description: '', severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: { file_path: '/repo/app.py', start_line: 12 },
      source_raw: btoa(full),
    } } }));
    renderPage();
    await waitFor(() => expect(screen.getByText('工具输出中的代码行')).toBeTruthy());
    expect(screen.getByText(/SECRET = "hunter2"/)).toBeTruthy();
  });
  it('无 source_raw 时如实声明"未携带代码片段"而非留白', async () => {
    getMock.mockImplementationOnce(async (): Promise<{ data: { finding: any } }> => ({ data: { finding: {
      finding_id: 'f-2', task_id: 't1', source_tool: 'ai_agent', source_rule_id: '',
      cwe_id: '', title: 't', description: '', severity: 'SEVERITY_LOW', confidence: 0,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: null,
      source_raw: '',
    } } }));
    renderPage();
    await waitFor(() => expect(screen.getByText(/该发现未携带代码片段/)).toBeTruthy());
  });
  // 2026-08-30 会话#41 回归：atob 的 Latin-1 语义会把 UTF-8 中文拆成乱码
  // （"注入"→"æ³¨å…¥"）——source_raw 必须经字节层 UTF-8 解码。
  it('源码含中文（UTF-8 多字节）时按原文呈现而非乱码', async () => {
    const zh = JSON.stringify({
      code: '# CWE-89 SQL 注入: f-string 拼接\nquery = "SELECT * FROM users WHERE id = " + user_id\n',
      filename: 'app.py', line_number: 15, test_id: 'B608',
    });
    getMock.mockImplementationOnce(async (): Promise<{ data: { finding: any } }> => ({ data: { finding: {
      finding_id: 'f-4', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B608',
      cwe_id: 'CWE-89', title: 'SQL 注入', description: '拼接 SQL',
      severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: { file_path: 'app.py', start_line: 15 },
      // btoa 只收 Latin-1 → 先经 UTF-8 字节层编码（与后端 Go 存储行为对齐）
      source_raw: btoa(String.fromCharCode(...new TextEncoder().encode(zh))),
    } } }));
    renderPage();
    await waitFor(() => expect(screen.getByText(/CWE-89 SQL 注入: f-string 拼接/)).toBeTruthy());
    expect(screen.queryByText(/æ³¨å…¥/)).toBeNull();
  });
});

// ADR-158 回归：OpenGrep dataflow_trace 经 source_raw 透传 → 变量级污点链路渲染
it('taint 发现携带 dataflow_trace 时渲染 SOURCE/传播/SINK 变量级链路', async () => {
  const trace = {
    tool: 'opengrep', code: 'cursor.execute(query)', line: 6, file: 'app.py',
    rule: 'auditmind-sql-taint-user-param', taint: true,
    dataflow_trace: {
      taint_source: ['CliLoc', [{ path: 'app.py', start: { line: 4, col: 5 }, end: { line: 4, col: 56 } }, 'query = "SELECT * FROM users WHERE id = " + user_id']],
      intermediate_vars: [{ location: { path: 'app.py', start: { line: 4, col: 5 }, end: { line: 4, col: 10 } }, content: 'query' }],
      taint_sink: ['CliLoc', [{ path: 'app.py', start: { line: 6, col: 5 }, end: { line: 6, col: 26 } }, 'cursor.execute(query)']],
    },
  };
  getMock.mockImplementationOnce(async (): Promise<{ data: { finding: any } }> => ({ data: { finding: {
    finding_id: 'f-5', task_id: 't1', source_tool: 'opengrep',
    source_rule_id: 'auditmind-sql-taint-user-param',
    cwe_id: 'CWE-89', title: 'SQL 注入', description: '', severity: 'SEVERITY_ERROR', confidence: 0.9,
    ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
    location: { file_path: 'app.py', start_line: 6 },
    source_raw: btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(trace)))),
  } } }));
  renderPage();
  await waitFor(() => expect(screen.getByText(/污点传播链路/)).toBeTruthy());
  expect(screen.getByText(/污点来源 SOURCE/)).toBeTruthy();
  expect(screen.getByText(/变量 query/)).toBeTruthy();
  expect(screen.getByText(/汇点 SINK/)).toBeTruthy();
});
