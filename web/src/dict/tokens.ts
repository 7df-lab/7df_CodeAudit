// 语义配色 token（2026-09-13）：severity/status/verdict/stage 配色唯一事实源。
// 值域键与 dict/index.ts 同名（proto 枚举名，不自造）；缺键 → undefined → Tag 无色兜底。
// 层级语义：实心色（hex）= 缺陷本身的属性（severity 阶梯）；antd 语义 preset（浅底 pill）
// = 工作流状态（任务状态/AI 结论）。此前三处页面各持互斥映射（critical 弱于 high、详情页
// 恒红/恒蓝、verdict 红绿与全站"红=危险"撞义）——本文件收敛为单一来源。

// severity 六级色阶（实心）：绛红 > 警红 > 琥珀 > 秋黄；提示级钢灰不抢注意；未知无色。
// 修正：旧映射 CRITICAL='volcano' 视觉强度低于 HIGH='red'（倒置），且缺 SEVERITY_INFO。
export const SEVERITY_COLOR: Record<string, string> = {
  SEVERITY_CRITICAL: '#A8071A',
  SEVERITY_HIGH: '#FA541C',
  SEVERITY_MEDIUM: '#FA8C16',
  SEVERITY_LOW: '#D4B106',
  SEVERITY_INFO: '#595959',
};

// 任务状态（antd 语义 preset）：执行中=processing / 完成=success / 失败与重试耗尽=error /
// 超时与暂停=warning / 其余（已创建/已排队/已取消/保留值）default。
// 修正：TaskDetail 头部曾恒 blue（失败任务也蓝）、ProjectDetail 自持一份粒度不同的三元。
export const STATUS_COLOR: Record<string, string> = {
  TASK_STATUS_RUNNING: 'processing',
  TASK_STATUS_COMPLETED: 'success',
  TASK_STATUS_FAILED: 'error',
  TASK_STATUS_DEAD: 'error',
  TASK_STATUS_TIMEOUT: 'warning',
  TASK_STATUS_PAUSED: 'warning',
};

// AI 结论（verdict）：真漏洞=红系（与全站"红=危险"对齐），降噪成功（误报）=灰，
// 待定（可能为真/需人工复核）=橙。修正：旧 TRUE_POSITIVE=green 与 FALSE_POSITIVE=red
// 的红绿语义与全站相反（红曾表示"不是漏洞"）。
export const VERDICT_COLOR: Record<string, string> = {
  AI_VERDICT_TRUE_POSITIVE: 'error',
  AI_VERDICT_LIKELY_TRUE: 'warning',
  AI_VERDICT_NEEDS_MANUAL: 'warning',
  AI_VERDICT_FALSE_POSITIVE: 'default',
  AI_VERDICT_LIKELY_FALSE: 'default',
  AI_VERDICT_UNCERTAIN: 'default',
};

// 阶段进度圆点（实心 hex，非 Tag 体系）：完成绿 / 执行中=主色靛墨 / 失败红；
// 未列出状态由调用点灰点兜底
export const STAGE_DOT_COLOR: Record<string, string> = {
  STAGE_STATUS_COMPLETED: '#52c41a',
  STAGE_STATUS_RUNNING: '#3056D3',
  STAGE_STATUS_FAILED: '#ff4d4f',
};

// 证据等宽字体栈（零 webfont，系统栈兜底）：凡机器产生的标识（任务/报告短 ID、文件路径、
// 代码、日志、行号）一律等宽——"证据嗓音"。中文回落系统字体，仅 ASCII 走等宽。
export const MONO_FONT =
  '"JetBrains Mono", "SFMono-Regular", Menlo, Consolas, "Liberation Mono", monospace';
