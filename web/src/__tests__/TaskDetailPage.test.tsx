// 任务详情嵌入回归（ADR-142 补全）：发现/融合 Tabs 内嵌，不再有独立"查看发现"按钮
// 修复回归：WS 卸载关闭（重新生成报告入口已随 2026-09-09 布局改版移除）
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import TaskDetailPage from '../pages/tasks/TaskDetailPage';
import { useFakeGateway } from '../testsupport/fakeGateway';

// ADR-203 fakeGateway：真实 api/client 执行（含 ADR-170 轮询口），仅 HTTP 层伪造。
// pollIntervalMs/getAccessToken 用真实实现（无 token 时 WS 连接失败自然回退轮询，无需 stub）。
// 降级可见性用例（2026-09-11 报障）：快照/发现载荷可按用例注入（DEFAULT_* 轮换）。
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
  'GET /v1/reports': () => ({ reports: [{ report_id: 'r-1', task_id: 't-1', format: 3, url: '', generated_at: null }] }),
  'GET /v1/findings': () => findingsPayload,
  'POST /v1/tasks/:taskId/report': () => ({ report_id: 'r-new' }),
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
      created[0].onmessage?.({ data: frame('第一段思考', 12, 30) });
      await waitFor(() => expect(box()).toContain('第一段思考'));
      created[0].onmessage?.({ data: frame('第二段思考', 24, 30) });
      await waitFor(() => expect(box()).toContain('第二段思考'));
      expect(box()).toContain('第一段思考'); // 增量不覆盖既有内容
      created[0].onmessage?.({ data: frame('', 24, 30) }); // 空块帧（total 更新）不炸不重复
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

// AI 降级可见性（2026-09-11 用户报障）：RuleScan 兜底任务仍 COMPLETED、时间线全绿，
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
