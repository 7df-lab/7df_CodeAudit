import * as assert from 'assert';
import { TaskWatcher, type WebSocketLike } from '../src/taskWatcher';
import type { CodeAuditClient } from '../src/apiClient';
import type { TaskSnapshot } from '../src/types';

// 手动定时器队列 + 可控假 socket，驱动 TaskWatcher 的 WS/轮询双通道
function snap(status: string): TaskSnapshot {
  return { task: { task_id: 't1', project_id: 'p', scan_mode: '', sast_tools: [], status, stages: [], error_message: '' } };
}

function makeWatcher(over: Partial<ConstructorParameters<typeof TaskWatcher>[0]> = {}): {
  watcher: TaskWatcher;
  fire: (fn: () => void, ms: number) => unknown;
  runDue: () => void;
  sockets: { ws: WebSocketLike; delivered: { open?: () => void; close?: (ev?: { code?: number; reason?: string }) => void; err?: (m: string) => void; msg?: (d: string) => void }[] };
} {
  const timers: { fn: () => void; ms: number }[] = [];
  const sockets: {
    ws: WebSocketLike;
    delivered: { open?: () => void; close?: (ev?: { code?: number; reason?: string }) => void; err?: (m: string) => void; msg?: (d: string) => void }[];
  } = { ws: null as never, delivered: [] };
  const fire = (fn: () => void) => { timers.push({ fn, ms: 0 }); return timers.length; };
  const runDue = () => {
    const due = timers.splice(0, timers.length);
    for (const t of due) t.fn();
  };
  const opts = {
    taskId: 't1',
    client: { taskSnapshot: async () => snap('TASK_STATUS_RUNNING'), rateLimitUntil: 0 } as unknown as CodeAuditClient,
    wsUrl: (id: string) => `ws://x/v1/tasks/${id}/ws`,
    getAccessToken: () => 'tok',
    setTimeoutFn: (fn: () => void, _ms: number) => fire(fn),
    clearTimeoutFn: () => undefined,
    makeSocket: () => {
      const ws: WebSocketLike = { close: () => undefined };
      sockets.delivered.push({
        open: () => ws.onopen?.(),
        close: (ev) => ws.onclose?.(ev),
        err: (m: string) => ws.onerror?.({ message: m }),
        msg: (d: string) => ws.onmessage?.({ data: d }),
      });
      sockets.ws = ws;
      return ws;
    },
    ...over,
  };
  return { watcher: new TaskWatcher(opts), fire: fire as never, runDue, sockets };
}

describe('TaskWatcher', () => {
  it('WS 帧驱动状态；终态帧触发 terminal 事件并关闭（不再重连）', async () => {
    const { watcher, runDue, sockets } = makeWatcher();
    const terminal: string[] = [];
    const snapshots: string[] = [];
    watcher.on('terminal', (s: string) => terminal.push(s));
    watcher.on('snapshot', (s: TaskSnapshot) => snapshots.push(s.task.status));
    watcher.start();
    assert.strictEqual(sockets.delivered.length, 1, 'start 建立一条 WS');
    await new Promise((r) => setTimeout(r, 0)); // 首次兜底轮询的微任务完成
    assert.strictEqual(snapshots.length, 1, '立即兜底轮询一次');
    sockets.delivered[0].open!();
    sockets.delivered[0].msg!(JSON.stringify(snap('TASK_STATUS_RUNNING')));
    sockets.delivered[0].msg!(JSON.stringify(snap('TASK_STATUS_COMPLETED')));
    assert.deepStrictEqual(terminal, ['TASK_STATUS_COMPLETED']);
    // 模拟后续时间流逝：close 已置位，runDue 不应建立新连接
    runDue();
    assert.strictEqual(sockets.delivered.length, 1);
  });

  it('WS 无环境（makeSocket 抛异常）→ 纯轮询兜底至终态', async () => {
    let polls = 0;
    const { watcher, runDue } = makeWatcher({
      makeSocket: () => {
        throw new Error('no WebSocket');
      },
      client: {
        rateLimitUntil: 0,
        taskSnapshot: async () => {
          polls++;
          return snap(polls >= 2 ? 'TASK_STATUS_DEAD' : 'TASK_STATUS_RUNNING');
        },
      } as unknown as CodeAuditClient,
    });
    const terminal: string[] = [];
    watcher.on('terminal', (s: string) => terminal.push(s));
    watcher.start(); // poll #1 RUNNING → 调度下一轮
    await new Promise((r) => setTimeout(r, 0)); // 首次轮询微任务完成并调度下一轮定时器
    runDue(); // poll #2 DEAD → 终态
    await new Promise((r) => setTimeout(r, 0)); // 第二轮轮询的微任务完成
    assert.deepStrictEqual(terminal, ['TASK_STATUS_DEAD']);
  });

  it('429 限流窗口内跳过本轮轮询', async () => {
    let polls = 0;
    const { watcher, runDue } = makeWatcher({
      makeSocket: () => { throw new Error('no WS'); },
      client: {
        rateLimitUntil: Date.now() + 60_000,
        taskSnapshot: async () => { polls++; return snap('TASK_STATUS_RUNNING'); },
      } as unknown as CodeAuditClient,
    });
    watcher.start();
    assert.strictEqual(polls, 0, '首轮即被限流窗口拦截');
    runDue();
    assert.strictEqual(polls, 0);
  });

  it('onWsEvent 透出 open/close/error 原始细节（诊断通道）', async () => {
    const events: string[] = [];
    const { watcher, runDue, sockets } = makeWatcher({
      onWsEvent: (kind, detail) => events.push(`${kind}:${detail}`),
    });
    watcher.start();
    sockets.delivered[0].open!();
    sockets.delivered[0].msg!(JSON.stringify(snap('TASK_STATUS_COMPLETED')));
    // 终态后 watcher 已 close：close/error 原始事件仍上报，但不再重连
    sockets.delivered[0].close!();
    sockets.delivered[0].err!('boom');
    runDue();
    assert.deepStrictEqual(events, ['open:', 'close:', 'error:boom']);
    assert.strictEqual(sockets.delivered.length, 1, '终态后不重连');
  });

  it('同一连接 error+close 双事件只调度一次重连（防抖回归锁：曾经每代翻倍指数增长）', async () => {
    const harness = makeWatcher();
    harness.watcher.start();
    assert.strictEqual(harness.sockets.delivered.length, 1);
    // 真实 WebSocket 失败序列：先 onerror 后 onclose（同一连接两个事件都调 onDown）
    harness.sockets.delivered[0].err!('ECONNREFUSED');
    harness.sockets.delivered[0].close!({ code: 1006, reason: '' });
    harness.runDue(); // 放行定时器：应只有 1 个重连定时器 → 只建 1 条新 socket
    assert.strictEqual(harness.sockets.delivered.length, 2, 'error+close 双事件不得建立两条并行 socket');
  });

  it('WS 关闭原因为 "task not found"（平台删除/归档）→ onTaskGone 且不再重连', async () => {    const gone: string[] = [];
    const harness = makeWatcher({ onTaskGone: (detail) => gone.push(detail) });
    harness.watcher.start();
    harness.sockets.delivered[0].open!();
    harness.sockets.delivered[0].close?.({ code: 1011, reason: 'task t1 not found' });
    harness.runDue();
    assert.strictEqual(gone.length, 1, 'onTaskGone 恰好一次');
    assert.match(gone[0], /not found/);
    assert.strictEqual(harness.sockets.delivered.length, 1, '任务已删除：不重连');
  });

  it('轮询快照 404 not found → onTaskGone 且终止轮询', async () => {
    let polls = 0;
    const gone: string[] = [];
    const { watcher, runDue } = makeWatcher({
      makeSocket: () => { throw new Error('no WS'); },
      onTaskGone: (detail) => gone.push(detail),
      client: {
        rateLimitUntil: 0,
        taskSnapshot: async () => {
          polls++;
          throw new Error('GET /v1/tasks/t1/snapshot -> 404: {"error":"NotFound: task t1 not found"}');
        },
      } as unknown as CodeAuditClient,
    });
    watcher.start();
    await new Promise((r) => setTimeout(r, 0)); // 首轮轮询微任务完成（404 → onTaskGone）
    runDue();
    await new Promise((r) => setTimeout(r, 0));
    assert.strictEqual(polls, 1, '404 后不再续轮询');
    assert.strictEqual(gone.length, 1);
    assert.match(gone[0], /404/);
  });
});
