// dev:mock 视觉走查网关（2026-09-13）：npm run dev:mock 启动——无后端
// 打开全站做设计走查。复用 mockAdapter（与测试台同一段匹配/回放逻辑，ADR-203 纪律：
// 真实 client 拦截器链全量执行、未建模路由响亮失败）。演示数据刻意覆盖设计敏感面：
// severity 五级全 ladder、taint 链路、AI 结论含 Source→Sink 引用、日志三级别、
// 全部任务状态代表值——供视觉验收对色板/等宽/状态语义做逐项核对。
// 注意：这是**设计走查台**，不是测试数据源（测试路由表仍在各测试文件内，场景化断言）。
import { api } from '../api/client';
import { buildGatewayAdapter, type RouteValue } from './mockAdapter';

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));

// ---- 演示数据 ----

const DEMO_USER = { user_id: 'u-demo', username: 'demo-admin', email: 'demo@codeaudit.local', role: 'ROLE_ADMIN', must_change_password: false, state: 'USER_STATE_ACTIVE' };

const DEMO_PROJECTS = [
  { project_id: 'gw-demo-pay', name: '支付网关', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_PARALLEL', created_at: '2026-09-10T09:00:00Z' },
  { project_id: 'gw-demo-web', name: '官网前台', repo_url: 'https://git.internal/web/home.git', default_branch: 'master', default_scan_mode: '', created_at: '2026-09-11T14:30:00Z' },
];

const DEMO_TASKS = [
  {
    task_id: 'gw-t00000a1f', project_id: 'gw-demo-pay', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep', 'semgrep'],
    status: 'TASK_STATUS_COMPLETED', error_message: '', retry_count: 0, created_at: '2026-09-12T10:00:00Z', updated_at: '2026-09-12T10:41:00Z',
    stages: [
      { stage_id: 's1', type: 'STAGE_TYPE_CODE_ANALYSIS', status: 'STAGE_STATUS_COMPLETED' },
      { stage_id: 's2', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_COMPLETED' },
      { stage_id: 's3', type: 'STAGE_TYPE_AI_INFERENCE', status: 'STAGE_STATUS_COMPLETED' },
      { stage_id: 's4', type: 'STAGE_TYPE_RESULT_FUSION', status: 'STAGE_STATUS_COMPLETED' },
      { stage_id: 's5', type: 'STAGE_TYPE_REPORT_GENERATION', status: 'STAGE_STATUS_COMPLETED' },
    ],
  },
  {
    task_id: 'gw-t00000b2e', project_id: 'gw-demo-pay', scan_mode: 'SCAN_MODE_SAST_ONLY', sast_tools: ['opengrep'],
    status: 'TASK_STATUS_RUNNING', error_message: '', retry_count: 0, created_at: '2026-09-13T09:12:00Z', updated_at: '2026-09-13T09:20:00Z',
    stages: [
      { stage_id: 's1', type: 'STAGE_TYPE_CODE_ANALYSIS', status: 'STAGE_STATUS_COMPLETED' },
      { stage_id: 's2', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_RUNNING' },
    ],
  },
  {
    task_id: 'gw-t00000c3d', project_id: 'gw-demo-web', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [],
    status: 'TASK_STATUS_FAILED', error_message: '推理 Provider 连接超时（3 次重试后放弃）', retry_count: 3, created_at: '2026-09-12T16:00:00Z', updated_at: '2026-09-12T16:12:00Z',
    stages: [
      { stage_id: 's1', type: 'STAGE_TYPE_AI_INFERENCE', status: 'STAGE_STATUS_FAILED' },
    ],
  },
];

const DEMO_LOGS = [
  { log_id: 'dL1', task_id: 'gw-t00000a1f', ts_ms: 1757155200000, level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '任务启动：模式C SAST+AI 融合（opengrep+semgrep 并行）' },
  { log_id: 'dL2', task_id: 'gw-t00000a1f', ts_ms: 1757155204000, level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '沙箱已就绪（dsh-runtime 镜像 v1.4.2）' },
  { log_id: 'dL3', task_id: 'gw-t00000a1f', ts_ms: 1757155230000, level: 'TASK_LOG_LEVEL_WARN', source: 'dsh-agent', message: 'AI 通道延迟偏高，降级链待命（阈值 8s）' },
  { log_id: 'dL4', task_id: 'gw-t00000a1f', ts_ms: 1757155400000, level: 'TASK_LOG_LEVEL_INFO', source: 'fusion', message: '融合完成：SAST 14 条 ∪ AI 9 条 → 去重合并 11 条' },
  { log_id: 'dL5', task_id: 'gw-t00000a1f', ts_ms: 1757155460000, level: 'TASK_LOG_LEVEL_ERROR', source: 'report', message: '报告模板变量缺失（已在本地兜底渲染）' },
];

//  对齐后端真实格式：标记行（💭/✍/📋）独立成行、正文另起行——
// 此前"标记+正文同行"会被 parseTimeline 的 startKind 整行消费（同行正文静默丢失，
// 实时态还渲染出带空正文的思考标题块）。会话头也对齐 bridge 真实帧文本。
const DEMO_AI_TEXT = [
  '══ DSH 会话开始（bridge）══',
  '── 第 1 轮开始 ──',
  '💭 [思考]',
  '用户代码中 SQL 拼接点集中在 dao 层；优先审计 OrderDao.findById 的参数流。',
  '✍ [输出]',
  '对 dao/OrderDao.java:49 的审计结论：orderNo 未参数化直拼 WHERE 子句，',
  '调用链为 web/OrderController.java:93（来源）→ dao/OrderDao.java:49-51（汇点），',
  '中间经 service/OrderService.java:120 透传，全程无净化器。判定：确认为真（高危）。',
  '📋 [任务下发]（512 字节）',
  '-- 对 web/UserController.java 进行同维度复查（关注反序列化入口）',
  '🤖 [子任务 sub-3] 启动',
  '🤖 [子任务 sub-3] 任务（1977 字节）',
  'UserController.java:210 存在 Jackson 多态反序列化默认开启（白盒证据见任务正文）。',
  '🤖 [子任务 sub-3] 回合结束',
  '✍ [输出]',
  '建议开启 default typing 白名单；该点判定：可能为真（中危，需人工复核）。',
  '── 回合结束: completed ──',
  '■ 会话空闲（收束）',
].join('\n');

// 演示源码（发现详情页代码上下文/Source→Sink 链路跳转都指向这份）
const DEMO_SOURCE = [
  'package com.demo.pay.dao;', '', 'import java.sql.*;', '',
  'public class OrderDao {', '  private final Connection conn;', '',
  '  public OrderDao(Connection conn) { this.conn = conn; }',
  '',
  '  // 汇点：orderNo 直拼 SQL，未走 PreparedStatement 占位符',
  '  public Order findByNo(String orderNo) throws SQLException {',
  '    String sql = "SELECT * FROM orders WHERE order_no = \'" + orderNo + "\'";',
  '    try (Statement st = conn.createStatement()) {',
  '      ResultSet rs = st.executeQuery(sql);',
  '      return rs.next() ? map(rs) : null;',
  '    }',
  '  }',
  '}',
].join('\n');

// severity 全 ladder + taint 链路 + AI 结论引用链
const DEMO_FINDINGS = [
  {
    finding_id: 'gw-f-sev-critical', task_id: 'gw-t00000a1f', title: 'SQL 注入：订单号直拼查询（source→sink 全链可达）',
    severity: 'SEVERITY_CRITICAL', cwe_id: 'CWE-89', source_tool: 'opengrep', source_rule_id: 'taint.java.sqli-order',
    confidence: 'HIGH', ai_verdict: 'AI_VERDICT_TRUE_POSITIVE', ai_confidence: 0.94,
    ai_reasoning: '[LLM:gpt-demo] dao/OrderDao.java:49 存在 SQL 拼接：来源 web/OrderController.java:93，经 service/OrderService.java:120 透传，汇点 dao/OrderDao.java:49-51，全程无净化——确认为真。',
    ai_fix_suggestion: '将 findByNo 改用 PreparedStatement 占位符（?）并校验 orderNo 格式（^\\w{1,32}$）。',
    description: '用户输入 orderNo 从 Controller 一路透传至 DAO 层直拼 SQL，无参数化、无白名单校验，可构造布尔盲注。',
    location: { file_path: '/app/data/repos/pay/src/main/java/com/demo/pay/dao/OrderDao.java', start_line: 14, end_line: 16 },
    source_raw: b64(JSON.stringify({
      code: 'String sql = "SELECT * FROM orders WHERE order_no = \'" + orderNo + "\'";', line: 14,
      context: { start_line: 8, end_line: 20, lines: DEMO_SOURCE.split('\n').slice(7, 21) },
      dataflow_trace: {
        taint_source: ['CliLoc', [{ path: '/app/data/repos/pay/src/main/java/com/demo/pay/web/OrderController.java', start: { line: 93 } }, 'String orderNo = req.getParameter("order_no");']],
        intermediate_vars: [{ content: 'orderNo', location: { path: '/app/data/repos/pay/src/main/java/com/demo/pay/service/OrderService.java', start: { line: 120 } } }],
        taint_sink: ['CliLoc', [{ path: '/app/data/repos/pay/src/main/java/com/demo/pay/dao/OrderDao.java', start: { line: 14 } }, 'st.executeQuery(sql)']],
      },
    })),
    created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T11:00:00Z',
  },
  {
    finding_id: 'gw-f-sev-high', task_id: 'gw-t00000a1f', title: '不安全的反序列化：Jackson default typing 开启',
    severity: 'SEVERITY_HIGH', cwe_id: 'CWE-502', source_tool: 'ai_agent', source_rule_id: '', confidence: '',
    ai_verdict: 'AI_VERDICT_LIKELY_TRUE', ai_confidence: 0.71,
    ai_reasoning: '[LLM:gpt-demo] web/UserController.java:210 的 ObjectMapper 开启了 activateDefaultTyping，多态反序列化面暴露——可能为真。',
    description: 'ObjectMapper 开启 default typing 且未限制白名单，可构造多态 gadget 链。',
    location: { file_path: '/app/data/repos/pay/src/main/java/com/demo/pay/web/UserController.java', start_line: 210, end_line: 210 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:40:00Z',
  },
  {
    finding_id: 'gw-f-sev-medium', task_id: 'gw-t00000a1f', title: '弱哈希：MD5 用于口令摘要',
    severity: 'SEVERITY_MEDIUM', cwe_id: 'CWE-328', source_tool: 'semgrep', source_rule_id: 'crypto.weak-hash-md5',
    confidence: 'CERTAIN', ai_verdict: 'AI_VERDICT_NEEDS_MANUAL', ai_confidence: 0.4,
    ai_reasoning: '[LLM:gpt-demo] util/HashUtil.java:18 口令摘要使用 MD5——若为遗留兼容场景可降级处理，需人工复核。',
    description: '口令摘要算法为 MD5，碰撞攻击成本已不可接受。',
    location: { file_path: '/app/data/repos/pay/src/main/java/com/demo/pay/util/HashUtil.java', start_line: 18, end_line: 18 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:41:00Z',
  },
  {
    finding_id: 'gw-f-sev-low', task_id: 'gw-t00000a1f', title: '日志中输出完整卡号（掩码缺失）',
    severity: 'SEVERITY_LOW', cwe_id: 'CWE-532', source_tool: 'opengrep', source_rule_id: 'sensitive.log-pan',
    confidence: 'HIGH', ai_verdict: 'AI_VERDICT_FALSE_POSITIVE', ai_confidence: 0.66,
    ai_reasoning: '[LLM:gpt-demo] 该日志点位于测试桩代码，生产路径不可达——误报。',
    description: '支付回调日志输出未掩码卡号。',
    location: { file_path: '/app/data/repos/pay/src/test/java/com/demo/pay/CallbackLogTest.java', start_line: 40, end_line: 40 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:42:00Z',
  },
  {
    finding_id: 'gw-f-sev-info', task_id: 'gw-t00000a1f', title: '依赖提示：log4j 2.17 以下版本存在已知 CVE',
    severity: 'SEVERITY_INFO', cwe_id: 'CWE-1104', source_tool: 'opengrep', source_rule_id: 'dep.log4j-version',
    confidence: '', ai_verdict: 'AI_VERDICT_UNCERTAIN', ai_confidence: 0, ai_reasoning: '',
    description: 'pom.xml 声明 log4j 2.14.1，建议升至 2.17+。',
    location: { file_path: '/app/data/repos/pay/pom.xml', start_line: 88, end_line: 88 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:40:00Z',
  },
];

const DEMO_NOTIFICATIONS = [
  { notification_id: 'n-1', user_id: 'u-demo', title: '任务已完成', body: '支付网关 · 模式C 融合扫描完成：发现 5 条（严重 1 / 高危 1）', read: false, created_at: '2026-09-12T10:41:00Z' },
  { notification_id: 'n-2', user_id: 'u-demo', title: 'AI 推理失败', body: '官网前台 · 纯AI 扫描失败：推理 Provider 连接超时', read: false, created_at: '2026-09-12T16:12:00Z' },
  { notification_id: 'n-3', user_id: 'u-demo', title: '报告已生成', body: '任务 gw-t00000a1f 的 JSON 报告已生成，可在线查看', read: true, created_at: '2026-09-12T10:42:00Z' },
];

const DEMO_REPORTS = [
  { report_id: 'rpt-demo-0001-json-format-aaaaaaaaaaaaaaaaaaaaaaaaaaaa', task_id: 'gw-t00000a1f', format: 'REPORT_FORMAT_JSON', url: '', generated_at: '2026-09-12T10:42:00Z' },
  { report_id: 'rpt-demo-0002-html-format-bbbbbbbbbbbbbbbbbbbbbbbbbbbb', task_id: 'gw-t00000a1f', format: 'REPORT_FORMAT_HTML', url: '', generated_at: '2026-09-12T10:45:00Z' },
];

const DEMO_USERS = [
  { user_id: 'u-demo', username: 'demo-admin', email: 'demo@codeaudit.local', role: 'ROLE_ADMIN', state: 'USER_STATE_ACTIVE', must_change_password: false, created_at: '2026-09-01T09:00:00Z' },
  { user_id: 'u-2', username: 'dev-wang', email: 'wang@codeaudit.local', role: 'ROLE_DEVELOPER', state: 'USER_STATE_ACTIVE', must_change_password: true, created_at: '2026-09-03T11:00:00Z' },
  { user_id: 'u-3', username: 'audit-li', email: 'li@codeaudit.local', role: 'ROLE_VIEWER', state: 'USER_STATE_INACTIVE', must_change_password: false, created_at: '2026-09-05T15:00:00Z' },
];

const DEMO_PROVIDERS = [
  { name: 'openai-compat-demo', kind: 'openai_compatible', base_url: 'https://llm.internal/v1', model: 'demo-pro', api_key_set: true, in_use: true, enabled: true },
  { name: 'anthropic-relay', kind: 'anthropic', base_url: 'https://relay.internal/anthropic', model: 'claude-demo', api_key_set: true, in_use: false, enabled: false },
];

const routes: Record<string, RouteValue> = {
  // 会话
  'GET /v1/users/me': DEMO_USER,
  'POST /v1/auth/login': { access_token: 'demo-access', refresh_token: 'demo-refresh' },
  'POST /v1/auth/logout': {},
  // 项目域
  'GET /v1/projects': () => ({ projects: DEMO_PROJECTS, pagination: { next_cursor: '', has_next: false, total: DEMO_PROJECTS.length } }),
  'GET /v1/projects/:projectId': ({ params }: { params: Record<string, string> }) =>
    DEMO_PROJECTS.find((p) => p.project_id === params.projectId) ?? {},
  'GET /v1/projects/:projectId/config': { config: { project_id: 'gw-demo-pay', upload_file_id: 'file-demo-pay', upload_file_name: 'pay-service-2026Q3.zip' } },
  // 任务域
  'GET /v1/tasks': () => ({ tasks: DEMO_TASKS, pagination: { next_cursor: '', has_next: false, total: DEMO_TASKS.length } }),
  'GET /v1/tasks/:taskId/snapshot': ({ params }: { params: Record<string, string> }) => {
    const t = DEMO_TASKS.find((x) => x.task_id === params.taskId) ?? DEMO_TASKS[0];
    return {
      task: t,
      progress: null,
      logs: { logs: DEMO_LOGS.filter((l) => l.task_id === t.task_id) },
      ai: { chunk: b64(DEMO_AI_TEXT), next_cursor: String(DEMO_AI_TEXT.length), complete: true, total_bytes: String(DEMO_AI_TEXT.length) },
    };
  },
  'GET /v1/tasks/:taskId/source-file': ({ query }: { query: URLSearchParams }) => ({
    path: query.get('path') ?? '', content: DEMO_SOURCE, total_lines: DEMO_SOURCE.split('\n').length,
    bytes: DEMO_SOURCE.length, root_via: 'demo', resolved_via: 'demo',
  }),
  'GET /v1/tasks/:taskId/comparison-report': {
    report_id: 'cr-demo',
    summary: {
      sast_total: 11, ai_total: 9,
      both_found: 7, sast_only: 4, ai_only: 2, disagreement: 1,
      metrics: { sast_precision: 0.777, sast_recall: 0.636, sast_f1: 0.7, ai_precision: 0.778, ai_recall: 0.875, ai_f1: 0.824 },
    },
    venn_data_url: '',
  },
  // 发现域
  'GET /v1/findings': () => ({ findings: DEMO_FINDINGS, pagination: { next_cursor: '', has_next: false, total: DEMO_FINDINGS.length } }),
  'GET /v1/findings/:findingId': ({ params }: { params: Record<string, string> }) =>
    ({ finding: DEMO_FINDINGS.find((f) => f.finding_id === params.findingId) ?? DEMO_FINDINGS[0] }),
  // 报告/通知/工具
  'GET /v1/reports': () => ({ reports: DEMO_REPORTS, pagination: { next_cursor: '', has_next: false, total: DEMO_REPORTS.length } }),
  'GET /v1/reports/:reportId/download': '{}',
  'GET /v1/notifications': { notifications: DEMO_NOTIFICATIONS },
  'GET /v1/tools': { tools: [{ id: 'opengrep', name: 'OpenGrep' }, { id: 'semgrep', name: 'Semgrep' }] },
  // 管理面
  'GET /v1/users': () => ({ users: DEMO_USERS, pagination: { next_cursor: '', has_next: false, total: DEMO_USERS.length } }),
  'GET /v1/inference/providers': { providers: DEMO_PROVIDERS },
  'GET /v1/inference/route': { default_provider: 'openai-compat-demo', task_routes: {} },
};

// dev:mock 安装（main.tsx 在 VITE_MOCK_GATEWAY 置位时调用）。不卸载——走查台全程有效。
// 两层拦截：① axios adapter（页面数据面）；② window.fetch——会话启动的 /v1/auth/refresh
// 刻意不走 api 实例（client.ts，防 401 递归刷新），不吃 adapter，须单独兜住，否则
// boot 失败全站弹回登录页。
export function installDemoGateway(): void {
  // 播种 refresh token：bootRefresh 无令牌时不发请求直接判未登录（client.ts requestRefresh
  // 前置检查），无此播种全站弹回登录页
  if (!localStorage.getItem('codeaudit.refresh_token')) {
    localStorage.setItem('codeaudit.refresh_token', 'demo-refresh');
  }
  (api.defaults as { adapter?: unknown }).adapter = buildGatewayAdapter(routes, []);
  const origFetch = window.fetch.bind(window);
  window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (url.includes('/v1/auth/refresh')) {
      return Promise.resolve(new Response(
        JSON.stringify({ access_token: 'demo-access', refresh_token: 'demo-refresh', expires_in_s: 1800 }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      ));
    }
    return origFetch(input, init);
  };
}
