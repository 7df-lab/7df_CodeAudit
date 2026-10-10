// 服务端类型（14号 P4 展示即数据：字段=proto message 字段的 snake_case JSON）
export interface ScanTask {
  task_id: string;
  project_id: string;
  scan_mode: string;
  agent?: string; // 切片 7：任务级 Agent（空/默认=部署默认 dsh）
  config?: Record<string, string>; // 约定键暴露（execution=graph 图任务标记/graph_nodes_* 摘要，切片5+6 暴露批）
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
  logs?: TaskLogs;
  ai?: AIInteractionLog;
}

// 图节点 AI 交互通道（graph-ui-channels 批3）：键=nodeID，游标语义与主面同构。
export interface AILogChannel {
  node_id: string;
  chunk: string; // base64（proto bytes → protojson）
  next_cursor: string | number;
  complete: boolean;
  total_bytes: string | number;
  /** b4：到达粒度（protojson enum 名 AI_LOG_GRANULARITY_*；归一化消费见
   *  graphChannels.normalizeGranularity；UNSPECIFIED/缺失=回落 agent 判别过渡案） */
  granularity?: string;
}

// AI 交互日志（ADR-168 补遗②）：按 event type 人性化渲染的交互流；字节游标增量
export interface AIInteractionLog {
  chunk: string; // proto bytes → protojson base64 字符串
  next_cursor: string | number; // proto int64 → protojson 字符串
  complete: boolean;
  total_bytes: string | number;
  // graph-ui-channels 批3（2026-09-23）：图任务逐节点通道增量（非图任务为空）。
  channels?: AILogChannel[];
  /** b4：主面到达粒度（与通道同源——同条目同方言；UNSPECIFIED=盘回落/旧后端） */
  granularity?: string;
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
  // 判定两轴（2026-10-09 第一性原理重构）：direction=未判定/判真/判假；
  // tier=确定/可能（方向未判定时恒 UNSPECIFIED）。展示/筛选/导出一律经 dict/verdict 组合五态
  verdict_direction: string;
  verdict_tier: string;
  ai_confidence: number;
  ai_reasoning: string;
  ai_fix_suggestion: string;
  // R97 报障批（2026-09-18）：服务端 NormalizeDiffPatch 校验后的 apply_patch 修复补丁
  // ——空=未产出/失配丢弃；protojson string 字段原文（非 base64）
  diff_patch?: string;
  created_at?: string | null;
  updated_at?: string | null; // ADR-152: 复核时间（此前恒空，页面无从显示变动）
  // proto L67 source_raw（原始输出 JSON 序列化；protojson bytes→base64）——ADR-141 复核上下文
  source_raw?: string;
  location?: { file_path: string; start_line: number; end_line?: number } | null;
  dedup_group: string;
  matched_findings: string[];
  is_unique: boolean;
  // 增量扫描继承标记（ADR-225）：空=本任务实扫产出；非空=从该基线任务复制的继承项
  //（连带 verdict/AI 建议终态；双视图"全部/新发现/继承"筛选的数据源）
  inherited_from_task_id?: string;
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
  format: string; // B5-P1-1: protojson 枚举名直出（如 REPORT_FORMAT_JSON）——网关 transcode 未开 UseEnumNumbers，数值形状是假契约
  url: string;
  generated_at: string | null;
}

// GET /v1/projects 列表响应（proto ListProjectsResponse；ADR-164 服务端游标翻页）
export interface ListProjectsResponse {
  projects: Project[];
  pagination?: PaginationResponse;
}

// ===== 推理 provider/路由管理（ADR-217）：结构 = engine gateway transcode protojson 直出 =====

// proto InferenceProviderInfo——凭据永不回流（服务端读路径按省略脱敏）
export interface InferenceProvider {
  name: string;
  type: string;
  config: Record<string, string>;
}

// proto InferenceRouteInfo——version uint64 → protojson 字符串
export interface InferenceRoute {
  provider: string;
  model: string;
  version: string;
}

export interface ValidatedEndpoint {
  url: string;
  protocol: string;
}

// proto SetInferenceRouteResponse——网关连通性验证回执逐层透传（manager→engine→前端）
export interface SetInferenceRouteResponse extends InferenceRoute {
  validation_performed: boolean;
  validated_endpoints: ValidatedEndpoint[];
}

export interface UpsertInferenceProviderResponse {
  name: string;
  created: boolean;
}

// ===== 图执行视图（platform-graph-web-exposure §1 P1a，2026-09-20）=====
// proto TaskGraph / TaskGraphNode / TaskGraphPlanTrigger（GET /v1/tasks/{id}/graph；
// graph.log 回放折叠投影，声明序=seq 因果序；域中立 C4）。

export interface TaskGraphNode {
  id: string;
  type: string; // planner | executor | verifier
  intent: string;
  declared_by: string;
  from: string[];
  status: string; // declared | running | completed | failed | rejected | indeterminate
  attempts: number;
  started_at: string | null;
  ended_at: string | null;
  result_ref: string;
  declared_seq?: number; // 声明事件 seq——计划批次归属键（v1 批次带分组）
  holder?: string; // v0.5 认领层：engine-worker | human（空=未认领）
  lease_until?: string; // v0.5：human claim 租约截止 RFC3339（空=无租约）
}

export interface TaskGraphPlanTrigger {
  trigger: string; // initial | new_evidence | open_drained
  detail: string;
  at: string | null;
  seq: number;
}

export interface TaskGraph {
  task_id: string;
  nodes: TaskGraphNode[];
  plan_triggers: TaskGraphPlanTrigger[];
  paused: boolean;
  pause_reason: string;
  converged: boolean;
  converge_summary: string;
  open_remaining: number;
  updated_at: string | null;
  open_node_ids?: string[]; // declared-only open 节点集（§1.1 paused 挂起横幅"未决节点 n"数据源）
}

// ===== 编排（负载 pack）生命周期（编排 CRUD 批 2026-09-21）=====
// proto OrchestrationInfo / OrchestrationRevision（GET/POST /v1/orchestrations、
// PUT/DELETE /v1/orchestrations/{name}；append-only 修订语义）

export interface OrchestrationRevision {
  revision: number;
  spec_hash: string;
  created_by: string;
  created_at: string | null;
}

export interface OrchestrationInfo {
  name: string;
  latest_revision: number;
  spec_yaml: string;
  spec_hash: string;
  execution: string; // session | graph
  description: string;
  deprecated: boolean;
  readonly: boolean; // 内置编排（configs/packs 装载）——不可改/停
  history?: OrchestrationRevision[];
  updated_at: string | null;
}

// ===== 任务计量（platform-graph-web-exposure §2.2 P0，2026-09-20）=====
// proto TaskModelUsage / TaskUsageSummary（GET /v1/tasks/{id}/usage；baton
// usage.recorded 事件经 engine usage_collector 归集 PG，ADR-247）。
// uint64 计数字段 protojson 序列化为字符串（与 ts_ms 同口径）。
// 计费输入 = input + cache_read + cache_write（baton Usage 同源口径）。

export interface TaskModelUsage {
  model: string;
  calls: string | number;
  input_tokens: string | number;
  output_tokens: string | number;
  cache_read_tokens?: string | number;
  cache_write_tokens?: string | number;
  reasoning_tokens?: string | number;
  total_tokens?: string | number;
}

export interface TaskUsageSummary {
  task_id: string;
  models: TaskModelUsage[];
  totals: TaskModelUsage;
}

// Agent 清单（TP20-T2 切换 Agent 收口；= proto AgentProviderInfo 的 snake_case
// protojson 形状）。capabilities 五布尔=provider 注册表（agent_providers.yaml）
// 如实声明，未声明=false 保守默认；is_default=首条（「默认」空值的语义锚）。
export interface AgentProviderInfo {
  id: string;
  dialect: string;
  is_default: boolean;
  tool_events: boolean;
  structured_submit: boolean;
  interaction: boolean;
  session_resume: boolean;
  conclude_semantics: boolean;
}

// V2.2 邀请码管理（ADR-255）。state 只回 active|revoked——「已用尽/已过期」为
// 派生态，消费端从数据现算（与后端 repo.InviteUsable 同口径）。
export interface InviteCode {
  id: string;
  code: string;
  max_uses: number;
  used_count: number;
  expires_at: string | null;
  state: string;
  note: string;
  created_by: string;
  created_at: string | null;
  last_used_by?: string;
  last_used_at?: string | null;
  role?: string; // ADR-257: 注册落地角色（ROLE_DEVELOPER|ROLE_VIEWER；旧码缺省 DEVELOPER）
}

export interface InviteCodesListResp {
  codes: InviteCode[];
  config_codes?: string[];
  pagination?: { next_cursor?: string; has_next?: boolean; total?: number };
}

export interface CreateInviteCodeBody {
  max_uses?: number;
  expires_at?: string;
  never_expires?: boolean;
  note?: string;
  role?: string; // ADR-257: 仅 ROLE_DEVELOPER|ROLE_VIEWER；缺省 DEVELOPER；ADMIN 服务端拒
}

export interface RegistrationPolicy {
  mode: 'invitation' | 'open' | 'disabled';
}
