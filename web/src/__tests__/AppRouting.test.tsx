// 路由与全局壳契约（docs/internal-interfaces.md §8 [I-80..I-84, I-101]）。
// 此前 App.tsx 守卫链零直测——未登录跳转 / must_change_password 锁死 / RequireAdmin / 404 兜底
// 都是用户可见行为（ADR-147/205 历史修复点），在此行为级锁定。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, useNavigate } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import App from '../App';
import { SessionProvider } from '../auth/session';
import { TOKEN_KEY, clearSession } from '../api/client';
import { useFakeGateway } from '../testsupport/fakeGateway';

let me: Record<string, unknown> = { user_id: 'u-1', username: 'alice', email: 'a@x', role: 'ROLE_ADMIN' };
const b64 = (s: string) => Buffer.from(s, 'utf-8').toString('base64');
const routes: Record<string, unknown> = {
  'GET /v1/users/me': () => me,
  'GET /v1/notifications': () => ({
    notifications: [
      { notification_id: 'n-1', user_id: 'u-1', title: 't1', body: 'b1', read: false, created_at: null },
      { notification_id: 'n-2', user_id: 'u-1', title: 't2', body: 'b2', read: false, created_at: null },
      { notification_id: 'n-3', user_id: 'u-1', title: 't3', body: 'b3', read: true, created_at: null },
    ],
  }),
  'GET /v1/projects': { projects: [], pagination: { next_cursor: '', has_next: false, total: 0 } },
  'GET /v1/users': {
    users: [
      { user_id: 'u-1', username: 'alice', email: 'a@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_ADMIN', created_at: null },
      { user_id: 'u-2', username: 'dev1', email: 'd@x', state: 'USER_STATE_ACTIVE', role: 'ROLE_DEVELOPER', created_at: null },
    ],
    pagination: { total: 2 },
  },
  // B4-1 用例：任务详情快照（t-a 带 AI 片段与日志，t-b 全新干净流）
  'GET /v1/tasks/:taskId/snapshot': (ctx: { params: Record<string, string> }) => {
    if (ctx.params.taskId === 't-a') {
      return {
        task: { task_id: 't-a', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], retry_count: 0 },
        progress: { task_id: 't-a', status: 'TASK_STATUS_RUNNING', overall_percent: 10, stages: [] },
        logs: { logs: [{ log_id: 'a-1', task_id: 't-a', ts_ms: 1756630000000, level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '任务A专属日志行' }] },
        ai: { chunk: b64('任务A专属AI片段'), next_cursor: '21', complete: false, total_bytes: '21' },
      };
    }
    return {
      task: { task_id: 't-b', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], retry_count: 0 },
      progress: { task_id: 't-b', status: 'TASK_STATUS_RUNNING', overall_percent: 5, stages: [] },
      logs: { logs: [{ log_id: 'b-1', task_id: 't-b', ts_ms: 1756630005000, level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '任务B专属日志行' }] },
      ai: { chunk: b64('任务B专属AI片段'), next_cursor: '21', complete: false, total_bytes: '21' },
    };
  },
  // I-84 导航重整用例：发现详情深链载荷（最小可渲染形状，同 codeContext.test 口径；
  // source-file 端点不建模 → 组件走 ADR-195 降级片段路径，与本断言面无关）
  'GET /v1/findings/:findingId': () => ({
    finding: {
      finding_id: 'f-1', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
      cwe_id: 'CWE-259', title: 'nav-order finding', description: 'd',
      severity: 'SEVERITY_MEDIUM', confidence: 0.5,
      verdict_direction: 'VERDICT_DIRECTION_UNSPECIFIED', verdict_tier: 'VERDICT_TIER_UNSPECIFIED', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '',
      location: { file_path: 'app.py', start_line: 1 },
      source_raw: b64(JSON.stringify({ code: 'x = 1\n', filename: 'app.py', line_number: 1, test_id: 'B105' })),
    },
  }),
};
const gateway = useFakeGateway(routes);

function renderApp(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SessionProvider>
        <MemoryRouter initialEntries={[path]}>
          <App />
        </MemoryRouter>
      </SessionProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  clearSession();
  me = { user_id: 'u-1', username: 'alice', email: 'a@x', role: 'ROLE_ADMIN' };
  // 带token进站必经 bootRefresh（裸 fetch）——默认成功
  vi.stubGlobal('fetch', vi.fn(async () =>
    new Response(JSON.stringify({ access_token: 'acc-boot', refresh_token: 'ref-boot', expires_in_s: 1800 }),
      { status: 200, headers: { 'Content-Type': 'application/json' } })));
});
afterEach(() => { vi.unstubAllGlobals(); });

describe('I-81 Shell 守卫链', () => {
  it('未登录访问受保护路由 → 跳 /login（登录卡可见）', async () => {
    renderApp('/projects');
    expect(await screen.findByText('CodeAudit 控制台')).toBeTruthy();
  });

  it('must_change_password=true → 锁死 /change-password（访问 /projects 也被重定向，ADR-205）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    me = { ...me, must_change_password: true };
    renderApp('/projects');
    // 菜单按钮与卡片标题同为"修改密码"——断言强改密页说明文案（该页独有）
    expect(await screen.findByText(/必须设置新密码后才能继续使用/)).toBeTruthy();
    expect((await screen.findAllByText('修改密码')).length).toBeGreaterThanOrEqual(1);
  });

  it('已登录正常进站 → 受保护路由可达（项目页；菜单与页标题同文案取 all）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/projects');
    expect((await screen.findAllByText('项目')).length).toBeGreaterThanOrEqual(1);
  });
});

describe('I-82 RequireAdmin + I-83 未读角标', () => {
  it('ROLE_ADMIN 访问 /admin/users → 用户列表加载（dev1 行可见）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/admin/users');
    expect(await screen.findByText('dev1')).toBeTruthy();
  });

  it('非 admin 访问 /admin/users → 403 页（后端 requireAdmin 为最终防线，前端仅体验层）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    me = { ...me, role: 'ROLE_DEVELOPER' };
    renderApp('/admin/users');
    expect(await screen.findByText('权限不足')).toBeTruthy();
  });

  it('未读角标 = notifications 未读计数（2 条未读 → 导航 Badge 计数，60s 兜底轮询的初始拉取；设计整改 P2 文本括号→Badge）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/projects');
    expect(await screen.findByText('通知')).toBeTruthy();
    // antd Badge 计数挂 title 属性（0 时 Badge 不渲染）
    await waitFor(() => expect(screen.getByTitle('2')).toBeTruthy());
  });
});

describe('I-80 路由兜底', () => {
  it('未知路由 → 404 页（不静默重定向）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/definitely/not/exist');
    expect(await screen.findByText(/页面不存在/)).toBeTruthy();
  });
});

// 2026-10-04 导航重整（用户指令"基于第一性原理"）：序/分区（I-101）= 工件流 │ 配置治理横切；
// 编排是被任务引用的定义库（写面 admin、内置只读），2026-09-20 入口打通批就地插在
// 任务与报告中心之间无排序理由，凿断 项目→任务→报告 因果链，本批迁至分界线后首位；
// 品牌区补"点 logo 回家"惯例（此前死 div）；findings 深链归"任务"高亮（I-84 产物下钻不断灯）。
describe('I-101 导航序·分区·品牌回家 + I-84 findings 高亮', () => {
  const menuLabels = () =>
    Array.from(document.querySelectorAll('li.ant-menu-item'), (li) => (li.textContent ?? '').trim());

  it('admin：序=首页·项目·任务·报告中心│编排·用户管理·工作区·通知；分界线钉在报告中心与编排之间（2026-10-05 父含子：「推理 Provider」项退役）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/projects');
    await screen.findAllByText('项目');
    expect(menuLabels()).toEqual([
      '首页', '项目', '任务', '报告中心', '编排', '用户管理', '工作区',
      expect.stringContaining('通知'),
    ]);
    // 自绘分隔线（antd 横向 {type:'divider'} 隐形，见 App.tsx 注）钉位：
    // 左邻=工件流 Menu（末项 报告中心），右邻=配置治理 Menu（首项 编排）；
    // rc-menu 横向 ul 尾部挂空 measure li，故取"直接子级菜单项"而非 lastElementChild
    // 自绘分隔线（antd 横向 {type:'divider'} 隐形，见 App.tsx 注）钉位：
    // 文档序 = 报告中心 < 分界线 < 编排。不耦合 antd Menu 的兄弟结构
    // 
    const divider = document.querySelector('[data-testid="nav-zone-divider"]');
    expect(divider).toBeTruthy();
    const itemOf = (t: string) =>
      Array.from(document.querySelectorAll('li.ant-menu-item')).find((li) => (li.textContent ?? '').trim() === t);
    const reports = itemOf('报告中心');
    const orch = itemOf('编排');
    expect(reports).toBeTruthy();
    expect(orch).toBeTruthy();
    expect(reports!.compareDocumentPosition(divider!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(divider!.compareDocumentPosition(orch!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('非 admin：管理面三项不渲染，分区结构不变（工件流│编排·通知）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    me = { ...me, role: 'ROLE_DEVELOPER' };
    renderApp('/projects');
    await screen.findAllByText('项目');
    expect(menuLabels()).toEqual(['首页', '项目', '任务', '报告中心', '编排', expect.stringContaining('通知')]);
    expect(document.querySelector('[data-testid="nav-zone-divider"]')).toBeTruthy();
  });

  it('品牌区点击回首页（消融对照：点击前"首页"未选中，点击后选中——此前品牌区是死 div）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/projects');
    await screen.findAllByText('项目');
    const header = document.querySelector('.ant-layout-header') as HTMLElement;
    expect(within(header).getByText('首页').closest('li')?.className).not.toContain('ant-menu-item-selected');
    fireEvent.click(within(header).getByRole('link', { name: '返回首页' }));
    await waitFor(() =>
      expect(within(header).getByText('首页').closest('li')?.className).toContain('ant-menu-item-selected'));
  });

  it('/findings/:fid 深链 → "任务"高亮（产物下钻不断灯；此前 startsWith 链漏 findings，全导航无选中态）', async () => {
    localStorage.setItem(TOKEN_KEY, 'ref-1');
    renderApp('/findings/f-1');
    expect(await screen.findByText('nav-order finding')).toBeTruthy();
    const header = document.querySelector('.ant-layout-header') as HTMLElement;
    expect(within(header).getByText('任务').closest('li')?.className).toContain('ant-menu-item-selected');
  });
});

// 测试内导航钩子：经真实 react-router 导航触发同路由 :id 切换（非 rerender 整树）
function NavButton({ to }: { to: string }) {
  const navigate = useNavigate();
  return <button onClick={() => navigate(to)}>{`goto-${to}`}</button>;
}

// B4-1（审计修复）：同路由 `/tasks/:id` 切换任务必须重建任务详情实例（TaskDetailWithParams
// key={id}）——此前 react-router 复用组件实例，旧任务的增量游标（logs_after/ai_cursor refs）
// 与已吸收的日志/AI 正文残留进新任务：新任务快照带旧游标（服务端从旧位置起算，头部内容
// 永久丢失）、面板残留旧任务 AI 正文片段。
describe('I-A3 同路由切换任务清态（B4-1）', () => {
  it('/tasks/t-a → /tasks/t-b：B 面板不含 A 的 AI 正文/日志；B 首个快照请求游标从零起', async () => {
    // WS 桩：不真连（jsdom 真 WebSocket 会打网络且异步错误偶发污染 Errors 计数）；
    // 永不 onopen → 页面自然回退快照轮询，数据路径与真实断线场景一致
    class FakeWS {
      onopen: (() => void) | null = null;
      onmessage: ((ev: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      close() { this.onclose?.(); }
    }
    const origWS = (globalThis as { WebSocket?: unknown }).WebSocket;
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS as unknown;
    try {
      localStorage.setItem(TOKEN_KEY, 'ref-1');
      const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      render(
        <QueryClientProvider client={qc}>
          <SessionProvider>
            <MemoryRouter initialEntries={['/tasks/t-a']}>
              <NavButton to="/tasks/t-b" />
              <App />
            </MemoryRouter>
          </SessionProvider>
        </QueryClientProvider>,
      );
      // A 面板已吸收 AI 正文与日志（游标已推进：logAfter=a-1、aiCursor=20）
      await waitFor(() => expect(screen.getByText(/任务 t-a/)).toBeTruthy());
      await waitFor(() => expect(screen.getByTestId('ai-interaction-log-box').textContent).toContain('任务A专属AI片段'));
      await waitFor(() => expect(screen.getByText('任务A专属日志行')).toBeTruthy());

      fireEvent.click(screen.getByText('goto-/tasks/t-b'));
      // B 面板加载：不含 A 残留（key 重建实例 → state/refs 全复位）
      await waitFor(() => expect(screen.getByText(/任务 t-b/)).toBeTruthy());
      await waitFor(() => expect(screen.queryByText(/任务A专属AI片段/)).toBeNull());
      expect(screen.queryByText('任务A专属日志行')).toBeNull();
      await waitFor(() => expect(screen.getByText('任务B专属日志行')).toBeTruthy());

      // 游标从零起：B 的首个快照请求不得携带 A 推进出的 logs_after/ai_cursor
      const bGets = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/tasks/t-b/snapshot');
      expect(bGets.length).toBeGreaterThan(0);
      expect(bGets[0].query).not.toContain('logs_after');
      expect(bGets[0].query).not.toContain('ai_cursor');
      // 对照组：A 面板确实走过快照（游标机制在旧实例中工作，残留才会经它泄漏）
      const aGets = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/tasks/t-a/snapshot');
      expect(aGets.length).toBeGreaterThan(0);
    } finally {
      (globalThis as { WebSocket?: unknown }).WebSocket = origWS;
    }
  });
});
