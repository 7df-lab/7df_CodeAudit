// dev:mock 视觉走查网关：npm run dev:mock 启动——无后端
// 打开全站做设计走查。复用 mockAdapter（与测试台同一段匹配/回放逻辑，ADR-203 纪律：
// 真实 client 拦截器链全量执行、未建模路由响亮失败）。演示数据刻意覆盖设计敏感面：
// severity 五级全 ladder、taint 链路、AI 结论含 Source→Sink 引用、日志三级别、
// 全部任务状态代表值——供视觉验收对色板/等宽/状态语义做逐项核对。
// 注意：这是**设计走查台**，不是测试数据源（测试路由表仍在各测试文件内，场景化断言）。
import { api } from '../api/client';
import { buildGatewayAdapter, httpError, type RouteValue } from './mockAdapter';

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));

// ---- 演示数据 ----

const DEMO_USER = { user_id: 'u-demo', username: 'demo-admin', email: 'demo@codeaudit.local', role: 'ROLE_ADMIN', must_change_password: false, state: 'USER_STATE_ACTIVE' };

const DEMO_PROJECTS = [
  { project_id: 'gw-demo-pay', name: '支付网关', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_PARALLEL', created_at: '2026-09-10T09:00:00Z' },
  { project_id: 'gw-demo-web', name: '官网前台', repo_url: 'https://git.internal/web/home.git', default_branch: 'master', default_scan_mode: '', created_at: '2026-09-11T14:30:00Z' },
  // 2026-10-04 源码补配走查态：向导就地创建的无来源项目（无 repo_url 无 config）——
  // 详情页"配置源码"入口/建任务警告/向导警告的走查锚（用户报障场景）
  { project_id: 'gw-demo-bare', name: '裸项目（就地创建）', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_PARALLEL', created_at: '2026-10-04T09:00:00Z' },
  // 2026-10-08 报障走查态：API/e2e 路径上传的无名档（有 upload_file_id 无名）——
  // 列表"压缩包（名称未记录）"/详情短文案的走查锚
  { project_id: 'gw-demo-unnamed', name: '批量建档（无名上传）', repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_SAST_ONLY', created_at: '2026-09-20T10:00:00Z' },
];

const DEMO_TASKS = [
  {
    task_id: 'gw-t00000a1f', project_id: 'gw-demo-pay', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: ['opengrep', 'semgrep'],
    config: { execution: 'graph' }, // graph-ui-channels 批3 走查标记：图任务=画布占主面板位
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
    status: 'TASK_STATUS_FAILED', error_message: 'project_path 未配置：请上传代码压缩包或为项目配置 repo_url（ADR-148/ADR-163）', retry_count: 0, created_at: '2026-09-12T16:00:00Z', updated_at: '2026-09-12T16:12:00Z',
    // P-34 走查态：早失败任务的 R63 收敛形态——首阶段 FAILED，余阶段 SKIPPED+回填
    // 时间戳（真实引擎行为）；渲染面应出灰跳过图标+"已跳过"，不得伪装绿勾耗时
    stages: [
      { stage_id: 's1', type: 'STAGE_TYPE_CODE_ANALYSIS', status: 'STAGE_STATUS_FAILED', started_at: '2026-09-12T16:00:02Z', completed_at: '2026-09-12T16:00:03Z', error_message: 'project_path 未配置' },
      { stage_id: 's2', type: 'STAGE_TYPE_SAST_SCAN', status: 'STAGE_STATUS_SKIPPED', started_at: '2026-09-12T16:00:03Z', completed_at: '2026-09-12T16:00:03Z', error_message: '' },
      { stage_id: 's3', type: 'STAGE_TYPE_AI_INFERENCE', status: 'STAGE_STATUS_SKIPPED', started_at: '2026-09-12T16:00:03Z', completed_at: '2026-09-12T16:00:03Z', error_message: '' },
      { stage_id: 's4', type: 'STAGE_TYPE_RESULT_FUSION', status: 'STAGE_STATUS_SKIPPED', started_at: '2026-09-12T16:00:03Z', completed_at: '2026-09-12T16:00:03Z', error_message: '' },
      { stage_id: 's5', type: 'STAGE_TYPE_REPORT_GENERATION', status: 'STAGE_STATUS_SKIPPED', started_at: '2026-09-12T16:00:03Z', completed_at: '2026-09-12T16:00:03Z', error_message: '' },
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

// 2026-09-14 审计 P3 对齐后端真实格式：标记行（💭/✍/📋）独立成行、正文另起行——
// 此前"标记+正文同行"会被 parseTimeline 的 startKind 整行消费（同行正文静默丢失，
// 实时态还渲染出带空正文的思考标题块）。会话头也对齐 bridge 真实帧文本。
// 图任务演示文本（graph-ui-channels 批3 走查）：带 g-* 骨架行+`# 图节点任务`标记——
// 旧任务回落切段（segmentFlatByMarkers）的走查面；形状对齐 gw-3e85 真实样本。
const DEMO_GRAPH_AI_TEXT = [
  '══ DSH 会话开始（bridge）══',
  '🤖 [子任务 g-n001] 启动',
  '🤖 [子任务 g-n001] 任务（557 字节）',
  '# 图节点任务（n001 · executor）',
  '',
  '## 任务目标',
  '完成本 pack 声明的负载目标',
  '',
  '## 本节点意图',
  '初扫注入面：SQL 注入与硬编码凭据',
  '',
  '── 第 1 轮开始 ──',
  '💭 [思考]',
  'SQL 拼接点集中在 dao 层；优先审计 OrderDao.findById 的参数流。',
  '✍ [输出]',
  '对 dao/OrderDao.java:49 的审计结论：orderNo 未参数化直拼 WHERE 子句，判定确认为真（高危）。',
  '🤖 [子任务 g-n001] 回合结束',
  '🤖 [子任务 g-n002] 启动',
  '🤖 [子任务 g-n002] 任务（537 字节）',
  '# 图节点任务（n002 · verifier）',
  '',
  '## 本节点意图',
  '复核补丁：修复面回归与误报甄别',
  '',
  '── 第 1 轮开始 ──',
  '💭 [思考]',
  '补丁已参数化直拼；复核 ORDER 列表的二次注入与误报。',
  '✍ [输出]',
  '复核结论：修复有效，无回归；1 项误报已甄别剔除。',
  '🤖 [子任务 g-n002] 回合结束',
].join('\n')

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
    confidence: 'HIGH', verdict_direction: 'VERDICT_DIRECTION_TRUE', verdict_tier: 'VERDICT_TIER_DEFINITE', ai_confidence: 0.94,
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
    verdict_direction: 'VERDICT_DIRECTION_TRUE', verdict_tier: 'VERDICT_TIER_LIKELY', ai_confidence: 0.71,
    ai_reasoning: '[LLM:gpt-demo] web/UserController.java:210 的 ObjectMapper 开启了 activateDefaultTyping，多态反序列化面暴露——可能为真。',
    description: 'ObjectMapper 开启 default typing 且未限制白名单，可构造多态 gadget 链。',
    location: { file_path: '/app/data/repos/pay/src/main/java/com/demo/pay/web/UserController.java', start_line: 210, end_line: 210 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:40:00Z',
  },
  {
    finding_id: 'gw-f-sev-medium', task_id: 'gw-t00000a1f', title: '弱哈希：MD5 用于口令摘要',
    severity: 'SEVERITY_MEDIUM', cwe_id: 'CWE-328', source_tool: 'semgrep', source_rule_id: 'crypto.weak-hash-md5',
    confidence: 'CERTAIN', verdict_direction: 'VERDICT_DIRECTION_UNSPECIFIED', verdict_tier: 'VERDICT_TIER_UNSPECIFIED', ai_confidence: 0.4,
    ai_reasoning: '[LLM:gpt-demo] util/HashUtil.java:18 口令摘要使用 MD5——若为遗留兼容场景可降级处理，需人工复核。',
    description: '口令摘要算法为 MD5，碰撞攻击成本已不可接受。',
    location: { file_path: '/app/data/repos/pay/src/main/java/com/demo/pay/util/HashUtil.java', start_line: 18, end_line: 18 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:41:00Z',
  },
  {
    finding_id: 'gw-f-sev-low', task_id: 'gw-t00000a1f', title: '日志中输出完整卡号（掩码缺失）',
    severity: 'SEVERITY_LOW', cwe_id: 'CWE-532', source_tool: 'opengrep', source_rule_id: 'sensitive.log-pan',
    confidence: 'HIGH', verdict_direction: 'VERDICT_DIRECTION_FALSE', verdict_tier: 'VERDICT_TIER_DEFINITE', ai_confidence: 0.66,
    ai_reasoning: '[LLM:gpt-demo] 该日志点位于测试桩代码，生产路径不可达——误报。',
    description: '支付回调日志输出未掩码卡号。',
    location: { file_path: '/app/data/repos/pay/src/test/java/com/demo/pay/CallbackLogTest.java', start_line: 40, end_line: 40 },
    source_raw: '', created_at: '2026-09-12T10:40:00Z', updated_at: '2026-09-12T10:42:00Z',
  },
  {
    finding_id: 'gw-f-sev-info', task_id: 'gw-t00000a1f', title: '依赖提示：log4j 2.17 以下版本存在已知 CVE',
    severity: 'SEVERITY_INFO', cwe_id: 'CWE-1104', source_tool: 'opengrep', source_rule_id: 'dep.log4j-version',
    confidence: '', verdict_direction: 'VERDICT_DIRECTION_UNSPECIFIED', verdict_tier: 'VERDICT_TIER_UNSPECIFIED', ai_confidence: 0, ai_reasoning: '',
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

// 演示报告正文（报告阅读视图 /reports/:id 走查数据，形状=<host-id> sim 库实测）：
// 严重度全阶梯各 1 条 + verdict 覆盖（确认真/可能真/误报/需人工/未判定）+
// 第 5 条无 source_raw（"未携带代码片段"降级走查）。
const DEMO_REPORT_JSON = {
  task_id: 'gw-t00000a1f',
  generated_at: '2026-09-12T10:42:00Z',
  template: '',
  summary: { total_findings: 5, true_positives: 1, false_positives: 1, not_reviewed: 3 },
  findings: [
    {
      finding_id: 'gw-t00000a1f-sbx-crit-1', severity: 'SEVERITY_CRITICAL', verdict: 'AI_VERDICT_TRUE_POSITIVE',
      file: 'pay/src/main/java/com/demo/pay/util/HashUtil.java', line: 18, cwe: 'CWE-328', rule_id: 'dsh-headless',
      title: '第 18 行使用 MD5 对支付回调签名做摘要：摘要算法可碰撞，攻击者可构造替代报文通过验签，直接造成资金操作伪造。该函数位于回调验签主路径，影响面为全部回调交易。',
      source_raw: JSON.stringify({
        code: 'MessageDigest md = MessageDigest.getInstance("MD5");',
        line: 18,
        context: {
          start_line: 15, end_line: 21,
          lines: [
            'public boolean verify(String payload, String sig) {',
            '    // 回调验签：MD5 摘要比对',
            '    MessageDigest md = MessageDigest.getInstance("MD5");',
            '    byte[] d = md.digest(payload.getBytes(StandardCharsets.UTF_8));',
            '    return hex(d).equals(sig);',
            '}',
            '',
          ],
        },
      }),
    },
    {
      finding_id: 'gw-t00000a1f-sbx-high-1', severity: 'SEVERITY_HIGH', verdict: 'AI_VERDICT_LIKELY_TRUE',
      file: 'pay/src/main/java/com/demo/pay/CallbackServlet.java', line: 42, cwe: 'CWE-89', rule_id: 'dsh-headless',
      title: '第 42 行将回调报文中的 orderId 直接拼入 SQL 查询语句：调用方可控输入未参数化即进入 executeQuery，可构造布尔注入读取订单表外的数据。建议改为 PreparedStatement 绑定参数。',
      source_raw: JSON.stringify({
        code: 'ResultSet rs = stmt.executeQuery("SELECT * FROM orders WHERE id = \'" + orderId + "\'");',
        line: 42,
        context: {
          start_line: 39, end_line: 45,
          lines: [
            'String orderId = req.getParameter("orderId");',
            'Connection conn = ds.getConnection();',
            'Statement stmt = conn.createStatement();',
            'ResultSet rs = stmt.executeQuery("SELECT * FROM orders WHERE id = \'" + orderId + "\'");',
            'return render(rs);',
            '',
            '',
          ],
        },
      }),
    },
    {
      finding_id: 'gw-t00000a1f-sbx-med-1', severity: 'SEVERITY_MEDIUM', verdict: 'AI_VERDICT_NEEDS_MANUAL',
      file: 'pay/src/main/resources/application.yml', line: 33, cwe: '', rule_id: 'config.debug-on',
      title: '生产配置中 management.endpoints.web.exposure.include 包含 heapdump：运维端点暴露堆转储可能泄露内存中的凭据与个人数据，需确认网关是否已对该路径鉴权。',
      source_raw: JSON.stringify({
        code: 'include: ["heapdump","health"]',
        line: 33,
        context: {
          start_line: 30, end_line: 35,
          lines: [
            'management:',
            '  endpoints:',
            '    web:',
            '      exposure:',
            '        include: ["heapdump","health"]',
            '',
          ],
        },
      }),
    },
    {
      finding_id: 'gw-t00000a1f-sbx-low-1', severity: 'SEVERITY_LOW', verdict: 'AI_VERDICT_FALSE_POSITIVE',
      file: 'pay/src/test/java/com/demo/pay/CallbackLogTest.java', line: 40, cwe: 'CWE-532', rule_id: 'sensitive.log-pan',
      title: '日志中输出完整卡号（掩码缺失）。该日志点位于测试桩代码，生产路径不可达——复核为误报。',
      source_raw: JSON.stringify({
        code: 'LOG.info("callback pan={}", pan);',
        line: 40,
        context: {
          start_line: 38, end_line: 42,
          lines: [
            '@Test',
            'void callbackLogsPan() {',
            '    LOG.info("callback pan={}", pan);',
            '    assertTrue(handler.accepted());',
            '}',
            '',
          ],
        },
      }),
    },
    {
      finding_id: 'gw-t00000a1f-dep-info-1', severity: 'SEVERITY_INFO', verdict: 'AI_VERDICT_UNCERTAIN',
      file: 'pay/pom.xml', line: 88, cwe: 'CWE-1104', rule_id: 'dep.log4j-version',
      title: '依赖提示：pom.xml 声明 log4j 2.14.1，低于 2.17 存在已知 CVE（Log4Shell 族），建议随依赖升级批次处理。',
      source_raw: '',
    },
  ],
};

const DEMO_USERS = [
  { user_id: 'u-demo', username: 'demo-admin', email: 'demo@codeaudit.local', role: 'ROLE_ADMIN', state: 'USER_STATE_ACTIVE', must_change_password: false, created_at: '2026-09-01T09:00:00Z' },
  { user_id: 'u-2', username: 'dev-wang', email: 'wang@codeaudit.local', role: 'ROLE_DEVELOPER', state: 'USER_STATE_ACTIVE', must_change_password: true, created_at: '2026-09-03T11:00:00Z' },
  { user_id: 'u-3', username: 'audit-li', email: 'li@codeaudit.local', role: 'ROLE_VIEWER', state: 'USER_STATE_INACTIVE', must_change_password: false, created_at: '2026-09-05T15:00:00Z' },
];

// ADR-217 契约：{name, type, config}，响应永不带凭据
const DEMO_PROVIDERS = [
  { name: 'openai-compat-demo', type: 'openai', config: { OPENAI_BASE_URL: 'https://llm.internal/v1' } },
  { name: 'anthropic-relay', type: 'anthropic', config: { ANTHROPIC_BASE_URL: 'https://relay.internal/anthropic' } },
];

// V2.2 (ADR-255)：邀请码演示数据（三种派生态各一，走查状态 Tag 用）
const DEMO_INVITES = [
  { id: 'invite-demo-1', code: 'CODEAUDIT-7YQ2M-P4W8K', max_uses: 1, used_count: 0, expires_at: null, state: 'active', note: '给新同事入职', created_by: 'admin', created_at: '2026-10-04T00:00:00Z' },
  { id: 'invite-demo-2', code: 'CODEAUDIT-H3T7C-XR9DV', max_uses: 5, used_count: 5, expires_at: null, state: 'active', note: '批量（已用尽）', created_by: 'admin', created_at: '2026-10-03T00:00:00Z' },
  { id: 'invite-demo-3', code: 'CODEAUDIT-Z6N2F-QM5BT', max_uses: 0, used_count: 3, expires_at: '2026-09-01T00:00:00Z', state: 'active', note: '不限量（已过期）', created_by: 'admin', created_at: '2026-08-01T00:00:00Z' },
];

// ---- 可变走查态：写路由就地变更，刷新页面前持续可见 ----
let demoSeq = 1;
const demoConfigs: Record<string, { project_id: string; config: Record<string, string> }> = {
  'gw-demo-pay': { project_id: 'gw-demo-pay', config: { upload_file_id: 'file-demo-pay', upload_file_name: 'pay-service-2026Q3.zip' } },
  'gw-demo-unnamed': { project_id: 'gw-demo-unnamed', config: { upload_file_id: 'file-20260920100154.655965744' } },
};

const routes: Record<string, RouteValue> = {
  // 会话
  'GET /v1/users/me': DEMO_USER,
  'POST /v1/auth/login': { access_token: 'demo-access', refresh_token: 'demo-refresh' },
  'POST /v1/auth/logout': {},
  // 项目域
  'GET /v1/projects': () => ({ projects: DEMO_PROJECTS, pagination: { next_cursor: '', has_next: false, total: DEMO_PROJECTS.length } }),
  'GET /v1/projects/:projectId': ({ params }: { params: Record<string, string> }) =>
    DEMO_PROJECTS.find((p) => p.project_id === params.projectId) ?? {},
  // 按项目回放；无 config 的项目 404——对齐真实 project-service（前端把 404 判"未配置"，
  // 2026-10-04 报障修复口径的走查前提）
  'GET /v1/projects/:projectId/config': ({ params }: { params: Record<string, string> }) => {
    const hit = demoConfigs[params.projectId];
    if (hit) return hit;
    throw httpError(404, { error: `config for project ${params.projectId} not found` });
  },
  // 源码补配写通道：仓库档=整对象 PUT / 上传档=config PUT
  'PUT /v1/projects/:projectId': ({ params, body }: { params: Record<string, string>; body: { project: Record<string, string> } }) => {
    const idx = DEMO_PROJECTS.findIndex((p) => p.project_id === params.projectId);
    if (idx < 0) throw httpError(404, { error: `project ${params.projectId} not found` });
    DEMO_PROJECTS[idx] = { ...DEMO_PROJECTS[idx], ...body.project, project_id: params.projectId };
    return DEMO_PROJECTS[idx];
  },
  'PUT /v1/projects/:projectId/config': ({ params, body }: { params: Record<string, string>; body: { config: { project_id: string; config: Record<string, string> } } }) => {
    demoConfigs[params.projectId] = { project_id: params.projectId, config: body.config.config };
    return demoConfigs[params.projectId];
  },
  // 向导就地创建（走查主链路）：POST /v1/projects → 新项目入列（无源码态）
  'POST /v1/projects': ({ body }: { body: { project: { name: string } } }) => {
    const p = { project_id: `gw-demo-live-${demoSeq++}`, name: body.project.name, repo_url: '', default_branch: 'main', default_scan_mode: 'SCAN_MODE_PARALLEL', created_at: new Date().toISOString() };
    DEMO_PROJECTS.push(p);
    return p;
  },
  // 压缩包直传（ProjectSourceModal 上传档）：file_id 回执入 config 由前端 PUT 落
  'POST /v1/uploads/archive': () => ({ upload_id: `up-demo-${demoSeq++}`, file_id: `file-demo-up-${demoSeq}`, file_path: 'uploads/demo/src.zip', size_bytes: 1024 }),
  // 任务创建+启动（向导闭环尾）：入列 PENDING 无阶段任务
  'POST /v1/tasks': ({ body }: { body: { project_id: string; scan_mode: string } }) => {
    const t = {
      task_id: `gw-tlive-${demoSeq++}`, project_id: body.project_id, scan_mode: body.scan_mode, sast_tools: [],
      status: 'TASK_STATUS_PENDING', error_message: '', retry_count: 0, created_at: new Date().toISOString(), updated_at: new Date().toISOString(), stages: [],
    };
    DEMO_TASKS.push(t as (typeof DEMO_TASKS)[number]);
    return { task_id: t.task_id };
  },
  'POST /v1/tasks/:taskId/start': ({ params }: { params: Record<string, string> }) => {
    const t = DEMO_TASKS.find((x) => x.task_id === params.taskId);
    if (t) t.status = 'TASK_STATUS_RUNNING';
    return {};
  },
  // 任务域
  'GET /v1/tasks': () => ({ tasks: DEMO_TASKS, pagination: { next_cursor: '', has_next: false, total: DEMO_TASKS.length } }),
  'GET /v1/tasks/:taskId/snapshot': ({ params }: { params: Record<string, string> }) => {
    const isGraphTask = params.taskId === 'gw-t00000a1f'
    const t = DEMO_TASKS.find((x) => x.task_id === params.taskId) ?? DEMO_TASKS[0];
    return {
      task: t,
      progress: null,
      logs: { logs: DEMO_LOGS.filter((l) => l.task_id === t.task_id) },
      ai: { chunk: b64(isGraphTask ? DEMO_GRAPH_AI_TEXT : DEMO_AI_TEXT), next_cursor: String(isGraphTask ? DEMO_GRAPH_AI_TEXT.length : DEMO_AI_TEXT.length), complete: true, total_bytes: String(isGraphTask ? DEMO_GRAPH_AI_TEXT.length : DEMO_AI_TEXT.length) },
    };
  },
  'GET /v1/tasks/:taskId/source-file': ({ query }: { query: URLSearchParams }) => ({
    path: query.get('path') ?? '', content: DEMO_SOURCE, total_lines: DEMO_SOURCE.split('\n').length,
    bytes: DEMO_SOURCE.length, root_via: 'demo', resolved_via: 'demo',
  }),
  // 图执行视图：走查形态——双节点+计划带+收敛
  'GET /v1/tasks/:taskId/graph': {
    task_id: 'demo',
    nodes: [
      { id: 'n001', type: 'executor', intent: '初扫注入面：SQL 注入与硬编码凭据', declared_by: 'pack', from: [], status: 'completed', attempts: 0, started_at: '2026-09-20T10:00:00Z', ended_at: '2026-09-20T10:02:14Z', result_ref: '3 处发现（B608×2/B105）', declared_seq: 1 },
      { id: 'n002', type: 'verifier', intent: '复核补丁：修复面回归与误报甄别', declared_by: 'planner', from: ['n001'], status: 'running', attempts: 1, started_at: '2026-09-20T10:03:00Z', ended_at: '', result_ref: '', declared_seq: 5 },
    ],
    plan_triggers: [
      { trigger: 'initial', detail: '起初始 planner', at: '2026-09-20T09:59:00Z', seq: 0 },
      { trigger: 'new_evidence', detail: 'n001 报告可疑残面，重规划复核节点', at: '2026-09-20T10:02:20Z', seq: 4 },
    ],
    paused: false,
    pause_reason: '',
    converged: false,
    converge_summary: '',
    open_remaining: 1,
    updated_at: '2026-09-20T10:03:00Z',
  },
  // 任务计量：走查形态——主模型+轻模型的成本结构
  'GET /v1/tasks/:taskId/usage': {
    task_id: 'demo',
    models: [
      { model: 'glm-4.7', calls: '18', input_tokens: '41200', output_tokens: '9800', cache_read_tokens: '63400', cache_write_tokens: '8100', reasoning_tokens: '2200', total_tokens: '122500' },
      { model: 'glm-4.7-flash', calls: '6', input_tokens: '5400', output_tokens: '1200', cache_read_tokens: '2100', cache_write_tokens: '0', reasoning_tokens: '0', total_tokens: '8700' },
    ],
    totals: { model: '', calls: '24', input_tokens: '46600', output_tokens: '11000', cache_read_tokens: '65500', cache_write_tokens: '8100', reasoning_tokens: '2200', total_tokens: '131200' },
  },
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
  // E-29①：报告正文。JSON 档=真实形状（<host-id> sim 库抽样：findings/summary/generated_at/
  // task_id；source_raw 为明文 JSON 字符串）；走查纪律——severity 全阶梯+verdict 各态
  // +一条无代码片段（降级分支）+复核结论非零（审计句走查）
  'GET /v1/reports/:reportId/download': ({ params }: { params: Record<string, string> }) => {
    if (params.reportId.includes('html-format')) {
      return '<!doctype html><html><head><title>支付网关扫描报告</title></head><body><h1>支付网关 · 模式C 融合扫描报告</h1><p>HTML 排版成品档（演示）。</p></body></html>';
    }
    return JSON.stringify(DEMO_REPORT_JSON);
  },
  'GET /v1/notifications': { notifications: DEMO_NOTIFICATIONS },
  'GET /v1/tools': { tools: [{ id: 'opengrep', name: 'OpenGrep' }, { id: 'semgrep', name: 'Semgrep' }] },
  // TP20-T2 Agent 清单（创建页/切换 Modal 同源；演示档=dsh 默认+maka）
  'GET /v1/agents': { agents: [
    { id: 'dsh', dialect: 'dsh-sse', is_default: true, tool_events: true, structured_submit: true, interaction: false, session_resume: false, conclude_semantics: true },
    { id: 'maka', dialect: 'maka-runtime-host', is_default: false, tool_events: false, structured_submit: false, interaction: false, session_resume: false, conclude_semantics: false },
  ] },
  // 管理面
  'GET /v1/users': () => ({ users: DEMO_USERS, pagination: { next_cursor: '', has_next: false, total: DEMO_USERS.length } }),
  // V2.2 (ADR-255)：注册策略公开读 + 邀请码管理面（dev:mock 注册页与管理 Tab 走查）
  'GET /v1/auth/registration-policy': { mode: 'invitation' },
  'GET /v1/invites': () => ({
    codes: DEMO_INVITES,
    config_codes: ['CODEAUDIT-BOOTSTRAP-2026'],
    pagination: { next_cursor: '', has_next: false, total: DEMO_INVITES.length },
  }),
  'POST /v1/invites': () => DEMO_INVITES[0],
  'GET /v1/inference/providers': { providers: DEMO_PROVIDERS },
  // E-38 形状（version=protojson 字符串）。team-alpha 故意未配置——就绪度警示态走查（父含子）
  'GET /v1/inference/route': (ctx: { query: URLSearchParams }) =>
    ctx.query.get('workspace') === 'team-alpha'
      ? { provider: '', model: '', version: '0' }
      : { provider: 'openai-compat-demo', model: 'demo-pro', version: '7' },
  // 父含子：工作区行内推理配置抽屉走查数据（default=已就绪, team-alpha=未配置警示态）
  'GET /v1/workspaces': { workspaces: [
    { name: 'default', phase: 'PHASE_ACTIVE', labels: {} },
    { name: 'team-alpha', phase: 'PHASE_ACTIVE', labels: { team: 'alpha' } },
  ] },
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
