// ADR-141 回归：复核页面必须呈现代码上下文（人工复核的最低内容条件）
// ADR-195：代码上下文升级为源码全文滚动视图（链路点选定位 + 端点失败降级回 ±10 片段）
// ADR-203：getSourceFile 走真实实现（client.ts），仅其 HTTP 端点被 fakeGateway 伪造——
// 降级语义（错误详情提取）与请求形状（path query）都在真实代码路径上受测。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { beforeEach, describe, expect, it } from 'vitest';
import type { SourceFileResp } from '../api/client';
import FindingDetailPage from '../pages/findings/FindingDetailPage';
import { httpError, useFakeGateway, type HandlerCtx, type RouteHandler } from '../testsupport/fakeGateway';

// bandit 原始输出（含 code 块）→ protojson bytes → base64
const RAW = JSON.stringify({
  code: 'SECRET = "hunter2"\nTOKEN = "abc123"\n',
  filename: 'app.py', line_number: 12, test_id: 'B105',
});
const SOURCE_RAW = btoa(RAW);

// 每用例重置（beforeEach），个别用例覆写 finding 载荷 / 全文端点行为
let detailPayload: unknown;
let sourceFileHandler: RouteHandler | undefined;
const gateway = useFakeGateway({
  'GET /v1/findings/:findingId': () => ({ finding: detailPayload }),
  'GET /v1/tasks/:taskId/source-file': (ctx: HandlerCtx) => {
    if (sourceFileHandler) return sourceFileHandler(ctx);
    // ADR-195: 默认失败 → 降级片段
    httpError(500, { error: 'source root unavailable' });
  },
});

beforeEach(() => {
  detailPayload = {
    finding_id: 'f-1', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
    cwe_id: 'CWE-259', title: 'hardcoded password', description: 'desc',
    severity: 'SEVERITY_MEDIUM', confidence: 0.8,
    ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
    ai_reasoning: '', ai_fix_suggestion: '',
    location: { file_path: 'app.py', start_line: 12 },
    source_raw: SOURCE_RAW,
  };
  sourceFileHandler = undefined;
});

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

describe('人工复核内容条件（ADR-141；全文端点失败降级回片段）', () => {
  it('降级片段：解码 source_raw、按起始行号标注、含代码行', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/代码上下文/)).toBeTruthy());
    // 全文端点失败是异步事件——片段渲染等一拍（ADR-195）
    await waitFor(() => expect(screen.getByText(/源码全文不可用/)).toBeTruthy());
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
    detailPayload = {
      finding_id: 'f-3', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
      cwe_id: '', title: 't2', description: '', severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: { file_path: '/repo/app.py', start_line: 12 },
      source_raw: btoa(full),
    };
    renderPage();
    await waitFor(() => expect(screen.getByText(/工具输出中的代码行/)).toBeTruthy());
    await waitFor(() => expect(screen.getByText(/SECRET = "hunter2"/)).toBeTruthy());
  });
  it('无 location 时如实声明"未携带位置信息"而非留白', async () => {
    detailPayload = {
      finding_id: 'f-2', task_id: 't1', source_tool: 'ai_agent', source_rule_id: '',
      cwe_id: '', title: 't', description: '', severity: 'SEVERITY_LOW', confidence: 0,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: null,
      source_raw: '',
    };
    renderPage();
    await waitFor(() => expect(screen.getByText(/该发现未携带位置信息/)).toBeTruthy());
  });
  // 2026-08-30 会话#41 回归：atob 的 Latin-1 语义会把 UTF-8 中文拆成乱码
  // （"注入"→"æ³¨å…¥"）——source_raw 必须经字节层 UTF-8 解码。
  it('源码含中文（UTF-8 多字节）时按原文呈现而非乱码', async () => {
    const zh = JSON.stringify({
      code: '# CWE-89 SQL 注入: f-string 拼接\nquery = "SELECT * FROM users WHERE id = " + user_id\n',
      filename: 'app.py', line_number: 15, test_id: 'B608',
    });
    detailPayload = {
      finding_id: 'f-4', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B608',
      cwe_id: 'CWE-89', title: 'SQL 注入', description: '拼接 SQL',
      severity: 'SEVERITY_HIGH', confidence: 0.8,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: { file_path: 'app.py', start_line: 15 },
      // btoa 只收 Latin-1 → 先经 UTF-8 字节层编码（与后端 Go 存储行为对齐）
      source_raw: btoa(String.fromCharCode(...new TextEncoder().encode(zh))),
    };
    renderPage();
    await waitFor(() => expect(screen.getByText(/CWE-89 SQL 注入: f-string 拼接/)).toBeTruthy());
    expect(screen.queryByText(/æ³¨å…¥/)).toBeNull();
  });
});

describe('源码全文滚动视图（ADR-195）', () => {
  const FULL = Array.from({ length: 60 }, (_, i) => `line-${i + 1}`).join('\n');

  it('端点成功：全文渲染 + 定位说明 + 漏洞行数据锚点；无链路时选择器禁用仅漏洞文件', async () => {
    sourceFileHandler = (): SourceFileResp => ({
      path: 'app.py', content: FULL, total_lines: 60, bytes: 600, root_via: 'upload_link', resolved_via: 'exact',
    });
    renderPage();
    await waitFor(() => expect(screen.getByText('源码全文')).toBeTruthy());
    const viewer = screen.getByTestId('source-viewer');
    expect(viewer.querySelector('[data-line="12"]')?.textContent).toContain('line-12');
    expect(viewer.querySelector('[data-line="60"]')?.textContent).toContain('line-60'); // 全文非片段
    expect(screen.getByText(/已居中定位到第 12 行（漏洞位置）/)).toBeTruthy();
    // 无 AI 结论 → 无链路 chips、选择器禁用、提示"仅漏洞所在文件可选"
    expect(screen.queryAllByTestId(/chain-hop-/).length).toBe(0);
    expect(screen.getByText(/仅漏洞所在文件可选/)).toBeTruthy();
  });

  it('AI 结论链路：chips 按原文顺序渲染，点击切换文件并带上定位行', async () => {
    detailPayload = {
      finding_id: 'f-9', task_id: 't1', source_tool: 'ai_agent', source_rule_id: '',
      cwe_id: 'CWE-306', title: 'HTTP API 无认证', description: '', severity: 'SEVERITY_HIGH', confidence: 0.9,
      ai_verdict: 'AI_VERDICT_LIKELY_TRUE', ai_confidence: 0.9, ai_fix_suggestion: '',
      ai_reasoning: '[DSH-sandbox] Listener.java:49 authFilter 默认 null；而 Api.java 暴露的端点：publish（153-167）。',
      location: { file_path: 'src/main/Listener.java', start_line: 49 },
      source_raw: '',
    };
    sourceFileHandler = (ctx): SourceFileResp => ({
      path: String(ctx.query.get('path') ?? ''), content: 'a\nb\nc', total_lines: 3, bytes: 6, root_via: 'upload_link', resolved_via: 'basename',
    });
    renderPage();
    await waitFor(() => expect(screen.getByTestId('chain-hop-0')).toBeTruthy());
    expect(screen.getByTestId('chain-hop-1').textContent).toContain('Api.java:153-167');
    expect(screen.getByTestId('chain-hop-1').textContent).toContain('汇'); // 端点关键词标注
    // 点击第 2 跳 → 以该文件请求全文（服务端回退解析裸文件名）
    fireEvent.click(screen.getByTestId('chain-hop-1'));
    // 请求形状断言：path query 携带裸文件名（服务端回退解析的入参）
    await waitFor(() => {
      const srcReq = gateway.requests.filter((r) => r.url === '/v1/tasks/t1/source-file').pop();
      expect(srcReq?.query).toContain('path=Api.java');
    });
    expect(screen.getByText(/已居中定位到第 153 行（链路引用）/)).toBeTruthy();
  });
});

// ADR-158 回归：OpenGrep dataflow_trace 经 source_raw 透传 → 变量级污点链路渲染
it('taint 发现携带 dataflow_trace 时渲染 SOURCE/传播/SINK 变量级链路', async () => {
  const trace = {
    tool: 'opengrep', code: 'cursor.execute(query)', line: 6, file: 'app.py',
    rule: 'codeaudit-sql-taint-user-param', taint: true,
    dataflow_trace: {
      taint_source: ['CliLoc', [{ path: 'app.py', start: { line: 4, col: 5 }, end: { line: 4, col: 56 } }, 'query = "SELECT * FROM users WHERE id = " + user_id']],
      intermediate_vars: [{ location: { path: 'app.py', start: { line: 4, col: 5 }, end: { line: 4, col: 10 } }, content: 'query' }],
      taint_sink: ['CliLoc', [{ path: 'app.py', start: { line: 6, col: 5 }, end: { line: 6, col: 26 } }, 'cursor.execute(query)']],
    },
  };
  detailPayload = {
    finding_id: 'f-5', task_id: 't1', source_tool: 'opengrep',
    source_rule_id: 'codeaudit-sql-taint-user-param',
    cwe_id: 'CWE-89', title: 'SQL 注入', description: '', severity: 'SEVERITY_ERROR', confidence: 0.9,
    ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
    location: { file_path: 'app.py', start_line: 6 },
    source_raw: btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(trace)))),
  };
  renderPage();
  await waitFor(() => expect(screen.getByText(/污点传播链路/)).toBeTruthy());
  expect(screen.getByText(/污点来源 SOURCE/)).toBeTruthy();
  expect(screen.getByText(/变量 query/)).toBeTruthy();
  expect(screen.getByText(/汇点 SINK/)).toBeTruthy();
});
