import * as assert from 'assert';
import { AiContextViewProvider, type WebviewViewLike } from '../src/aiContextViewProvider';
import type { ProgressState } from '../src/progressModel';

function fakeView(): { view: WebviewViewLike; posted: unknown[] } {
  const posted: unknown[] = [];
  const view: WebviewViewLike = {
    webview: {
      html: '',
      options: undefined,
      postMessage: (msg: unknown) => { posted.push(msg); return { then: () => undefined }; },
    },
    visible: true,
    show: () => undefined,
    onDidChangeVisibility: () => undefined,
    onDidDispose: () => undefined,
  };
  return { view, posted };
}

function state(): ProgressState {
  return {
    taskId: 'gw-demo', status: 'TASK_STATUS_RUNNING', percent: 40, stages: [],
    logs: [], lastLogId: '', aiText: '正文', aiCursor: 100, aiComplete: false,
    aiTotalBytes: 100, wsLive: true, updatedAt: 0, version: 3,
  };
}

describe('AiContextViewProvider', () => {
  it('resolveWebviewView 必须开 enableScripts——漏掉则页内脚本不执行、增量消息全部静默丢弃（回归锁）', () => {
    let s: ProgressState | null = state();
    const p = new AiContextViewProvider(() => s, (x) => `<html>${x ? 'on' : 'off'}</html>`);
    const { view } = fakeView();
    p.resolveWebviewView(view);
    assert.deepStrictEqual(view.webview.options, { enableScripts: true });
    assert.ok(view.webview.html.includes('on'), '初始整页渲染当前态');
  });

  it('postUpdate 发送 type=update 增量消息；无状态/已 dispose 时静默跳过', () => {
    let s: ProgressState | null = state();
    const p = new AiContextViewProvider(() => s, () => '<html></html>');
    const { view, posted } = fakeView();
    p.resolveWebviewView(view);
    p.postUpdate();
    assert.strictEqual(posted.length, 1);
    assert.strictEqual((posted[0] as { type: string }).type, 'update');
    assert.ok((posted[0] as { aiHtml: string }).aiHtml.includes('正文'));

    s = null; // 空态：不发送
    p.postUpdate();
    assert.strictEqual(posted.length, 1);
  });
});
