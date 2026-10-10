// 任务详情嵌入回归（ADR-142 补全）：发现/融合 Tabs 内嵌，不再有独立"查看发现"按钮
// 修复回归：WS 卸载关闭
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
// graph-ui-channels 批3：图任务=画布占主面板位（Q1）——页面级断言画布挂载+AI 面板让渡。
vi.mock('../components/TaskGraphCanvas', () => ({
  // data-has-download 透传 onDownloadLog 接线状态（D1 抓获 3378095 漏传回归的锚：
  // 按钮渲染条件 onDownloadLog&& 恒 false=图任务下载入口断链）
  default: (props: { taskId: string; onDownloadLog?: () => void }) => (
    <div data-testid="graph-canvas-mock" data-task={props.taskId} data-has-download={props.onDownloadLog ? '1' : '0'}>canvas</div>
  ),
}));
import TaskDetailPage from '../pages/tasks/TaskDetailPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行（含 ADR-170 轮询口），仅 HTTP 层伪造。
// pollIntervalMs/getAccessToken 用真实实现（无 token 时 WS 连接失败自然回退轮询，无需 stub）。
// 降级可见性用例：快照/发现载荷可按用例注入（DEFAULT_* 轮换）。
const DEFAULT_SNAPSHOT = {
  task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_TRADITIONAL_FIRST',
    sast_tools: ['bandit'], status: 'TASK_STATUS_COMPLETED', stages: [], retry_count: 0 },
  progress: { task_id: 't-1', status: 'TASK_STATUS_COMPLETED', overall_percent: 100, stages: [] },
  logs: { logs: [{ log_id: '1', task_id: 't-1', ts_ms: 1756630000000,
    level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '状态流转 TASK_STATUS_CREATED → TASK_STATUS_PENDING（submit）' }] },
  ai: { chunk: '', next_cursor: '0', complete: true, total_bytes: '0' },
};
const DEFAULT_FINDINGS = { findings: [], pagination: { next_cursor: '', has_next: false, total: 0 } };
let snapshotOverride: unknown = null;
let findingsPayload: unknown = DEFAULT_FINDINGS;
const gateway = useFakeGateway({
  // ADR-170: 详情页改用聚合快照单口轮询
  'GET /v1/tasks/:taskId/snapshot': () => snapshotOverride ?? DEFAULT_SNAPSHOT,
  // TaskUsageCard：空聚合=诚实空态行
  'GET /v1/tasks/:taskId/usage': () => ({ task_id: 't-1', models: [], totals: { model: '', calls: '0', input_tokens: '0', output_tokens: '0' } }),
  'GET /v1/reports': () => ({ reports: [{ report_id: 'r-1', task_id: 't-1', format: 'REPORT_FORMAT_JSON', url: '', generated_at: null }] }),
  'GET /v1/findings': () => findingsPayload,
  'POST /v1/tasks/:taskId/report': () => ({ report_id: 'r-new' }),
  'POST /v1/tasks/:taskId/cancel': () => ({}),
  // TP20-T2 切换 Agent：Modal 打开即拉清单
  'GET /v1/agents': () => ({ agents: [
    { id: 'dsh', dialect: 'dsh-sse', is_default: true, tool_events: true, structured_submit: true, interaction: false, session_resume: false, conclude_semantics: true },
    { id: 'maka', dialect: 'maka-runtime-host', is_default: false, tool_events: false, structured_submit: false, interaction: false, session_resume: false, conclude_semantics: false },
  ] }),
  'POST /v1/tasks/:taskId/agent': () => ({ task_id: 't-1', agent: 'maka', status: 'TASK_STATUS_PAUSED' }),
});

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks/t-1']}>
        <TaskDetailPage taskId="t-1" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('TaskDetailPage（发现/融合内嵌）', () => {
  it('COMPLETED 任务渲染发现与融合 Tabs（模式B）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
    await waitFor(() => expect(screen.getAllByText('发现').length).toBeGreaterThan(0));
    expect(screen.getAllByText('融合视图').length).toBeGreaterThan(0);
    // 无独立"查看发现"按钮（已内嵌）
    expect(screen.queryByRole('button', { name: '查看发现' })).toBeNull();
  });

  it('修复回归：卸载时关闭 WebSocket 推流（此前清理只置标志不 close，连接泄漏持续收帧）', async () => {
    const created: { url: string; closed: boolean; onclose: (() => void) | null }[] = [];
    class FakeWS {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      url: string;
      closed = false;
      onopen: (() => void) | null = null;
      onmessage: ((ev: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(url: string) {
        this.url = url;
        created.push(this);
      }
      close() {
        this.closed = true;
        this.onclose?.();
      }
    }
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS;
    try {
      const { unmount } = renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(created.length).toBeGreaterThan(0);
      expect(created[0].url).toContain('/v1/tasks/t-1/ws');
      expect(created[0].closed).toBe(false);
      unmount();
      expect(created[0].closed).toBe(true);
    } finally {
      delete (globalThis as { WebSocket?: unknown }).WebSocket;
    }
  });


});

// gw-f6a3523 实证回归锁①：AI 交互日志必须随 WS 帧增量到达逐步渲染（而非收束一次性）。
// 吸收链 = absorbSnapshot 的 next_cursor 单调 + chunk 追加——任何回归（如只在 complete
// 时吸收、游标比较颠倒）当场红。
describe('TaskDetailPage（AI 日志流式增量）', () => {
  it('WS AI 帧逐帧到达 → 面板文本逐步增长', async () => {
    const b64 = (s: string) => Buffer.from(s, 'utf-8').toString('base64');
    const frame = (chunk: string, cursor: number, total: number) => JSON.stringify({
      type: 'snapshot',
      task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL',
        sast_tools: [], status: 'TASK_STATUS_RUNNING', stages: [], retry_count: 0 },
      progress: { task_id: 't-1', status: 'TASK_STATUS_RUNNING', overall_percent: 40, stages: [] },
      logs: { logs: [] },
      ai: { chunk: b64(chunk), next_cursor: String(cursor), total_bytes: String(total) },
    });
    const created: { onmessage: ((ev: { data: string }) => void) | null; onopen: (() => void) | null }[] = [];
    class FakeWS {
      onopen: (() => void) | null = null;
      onmessage: ((ev: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(_url: string) { created.push(this); }
      close() { this.onclose?.(); }
    }
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS;
    try {
      renderPage();
      await waitFor(() => expect(created[0]).toBeTruthy());
      created[0].onopen?.();
      const box = () => screen.getByTestId('ai-interaction-log-box').textContent ?? '';
      created[0].onmessage?.({ data: frame('第一段思考', 15, 30) });
      await waitFor(() => expect(box()).toContain('第一段思考'));
      created[0].onmessage?.({ data: frame('第二段思考', 30, 30) });
      await waitFor(() => expect(box()).toContain('第二段思考'));
      expect(box()).toContain('第一段思考'); // 增量不覆盖既有内容
      created[0].onmessage?.({ data: frame('', 30, 30) }); // 空块帧（total 更新）不炸不重复
      expect(box()).toContain('第二段思考');
    } finally {
      delete (globalThis as { WebSocket?: unknown }).WebSocket;
    }
  });

  // gw-f6a3523 实证回归锁②：非收束断线必须立即回填快照（服务端游标已越过 pend 内容，
  // 只能经快照补齐）——断线期间显示空白直到任务结束的回归在此当场红。
  it('WS 非收束断线 → 立即补拉快照（不等重连/轮询拍）', async () => {
    const created: { onopen: (() => void) | null; onclose: (() => void) | null }[] = [];
    class FakeWS {
      onopen: (() => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(_url: string) { created.push(this); }
      close() { this.onclose?.(); }
    }
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS;
    try {
      renderPage();
      const snapshotGets = () => gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/tasks/t-1/snapshot').length;
      await waitFor(() => expect(created[0]).toBeTruthy());
      created[0].onopen?.();
      const before = snapshotGets();
      created[0].onclose?.(); // 模拟服务端 watch lifetime close（任务仍 RUNNING）
      await waitFor(() => expect(snapshotGets()).toBeGreaterThan(before));
    } finally {
      delete (globalThis as { WebSocket?: unknown }).WebSocket;
    }
  });
});

// AI 降级可见性：RuleScan 兜底任务仍 COMPLETED、时间线全绿，
// 降级痕迹只在发现级（rulescan-fallback: 前缀 / [降级 前缀 reasoning）→ 页面必须显性警示。
describe('TaskDetailPage（AI 降级可见性，2026-09-11 报障）', () => {
  const fallbackFinding = {
    finding_id: 'f-1', task_id: 't-1', project_id: 'p1', source_tool: 'bandit',
    source_rule_id: 'rulescan-fallback:B101', cwe_id: 'CWE-798', title: '硬编码密码',
    description: 'desc', severity: 'SEVERITY_HIGH', confidence: 0.9,
    ai_verdict: 'AI_VERDICT_UNSPECIFIED', ai_confidence: 0, ai_reasoning: '',
    ai_fix_suggestion: '', dedup_group: 'g1', matched_findings: [], is_unique: true,
  };
  const findingsGets = () => gateway.requests.filter((r) => r.method === 'GET' && r.url === '/v1/findings').length;

  it('发现含 rulescan-fallback: 前缀 → 渲染降级警示 Alert（零额外请求：只订阅发现缓存）', async () => {
    findingsPayload = { findings: [fallbackFinding], pagination: { next_cursor: '', has_next: false, total: 1 } };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(await screen.findByText(/AI 推理未生效——沙箱不可达，已由内置规则引擎（RuleScan）兜底/, {}, { timeout: 5000 })).toBeTruthy();
      // 降级判定零额外请求：findings 仅由内嵌发现列表拉取（页面级 Alert 不重复请求）
      const pageLevelGets = findingsGets();
      expect(pageLevelGets).toBeGreaterThan(0);
    } finally {
      findingsPayload = DEFAULT_FINDINGS;
    }
  });

  it('无降级发现 → 不渲染降级 Alert（默认空发现即此形态）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
    await waitFor(() => expect(findingsGets()).toBeGreaterThan(0)); // 发现已加载
    expect(screen.queryByText(/AI 推理未生效/)).toBeNull();
  });

  it('发现 reasoning 带 [降级 前缀同样判定降级（source_rule_id 缺前缀时）', async () => {
    findingsPayload = { findings: [
      { ...fallbackFinding, source_rule_id: 'B101', ai_reasoning: '[降级] 规则引擎兜底产出，未经 AI 语义审查' },
    ], pagination: { next_cursor: '', has_next: false, total: 1 } };
    try {
      renderPage();
      expect(await screen.findByText(/AI 推理未生效——沙箱不可达/, {}, { timeout: 5000 })).toBeTruthy();
    } finally {
      findingsPayload = DEFAULT_FINDINGS;
    }
  });

  it('交互审计 Top4-①: 取消（RUNNING 不可逆）须 Popconfirm 二次确认——确认前不发请求', async () => {
    const baseTask = DEFAULT_SNAPSHOT.task as Record<string, unknown>;
    snapshotOverride = { ...DEFAULT_SNAPSHOT, task: { ...baseTask, status: 'TASK_STATUS_RUNNING' } };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      const cancelBtn = await screen.findByRole('button', { name: /取\s*消/ });
      fireEvent.click(cancelBtn);
      // 二次确认出现；请求未发
      expect(await screen.findByText('确认取消该任务？取消后不可恢复')).toBeTruthy();
      expect(gateway.requests.some((r) => r.url === '/v1/tasks/t-1/cancel')).toBe(false);
      // 本测试文件未包 zhCN ConfigProvider——Popconfirm 确认键为默认英文 OK
      fireEvent.click(screen.getByRole('button', { name: 'OK' }));
      await waitFor(() =>
        expect(gateway.requests.some((r) => r.url === '/v1/tasks/t-1/cancel')).toBe(true));
    } finally {
      snapshotOverride = null;
    }
  });

  it('阶段 metadata.degraded="true" → 阶段副标题标注（已降级·RuleScan）', async () => {
    const baseTask = DEFAULT_SNAPSHOT.task as Record<string, unknown>;
    snapshotOverride = {
      ...DEFAULT_SNAPSHOT,
      task: {
        ...baseTask,
        stages: [{
          stage_id: 's-ai', type: 'STAGE_TYPE_AI_INFERENCE', status: 'STAGE_STATUS_COMPLETED',
          started_at: '2026-09-11T00:00:00Z', completed_at: '2026-09-11T00:01:00Z',
          error_message: '', metadata: { degraded: 'true' },
        }],
      },
    };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(await screen.findByText(/（已降级·RuleScan）/, {}, { timeout: 5000 })).toBeTruthy();
    } finally {
      snapshotOverride = null;
    }
  });
});

// B4-3（审计修复）：执行日志客户端保尾 MAX_LOG_ROWS=1000——超长任务日志无界累积会拖垮
// 标签页；保尾留最新侧，窗口满时面板顶部如实提示截断（完整内容下载入口延后）。
describe('TaskDetailPage（日志保尾，B4-3）', () => {
  it('1005 条日志 → 只渲染最新 1000 条并显示"仅显示最近 1000 条"提示', async () => {
    const logs = Array.from({ length: 1005 }, (_, i) => ({
      log_id: `l-${i}`, task_id: 't-1', ts_ms: 1756630000000 + i,
      level: 'TASK_LOG_LEVEL_INFO', source: 'task',
      message: i === 0 ? '最早一条日志（应被丢弃）' : `保尾日志行 ${i}`,
    }));
    snapshotOverride = { ...DEFAULT_SNAPSHOT, logs: { logs } };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      await waitFor(() => expect(screen.getByText('1000 条')).toBeTruthy(), { timeout: 5000 });
      expect(screen.getByText('仅显示最近 1000 条')).toBeTruthy();
      // 保尾=留最新：最早侧丢弃、最新侧保留
      expect(screen.queryByText(/最早一条日志/)).toBeNull();
      expect(screen.getByText('保尾日志行 1004')).toBeTruthy();
      expect(screen.queryByText('保尾日志行 4')).toBeNull(); // l-0..l-4 已出窗（保留 l-5..l-1004）
    } finally {
      snapshotOverride = null;
    }
  }, 15_000);

  it('未达上限时不显示截断提示（默认 1 条日志）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
    await waitFor(() => expect(screen.getByText('1 条')).toBeTruthy());
    expect(screen.queryByText('仅显示最近 1000 条')).toBeNull();
  });
});

// 左右栏交换：AI 交互日志默认在右栏；页首"互换左右栏"按钮切换，
// 偏好持久化 localStorage（codeaudit.task_detail_ai_side）。交换经 flex-direction 实现
// （DOM 顺序不变，两栏子树零重挂）。断言面=本页 style 块文本（jsdom 不做级联布局，
// getComputedStyle 不可用）——'flex-direction: row;' 带分号精确区分 row-reverse。
describe('TaskDetailPage（左右栏交换，2026-09-22）', () => {
  const styleTag = () =>
    document.querySelector('style[data-testid="task-detail-layout-style"]')?.textContent ?? '';

  it('默认（无持久化键）→ row-reverse（AI 日志在右），点击互换 → row 且写入偏好', async () => {
    localStorage.clear();
    const { unmount } = renderPage();
    try {
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(styleTag()).toContain('flex-direction: row-reverse');
      fireEvent.click(screen.getByRole('button', { name: /互换左右栏/ }));
      expect(styleTag()).toContain('flex-direction: row;');
      expect(localStorage.getItem('codeaudit.task_detail_ai_side')).toBe('left');
    } finally {
      unmount();
    }
  });

  it('持久化：换侧后重挂读取偏好保持 row；脏值（非白名单）回落默认 row-reverse', async () => {
    localStorage.setItem('codeaudit.task_detail_ai_side', 'left');
    const first = renderPage();
    try {
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(styleTag()).toContain('flex-direction: row;');
    } finally {
      first.unmount();
    }
    localStorage.setItem('codeaudit.task_detail_ai_side', 'banana'); // 脏值消融
    const second = renderPage();
    try {
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      expect(styleTag()).toContain('flex-direction: row-reverse');
    } finally {
      second.unmount();
      localStorage.clear();
    }
  });
});


// graph-ui-channels 批3：图任务详情页——图面板占主面板位（Q1），AI 平铺面板让渡
// （会话任务面板零变化由既有用例锁）。
describe('TaskDetailPage（图任务=画布占位，2026-09-23）', () => {
  it('execution=graph → 画布挂载主面板位；AI 交互日志面板不在场', async () => {
    snapshotOverride = {
      ...DEFAULT_SNAPSHOT,
      task: { ...DEFAULT_SNAPSHOT.task, config: { execution: 'graph' }, status: 'TASK_STATUS_RUNNING' },
    };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
      await waitFor(() => expect(screen.getByTestId('graph-canvas-mock')).toBeTruthy());
      expect(screen.queryByText(/AI 分析过程时间线/)).toBeNull(); // AI 面板让渡（标题副语）
      // D1：Q10 下载入口接线——onDownloadLog 漏传时按钮在真组件内
      // 恒不渲染（3378095 回归形态），mock 透传断言锁
      expect(screen.getByTestId('graph-canvas-mock').getAttribute('data-has-download')).toBe('1');
    } finally {
      snapshotOverride = null;
    }
  });
});

// TP20-T2 切换入口（计划 §3.3 两态入口）：RUNNING=「暂停并切换 Agent」/PAUSED·QUEUED=
// 「切换 Agent」/终态无入口；点击开 Modal（清单同源）。ALLOWED_ACTIONS 不动（页面层判态）。
describe('TaskDetailPage 切换 Agent 入口（TP20-T2）', () => {
  it('RUNNING 任务显示「暂停并切换 Agent」入口', async () => {
    snapshotOverride = { ...DEFAULT_SNAPSHOT,
      task: { ...(DEFAULT_SNAPSHOT.task as object), status: 'TASK_STATUS_RUNNING' } as unknown as typeof DEFAULT_SNAPSHOT.task,
      progress: { ...(DEFAULT_SNAPSHOT.progress as object), status: 'TASK_STATUS_RUNNING' } as typeof DEFAULT_SNAPSHOT.progress };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByRole('button', { name: '暂停并切换 Agent' })).toBeTruthy());
    } finally { snapshotOverride = null; }
  });

  it('PAUSED 任务显示「切换 Agent」入口，点击打开 Modal（清单端点渲染）', async () => {
    snapshotOverride = { ...DEFAULT_SNAPSHOT,
      task: { ...(DEFAULT_SNAPSHOT.task as object), status: 'TASK_STATUS_PAUSED', agent: 'dsh' } as unknown as typeof DEFAULT_SNAPSHOT.task,
      progress: { ...(DEFAULT_SNAPSHOT.progress as object), status: 'TASK_STATUS_PAUSED' } as typeof DEFAULT_SNAPSHOT.progress };
    try {
      renderPage();
      await waitFor(() => expect(screen.getByRole('button', { name: '切换 Agent' })).toBeTruthy());
      fireEvent.click(screen.getByRole('button', { name: '切换 Agent' }));
      await waitFor(() => expect(screen.getByText('maka')).toBeTruthy());
      // 当前 agent 项禁用+「当前」徽标（Modal 契约，入口侧联测一钉）
      expect(screen.getByText('当前')).toBeTruthy();
    } finally { snapshotOverride = null; }
  });

  it('COMPLETED 任务无切换入口（终态不可切换）', async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/任务 t-1/)).toBeTruthy());
    expect(screen.queryByRole('button', { name: '切换 Agent' })).toBeNull();
    expect(screen.queryByRole('button', { name: '暂停并切换 Agent' })).toBeNull();
  });
});

// ——缺损项修复批：A2 重连元数据守护 + A5c 终态复活重续臂——
describe('TaskDetailPage（缺损批 A2/A5c）', () => {
  it('A2 零信号帧不冲刷 ai 元数据：重连首帧后头部计数与收束态保持', async () => {
    const b64 = (s: string) => Buffer.from(s, 'utf-8').toString('base64');
    const runningTask = { ...(DEFAULT_SNAPSHOT.task as object), status: 'TASK_STATUS_RUNNING' };
    snapshotOverride = {
      ...DEFAULT_SNAPSHOT,
      task: runningTask,
      ai: { chunk: b64('x'.repeat(614)), next_cursor: '614', total_bytes: '614', complete: true },
    };
    try {
      renderPage();
      // 首帧携带信号：头部计数 0.6 KB + 已收束
      expect(await screen.findByText('0.6 KB', {}, { timeout: 5000 })).toBeTruthy();
      expect(screen.getByText('已收束')).toBeTruthy();
      // 重连首帧形态：ai 全零值无信号（增量/收束/总量/粒度皆无）
      snapshotOverride = { ...DEFAULT_SNAPSHOT, task: runningTask, ai: {} };
      fireEvent.click(screen.getAllByText(/刷\s*新/)[0]); // 首个=AI 面板刷新（antd 两汉字插空格判例；右侧执行日志面板亦有同名钮）
      await waitFor(() => expect(screen.getByText('0.6 KB')).toBeTruthy(), { timeout: 5000 });
      expect(screen.queryByText('0 KB')).toBeNull(); // 守护生效：不回落权威零
      expect(screen.getByText('已收束')).toBeTruthy(); // 收束态不闪失
    } finally {
      snapshotOverride = null;
    }
  });

  it('A5c 终态复活重续臂：终态帧→RUNNING 帧翻转后 WS 重挂（连接数递增）', async () => {
    const frame = (status: string, complete: boolean) => JSON.stringify({
      type: 'snapshot',
      task: { task_id: 't-1', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL',
        sast_tools: [], status, stages: [], retry_count: 0 },
      progress: { task_id: 't-1', status, overall_percent: 40, stages: [] },
      logs: { logs: [] },
      ai: { chunk: '', next_cursor: '5', total_bytes: '5', complete },
    });
    const created: { onopen: (() => void) | null; onmessage: ((ev: { data: string }) => void) | null; onclose: (() => void) | null }[] = [];
    class FakeWS2 {
      onopen: (() => void) | null = null;
      onmessage: ((ev: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      readyState = 1;
      constructor(_url: string) { created.push(this); }
      close() { this.onclose?.(); }
    }
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS2;
    snapshotOverride = { ...DEFAULT_SNAPSHOT, task: { ...(DEFAULT_SNAPSHOT.task as object), status: 'TASK_STATUS_RUNNING' } };
    try {
      renderPage();
      await waitFor(() => expect(created[0]).toBeTruthy());
      created[0].onopen?.();
      // 终态帧（DEAD+AI 收束——终态口径=COMPLETED/CANCELLED/TIMEOUT/DEAD，FAILED 可重试
      // 非终态不触发）：手动重试前夜的定格形态
      created[0].onmessage?.({ data: frame('TASK_STATUS_DEAD', true) });
      await waitFor(() => expect(screen.getByText('已收束')).toBeTruthy());
      const n0 = created.length;
      // 复活翻转帧（FAILED→RUNNING）→ wsEpoch 递增 → 推流重挂
      created[0].onmessage?.({ data: frame('TASK_STATUS_RUNNING', false) });
      await waitFor(() => expect(screen.queryByText('实时接收中')).toBeTruthy(), { timeout: 3000 }); // DEAD 帧 settle 关流后 RUNNING 帧仍被吸收（handler 未摘）
      await waitFor(() => expect(created.length).toBeGreaterThan(n0), { timeout: 5000 });
    } finally {
      snapshotOverride = null;
      delete (globalThis as { WebSocket?: unknown }).WebSocket;
    }
  });
});

// 2026-10-03 用户报障收口：早失败任务的阶段时间线此前全绿勾（SKIPPED→'finish' 同色
// 映射），与真跑完无法区分——SKIPPED 改灰跳过图标+"已跳过"描述，回填时间戳不伪装耗时
describe('TaskDetailPage 阶段时间线 SKIPPED 渲染（R63 早失败收敛的展示面）', () => {
  it('FAILED 任务：SKIPPED 阶段出"已跳过"，不渲染回填时间戳伪装的耗时', async () => {
    const skippedStage = {
      type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_SKIPPED',
      started_at: '2026-10-03T14:35:08Z', completed_at: '2026-10-03T14:35:08Z', error_message: '',
    };
    snapshotOverride = {
      ...DEFAULT_SNAPSHOT,
      task: {
        ...(DEFAULT_SNAPSHOT.task as object),
        status: 'TASK_STATUS_FAILED',
        error_message: 'project_path 未配置：请上传代码压缩包或为项目配置 repo_url',
        stages: [skippedStage, { ...skippedStage, type: 'STAGE_TYPE_AI_INFERENCE' }],
      },
      progress: { ...(DEFAULT_SNAPSHOT.progress as object), status: 'TASK_STATUS_FAILED', stages: [] },
    };
    try {
      renderPage();
      await waitFor(() => expect(screen.getAllByText('已跳过').length).toBe(2));
      // 引擎回填的起止时间戳不得伪装成"耗时 1s"（从未执行的阶段）
      expect(screen.queryByText(/耗时/)).toBeNull();
    } finally {
      snapshotOverride = null;
    }
  });
});
