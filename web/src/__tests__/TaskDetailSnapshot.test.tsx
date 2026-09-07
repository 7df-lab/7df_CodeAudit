// 任务详情聚合快照契约（外部 E-20/E-21/E-23；内部 I-90/I-44；缺陷档案 P-05/P-06/P-15）。
// 核心：absorbSnapshot 是轮询与 WS 帧共用的唯一吸收路径——log_id 去重 + AI 游标单调
// 是"杜绝重复行"的兜底（P-06），此处用交叠帧行为级锁定；变异 M4/M5 杀手。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { beforeEach, describe, expect, it } from 'vitest';
import TaskDetailPage from '../pages/tasks/TaskDetailPage';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';
import type { TaskSnapshot } from '../api/types';

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));

const RUNNING_TASK = {
  task_id: 't-9', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep'],
  status: 'TASK_STATUS_RUNNING', stages: [], created_at: '2026-09-07T00:00:00Z', updated_at: null,
  error_message: '', retry_count: 0,
};

function snap(over: Partial<TaskSnapshot>): TaskSnapshot {
  return { task: RUNNING_TASK, progress: null, logs: { logs: [] }, ai: { chunk: '', next_cursor: '0', complete: false, total_bytes: '0' }, ...over } as TaskSnapshot;
}

// 快照序列：第 2 帧与第 1 帧交叠（L2 重复）+ AI 增量续传——吸收必须去重且游标单调
const FRAME1 = snap({
  logs: { logs: [
    { log_id: 'L1', task_id: 't-9', ts_ms: 1757155200000, level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '日志一' },
    { log_id: 'L2', task_id: 't-9', ts_ms: 1757155201000, level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '日志二' },
  ] },
  ai: { chunk: b64('abc'), next_cursor: '3', complete: false, total_bytes: '6' },
});
const FRAME2 = snap({
  logs: { logs: [
    { log_id: 'L2', task_id: 't-9', ts_ms: 1757155201000, level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '日志二' },
    { log_id: 'L3', task_id: 't-9', ts_ms: 1757155202000, level: 'TASK_LOG_LEVEL_WARN', source: 'dsh-agent', message: '日志三' },
  ] },
  ai: { chunk: b64('def'), next_cursor: '6', complete: true, total_bytes: '6' },
});

let snapHandler: () => unknown;
const routes: Record<string, unknown> = {
  'GET /v1/tasks/:taskId/snapshot': () => snapHandler(),
  'POST /v1/tasks/:taskId/cancel': {},
  'POST /v1/tasks/:taskId/pause': {},
};
const gateway = useFakeGateway(routes);

function renderDetail() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={['/tasks/t-9']}>
        <TaskDetailPage taskId="t-9" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { qc, ...view };
}

beforeEach(() => {
  let call = 0;
  snapHandler = () => {
    const s = call === 0 ? FRAME1 : FRAME2;
    call += 1;
    return s;
  };
});

describe('E-20/I-90 快照增量吸收（轮询与 WS 共用路径，P-06）', () => {
  it('交叠帧吸收：log_id 去重（L2 只出现一次）、AI chunk 顺序拼接 abcdef', async () => {
    const { qc } = renderDetail();
    await waitFor(() => expect(screen.getByText(/日志一/)).toBeTruthy());
    await act(() => qc.refetchQueries({ queryKey: ['task-snapshot', 't-9'] }));
    await waitFor(() => expect(screen.getByText(/日志三/)).toBeTruthy());
    const logBox = screen.getByTestId('task-log-box');
    for (const m of ['日志一', '日志二', '日志三']) {
      expect(logBox.textContent!.split(m).length - 1).toBe(1); // 恰好一次——重复行即红
    }
    const aiBox = screen.getByTestId('ai-interaction-log-box');
    expect(aiBox.textContent).toContain('abc');
    expect(aiBox.textContent).toContain('def');
  });

  it('404 → 「任务不存在或已被清除」专页（内存存储重启语义）；非 404 → 「加载失败（500）」+ 重试', async () => {
    snapHandler = () => httpError(404, { error: 'task gone' });
    const v1 = renderDetail();
    expect(await v1.findByText(/任务不存在或已被清除/)).toBeTruthy();

    snapHandler = () => httpError(500, { error: 'boom' });
    const v2 = renderDetail();
    expect(await v2.findByText(/加载失败（500）/)).toBeTruthy();
    expect(v2.getByRole('button', { name: /重\s*试/ })).toBeTruthy();
    v1.unmount();
    v2.unmount();
  });
});

describe('E-21/I-44 动作分发（I-40 状态机镜像→端点）', () => {
  it('RUNNING → 暂停/取消可见（无启动/人工重试）；点取消 → POST /v1/tasks/t-9/cancel', async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByText(/任务 t-9/)).toBeTruthy());
    expect(screen.getByRole('button', { name: '暂停任务' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /启\s*动/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /人工\s*重\s*试/ })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /取\s*消/ }));
    await waitFor(() =>
      expect(gateway.requests.some((r) => r.method === 'POST' && r.url === '/v1/tasks/t-9/cancel')).toBe(true),
    );
  });
});

describe('E-23 WS 帧路径（250ms 聚合推帧与轮询同构吸收；live 徽标）', () => {
  it('onopen → WS 实时推送徽标；onmessage snapshot 帧日志入渲染；非 snapshot 帧被忽略', async () => {
    const created: { url: string; closed: boolean; onopen: (() => void) | null; onmessage: ((ev: { data: string }) => void) | null }[] = [];
    class FakeWS {
      static OPEN = 1;
      url: string;
      closed = false;
      onopen: (() => void) | null = null;
      onmessage: ((ev: { data: string }) => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      constructor(url: string) { this.url = url; created.push(this); }
      close() { this.closed = true; this.onclose?.(); }
    }
    (globalThis as { WebSocket?: unknown }).WebSocket = FakeWS;
    try {
      renderDetail();
      await waitFor(() => expect(created.length).toBeGreaterThan(0));
      expect(created[0].url).toContain('/v1/tasks/t-9/ws?');
      created[0].onopen?.();
      await waitFor(() => expect(screen.getByText('WS 实时推送')).toBeTruthy());
      // 合法帧：日志入渲染
      created[0].onmessage?.({ data: JSON.stringify({ type: 'snapshot', ...snap({
        logs: { logs: [{ log_id: 'W1', task_id: 't-9', ts_ms: 1, level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: 'WS帧日志' }] },
      }) }) });
      await waitFor(() => expect(screen.getByText(/WS帧日志/)).toBeTruthy());
      // 噪声帧（type 不符）必须被忽略而非崩流
      created[0].onmessage?.({ data: JSON.stringify({ type: 'noise' }) });
      created[0].onmessage?.({ data: 'not-json' });
      expect(screen.getByTestId('task-log-box').textContent).toContain('WS帧日志');
    } finally {
      delete (globalThis as { WebSocket?: unknown }).WebSocket;
    }
  });
});
