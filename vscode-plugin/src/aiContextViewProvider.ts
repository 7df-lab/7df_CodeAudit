// AI 交互上下文视图的 VS Code 胶水（WebviewView provider）：
// 初始整页渲染一次，流式增量经 postMessage 更新（绝不整页重载——WS 推流亚秒级
// 演进，重载会不断销毁滚动位置：滑不动也读不了历史）。内容渲染与消息构造全部
// 来自 aiContextView 纯函数，此处只做胶水。
//
// 本文件刻意不 import vscode：以结构化最小接口描述 WebviewView 的使用面，
// 使 enableScripts/postMessage 行为可在 Node 单测中直接驱动（回归锁）。
import { buildViewUpdate } from './aiContextView';
import type { ProgressState } from './progressModel';

export interface WebviewLike {
  html: string;
  options?: { enableScripts?: boolean };
  postMessage(msg: unknown): { then(cb: (ok: boolean) => void): void };
}

export interface WebviewViewLike {
  webview: WebviewLike;
  visible: boolean;
  show(preserveFocus?: boolean): unknown;
  onDidChangeVisibility(cb: () => void): unknown;
  onDidDispose(cb: () => void): unknown;
}

export class AiContextViewProvider {
  public view: WebviewViewLike | null = null;

  constructor(
    private readonly getState: () => ProgressState | null,
    private readonly render: (state: ProgressState | null) => string,
    /** 整页渲染后的回调（extension 侧重置增量游标，避免紧随其后的冗余推送） */
    private readonly onRendered?: () => void,
  ) {}

  resolveWebviewView(view: WebviewViewLike): void {
    this.view = view;
    // enableScripts 必须开：流式增量依赖页内脚本监听 postMessage（贴底跟随/滚动保持）。
    // 漏掉它时 webview 只渲染初始 HTML、脚本不执行——所有增量消息被静默丢弃，
    // 表现为"任务日志/AI 上下文永远停留在第一帧"（初始态恰好是"轮询回退/暂无日志"）。
    view.webview.options = { enableScripts: true };
    view.webview.html = this.render(this.getState());
    this.onRendered?.();
    // 可见性恢复：上下文未被回收时以增量消息刷新——保留滚动位置与"贴底才跟随"策略；
    // 上下文已被回收则 VS Code 会重新走 resolveWebviewView（整页渲染当前态）
    view.onDidChangeVisibility(() => {
      if (this.view?.visible) this.postUpdate();
    });
    view.onDidDispose(() => {
      this.view = null;
    });
  }

  /** 推流增量：页内脚本按贴底策略更新 DOM（上滑阅读历史时不拽回） */
  postUpdate(): void {
    const s = this.getState();
    if (s && this.view) {
      void this.view.webview.postMessage(buildViewUpdate(s));
    }
  }
}
