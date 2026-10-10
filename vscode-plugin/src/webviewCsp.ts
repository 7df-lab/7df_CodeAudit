// webview CSP 构造（fix-plan-0911 §14 纵深加固）：script-src 从 'unsafe-inline' 收紧为
// nonce 形态——每次渲染生成随机 16 字节 hex nonce，页内 inline script 必须携带同值
// nonce 才能执行（VS Code webview 官方推荐模式）。视图内容虽已全量 escapeHtml，
// nonce 是第二道闸：即便未来漏出活体 <script> 也因无 nonce 被拒。
// style-src 'unsafe-inline' 保留（页内 <style> 标签样式，CSP 无 style nonce 机制）。
// 无 inline script 的页面用 NO_SCRIPT_CSP（script-src 'none'，更简单更硬）。
import { randomBytes } from 'crypto';

export interface WebviewScriptCsp {
  /** 随机 nonce（hex），页内 <script nonce="…"> 必须与 CSP 内同值 */
  nonce: string;
  /** CSP meta 的 content 值：script-src 'nonce-…' */
  content: string;
}

export function newWebviewScriptCsp(): WebviewScriptCsp {
  const nonce = randomBytes(16).toString('hex');
  return { nonce, content: `default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-${nonce}'` };
}

/** 无 inline script 页面（如空态页）的 CSP：script-src 'none'——脚本全灭，无需 nonce */
export const NO_SCRIPT_CSP = `default-src 'none'; style-src 'unsafe-inline'; script-src 'none'`;
