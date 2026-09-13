// 证据面板色板（2026-09-13）：碳黑 #0D1117 是全站唯一暗底——执行日志/
// AI 交互时间线/代码查看器三块"证据面"共用一套语义色（此前三处各自持色板，代码查看器
// 还是另一套 #0b1021 暗底）。语义 6 色源自日志面板既成事实（GitHub Dark 系），正式化为
// token；亮色 UI 区不使用本色板（亮区语义色走 dict/tokens.ts 与 antd preset）。
export const EVIDENCE = {
  bg: '#0D1117',                     // 面板底
  border: '#30363D',                 // 边框/分隔
  text: '#C9D1D9',                   // 正文
  textMuted: '#8B949E',              // 弱化：时间戳/行号/元信息/空态
  link: '#58A6FF',                   // 链接/日志 source/链路定位行
  ai: '#A371F7',                     // AI/模型思考
  ok: '#7EE787',                     // 模型回复/输出
  system: '#39C5CF',                 // 系统/任务事件
  warn: '#D29922',                   // 日志 WARN 级
  accent: '#FFA657',                 // 子任务/交互强调
  error: '#F85149',                  // 错误
  matchBg: 'rgba(255,210,75,0.12)',  // 代码：漏洞所在行底
  matchText: '#FFD24B',              // 代码：漏洞所在行文字
  hopBg: 'rgba(88,166,255,0.10)',    // 代码：链路定位行底
} as const;
