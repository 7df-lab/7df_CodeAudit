// 任务跟踪：WS 秒级推送（ADR-172，token 走 query 参数）+ 断线回退 10s 快照轮询。
// 逻辑与 web/console TaskDetailPage 同构；轮询间隔对齐 console 断线兜底口径（10s/次）。
import { EventEmitter } from 'events';
import type { CodeAuditClient } from './apiClient';
import { isTerminalTaskStatus, type TaskSnapshot } from './types';

export const WS_BACKOFF_POLL_MS = 10_000;
export const WS_RECONNECT_MS = 5_000;

export interface TaskWatcherOpts {
  taskId: string;
  client: CodeAuditClient;
  wsUrl: (taskId: string, token: string) => string;
  getAccessToken: () => string;
  setWsLive?: (live: boolean) => void;
  /** WS 连接状态原始事件（诊断用）：open/close/error 的原始细节，error 常见为代理劫持/网络不可达 */
  onWsEvent?: (kind: 'open' | 'close' | 'error', detail: string) => void;
  /** 任务已在平台删除/归档（WS 1011 "not found" / 快照 404）：重连与轮询永远无意义，终止并交上层清理恢复态 */
  onTaskGone?: (detail: string) => void;
  /** 轮询回退的增量游标（logs/ai 已吸收位置；WS 路径不走此钩子——wsUrl 自带游标重连续订） */
  cursors?: () => { logsAfter?: string; aiCursor?: number | string };
  // 注入定时器/时钟以便单测
  setTimeoutFn?: (fn: () => void, ms: number) => unknown;
  clearTimeoutFn?: (t: unknown) => void;
  makeSocket?: (url: string) => WebSocketLike;
  now?: () => number;
}

export interface WebSocketLike {
  close(): void;
  onopen?: () => void;
  onclose?: (ev?: { code?: number; reason?: string }) => void;
  onerror?: (ev?: { message?: string; error?: { message?: string } }) => void;
  onmessage?: (ev: { data: string }) => void;
}

export class TaskWatcher extends EventEmitter {
  private closed = false;
  private timers = new Set<unknown>();
  private socket: WebSocketLike | null = null;
  // 重连防抖：真实 WebSocket 连接失败按标准序列先 onerror 后 onclose——两者都会调
  // onDown()，不加防抖时每代失败调度 2 个重连定时器 → 下一代 2 条并行 socket、
  // 4 个定时器，指数增长（网关持续不可达时耗尽扩展宿主资源）。同一时刻至多一个挂起重连。
  private reconnectPending = false;

  constructor(private opts: TaskWatcherOpts) {
    super();
  }

  start(): void {
    this.connectWs();
    void this.pollOnce(); // 立即拉一次快照兜底（WS 建立前也有状态）
  }

  private later(fn: () => void, ms: number): void {
    const defaultSet = (f: () => void, m: number) => setTimeout(f, m);
    const t = (this.opts.setTimeoutFn ?? defaultSet)(fn, ms);
    this.timers.add(t as unknown as NodeJS.Timeout);
  }

  private settle(snap: TaskSnapshot): void {
    // closed 复查：close() 后仍在途/已缓冲的 WS 消息帧不得再 emit——防 terminal 双发
    // （上层收尾重跑=完成通知弹两次、findings 重复拉取）与迟到快照污染
    if (this.closed) return;
    this.emit('snapshot', snap);
    if (isTerminalTaskStatus(snap.task.status)) {
      this.emit('terminal', snap.task.status, snap);
      this.close();
    }
  }

  private connectWs(): void {
    if (this.closed || this.reconnectPending) return;
    // 防御性关闭上一条 socket（正常重连路径其已 closed；覆盖竞态残留）
    try {
      this.socket?.close();
    } catch {
      /* ignore */
    }
    const make = this.opts.makeSocket ?? ((url: string) => new WebSocket(url) as unknown as WebSocketLike);
    let ws: WebSocketLike;
    try {
      ws = make(this.opts.wsUrl(this.opts.taskId, this.opts.getAccessToken()));
    } catch {
      this.opts.setWsLive?.(false);
      return; // 无 WebSocket 环境 → 轮询兜底
    }
    this.socket = ws;
    ws.onmessage = (ev) => {
      try {
        this.settle(JSON.parse(ev.data) as TaskSnapshot);
      } catch {
        /* 非 JSON 帧忽略 */
      }
    };
    ws.onopen = () => {
      this.opts.onWsEvent?.('open', '');
      this.opts.setWsLive?.(true);
    };
    const onDown = () => {
      if (this.closed || this.reconnectPending) return;
      this.opts.setWsLive?.(false);
      this.reconnectPending = true;
      this.later(() => {
        this.reconnectPending = false;
        this.connectWs();
      }, WS_RECONNECT_MS); // 5s 重连，终态后 close() 阻止
    };
    ws.onclose = (ev) => {
      this.opts.onWsEvent?.('close', ev?.code !== undefined ? `code=${ev.code} reason=${ev.reason ?? ''}`.trim() : '');
      // 任务已被平台删除/归档：服务端握手后立即关闭（1011 "task … not found"），
      // 重连永远拿不到快照也等不到终态——终止而非 5s 死循环
      if (/not found/i.test(ev?.reason ?? '')) {
        // closed 复查：轮询 404 可能已先行收束（双通道竞态）——后到的 1011 不得二次 onTaskGone
        if (this.closed) return;
        this.close();
        this.opts.setWsLive?.(false);
        this.opts.onTaskGone?.(ev?.reason ?? 'task not found');
        return;
      }
      onDown();
    };
    ws.onerror = (ev) => {
      const err = ev?.error?.message ?? ev?.message ?? '';
      this.opts.onWsEvent?.('error', err);
      onDown();
    };
  }

  private async pollOnce(): Promise<void> {
    if (this.closed) return;
    // 429 退避：限流窗口内跳过本轮
    const now = this.opts.now ?? (() => Date.now());
    if (this.opts.client.rateLimitUntil > now()) {
      this.later(() => void this.pollOnce(), WS_BACKOFF_POLL_MS);
      return;
    }
    try {
      const snap = await this.opts.client.taskSnapshot(this.opts.taskId, this.opts.cursors?.());
      // await 后复查：请求在途期间 watcher 可能已被 close（终态自关 / watchTask
      // 换新任务替换 / bindTask 切绑收口）——关闭后到达的快照不得再进 settle
      if (this.closed) return;
      this.settle(snap);
      if (!this.closed) this.later(() => void this.pollOnce(), WS_BACKOFF_POLL_MS);
    } catch (e) {
      // 快照 404 "not found"：任务已被平台删除/归档，轮询无意义——终止（WS 路径 onclose 同口径）
      const msg = e instanceof Error ? e.message : String(e);
      if (/404/.test(msg) && /not found/i.test(msg)) {
        // 在途 404 复查：await 期间 watcher 已被关闭（最常见=换任务替换）时，
        // 这个 404 属于旧任务——不得触发 onTaskGone（上层会把新任务 progress 标 DEAD）
        if (this.closed) return;
        // 统一走 close() 收束（关 socket + 清定时器）：只置标志会留活连接与挂起重连
        // 定时器，且随后的迟到 WS 1011 close 帧会二次触发 onTaskGone（双通道竞态）
        this.close();
        this.opts.onTaskGone?.(msg);
        return;
      }
      if (!this.closed) this.later(() => void this.pollOnce(), WS_BACKOFF_POLL_MS);
    }
  }

  close(): void {
    this.closed = true;
    const clear = (t: unknown) => (this.opts.clearTimeoutFn ?? ((x: unknown) => clearTimeout(x as NodeJS.Timeout)))(t);
    for (const t of this.timers) clear(t);
    this.timers.clear();
    try {
      this.socket?.close();
    } catch {
      /* ignore */
    }
  }
}
