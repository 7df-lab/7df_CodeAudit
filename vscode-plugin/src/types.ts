// 平台 DTO：字段 = proto message 字段的 snake_case JSON（与 web/console/src/api/types.ts 同源，
// 依据: proto/codeaudit_common.proto UnifiedFinding/ScanTask/TaskProgress）。

export interface LoginResponse {
  access_token: string;
  refresh_token: string;
  expires_in_s: number;
}

export interface Project {
  project_id: string;
  name: string;
  repo_url: string;
  default_branch: string;
  default_scan_mode: string;
  created_at: string | null;
}

export interface ToolInfo {
  tool_id: string;
  name: string;
  supported_languages: string[];
  valid: boolean;
}

export interface UploadResult {
  upload_id: string;
  /** 新存储桶方案（ADR-148/163）：上传原件 zip 的桶内路径，start 时平台自行解压 */
  file_path?: string;
  file_id?: string;
  size_bytes?: number;
  /** 旧平台：解压后目录（新方案不再返回） */
  dir?: string;
  files?: number;
}

export interface TaskStage {
  stage_id: string;
  type: string;
  status: string;
  error_message: string;
  started_at?: string | null; // protojson Timestamp ISO 串（proto3 optional 未设=null）
  completed_at?: string | null;
}

export interface ScanTask {
  task_id: string;
  project_id: string;
  scan_mode: string;
  sast_tools: string[];
  status: string;
  stages: TaskStage[];
  error_message: string;
}

/** 任务进度（proto TaskProgress；stages 比 ScanTask.stages 更实时，展示优先取此路） */
export interface TaskProgressFull {
  task_id: string;
  status: string;
  overall_percent: number;
  stages?: TaskStage[] | null;
}

/** 任务执行日志条目（proto TaskLogEntry，ADR-167；GUI 面板数据源） */
export interface TaskLogEntry {
  log_id: string;
  task_id: string;
  ts_ms: string | number; // protojson int64 → 十进制字符串；两种都容忍
  level: string; // TaskLogLevel 枚举名
  source: string; // sandbox / dsh-agent / task / orchestrator
  message: string;
}

/** AI 交互上下文增量（proto GetAIInteractionLogResponse；chunk 为 base64 的 utf-8 渲染文本） */
export interface AiLogChunk {
  chunk: string;
  next_cursor: string | number;
  complete: boolean;
  total_bytes: string | number;
}

export interface TaskSnapshot {
  task: ScanTask;
  progress?: TaskProgressFull | null;
  /** WS 帧（ADR-189 task/progress/logs/ai 四路同构）携带的增量日志与 AI 正文；轮询聚合口同键 */
  logs?: { logs?: TaskLogEntry[] | null } | null;
  ai?: AiLogChunk | null;
}

/** 任务列表项（GET /v1/tasks；历史任务绑定/切换用） */
export interface TaskSummary {
  task_id: string;
  project_id: string;
  scan_mode: string;
  sast_tools: string[];
  status: string;
  created_at: string | null;
  updated_at: string | null;
  error_message: string;
}

export interface PaginationResponse {
  next_cursor: string;
  has_next: boolean;
  total: number;
}

export interface LocationInfo {
  file_path: string;
  start_line: number;
  end_line?: number;
  start_column?: number;
  end_column?: number;
  function_name?: string;
}

export interface UnifiedFinding {
  finding_id: string;
  task_id: string;
  project_id: string;
  source_tool: string;
  source_rule_id: string;
  cwe_id: string;
  title: string;
  description: string;
  severity: string; // proto Severity 枚举名，如 SEVERITY_HIGH
  confidence: number;
  ai_verdict: string;
  ai_confidence: number;
  ai_reasoning: string;
  ai_fix_suggestion: string;
  // apply_patch 语法机器补丁（proto field 24，ADR-183 全链产出，服务端 NormalizeDiffPatch
  // 已按工作区逐字校验；文件自扫描后未被改动的前提下，校验过的补丁在插件引擎实测 fuzz=0，
  // 已漂移时按四级容错锚定并透明标注）。空串 = 服务端校验拒绝置空
  // 或 ADR-183 之前的旧任务；此时消费方降级走 ai_fix_suggestion 围栏提取。
  diff_patch: string;
  location?: LocationInfo | null;
  dedup_group: string;
  is_unique: boolean;
}

export interface FindingsPage {
  findings: UnifiedFinding[];
  pagination: PaginationResponse;
}

export const TERMINAL_STATUSES = [
  'TASK_STATUS_COMPLETED',
  'TASK_STATUS_CANCELLED',
  'TASK_STATUS_TIMEOUT',
  'TASK_STATUS_DEAD',
];

export function isTerminalTaskStatus(status: string): boolean {
  return TERMINAL_STATUSES.includes(status);
}
