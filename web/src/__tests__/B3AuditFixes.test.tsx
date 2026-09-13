// 审计批次 B3 修复回归锁：
//   报告窗口 CSP 纵深防御——升级为 sandboxed iframe 通道：
//     报告内容经 <iframe sandbox src=blob:> 渲染（sandbox 空 token 脚本全灭），
//     CSP meta 仍前置于 blob 内容（双保险）；JSON 分支保持转义 <pre>
//   报告"重新生成"接线（D4 裁定）——点击即 POST report RPC，成功后列表失效重拉
//   mutation onError 补齐——项目删除/通知已读失败给出带状态码的即时反馈
// （无限查询分页用例在 FindingsPage/UsersPage 各自测试文件；截断提示在 views.test）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import TaskDetailPage from '../pages/tasks/TaskDetailPage';
import ReportsPage from '../pages/reports/ReportsPage';
import ProjectDetailPage from '../pages/ProjectDetailPage';
import NotificationsPage from '../pages/notifications/NotificationsPage';
import { Blob as NodeBlob } from 'node:buffer';
import { httpError, useFakeGateway, type HandlerCtx } from '../testsupport/fakeGateway';
import { openReportWindow, REPORT_WINDOW_CSP_META, withReportCspMeta } from '../api/client';

// useSession 整文件级 mock（NotificationsPage 需要 user；ProjectDetailPage 不消费 session）
vi.mock('../auth/session', () => ({
  useSession: () => ({
    user: { user_id: 'u-1', username: 'admin', email: '', role: 'ROLE_ADMIN', must_change_password: false },
  }),
}));

const routes: Record<string, unknown> = {
  'GET /v1/tasks/:taskId/snapshot': () => ({
    task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_SAST_ONLY', sast_tools: [],
      status: 'TASK_STATUS_COMPLETED', stages: [], retry_count: 0 },
    logs: { logs: [] },
    ai: { chunk: '', next_cursor: '0', complete: true, total_bytes: '0' },
  }),
  // 报告中心列表（带 pagination）与任务详情内联摘要查询（不带）共用此路由
  'GET /v1/reports': (ctx: HandlerCtx) => {
    if (ctx.query.has('pagination')) {
      return { reports: [{ report_id: 'r-1', task_id: 't-1', format: 'REPORT_FORMAT_HTML', url: '', generated_at: '2026-09-01T00:00:00Z' }],
        pagination: { next_cursor: '', has_next: false } };
    }
    return { reports: [{ report_id: 'r-1', task_id: 't-1', format: 'REPORT_FORMAT_HTML', url: '', generated_at: null }] };
  },
  // 缺省=HTML blob（报告中心在线查看）；CSP 用例按需覆写
  // NodeBlob：jsdom 的 Blob 未实现 .text()（浏览器无此问题），handler 用 Node 实现供视图消费
  'GET /v1/reports/:reportId/download': () =>
    new NodeBlob(['<html><head></head><body><p>blob-html</p></body></html>'], { type: 'text/html' }),
  'GET /v1/findings': () => ({ findings: [], pagination: { next_cursor: '', has_next: false, total: 0 } }),
  'GET /v1/projects': () => ({ projects: [], pagination: { next_cursor: '', has_next: false, total: 0 } }),
  'GET /v1/projects/:projectId': () => ({ project_id: 'p1', name: 'Demo', repo_url: '', default_branch: 'main', created_at: null }),
  'GET /v1/projects/:projectId/config': () => ({ project_id: 'p1', config: {} }),
  'GET /v1/tasks': () => ({ tasks: [], pagination: { next_cursor: '', has_next: false, total: 0 } }),
  'POST /v1/tasks/:taskId/report': () => ({ result: { report_id: 'r-new' } }),
  'DELETE /v1/projects/:projectId': {},
  'GET /v1/notifications': () => ({
    notifications: [{ notification_id: 'n-1', user_id: 'u-1', title: '通知一', body: 'b', read: false, created_at: null }],
  }),
  'POST /v1/notifications/:notificationId/read': {},
  'POST /v1/notifications/read-all': {},
};
const gateway = useFakeGateway(routes);

const defaultDownloadRoute = routes['GET /v1/reports/:reportId/download'];
const defaultDeleteRoute = routes['DELETE /v1/projects/:projectId'];
const defaultReadAllRoute = routes['POST /v1/notifications/read-all'];
const defaultReadRoute = routes['POST /v1/notifications/:notificationId/read'];

// 报告窗口桩：window.open 返回文档桩（createHTMLDocument 带 html/head/body，jsdom 未实现
// open），真实 openReportWindow 在其中执行 DOM 插桩；URL.createObjectURL jsdom 亦未实现——
// 桩补齐并捕获 Blob 留证（FileReader 读取内容，jsdom Blob 无 .text()）
const openedWindows: Window[] = [];
const createdBlobs: Blob[] = [];
let openSpy: { mockRestore: () => void; mock: { calls: unknown[][] } } | null = null;
let objectUrlStubbed = false;
function stubReportWindow() {
  openedWindows.length = 0;
  createdBlobs.length = 0;
  openSpy = vi.spyOn(window, 'open').mockImplementation(() => {
    // addEventListener：真实 Window 形状（B5 beforeunload revoke 依赖）；__listeners 供测试触发
    const w = {
      document: document.implementation.createHTMLDocument('report'),
      addEventListener: (t: string, fn: (e: Event) => void) => {
        ((w as unknown as { __listeners?: Record<string, (e: Event) => void> }).__listeners ??= {})[t] = fn;
      },
    } as unknown as Window;
    openedWindows.push(w);
    return w;
  });
  if (!objectUrlStubbed) {
    Object.defineProperty(URL, 'createObjectURL', {
      configurable: true,
      writable: true,
      value: (b: Blob) => {
        createdBlobs.push(b);
        return 'blob:mock-report-url';
      },
    });
    objectUrlStubbed = true;
  }
}
afterEach(() => {
  openSpy?.mockRestore();
  openSpy = null;
  if (objectUrlStubbed) {
    delete (URL as { createObjectURL?: unknown }).createObjectURL;
    objectUrlStubbed = false;
  }
  routes['GET /v1/reports/:reportId/download'] = defaultDownloadRoute;
  routes['DELETE /v1/projects/:projectId'] = defaultDeleteRoute;
  routes['POST /v1/notifications/read-all'] = defaultReadAllRoute;
  routes['POST /v1/notifications/:notificationId/read'] = defaultReadRoute;
});

function withProviders(ui: React.ReactElement, initial = '/') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[initial]}>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

// jsdom Blob 未实现 .text()——经 FileReader 读取 blob 内容断言
function blobText(b: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => resolve(String(fr.result));
    fr.onerror = () => reject(fr.error ?? new Error('FileReader 失败'));
    fr.readAsText(b);
  });
}

function expectCspMetaInjected(html: string, originalBody: string) {
  // meta 必须先于任何内容（blob 内容的首字节即 meta）
  expect(html).toContain(REPORT_WINDOW_CSP_META);
  expect(html.indexOf('Content-Security-Policy')).toBeGreaterThanOrEqual(0);
  expect(html.indexOf('Content-Security-Policy')).toBeLessThan(html.indexOf(originalBody));
  const doc = new DOMParser().parseFromString(html, 'text/html');
  const meta = doc.querySelector('meta[http-equiv="Content-Security-Policy"]');
  expect(meta).toBeTruthy();
  expect(meta!.getAttribute('content')).toContain("default-src 'none'"); // 脚本不可执行（语义不弱化）
  expect(meta!.getAttribute('content')).toContain("style-src 'unsafe-inline'");
}

// 新形态主断言：最后一次 openReportWindow 走 sandboxed iframe 通道，返回 blob 内容供后续断言
async function expectSandboxedIframeWindow(): Promise<string> {
  expect(openedWindows.length).toBeGreaterThanOrEqual(1);
  const w = openedWindows[openedWindows.length - 1];
  const iframe = w.document.querySelector('iframe');
  expect(iframe).toBeTruthy();
  // sandbox 空 token：无 allow-scripts / allow-same-origin——脚本全灭 + 来源隔离
  expect(iframe!.getAttribute('sandbox')).toBe('');
  expect(iframe!.getAttribute('src')).toBe('blob:mock-report-url');
  expect(createdBlobs.length).toBeGreaterThanOrEqual(1);
  const blob = createdBlobs[createdBlobs.length - 1];
  expect(blob.type).toContain('text/html');
  return blobText(blob);
}

describe('报告窗口 CSP 纵深防御（sandboxed iframe 通道）', () => {
  it('openReportWindow 单元：about:blank 宿主窗 + sandbox 空 token iframe + blob 前置 CSP meta', async () => {
    stubReportWindow();
    const ret = openReportWindow('<p>direct-unit</p>', 'text/html');
    expect(ret).toBe(openedWindows[0]);
    expect(openSpy!.mock.calls[0]).toEqual(['about:blank', '_blank']);
    const blobHtml = await expectSandboxedIframeWindow();
    expectCspMetaInjected(blobHtml, '<p>direct-unit</p>');
  });

  it('TaskDetailPage 在线查看（HTML 报告）：报告经 sandboxed iframe 渲染，CSP meta 先于正文进入 blob', async () => {
    // 摘要链路（snapshot→task-reports→report-content 三级查询）在满载并发下渲染时延抖动大，
    // 直接预置查询缓存让按钮首帧即现——本用例锁的是点击后的渲染通道行为，摘要查询形状
    // 由 'GET /v1/reports'（无 pagination 分支）路由兜底（后台失效重拉仍走真实客户端）。
    routes['GET /v1/reports/:reportId/download'] = () =>
      '<html><head></head><body><p>report body</p></body></html>';
    stubReportWindow();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(['task-snapshot', 't-1'], {
      task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_SAST_ONLY', sast_tools: [],
        status: 'TASK_STATUS_COMPLETED', stages: [], retry_count: 0 },
      logs: { logs: [] },
      ai: { chunk: '', next_cursor: '0', complete: true, total_bytes: '0' },
    });
    qc.setQueryData(['task-reports', 't-1'], {
      reports: [{ report_id: 'r-1', task_id: 't-1', format: 'REPORT_FORMAT_HTML', url: '', generated_at: null }],
    });
    qc.setQueryData(['report-content', 'r-1'], {
      format: 'json',
      content: '{"summary":{"total_findings":3,"true_positives":1,"false_positives":1,"not_reviewed":1}}',
    });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={['/tasks/t-1']}>
          <TaskDetailPage taskId="t-1" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    // 后台失效重拉会反复重渲染（旧节点被卸载后点击事件不会到达 React 根委托）——
    // waitFor 内每轮重查最新节点点击，直到开窗发生为止（多点几次无副作用）
    await waitFor(() => {
      fireEvent.click(screen.getByRole('button', { name: '在线查看完整报告' }));
      expect(openedWindows.length).toBeGreaterThan(0);
    }, { timeout: 8000 });
    const blobHtml = await expectSandboxedIframeWindow();
    expectCspMetaInjected(blobHtml, '<body>');
  });

  it('TaskDetailPage 在线查看（JSON 报告）：转义 <pre> 进 sandboxed iframe，尖括号全转义', async () => {
    routes['GET /v1/reports/:reportId/download'] = () =>
      '{"summary":{"note":"<script>alert(1)</script>"}}';
    stubReportWindow();
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(['task-snapshot', 't-1'], {
      task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_SAST_ONLY', sast_tools: [],
        status: 'TASK_STATUS_COMPLETED', stages: [], retry_count: 0 },
      logs: { logs: [] },
      ai: { chunk: '', next_cursor: '0', complete: true, total_bytes: '0' },
    });
    qc.setQueryData(['task-reports', 't-1'], {
      reports: [{ report_id: 'r-1', task_id: 't-1', format: 'REPORT_FORMAT_HTML', url: '', generated_at: null }],
    });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={['/tasks/t-1']}>
          <TaskDetailPage taskId="t-1" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => {
      fireEvent.click(screen.getByRole('button', { name: '在线查看完整报告' }));
      expect(openedWindows.length).toBeGreaterThan(0);
    }, { timeout: 8000 });
    const blobHtml = await expectSandboxedIframeWindow();
    expect(blobHtml).toContain('&lt;script&gt;alert(1)&lt;/script&gt;'); // 转义保留：标签不复活
    expect(blobHtml).not.toContain('<script'); // 原文尖括号不得进入 blob
    expectCspMetaInjected(blobHtml, '&lt;script&gt;');
  });

  it('ReportsPage 在线查看（HTML blob）：同样经 sandboxed iframe（不再直开 blob: URL / document.write）', async () => {
    stubReportWindow();
    withProviders(<ReportsPage />, '/reports');
    // 同 TaskDetail 用例：waitFor 内重查最新节点点击（满载下重渲染可能卸载旧节点）
    await waitFor(() => {
      fireEvent.click(screen.getByRole('button', { name: '在线查看' }));
      expect(openedWindows.length).toBeGreaterThan(0);
    }, { timeout: 8000 });
    const blobHtml = await expectSandboxedIframeWindow();
    expectCspMetaInjected(blobHtml, '<body>');
  });

  it('ReportsPage 在线查看（JSON blob）：转义 <pre> 进 sandboxed iframe，尖括号全转义', async () => {
    routes['GET /v1/reports/:reportId/download'] = () =>
      new NodeBlob(['{"summary":{"note":"<img src=x onerror=alert(1)>"}}'], { type: 'application/json' });
    stubReportWindow();
    withProviders(<ReportsPage />, '/reports');
    await waitFor(() => {
      fireEvent.click(screen.getByRole('button', { name: '在线查看' }));
      expect(openedWindows.length).toBeGreaterThan(0);
    }, { timeout: 8000 });
    const blobHtml = await expectSandboxedIframeWindow();
    expect(blobHtml).toContain('&lt;img src=x onerror=alert(1)&gt;');
    expect(blobHtml).not.toContain('<img');
    expectCspMetaInjected(blobHtml, '&lt;img');
  });
});

describe('报告"重新生成"接线（D4 裁定）', () => {
  it('点击重新生成 → POST /v1/tasks/t-1/report；成功后 reports 列表失效重拉', async () => {
    withProviders(<ReportsPage />, '/reports');
    const btn = await screen.findByRole('button', { name: '重新生成' }, { timeout: 8000 });
    expect((btn as HTMLButtonElement).disabled).toBe(false); // r-1 带 task_id，可重生成
    const listGets = () =>
      gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/reports' && r.query.includes(encodeURIComponent('"page_size":20'))).length;
    const before = listGets();
    // waitFor 内重查最新节点点击（防重渲染卸载旧节点后点击丢失）
    await waitFor(() => {
      fireEvent.click(screen.getByRole('button', { name: '重新生成' }));
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks/t-1/report')).toBe(true);
    }, { timeout: 8000 });
    await waitFor(() => expect(listGets()).toBeGreaterThan(before), { timeout: 8000 }); // invalidateQueries(['reports']) 生效
  });
});

describe('mutation onError（失败反馈携带状态码）', () => {
  it('项目删除失败（403）→ message.error 含 HTTP 403', async () => {
    routes['DELETE /v1/projects/:projectId'] = () => httpError(403, { error: 'forbidden' });
    withProviders(
      <Routes>
        <Route path="/projects/:id" element={<ProjectDetailPage />} />
      </Routes>,
      '/projects/p1',
    );
    await waitFor(() => expect(screen.getByText('Demo')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '删除项目' }));
    const ok = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-popconfirm-buttons .ant-btn-primary'),
    );
    fireEvent.click(ok!);
    expect(await screen.findByText(/项目删除失败（HTTP 403）/)).toBeTruthy();
  });

  it('全部已读失败（500）→ message.error 含 HTTP 500', async () => {
    routes['POST /v1/notifications/read-all'] = () => httpError(500, { error: 'boom' });
    withProviders(<NotificationsPage />, '/notifications');
    await waitFor(() => expect(screen.getByText('通知一')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '全部已读' }));
    expect(await screen.findByText(/全部已读失败（HTTP 500）/)).toBeTruthy();
  });

  it('单条标记已读失败（500）→ message.error 含 HTTP 500', async () => {
    routes['POST /v1/notifications/:notificationId/read'] = () => httpError(500, { error: 'boom' });
    withProviders(<NotificationsPage />, '/notifications');
    await waitFor(() => expect(screen.getByText('通知一')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '标记已读' }));
    expect(await screen.findByText(/标记已读失败（HTTP 500）/)).toBeTruthy();
  });
});

describe('B5 报告窗残余加固', () => {
  it('withReportCspMeta：doctype 内容 meta 插到 doctype 之后；无 doctype 维持前置', () => {
    const doc = '<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"></head><body><table></table></body></html>';
    const out = withReportCspMeta(doc);
    expect(out.startsWith('<!DOCTYPE html>')).toBe(true);
    expect(out.indexOf(REPORT_WINDOW_CSP_META)).toBeGreaterThan(0);
    expect(out.indexOf(REPORT_WINDOW_CSP_META)).toBeLessThan(out.indexOf('<html'));
    expect(withReportCspMeta('<table></table>').startsWith(REPORT_WINDOW_CSP_META)).toBe(true);
  });

  it('blob URL 在宿主窗卸载时 revoke（泄漏修复；窗口开着期间 blob 保持可取）', () => {
    stubReportWindow();
    const revokeSpy = vi.fn();
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: revokeSpy });
    try {
      const w = openReportWindow('<html><body>x</body></html>', 'text/html');
      expect(revokeSpy).not.toHaveBeenCalled();
      // 桩窗捕获 beforeunload 监听并触发（真实场景=用户关闭报告窗，宿主 document 卸载）
      const listeners = (w as unknown as { __listeners: Record<string, (e: Event) => void> }).__listeners;
      expect(listeners.beforeunload).toBeTruthy();
      listeners.beforeunload(new Event('beforeunload'));
      expect(revokeSpy).toHaveBeenCalledWith('blob:mock-report-url');
    } finally {
      delete (URL as { revokeObjectURL?: unknown }).revokeObjectURL;
    }
  });

  // 复审 R：beforeunload 在未交互 about:blank 窗口触发语义跨浏览器不保证——opener 侧
  // w.closed 1s 轮询兜底必须独立成立
  it('blob URL 在 w.closed 轮询下 revoke（beforeunload 未触发的浏览器兜底）', async () => {
    stubReportWindow();
    const revokeSpy = vi.fn();
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, writable: true, value: revokeSpy });
    try {
      const w = openReportWindow('<html><body>x</body></html>', 'text/html');
      let closed = false;
      Object.defineProperty(w!, 'closed', { configurable: true, get: () => closed });
      expect(revokeSpy).not.toHaveBeenCalled();
      closed = true;
      await new Promise((r) => setTimeout(r, 1300)); // 轮询粒度 1s
      expect(revokeSpy).toHaveBeenCalledWith('blob:mock-report-url');
    } finally {
      delete (URL as { revokeObjectURL?: unknown }).revokeObjectURL;
    }
  });

  it('withReportCspMeta：doctype 前有 HTML 注释变体同样插到 doctype 之后（复审 R）', () => {
    const doc = '<!-- generated -->\n<!doctype html><html><body>t</body></html>';
    const out = withReportCspMeta(doc);
    expect(out.indexOf('<!doctype html>')).toBeGreaterThan(0);
    expect(out.indexOf(REPORT_WINDOW_CSP_META)).toBeGreaterThan(out.indexOf('<!doctype html>'));
    expect(out.indexOf(REPORT_WINDOW_CSP_META)).toBeLessThan(out.indexOf('<html>'));
  });

  it('弹窗被拦（window.open→null）→ 返回 null（调用方据此提示）', () => {
    const spy = vi.spyOn(window, 'open').mockImplementation(() => null);
    try {
      expect(openReportWindow('<b></b>', 'text/html')).toBeNull();
    } finally {
      spy.mockRestore();
    }
  });

  it('ReportsPage 弹窗被拦 → message.warning 显式提示（不再静默）', async () => {
    vi.spyOn(window, 'open').mockImplementation(() => null);
    withProviders(<ReportsPage />, '/reports');
    await waitFor(() => {
      fireEvent.click(screen.getByRole('button', { name: '在线查看' }));
      expect(screen.getAllByText(/弹出窗口被浏览器拦截/).length).toBeGreaterThan(0);
    }, { timeout: 8000 });
  });
});
