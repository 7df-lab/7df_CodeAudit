// HTML 转义共用件（webview 渲染层）：aiContextView 与 findingDetailView 的
// 安全口径同源——模型/平台产出的正文一律经此转义后再进 DOM，绝不 innerHTML 原文拼接。
export function escapeHtml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
