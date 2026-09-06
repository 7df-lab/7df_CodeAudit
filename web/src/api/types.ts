// 服务端类型（14号 P4 展示即数据：字段=proto message 字段的 snake_case JSON）
export interface ScanTask {
  task_id: string;
  project_id: string;
  scan_mode: string;
  sast_tools: string[];
  status: string;
  stages: TaskStage[];
  created_at: string | null;
  updated_at: string | null;
  error_message: string;
  retry_count: number;
}

export interface TaskStage {
  stage_id: string;
  type: string;
  status: string;
  started_at: string | null;
  completed_at: string | null;
  error_message: string;
  metadata: Record<string, string>;
}

export interface TaskProgress {
  task_id: string;
  status: string;
  overall_percent: number;
  stages: TaskStage[];
}

// 任务执行日志（ADR-167）：沙箱生命周期/降级链/挖掘统计的流水线事件
export interface TaskLogEntry {
  log_id: string;
  task_id: string;
  ts_ms: number | string; // proto int64 → protojson 序列化为字符串（ADR-167 实测）
  level: string; // TASK_LOG_LEVEL_INFO | TASK_LOG_LEVEL_WARN | TASK_LOG_LEVEL_ERROR
  source: string; // sandbox / dsh-agent / task / orchestrator
  message: string;
}

export interface TaskLogs {
  logs: TaskLogEntry[];
}

// 详情页聚合快照（ADR-170）：单口轮询驱动整页，替代 4 个独立轮询器
export interface TaskSnapshot {
  task: ScanTask;
  progress?: TaskProgress | null;
  logs?: { logs: TaskLogEntry[] };
  ai?: { chunk: string; next_cursor: string | number; complete: boolean; total_bytes: string | number };
}

// AI 交互日志（ADR-168 补遗②）：按 event type 人性化渲染的交互流；字节游标增量
export interface AIInteractionLog {
  chunk: string; // proto bytes → protojson base64 字符串
  next_cursor: string | number; // proto int64 → protojson 字符串
  complete: boolean;
  total_bytes: string | number;
}

export interface ToolInfo {
  tool_id: string;
  name: string;
  supported_languages: string[];
  output_format: string;
  valid: boolean;
  errors: string[];
}

export interface Project {
  project_id: string;
  name: string;
  repo_url: string;
  default_branch: string;
  default_scan_mode: string;
  created_at: string | null;
}

export interface PaginationResponse {
  next_cursor: string;
  has_next: boolean;
  total: number;
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
  severity: string;
  confidence: number;
  ai_verdict: string;
  ai_confidence: number;
  ai_reasoning: string;
  ai_fix_suggestion: string;
  updated_at?: string | null; // ADR-152: 复核时间（此前恒空，页面无从显示变动）
  // proto L67 source_raw（原始输出 JSON 序列化；protojson bytes→base64）——ADR-141 复核上下文
  source_raw?: string;
  location?: { file_path: string; start_line: number; end_line?: number } | null;
  dedup_group: string;
  matched_findings: string[];
  is_unique: boolean;
}

export interface ComparisonMetrics {
  total_unique: number;
  sast_precision: number;
  sast_recall: number;
  sast_f1: number;
  ai_precision: number;
  ai_recall: number;
  ai_f1: number;
}

export interface ComparisonSummary {
  sast_total: number;
  ai_total: number;
  both_found: number;
  sast_only: number;
  ai_only: number;
  disagreement: number;
  metrics: ComparisonMetrics;
}

// proto L1445 ComparisonReport
export interface ComparisonReport {
  report_id: string;
  summary: ComparisonSummary;
  venn_data_url: string; // 诚实留空（ADR-133）：后端不产真实 URL
}

// proto L1263 Report
export interface ReportRow {
  report_id: string;
  task_id: string;
  format: number;
  url: string;
  generated_at: string | null;
}

// GET /v1/projects 列表响应（proto ListProjectsResponse；ADR-164 服务端游标翻页）
export interface ListProjectsResponse {
  projects: Project[];
  pagination?: PaginationResponse;
}
