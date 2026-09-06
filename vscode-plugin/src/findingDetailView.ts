// 漏洞详情视图渲染（纯函数，可单测）：侧栏 codeaudit 容器内与"任务进度"互斥的
// webview 视图。扫描完成后点击漏洞，任务进度（已完成，无增量价值）切换为漏洞详情。
// 内容：严重级徽章 + 标题 + 元信息（位置/CWE/工具/规则/AI 结论）+ 描述 + AI 修复建议
// + 机器补丁 + 操作按钮（打开位置 / AI 修复 / 回滚修复）。
// 安全口径：全部字段 escapeHtml 后进 DOM，绝不 innerHTML 原文拼接。
// 操作按钮经 postMessage({type:'action',...}) 回传插件侧执行对应命令。
import type { UnifiedFinding } from './types';

export interface FindingDetailData {
  /** null = 尚未选择漏洞：渲染空态 */
  finding: UnifiedFinding | null;
  /** 该发现的修复已应用（树上的 ✔ 已修复状态） */
  fixed: boolean;
}

export function escapeHtml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

const SEVERITY_LABEL: Record<string, string> = {
  SEVERITY_CRITICAL: '严重',
  SEVERITY_HIGH: '高危',
  SEVERITY_MEDIUM: '中危',
  SEVERITY_LOW: '低危',
  SEVERITY_INFO: '提示',
};

const VERDICT_LABEL: Record<string, string> = {
  AI_VERDICT_LIKELY_TRUE: 'AI 判定：大概率真实',
  AI_VERDICT_TRUE: 'AI 判定：真实',
  AI_VERDICT_LIKELY_FALSE: 'AI 判定：大概率误报',
  AI_VERDICT_FALSE: 'AI 判定：误报',
  AI_VERDICT_UNSPECIFIED: '',
};

function metaRow(label: string, value: string): string {
  if (!value) return '';
  return `<tr><th>${escapeHtml(label)}</th><td>${escapeHtml(value)}</td></tr>`;
}

function section(title: string, body: string): string {
  if (!body.trim()) return '';
  return `<h2>${escapeHtml(title)}</h2><div class="sec">${body}</div>`;
}

/** 推流动作消息：页内按钮 → postMessage → 插件侧执行对应命令 */
export interface FindingDetailAction {
  type: 'action';
  action: 'openLocation' | 'fix' | 'rollback';
}

export function renderFindingDetailHtml(data: FindingDetailData): string {
  const f = data.finding;
  const head = !f
    ? `<div class="empty">在「扫描结果」中点击任意漏洞，此处显示该漏洞的完整详情。</div>`
    : (() => {
        const sev = SEVERITY_LABEL[f.severity] ?? f.severity;
        const verdict = VERDICT_LABEL[f.ai_verdict] ?? '';
        const conf = f.ai_confidence > 0 ? `（置信度 ${(f.ai_confidence * 100).toFixed(0)}%）` : '';
        const loc = f.location;
        const actionBtn = data.fixed
          ? `<button id="rollback" class="act warn">回滚此修复</button>`
          : `<button id="fix" class="act primary">AI 修复此漏洞</button>`;
        return `<div class="head">
    <span class="sev s-${escapeHtml(f.severity)}">${escapeHtml(sev)}</span>
    <span class="t">${escapeHtml(f.title || f.cwe_id || '未命名发现')}</span>
    ${data.fixed ? '<span class="badge fixed">✔ 已修复（可回滚）</span>' : ''}
  </div>
  <table class="meta">
    ${loc ? metaRow('位置', `${loc.file_path}:${loc.start_line ?? '?'}${loc.end_line ? '-' + loc.end_line : ''}`) : ''}
    ${metaRow('CWE', f.cwe_id)}
    ${metaRow('工具', f.source_tool)}
    ${metaRow('规则', f.source_rule_id)}
    ${verdict ? metaRow('AI 结论', verdict + conf) : ''}
  </table>
  <div class="actions">${actionBtn}<button id="open" class="act">打开代码位置</button></div>
  ${section('描述', `<p>${escapeHtml(f.description || '（无描述）')}</p>`)}
  ${section('AI 分析', `<p>${escapeHtml(f.ai_reasoning || '')}</p>`)}
  ${section('修复建议', `<p>${escapeHtml(f.ai_fix_suggestion || '（平台未产出修复建议）')}</p>`)}
  ${f.diff_patch ? section('修复补丁（apply_patch）', `<pre>${escapeHtml(f.diff_patch)}</pre>`) : ''}`;
      })();
  return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'">
<style>
  :root { color-scheme: dark light; }
  body {
    font-family: var(--vscode-editor-font-family, monospace);
    font-size: var(--vscode-editor-font-size, 13px);
    color: var(--vscode-foreground);
    background: var(--vscode-sideBar-background);
    margin: 0; padding: 10px 12px 24px;
  }
  .empty { color: var(--vscode-descriptionForeground); padding: 8px 2px; }
  .head { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; margin: 4px 0 8px; }
  .head .t { font-weight: bold; font-size: 1.05em; }
  .sev { font-size: 11px; padding: 1px 8px; border-radius: 8px; border: 1px solid var(--vscode-panel-border, #444); flex: none; }
  .sev.s-SEVERITY_CRITICAL, .sev.s-SEVERITY_HIGH { color: var(--vscode-charts-red, #f14c4c); border-color: var(--vscode-charts-red, #f14c4c); }
  .sev.s-SEVERITY_MEDIUM { color: var(--vscode-charts-yellow, #cca700); border-color: var(--vscode-charts-yellow, #cca700); }
  .sev.s-SEVERITY_LOW, .sev.s-SEVERITY_INFO { color: var(--vscode-charts-blue, #4fc1ff); border-color: var(--vscode-charts-blue, #4fc1ff); }
  .badge { font-size: 11px; padding: 1px 8px; border-radius: 8px; color: var(--vscode-charts-green, #89d185); border: 1px solid var(--vscode-charts-green, #89d185); }
  table.meta { border-collapse: collapse; width: 100%; margin: 6px 0; }
  /* 值按词边界折行（路径/工具名不再被逐字符断成碎片）；超长无空格 token 才任意断 */
  table.meta th, table.meta td { text-align: left; vertical-align: top; padding: 2px 6px 2px 0; font-weight: normal; overflow-wrap: break-word; word-break: normal; }
  table.meta th { color: var(--vscode-descriptionForeground); white-space: nowrap; width: 5em; }
  .actions { display: flex; gap: 8px; margin: 10px 0 4px; flex-wrap: wrap; }
  button.act {
    font-family: inherit; font-size: 12px; padding: 4px 12px; cursor: pointer;
    background: var(--vscode-button-secondaryBackground, #3a3d41); color: var(--vscode-button-secondaryForeground, #fff);
    border: none; border-radius: 2px;
  }
  button.act.primary { background: var(--vscode-button-background, #0e639c); color: var(--vscode-button-foreground, #fff); }
  button.act.warn { background: var(--vscode-button-background, #0e639c); color: var(--vscode-button-foreground, #fff); }
  h2 { font-size: 12px; font-weight: bold; margin: 14px 0 4px; color: var(--vscode-descriptionForeground); text-transform: uppercase; letter-spacing: .05em; }
  .sec p { margin: 0 0 6px; white-space: pre-wrap; word-break: break-word; }
  .sec pre {
    white-space: pre-wrap; word-break: break-word; margin: 0; padding: 8px 10px;
    background: var(--vscode-textCodeBlock-background, #1e1e1e); border: 1px solid var(--vscode-panel-border, #333); border-radius: 3px;
    max-height: 20em; overflow-y: auto;
  }
</style>
</head>
<body>${head}</body>
<script>
  (function () {
    var vscode = acquireVsCodeApi();
    function post(action) { vscode.postMessage({ type: 'action', action: action }); }
    var fix = document.getElementById('fix');
    if (fix) fix.addEventListener('click', function () { post('fix'); });
    var rb = document.getElementById('rollback');
    if (rb) rb.addEventListener('click', function () { post('rollback'); });
    var open = document.getElementById('open');
    if (open) open.addEventListener('click', function () { post('openLocation'); });
  })();
</script>
</html>`;
}
