// 枚举中文映射（14号 §3.5 / §8 i18n 口径：纯展示翻译，值域=proto 枚举，不自造）
// AIVerdict 依据: proto AIVerdict 六值枚举
export const AI_VERDICT: Record<string, string> = {
  AI_VERDICT_UNSPECIFIED: '未判定',
  AI_VERDICT_TRUE_POSITIVE: '确认为真',
  AI_VERDICT_LIKELY_TRUE: '可能为真',
  AI_VERDICT_FALSE_POSITIVE: '误报',
  AI_VERDICT_LIKELY_FALSE: '可能误报',
  AI_VERDICT_NEEDS_MANUAL: '需人工复核',
  AI_VERDICT_UNCERTAIN: '不确定',
};

// ScanMode 依据: proto ScanMode / ADR-186 五模式矩阵（人类决策 2026-09-03）
// A=纯SAST（多工具并行→去重合并） / B=纯AI / C=SAST+AI并行融合（默认推荐） /
// D=AI增强SAST（扫描→同段去重→逐条沙箱验证→融合汇总） / E=SAST+AI并行对比（ADR-186 前称"模式D"）
// 旧两值已弃用：仅历史数据兼容展示，新建入口不再提供（键序即展示序，弃用项置尾）
export const SCAN_MODE: Record<string, string> = {
  SCAN_MODE_SAST_ONLY: '模式A 纯SAST',
  SCAN_MODE_AI_ONLY: '模式B 纯AI',
  SCAN_MODE_PARALLEL: '模式C SAST+AI融合（推荐）',
  SCAN_MODE_AI_ENHANCED_SAST: '模式D AI增强SAST',
  SCAN_MODE_COMPARE: '模式E SAST+AI对比',
  SCAN_MODE_TRADITIONAL_FIRST: '旧·SAST→AI增强',
  SCAN_MODE_SAST_REVIEW: '旧·SAST→AI审核',
};

// 已弃用扫描模式（ADR-182）：任务筛选等历史视图仍可显示，新建入口须过滤
export const DEPRECATED_SCAN_MODES = new Set(['SCAN_MODE_TRADITIONAL_FIRST', 'SCAN_MODE_SAST_REVIEW']);

// TaskStatus 依据: proto TaskStatus / 04 §1 状态机
export const TASK_STATUS: Record<string, string> = {
  TASK_STATUS_UNSPECIFIED: '未知',
  TASK_STATUS_CREATED: '已创建',
  TASK_STATUS_PENDING: '保留值', // 审批流废除（2026-09-01）后不再产生，仅历史数据兼容
  TASK_STATUS_QUEUED: '已排队',
  TASK_STATUS_RUNNING: '执行中',
  TASK_STATUS_COMPLETED: '已完成',
  TASK_STATUS_FAILED: '失败',
  TASK_STATUS_CANCELLED: '已取消',
  TASK_STATUS_TIMEOUT: '超时',
  TASK_STATUS_DEAD: '重试耗尽',
  TASK_STATUS_PAUSED: '已暂停',
};

// Severity 依据: proto Severity
export const SEVERITY: Record<string, string> = {
  SEVERITY_UNSPECIFIED: '未知',
  SEVERITY_CRITICAL: '严重',
  SEVERITY_HIGH: '高危',
  SEVERITY_MEDIUM: '中危',
  SEVERITY_LOW: '低危',
  SEVERITY_INFO: '提示',
};

export function zh(map: Record<string, string>, key: string | undefined | null): string {
  if (!key) return map.AI_VERDICT_UNSPECIFIED ?? '未知';
  return map[key] ?? key;
}

// StageStatus 依据: proto StageStatus
export const STAGE_STATUS: Record<string, string> = {
  STAGE_STATUS_UNSPECIFIED: '未知',
  STAGE_STATUS_PENDING: '等待',
  STAGE_STATUS_RUNNING: '执行中',
  STAGE_STATUS_COMPLETED: '完成',
  STAGE_STATUS_FAILED: '失败',
  STAGE_STATUS_SKIPPED: '跳过',
};

// StageType 依据: proto StageType
export const STAGE_TYPE: Record<string, string> = {
  STAGE_TYPE_UNSPECIFIED: '未指定',
  STAGE_TYPE_CODE_ANALYSIS: '代码分析',
  STAGE_TYPE_SAST_SCAN: 'SAST 扫描',
  STAGE_TYPE_AI_INFERENCE: 'AI 推理',
  STAGE_TYPE_RESULT_FUSION: '结果融合',
  STAGE_TYPE_REPORT_GENERATION: '报告生成',
  STAGE_TYPE_AI_REVIEW: 'AI 审核（旧模式D）',
};

// ReviewDepth 依据: proto ReviewConfig.ReviewDepth（经 config map 传递，R1）
export const REVIEW_DEPTH: Record<string, string> = {
  REVIEW_DEPTH_QUICK: '快速',
  REVIEW_DEPTH_STANDARD: '标准',
  REVIEW_DEPTH_THOROUGH: '彻底',
};

// Role 依据: proto Role 枚举（V2.1 ADR-205：admin/developer/viewer）
export const ROLE: Record<string, string> = {
  ROLE_UNSPECIFIED: '未指定',
  ROLE_ADMIN: '管理员',
  ROLE_DEVELOPER: '开发者',
  ROLE_VIEWER: '访客',
};

// UserState 依据: proto User.UserState 嵌套枚举
export const USER_STATE: Record<string, string> = {
  USER_STATE_UNSPECIFIED: '未知',
  USER_STATE_ACTIVE: '激活',
  USER_STATE_INACTIVE: '停用',
  USER_STATE_LOCKED: '锁定',
};

// ReportFormat 依据: proto ReportFormat 枚举（protojson 数值直出；0=历史未记录）
export const REPORT_FORMAT: Record<number, string> = { 1: 'PDF', 2: 'HTML', 3: 'JSON', 4: 'CSV' };

// 报告下载文件扩展名（下载文件名此前恒为 <id>.bin，用户拿到无法关联格式的文件）。
// 编排器当前只产 JSON（缺省）与 HTML；0/未知按 JSON 兜底。
export function reportFileExt(format: number | undefined): string {
  const ext: Record<number, string> = { 1: 'pdf', 2: 'html', 4: 'csv' };
  return ext[format ?? 0] ?? 'json';
}
