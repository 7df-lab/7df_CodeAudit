// AI 交互上下文视图渲染（纯函数，可单测）：任务态头（状态/进度条/阶段徽标）+
// 任务日志独立滚动窗（上方）+ AI 正文主区（下方）。
// 流式更新架构：初始 HTML 只渲染一次，后续增量经 postMessage 更新 DOM——
// 绝不整页重载（WS 推流亚秒级演进，整页重载会不断销毁滚动位置：用户滑不动、
// 也读不了历史）。贴底策略由页内脚本决定：仅当该滚动区本就贴底（距底 <48px）
// 才跟随新内容贴底；用户已上滑则保持阅读位置不动。
// 不 import vscode——extension 层只负责把 HTML/postMessage 塞进 webview view。
// 安全口径：AI 正文与日志均产生于平台/模型输出，一律 escapeHtml 后再进 DOM，
// 绝不 innerHTML 原文拼接。
import type { ProgressState } from './progressModel';
import { fmtBytes, stageLabel, taskStatusLabel } from './progressModel';

const LOG_TAIL = 200; // 日志展示条数（全量在平台环形缓存，面板取尾即可）
const AI_RENDER_CAP = 256 * 1024; // webview 单次渲染正文上限（超限保尾，防巨帧卡顿）

export function escapeHtml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

const STAGE_CHIP_CLASS: Record<string, string> = {
  STAGE_STATUS_PENDING: 'pending',
  STAGE_STATUS_RUNNING: 'running',
  STAGE_STATUS_COMPLETED: 'done',
  STAGE_STATUS_FAILED: 'failed',
  STAGE_STATUS_SKIPPED: 'skipped',
};

const LOG_LEVEL_CLASS: Record<string, string> = {
  TASK_LOG_LEVEL_ERROR: 'err',
  TASK_LOG_LEVEL_WARN: 'warn',
};

function h1Html(state: ProgressState, title: string): string {
  const aiBodyLen = state.aiText.length > AI_RENDER_CAP ? state.aiText.slice(-AI_RENDER_CAP) : state.aiText;
  const hasAi = aiBodyLen.trim().length > 0;
  const percent = Math.max(0, Math.min(100, state.percent));
  const live = state.wsLive;
  return `<span class="t">${escapeHtml(title)}</span>
    <span class="badge">${escapeHtml(taskStatusLabel(state.status) || '—')} · ${percent}%</span>
    <span class="badge ${live && state.status !== 'TASK_STATUS_PAUSED' ? 'live' : ''}">${state.status === 'TASK_STATUS_PAUSED' ? '已暂停（缓冲排空后静止）' : live ? 'WS 实时推送' : '轮询回退'}</span>
    <span class="badge">${hasAi ? `${escapeHtml(fmtBytes(Math.max(state.aiCursor, state.aiTotalBytes)))}${state.aiComplete ? ' · 已收束' : state.status === 'TASK_STATUS_PAUSED' ? ' · 已暂停' : ' · 流式中'}` : '暂无 AI 内容'}</span>`;
}

function chipsHtml(state: ProgressState): string {
  return state.stages
    .map((s) => {
      const cls = STAGE_CHIP_CLASS[s.status] ?? 'pending';
      return `<span class="chip ${cls}">${escapeHtml(stageLabel(s.type, s.stage_id))}</span>`;
    })
    .join('');
}

function logsInnerHtml(state: ProgressState): string {
  const tail = state.logs.slice(-LOG_TAIL);
  if (tail.length === 0) return '<div class="empty">暂无任务日志</div>';
  return tail
    .map((e) => {
      const t = new Date(Number(e.ts_ms) || 0);
      const hhmmss = Number.isFinite(t.getTime())
        ? t.toLocaleTimeString('zh-CN', { hour12: false })
        : String(e.ts_ms);
      const cls = LOG_LEVEL_CLASS[e.level] ?? '';
      return `<div class="log ${cls}"><span class="ts">${escapeHtml(hhmmss)}</span><span class="src">${escapeHtml(e.source || 'task')}</span><span class="msg">${escapeHtml(e.message)}</span></div>`;
    })
    .join('');
}

function aiBodyInnerHtml(state: ProgressState): string {
  const body = state.aiText.length > AI_RENDER_CAP ? state.aiText.slice(-AI_RENDER_CAP) : state.aiText;
  if (body.trim().length === 0) {
    return '<div class="empty">该任务尚无 AI 交互内容（纯 SAST 任务，或 AI 阶段未开始）。AI 阶段开始后此处实时显示 DSH 沙箱会话正文。</div>';
  }
  return `<pre id="ai">${escapeHtml(body)}</pre>`;
}

/** 推流增量消息（extension 经 webview.postMessage 发送；页内脚本按贴底策略更新 DOM） */
export interface AiViewUpdate {
  type: 'update';
  h1Html: string;
  percent: number;
  chipsHtml: string;
  logsHtml: string;
  aiHtml: string;
}

export function buildViewUpdate(state: ProgressState, title?: string): AiViewUpdate {
  return {
    type: 'update',
    h1Html: h1Html(state, title ?? `任务 ${state.taskId.slice(0, 8)}`),
    percent: Math.max(0, Math.min(100, state.percent)),
    chipsHtml: chipsHtml(state),
    logsHtml: logsInnerHtml(state),
    aiHtml: aiBodyInnerHtml(state),
  };
}

export interface AiContextViewData {
  /** null = 尚无任务：渲染空态页（底部面板视图常驻，首次打开前也需要内容） */
  state: ProgressState | null;
  /** 覆盖任务短标签（默认 任务 xxxxxxxx） */
  title?: string;
}

export function renderAiContextHtml(data: AiContextViewData): string {
  const { state } = data;
  if (!state) {
    return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'">
<style>
  body {
    font-family: var(--vscode-editor-font-family, monospace);
    font-size: var(--vscode-editor-font-size, 13px);
    color: var(--vscode-descriptionForeground);
    background: var(--vscode-editor-background);
    margin: 0; padding: 16px 20px;
  }
</style>
</head>
<body>暂无进行中或最近的任务——执行「CodeAudit: 扫描工作区」后，这里实时展示各阶段进度、DSH 沙箱会话正文与任务日志。</body>
</html>`;
  }
  const title = data.title ?? `任务 ${state.taskId.slice(0, 8)}`;
  const percent = Math.max(0, Math.min(100, state.percent));
  return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'">
<style>
  :root { color-scheme: dark light; }
  html, body { height: 100%; }
  body {
    font-family: var(--vscode-editor-font-family, monospace);
    font-size: var(--vscode-editor-font-size, 13px);
    color: var(--vscode-foreground);
    background: var(--vscode-editor-background);
    margin: 0;
    display: flex; flex-direction: column;
    overflow: hidden; /* 滚动都在各自分区内：AI 推流贴自己的底，不被任务日志挤动 */
  }
  header { flex: none; background: var(--vscode-editor-background); border-bottom: 1px solid var(--vscode-panel-border, #333); padding: 8px 12px; }
  .h1 { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
  .h1 .t { font-weight: bold; }
  .h1 .badge { font-size: 11px; padding: 1px 6px; border-radius: 8px; border: 1px solid var(--vscode-panel-border, #444); color: var(--vscode-descriptionForeground); }
  .h1 .badge.live { color: var(--vscode-charts-blue, #4fc1ff); }
  .bar { height: 4px; background: var(--vscode-inputValidation-infoBorder, #4fc1ff); margin: 8px 0 6px; }
  .bar > i { display: block; height: 100%; background: var(--vscode-charts-blue, #4fc1ff); transition: width .3s; }
  .chips { display: flex; gap: 6px; flex-wrap: wrap; }
  .chip { font-size: 11px; padding: 1px 8px; border-radius: 8px; border: 1px solid var(--vscode-panel-border, #444); color: var(--vscode-descriptionForeground); }
  .chip.running { color: var(--vscode-charts-blue, #4fc1ff); border-color: var(--vscode-charts-blue, #4fc1ff); }
  .chip.done { color: var(--vscode-charts-green, #89d185); border-color: var(--vscode-charts-green, #89d185); }
  .chip.failed { color: var(--vscode-charts-red, #f14c4c); border-color: var(--vscode-charts-red, #f14c4c); }
  .chip.skipped { text-decoration: line-through; opacity: .7; }
  /* 任务日志：上方独立窗口——固定高度自滚动，可拖边缘调高度（resize），可折叠让位给 AI 流 */
  details.logs-pane { flex: none; display: flex; flex-direction: column; min-height: 0; border-bottom: 1px solid var(--vscode-panel-border, #333); }
  details.logs-pane summary { cursor: pointer; user-select: none; padding: 6px 12px 4px; font-size: 12px; font-weight: bold; color: var(--vscode-descriptionForeground); text-transform: uppercase; letter-spacing: .05em; list-style: none; }
  details.logs-pane summary::before { content: '▸ '; }
  details.logs-pane[open] summary::before { content: '▾ '; }
  .logs { height: 28vh; min-height: 3em; resize: vertical; overflow-y: auto; padding: 0 12px 6px; }
  /* AI 交互上下文：下方主窗口，占满剩余空间；增量更新不整页重载——上滑阅读历史时
     滚动位置保持，仅原本贴底时才跟随新内容贴底（页内脚本策略） */
  section.ai-pane { flex: 1 1 auto; min-height: 0; overflow-y: auto; }
  section.ai-pane h2 { font-size: 12px; font-weight: bold; margin: 10px 12px 6px; color: var(--vscode-descriptionForeground); text-transform: uppercase; letter-spacing: .05em; }
  pre#ai { white-space: pre-wrap; word-break: break-word; margin: 0; padding: 0 12px 24px; }
  .log { display: flex; gap: 8px; padding: 1px 0; }
  .log .ts { color: var(--vscode-descriptionForeground); flex: none; }
  .log .src { color: var(--vscode-charts-purple, #c586c0); flex: none; min-width: 5.5em; }
  .log .msg { white-space: pre-wrap; word-break: break-word; }
  .log.err .msg { color: var(--vscode-charts-red, #f14c4c); }
  .log.warn .msg { color: var(--vscode-charts-yellow, #cca700); }
  .empty { padding: 2px 12px 8px; color: var(--vscode-descriptionForeground); }
</style>
</head>
<body>
<header>
  <div class="h1" id="h1">${h1Html(state, title)}</div>
  <div class="bar"><i id="barFill" style="width:${percent}%"></i></div>
  <div class="chips" id="chips">${chipsHtml(state)}</div>
</header>
<details class="logs-pane" open>
  <summary>任务日志（尾 ${LOG_TAIL} 条）</summary>
  <div class="logs" id="logsBox">${logsInnerHtml(state)}</div>
</details>
<section class="ai-pane" id="aiPane">
  <h2>AI 交互上下文</h2>
  <div id="aiBody">${aiBodyInnerHtml(state)}</div>
</section>
<script>
  (function () {
    var h1 = document.getElementById('h1');
    var barFill = document.getElementById('barFill');
    var chips = document.getElementById('chips');
    var logs = document.getElementById('logsBox');
    var aiPane = document.getElementById('aiPane');
    var aiBody = document.getElementById('aiBody');
    function stick() {
      if (aiPane) aiPane.scrollTop = aiPane.scrollHeight;
      if (logs) logs.scrollTop = logs.scrollHeight;
    }
    // 贴底（初始载入尾随视角）——多时机补偿：webview 视图的 html 赋值可能先于布局
    // 完成（show() 后立即赋值/面板展开中），载入瞬间 scrollHeight 尚未就绪，单次
    // 赋值会落空 → 停在第一行。rAF + 延时 + 字体就绪各补一次。
    stick();
    if (window.requestAnimationFrame) requestAnimationFrame(stick);
    setTimeout(stick, 50);
    setTimeout(stick, 250);
    if (document.fonts && document.fonts.ready && document.fonts.ready.then) {
      document.fonts.ready.then(stick);
    }
    var pane = document.querySelector('details.logs-pane');
    if (pane) pane.addEventListener('toggle', stick);
    // 流式增量：内容替换前记录各滚动区是否本就贴底——贴底才跟随（推流尾随），
    // 用户已上滑阅读历史则保持位置，绝不拽回
    function nearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < 48; }
    window.addEventListener('message', function (ev) {
      var m = ev.data;
      if (!m || m.type !== 'update') return;
      var aiPin = aiPane ? nearBottom(aiPane) : true;
      var logsPin = logs ? nearBottom(logs) : true;
      if (h1 && typeof m.h1Html === 'string') h1.innerHTML = m.h1Html;
      if (barFill && typeof m.percent === 'number') barFill.style.width = m.percent + '%';
      if (chips && typeof m.chipsHtml === 'string') chips.innerHTML = m.chipsHtml;
      if (logs && typeof m.logsHtml === 'string') {
        logs.innerHTML = m.logsHtml;
        if (logsPin) logs.scrollTop = logs.scrollHeight;
      }
      if (aiBody && typeof m.aiHtml === 'string') {
        aiBody.innerHTML = m.aiHtml;
        if (aiPin && aiPane) aiPane.scrollTop = aiPane.scrollHeight;
      }
    });
  })();
</script>
</body>
</html>`;
}
