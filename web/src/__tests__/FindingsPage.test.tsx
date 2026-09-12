// T3 回归：发现列表渲染 + 快捷 triage 调用体（PUT verdict）
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import FindingsPage from '../pages/findings/FindingsPage';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行，仅 HTTP 层伪造；未建模路由响亮失败
let findingsPayload: unknown;
const routes: Record<string, unknown> = {
  'GET /v1/findings': () => findingsPayload,
  'PUT /v1/findings/:findingId/verdict': () => ({}),
};
const gateway = useFakeGateway(routes);
function setDefaultFindings() {
  findingsPayload = { findings: [{
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
  }], pagination: { next_cursor: '', has_next: false, total: 2 } };
}
setDefaultFindings();

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
    const put = await waitFor(() => {
      const r = gateway.requests.find((x) => x.method === 'PUT');
      expect(r).toBeTruthy();
      return r!;
    });
    expect(put.url).toBe('/v1/findings/f-1/verdict');
    expect(put.body).toEqual({ verdict: 'AI_VERDICT_TRUE_POSITIVE', reasoning: 'console quick triage' });
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

// 修复回归：结论筛选此前整条链路死路——onChange 走 else 分支恒清空 verdictFilter，
// 且 filter 参数形状 {ai_verdict} 不契约（proto FilterRequest 只认 conditions）被网关
// DiscardUnknown 丢弃，服务端亦未接线 → 选任何具体结论都等于没选。现为纯客户端精确过滤。
describe('FindingsPage 结论筛选（客户端过滤）', () => {
  it('选择具体结论（需人工复核）→ 只剩匹配行，且请求不再携带死 filter 参数', async () => {
    setDefaultFindings();
    renderPage();
    await waitFor(() => expect(screen.getByText('hardcoded password')).toBeTruthy());
    await waitFor(() => expect(screen.getByText('SQL 注入：user_id 直接拼接')).toBeTruthy());
    // f-1=NEEDS_MANUAL、f-2=LIKELY_TRUE：选"需人工复核"后 f-2 应被过滤掉
    fireEvent.mouseDown(screen.getAllByRole('combobox')[0]); // [0]=结论筛选（新增严重度下拉在后）
    const opt = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-select-item-option[title="需人工复核"]'),
    );
    expect(opt).toBeTruthy();
    fireEvent.click(opt!);
    await waitFor(() => expect(screen.queryByText('SQL 注入：user_id 直接拼接')).toBeNull());
    expect(screen.getByText('hardcoded password')).toBeTruthy();
    // 结论筛选是纯客户端行为：任何 GET /v1/findings 都不应出现 filter 参数
    const gets = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/findings');
    expect(gets.length).toBeGreaterThan(0);
    expect(gets.every((r) => !r.query.includes('filter='))).toBe(true);
  });

  it('未判定分组：结论筛选切到"未判定"→ 已判定的行全部隐藏', async () => {
    setDefaultFindings();
    renderPage();
    await waitFor(() => expect(screen.getByText('hardcoded password')).toBeTruthy());
    fireEvent.mouseDown(screen.getAllByRole('combobox')[0]); // [0]=结论筛选（新增严重度下拉在后）
    const opt = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-select-item-option[title="未判定"]'),
    );
    fireEvent.click(opt!);
    await waitFor(() => expect(screen.queryByText('hardcoded password')).toBeNull());
    expect(screen.queryByText('SQL 注入：user_id 直接拼接')).toBeNull();
    expect(screen.getByText('暂无发现（任务完成或无命中）')).toBeTruthy();
  });
});

// ADR-159 回归：真解析 source_raw 的 dataflow_trace → "污点链路"徽标（非按工具名猜测）
it('携带 dataflow_trace 的行显示污点链路徽标', async () => {
  const trace = { tool: 'opengrep', taint: true,
    dataflow_trace: { taint_source: ['CliLoc', [{ path: 'app.py', start: { line: 4 }, end: { line: 4 } }], 'src'],
      intermediate_vars: [{ location: { path: 'app.py', start: { line: 4 } }, content: 'q' }],
      taint_sink: ['CliLoc', [{ path: 'app.py', start: { line: 7 }, end: { line: 7 } }, 'sink']] } };
  findingsPayload = { findings: [{
    finding_id: 'f-og', task_id: 't1', source_tool: 'opengrep',
    source_rule_id: 'codeaudit-sql-taint-user-param',
    cwe_id: 'CWE-89', title: 'SQL 注入', severity: 'SEVERITY_HIGH',
    confidence: 0.9, ai_verdict: 'AI_VERDICT_UNSPECIFIED', ai_confidence: 0,
    location: { file_path: 'app.py', start_line: 7 },
    source_raw: btoa(String.fromCharCode(...new TextEncoder().encode(JSON.stringify(trace)))),
  }], pagination: { next_cursor: '', has_next: false, total: 1 } };
  renderPage();
  await waitFor(() => expect(screen.getByText('污点链路')).toBeTruthy());
});

// ADR-225 D2 双视图：来源筛选（全部/新发现/继承）+ 行级继承角标（A21.1/A21.2）
describe('FindingsPage 来源筛选（ADR-225 双视图）', () => {
  it('继承项带「继承」角标；来源=继承 → 只剩继承行；来源=新发现 → 只剩实扫行', async () => {
    findingsPayload = { findings: [{
      finding_id: 't2-bandit-1', task_id: 't2', source_tool: 'bandit', source_rule_id: 'B608',
      cwe_id: 'CWE-89', title: '新发现-硬编码', severity: 'SEVERITY_HIGH',
      confidence: 0.9, ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
      location: { file_path: 'app.py', start_line: 3 },
    }, {
      finding_id: 't2-inh-1', task_id: 't2', source_tool: 'bandit', source_rule_id: 'B105',
      cwe_id: 'CWE-259', title: '继承-旧密码', severity: 'SEVERITY_MEDIUM',
      confidence: 0.7, ai_verdict: 'AI_VERDICT_TRUE_POSITIVE', ai_confidence: 0.7,
      inherited_from_task_id: 't1',
      location: { file_path: 'old.py', start_line: 8 },
    }], pagination: { next_cursor: '', has_next: false, total: 2 } };
    renderPage();
    await waitFor(() => expect(screen.getByText('新发现-硬编码')).toBeTruthy());
    expect(screen.getByText('继承-旧密码')).toBeTruthy();
    expect(screen.getByText('继承')).toBeTruthy(); // 行级继承角标（A21.2）

    // 来源筛选（[2]=来源下拉：[0]结论 [1]严重程度 之后）
    fireEvent.mouseDown(screen.getAllByRole('combobox')[2]);
    const opt = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-select-item-option[title="继承"]'),
    );
    fireEvent.click(opt!);
    await waitFor(() => expect(screen.queryByText('新发现-硬编码')).toBeNull());
    expect(screen.getByText('继承-旧密码')).toBeTruthy();

    fireEvent.mouseDown(screen.getAllByRole('combobox')[2]);
    const optNew = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-select-item-option[title="新发现"]'),
    );
    fireEvent.click(optNew!);
    await waitFor(() => expect(screen.queryByText('继承-旧密码')).toBeNull());
    expect(screen.getByText('新发现-硬编码')).toBeTruthy();
  });
});

// ===== B3-1（审计修复）：单 cursor useQuery → useInfiniteQuery 无限查询 =====
// 锁三点：翻页后前页保留 / has_next=false 停止 / 客户端筛选跨累计页生效且不重发请求。
describe('FindingsPage 无限查询分页（B3-1）', () => {
  const f1 = {
    finding_id: 'f-p1', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B105',
    cwe_id: 'CWE-798', title: '第一页发现', severity: 'SEVERITY_HIGH',
    confidence: 0.9, ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0,
    location: { file_path: 'a.py', start_line: 1 },
  };
  const f2 = {
    finding_id: 'f-p2', task_id: 't1', source_tool: 'bandit', source_rule_id: 'B608',
    cwe_id: 'CWE-89', title: '第二页发现', severity: 'SEVERITY_MEDIUM',
    confidence: 0.8, ai_verdict: 'AI_VERDICT_UNSPECIFIED', ai_confidence: 0,
    location: { file_path: 'b.py', start_line: 2 },
  };
  const twoPageRoute = () => {
    routes['GET /v1/findings'] = (ctx: { query: URLSearchParams }) => {
      const cursor = JSON.parse(ctx.query.get('pagination') ?? '{"cursor":""}').cursor ?? '';
      if (cursor === '') return { findings: [f1], pagination: { next_cursor: 'c2', has_next: true, total: 2 } };
      return { findings: [f2], pagination: { next_cursor: '', has_next: false, total: 2 } };
    };
  };

  it('加载更多翻页后前页保留、后页追加；共 N 条取服务端 total', async () => {
    twoPageRoute();
    renderPage();
    await waitFor(() => expect(screen.getByText('第一页发现')).toBeTruthy());
    expect(screen.getByText('共 2 条')).toBeTruthy(); // total=2（服务端口径，非已加载行数 1）
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
    await waitFor(() => expect(screen.getByText('第二页发现')).toBeTruthy());
    expect(screen.getByText('第一页发现')).toBeTruthy(); // 前页保留（此前单 cursor 整表替换会丢）
    // 第二页请求携带首页下发的 next_cursor
    const last = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/findings').pop()!;
    expect(last.query).toContain(encodeURIComponent('"cursor":"c2"'));
  });

  it('has_next=false 时不渲染加载更多按钮（游标尽头自停）', async () => {
    routes['GET /v1/findings'] = () => findingsPayload; // 恢复默认路由（上一用例改写了游标路由）
    setDefaultFindings(); // pagination: { next_cursor: '', has_next: false }
    renderPage();
    await waitFor(() => expect(screen.getByText('hardcoded password')).toBeTruthy());
    expect(screen.queryByRole('button', { name: '加载更多' })).toBeNull();
  });

  it('累计两页后切换筛选：过滤跨页生效且不重发请求（客户端筛选不进 queryKey）', async () => {
    twoPageRoute();
    renderPage();
    await waitFor(() => expect(screen.getByText('第一页发现')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }));
    await waitFor(() => expect(screen.getByText('第二页发现')).toBeTruthy());
    const getsBefore = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/findings').length;
    // 严重程度筛选（纯客户端）：只剩第二页的中危行
    fireEvent.mouseDown(screen.getAllByRole('combobox')[1]); // [1]=严重程度
    const opt = await waitFor(() =>
      document.body.querySelector<HTMLElement>('.ant-select-item-option[title="中危"]'),
    );
    fireEvent.click(opt!);
    await waitFor(() => expect(screen.queryByText('第一页发现')).toBeNull());
    expect(screen.getByText('第二页发现')).toBeTruthy(); // 筛选作用于累计页（无翻页错位）
    const getsAfter = gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/findings').length;
    expect(getsAfter).toBe(getsBefore); // 击键/筛选不触发重拉（进 queryKey 反而是回归）
  });
});

// B3-3（审计修复）：快捷 triage 此前失败静默——message.error 必须携带状态码
describe('FindingsPage 快捷 triage 失败反馈（B3-3）', () => {
  it('PUT verdict 409 → message.error 含 HTTP 409', async () => {
    routes['PUT /v1/findings/:findingId/verdict'] = () => httpError(409, { error: 'conflict' });
    renderPage();
    const confirmBtn = await waitFor(() => screen.getAllByRole('button', { name: /确\s*认/ })[0]);
    fireEvent.click(confirmBtn);
    expect(await screen.findByText(/结论回写失败（HTTP 409）/)).toBeTruthy();
  });
});

// B4-3（审计修复）：快捷 triage 成功联动失效融合/审核视图缓存——两者读 ai_verdict
// （FusionView 去重分区列 / ReviewView 结论列），不联动则任务详情内切 Tab 停留旧结论。
// 探针：与真实视图相同的 queryKey（['fusion-findings', taskId] / ['review-findings', taskId]），
// queryFn 计数（不打 HTTP——观测的是失效重拉行为本身）。
describe('FindingsPage 快捷 triage 缓存联动（B4-3）', () => {
  it('裁决成功 → fusion-findings / review-findings 前缀失效并重拉', async () => {
    // 自愈路由（前序用例会改写 GET 处理器——游标路由/载荷轮换——不恢复到默认形态）
    setDefaultFindings();
    routes['GET /v1/findings'] = () => findingsPayload;
    routes['PUT /v1/findings/:findingId/verdict'] = () => ({});
    let fusionFetches = 0;
    let reviewFetches = 0;
    function Probe() {
      useQuery({
        queryKey: ['fusion-findings', 't1'],
        queryFn: async () => { fusionFetches += 1; return findingsPayload; },
      });
      useQuery({
        queryKey: ['review-findings', 't1'],
        queryFn: async () => { reviewFetches += 1; return findingsPayload; },
      });
      return null;
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <Probe />
        <MemoryRouter initialEntries={['/tasks/t1/findings']}>
          <FindingsPage taskId="t1" />
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(fusionFetches).toBe(1)); // 探针初拉
    await waitFor(() => expect(reviewFetches).toBe(1));
    await waitFor(() => expect(screen.getByText('hardcoded password')).toBeTruthy());
    const confirmBtn = await waitFor(() => screen.getAllByRole('button', { name: /确\s*认/ })[0]);
    fireEvent.click(confirmBtn);
    await waitFor(() => expect(screen.getByText('结论已回写')).toBeTruthy());
    // 前缀失效 → 挂载中的探针查询重拉各一次
    await waitFor(() => expect(fusionFetches).toBe(2));
    await waitFor(() => expect(reviewFetches).toBe(2));
  });
});
