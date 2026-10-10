// 语义配色 token：severity/status/verdict/stage 配色唯一事实源。
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

// 判定 Tag 色义已并入 dict/verdict VERDICT_STATE_COLOR（2026-10-09 两轴重构：
// 真漏洞=红系、降噪=灰、待定=橙——色义不变，键改五态）

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

// ===== 图执行视图=====
// 此前 TaskGraphCard 自持三份硬编码（2026-09-20 code-review 硬违规：绕过本文件
// 事实源地位）。语义沿用全站规则：节点状态=实心 hex（缺陷/事实属性面）；节点类型
// =antd preset 词（工作流角色面，Tag 体系）。

// 节点状态点+左边框（实心 hex）：未决灰 / 运行中蓝（脉冲同色系）/ 完成绿 /
// 失败红（与全站"红=危险"对齐）/ 拒绝灰（删除线语义）/ 结果不明琥珀。
export const GRAPH_NODE_STATUS_COLOR: Record<string, string> = {
  declared: '#9e9e9e',
  running: '#1f6feb',
  completed: '#1a7f37',
  failed: '#cf222e',
  rejected: '#8b949e',
  indeterminate: '#d4a72c',
};

// 节点状态文案（全称口径）：G6View 节点标签 / GraphNodePopup 头部 Tag /
// TaskGraphCanvas 列表共用（2026-09-22 收敛自三份本地漂移副本——G6View 曾用缩略
// "已拒/不明"，统一为全称）。缺键由调用点回退原始 status 串。
export const GRAPH_NODE_STATUS_TEXT: Record<string, string> = {
  declared: '未决',
  running: '运行中',
  completed: '完成',
  failed: '失败',
  rejected: '已拒绝',
  indeterminate: '结果不明',
};

// 节点类型徽标（antd preset 词）：planner 紫 / executor 蓝 / verifier 青。
export const GRAPH_NODE_TYPE_COLOR: Record<string, string> = {
  planner: 'purple',
  executor: 'blue',
  verifier: 'cyan',
};

// 计划批次带（紫系——计划/编排语义色，与节点状态色正交）：底/左边框/文字。
export const GRAPH_PLAN_BAND = {
  bg: '#f3ecff',
  border: '#6b4fd8',
  text: '#6b4fd8',
} as const;
