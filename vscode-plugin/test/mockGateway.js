// 本地 mock 网关：真实平台暂不产出 ai_fix_suggestion（ADR-176 缺口），
// 用它驱动插件 diff 审批修复 + checkpoint 回滚的完整 UI 流程。
// 用法: node test/mockGateway.js [port=8080]
// 行为: 任务 start 后 1.5s 自动 COMPLETED（插件 TaskWatcher 轮询兜底每 10s 拉一次快照；
//       本 mock 不开 WS——顺带验证插件"WS 不可用 → 10s 轮询回退"的 UI 口径）。
// 覆盖: 登录/刷新/登出、项目列表、上传(旧口径 dir)、建任务/启动/暂停/恢复/取消、
//       快照(stages/logs/ai 四路帧)、findings 八类场景、任务列表。
const http = require('http');

const REAL_SUGGESTION_TEXT = '**根因**：外部可控输入 `user_id` 通过字符串拼接进入 SQL 语句，再交给 `cursor.execute` 作为 SQL 文本执行，输入未参数化也未校验，攻击者可注入任意 SQL 表达式拖取或篡改数据。\n\n**修法**：改用 sqlite3 的参数绑定：SQL 中使用 `?` 占位符，把 `user_id` 作为参数元组传入 `cursor.execute(query, (user_id,))`，由数据库驱动负责安全转义。';

// ADR-183 全链：真实网关沙箱产出的 diff_patch 原样回放（apply_patch 语法），
// 用于驱动插件 PatchParser 主路径（diff 审批→落盘→回滚）的 UI 验证。
const REAL_DIFF_PATCH_A = [
  '*** Begin Patch',
  '*** Update File: vuln_fix_demo.py',
  '@@     # SQL injection: string concatenation',
  '-    query = "SELECT * FROM users WHERE id = " + user_id',
  '-    cursor.execute(query)',
  '+    query = "SELECT * FROM users WHERE id = ?"',
  '+    cursor.execute(query, (user_id,))',
  '*** End Patch',
].join('\n');

// vuln_demo.py 第 7-8 行的 ```diff 围栏兜底路径（diff_patch 为空的旧任务口径）
const FENCED_DIFF_VULN_DEMO = [
  '该行为字符串拼接构造 SQL 导致的注入（CWE-89）。建议改用参数化查询：',
  '',
  '```diff',
  '--- a/vuln_demo.py',
  '+++ b/vuln_demo.py',
  '@@ -6,3 +6,3 @@',
  '-    # SQL injection: string concatenation',
  '-    query = "SELECT * FROM users WHERE id = " + user_id',
  '-    cursor.execute(query)',
  '+    # 参数化查询：用户输入不参与 SQL 文本拼接',
  '+    query = "SELECT * FROM users WHERE id = ?"',
  '+    cursor.execute(query, (user_id,))',
  '```',
].join('\n');

// vuln_demo.py 第 20 行附近硬编码口令的主路径补丁（同文件第二处发现——QuickFix 按行匹配用）
const REAL_DIFF_PATCH_B = [
  '*** Begin Patch',
  '*** Update File: vuln_demo.py',
  '@@     def upload():',
  '-    password = "admin123"',
  '+    password = os.environ["DEMO_PASSWORD"]',
  '*** End Patch',
].join('\n');

// low_risk_demo.py 的低风险补丁（LOW + 置信度 0.95 → 批量应用候选）
const REAL_DIFF_PATCH_C = [
  '*** Begin Patch',
  '*** Update File: low_risk_demo.py',
  '@@     # TODO: temporary debug flag',
  '-    DEBUG = True',
  '+    DEBUG = False',
  '*** End Patch',
].join('\n');

// vuln_demo.py delete_user 的近邻补丁（同文件第三处发现——修复↔回滚↔再修复循环压测用）
const REAL_DIFF_PATCH_D = [
  '*** Begin Patch',
  '*** Update File: vuln_demo.py',
  '@@ def delete_user(conn, user_id):',
  '     cursor = conn.cursor()',
  '-    cursor.execute("DELETE FROM users WHERE id = %s" % user_id)',
  '+    cursor.execute("DELETE FROM users WHERE id = ?", (user_id,))',
  '     return cursor.rowcount',
  '*** End Patch',
].join('\n');

// vuln_multi_demo.py 的乱序双段补丁：远端段（drop_table L11-13）写在前、近端段
// （load_user L4-6）写在后——回归用户实测「同文件无法定位修复位置」：顺序游标
// 不回溯时第二段锚定失败、整补丁被拒（引擎修复后应两段各锚其位并按位置排序应用）
const REAL_DIFF_PATCH_MULTI = [
  '*** Begin Patch',
  '*** Update File: vuln_multi_demo.py',
  '@@ def drop_table(conn, name):',
  '     # SQL injection: table name concatenation',
  '-    cursor.execute("DROP TABLE " + name)',
  '+    cursor.execute("DROP TABLE ?", (name,))',
  '@@ def load_user(conn, user_id):',
  '     # SQL injection: string concatenation',
  '-    query = "SELECT * FROM users WHERE id = " + user_id',
  '+    query = "SELECT * FROM users WHERE id = ?"',
  '*** End Patch',
].join('\n');
// 六类场景（对应 README「修复与回滚机制」的全部分支）：
//   fx-1 主路径 diff_patch（vuln_fix_demo.py L7-8 HIGH）
//   fx-2 兜底路径 ```diff 围栏（vuln_demo.py L7-8 HIGH，diff_patch 空）
//   fx-2b 同文件第二处主路径（vuln_demo.py L20-21 HIGH）——QuickFix 行匹配/树内联
//   fx-3 低风险批量候选（low_risk_demo.py L5-6 LOW，AI 置信度 0.95，带 diff_patch）
//   fx-4 无精确位置 → 只进侧栏树「(无位置)」组，不进 Problems
//   fx-5 无建议自然语言 → 诚实降级「暂无法自动修复」
//   fx-6 同文件近邻第三处（vuln_demo.py L14 HIGH）——修复↔回滚↔再修复循环压测
//   fx-7 乱序双段补丁（vuln_multi_demo.py 两函数）——同文件远段在前近段在后，回溯锚定
const FINDINGS = [
  {
    finding_id: 'mock-fx-1', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-89', title: '',
    description: '用户可控输入直接拼进 SQL 文本，可注入任意 SQL 片段。',
    severity: 'SEVERITY_HIGH', confidence: 0.95, ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
    ai_confidence: 0.93, ai_reasoning: 'user_id 未经校验直接拼接。',
    ai_fix_suggestion: REAL_SUGGESTION_TEXT, diff_patch: REAL_DIFF_PATCH_A,
    location: { file_path: 'vuln_fix_demo.py', start_line: 7, end_line: 8, function_name: 'get_user' },
    dedup_group: 'mock-dedup-1', is_unique: true,
  },
  {
    finding_id: 'mock-fx-2', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-89', title: '',
    description: '演示：结构化通道降级（diff_patch 空、建议带 ```diff 围栏）时的兜底路径。',
    severity: 'SEVERITY_HIGH', confidence: 0.9, ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
    ai_confidence: 0.88, ai_reasoning: '',
    ai_fix_suggestion: FENCED_DIFF_VULN_DEMO, diff_patch: '',
    location: { file_path: 'vuln_demo.py', start_line: 7, end_line: 8 },
    dedup_group: 'mock-dedup-2', is_unique: true,
  },
  {
    finding_id: 'mock-fx-2b', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-259', title: '',
    description: '同文件第二处：硬编码口令。灯泡 QuickFix 应按行命中本条而非同文件的 fx-2。',
    severity: 'SEVERITY_HIGH', confidence: 0.9, ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
    ai_confidence: 0.91, ai_reasoning: '口令字面量硬编码在源码中。',
    ai_fix_suggestion: REAL_SUGGESTION_TEXT, diff_patch: REAL_DIFF_PATCH_B,
    location: { file_path: 'vuln_demo.py', start_line: 20, end_line: 21 },
    dedup_group: 'mock-dedup-2b', is_unique: true,
  },
  {
    finding_id: 'mock-fx-3', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'semgrep', source_rule_id: 'python.flask.debug-enabled', cwe_id: 'CWE-489',
    title: '调试开关常量残留', description: 'DEBUG 常量为 True 可能泄露调试信息。',
    severity: 'SEVERITY_LOW', confidence: 0.8, ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
    ai_confidence: 0.95, ai_reasoning: '低风险：反转常量即可。',
    ai_fix_suggestion: '把 DEBUG 置为 False。', diff_patch: REAL_DIFF_PATCH_C,
    location: { file_path: 'low_risk_demo.py', start_line: 5, end_line: 6 },
    dedup_group: 'mock-dedup-3', is_unique: true,
  },
  {
    finding_id: 'mock-fx-4', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-798', title: '全局配置疑似泄漏凭据（无精确位置）',
    description: '该发现无 file_path/行号——验证「(无位置)」分组与 Problems 排除口径。',
    severity: 'SEVERITY_CRITICAL', confidence: 0.7, ai_verdict: 'AI_VERDICT_UNSPECIFIED',
    ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '人工排查部署配置。', diff_patch: '',
    location: null, dedup_group: 'mock-dedup-4', is_unique: true,
  },
  {
    finding_id: 'mock-fx-5', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'semgrep', source_rule_id: 'generic.info', cwe_id: '',
    title: '信息级提示：建议补充类型注解', description: '自然语言建议、无机器补丁——诚实降级路径。',
    severity: 'SEVERITY_INFO', confidence: 0.5, ai_verdict: '',
    ai_confidence: 0.3, ai_reasoning: '',
    ai_fix_suggestion: '建议为函数参数补充类型注解以提升可读性。', diff_patch: '',
    location: { file_path: 'low_risk_demo.py', start_line: 9, end_line: 9 },
    dedup_group: 'mock-dedup-5', is_unique: true,
  },
  {
    finding_id: 'mock-fx-6', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-89', title: '',
    description: '同文件近邻第三处：delete_user 拼接 SQL。验证同文件多次修复↔回滚↔再修复循环中的内容锚定。',
    severity: 'SEVERITY_HIGH', confidence: 0.9, ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
    ai_confidence: 0.92, ai_reasoning: 'user_id 拼接进 DELETE 语句文本。',
    ai_fix_suggestion: REAL_SUGGESTION_TEXT, diff_patch: REAL_DIFF_PATCH_D,
    location: { file_path: 'vuln_demo.py', start_line: 14, end_line: 14 },
    dedup_group: 'mock-dedup-6', is_unique: true,
  },
  {
    finding_id: 'mock-fx-7', task_id: 'mock-task-1', project_id: 'mock-proj-001',
    source_tool: 'ai_agent', source_rule_id: '', cwe_id: 'CWE-89', title: '',
    description: '乱序双段补丁：同一 diff_patch 先改 drop_table（L13）再改 load_user（L6），顺序与文件位置相反。',
    severity: 'SEVERITY_HIGH', confidence: 0.9, ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
    ai_confidence: 0.9, ai_reasoning: '两处拼接均可用占位符参数化。',
    ai_fix_suggestion: REAL_SUGGESTION_TEXT, diff_patch: REAL_DIFF_PATCH_MULTI,
    location: { file_path: 'vuln_multi_demo.py', start_line: 6, end_line: 7 },
    dedup_group: 'mock-dedup-7', is_unique: true,
  },
];

const STAGE_SEQ = [
  { stage_id: 'stage-1', type: 'STAGE_TYPE_CODE_ANALYSIS' },
  { stage_id: 'stage-2', type: 'STAGE_TYPE_AI_INFERENCE' },
  { stage_id: 'stage-3', type: 'STAGE_TYPE_RESULT_FUSION' },
];

const LOGS = [
  { log_id: '1', ts_ms: '0', level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '任务已创建（mock）' },
  { log_id: '2', ts_ms: '0', level: 'TASK_LOG_LEVEL_INFO', source: 'task', message: '代码分析阶段开始' },
  { log_id: '3', ts_ms: '0', level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '项目打包完成（10240 字节）' },
  { log_id: '4', ts_ms: '0', level: 'TASK_LOG_LEVEL_WARN', source: 'dsh-agent', message: '示例告警：依赖清单缺失 lock 文件' },
  { log_id: '5', ts_ms: '0', level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: 'AI 推理会话建立（DSH）' },
];

const AI_TEXT = [
  '=== DSH 沙箱会话正文（mock 回放）===',
  '',
  '[user] 请审计当前项目并给出修复建议。',
  '[assistant] 扫描发现 3 处需要关注的问题：',
  '1. vuln_fix_demo.py:7 SQL 注入（CWE-89）——建议参数化查询；',
  '2. vuln_demo.py:20 硬编码口令（CWE-259）——建议改读环境变量；',
  '3. low_risk_demo.py:5 调试开关残留（CWE-489）——低风险，直接修复。',
  '',
  '[assistant] 已生成机器补丁，等待任务收束后由插件消费。',
].join('\n');

const AI_CHUNK_B64 = Buffer.from(AI_TEXT, 'utf8').toString('base64');

let taskSeq = 0;
const tasks = new Map(); // id -> { status, startedAt, createdAt }

// 自动完成调度（start 时一次性定时，而非 GET 时惰性判定）：
// 惰性判定会被插件 10s 轮询抢先触发，导致外部 pause 永远打不进 RUNNING 窗口。
// 仅当此刻仍 RUNNING 才置 COMPLETED；pause/resume 由外部 API 自行驱动状态。
// MOCK_COMPLETE_MS 可拉长 RUNNING 窗口（如 15000）以便人工/GUI 测试暂停-恢复。
const AUTO_COMPLETE_MS = 1500;
function scheduleAutoComplete(t) {
  setTimeout(() => {
    if (t.status === 'TASK_STATUS_RUNNING') t.status = 'TASK_STATUS_COMPLETED';
  }, autoCompleteMs);
}

function stageStatuses(elapsedMs) {
  if (elapsedMs < 700) return ['STAGE_STATUS_RUNNING', 'STAGE_STATUS_PENDING', 'STAGE_STATUS_PENDING'];
  if (elapsedMs < 1500) return ['STAGE_STATUS_COMPLETED', 'STAGE_STATUS_RUNNING', 'STAGE_STATUS_PENDING'];
  return ['STAGE_STATUS_COMPLETED', 'STAGE_STATUS_COMPLETED', 'STAGE_STATUS_RUNNING'];
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  const send = (code, body) => {
    res.writeHead(code, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(body));
  };
  req.on('data', () => { /* drain */ });
  req.on('end', () => {
    if (req.method === 'POST' && url.pathname === '/v1/auth/login') {
      return send(200, { access_token: 'mock-access-token', refresh_token: 'mock-refresh-token', expires_in_s: 3600 });
    }
    if (req.method === 'POST' && url.pathname === '/v1/auth/logout') return send(200, {});
    if (req.method === 'POST' && url.pathname === '/v1/auth/refresh') {
      return send(200, { access_token: 'mock-access-token-2', refresh_token: 'mock-refresh-token-2', expires_in_s: 3600 });
    }
    if (req.method === 'GET' && url.pathname === '/v1/projects') {
      return send(200, {
        projects: [{
          project_id: 'mock-proj-001', name: 'Mock 演示项目', repo_url: '', default_branch: 'main',
          default_scan_mode: 'SCAN_MODE_AI_ONLY', created_at: '2026-09-02T00:00:00Z',
        }],
      });
    }
    if (req.method === 'POST' && url.pathname === '/v1/uploads/archive') {
      return send(200, { upload_id: 'mock-upload-1', dir: 'data/uploads/mock-upload-1', files: 6 });
    }
    if (req.method === 'POST' && url.pathname === '/v1/tasks') {
      const id = `mock-task-${++taskSeq}`;
      tasks.set(id, { status: 'TASK_STATUS_PENDING', startedAt: 0, createdAt: new Date().toISOString() });
      return send(200, { task_id: id, project_id: 'mock-proj-001', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [], status: 'TASK_STATUS_PENDING', stages: [], error_message: '' });
    }
    if (req.method === 'GET' && url.pathname === '/v1/tasks') {
      const list = [...tasks.entries()]
        .sort((a, b) => (a[1].createdAt < b[1].createdAt ? 1 : -1))
        .map(([id, t]) => ({ task_id: id, project_id: 'mock-proj-001', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [], status: t.status, created_at: t.createdAt, updated_at: t.createdAt, error_message: '' }));
      return send(200, { tasks: list });
    }
    const lifecycle = url.pathname.match(/^\/v1\/tasks\/([^/]+)\/(start|cancel|pause|resume)$/);
    if (req.method === 'POST' && lifecycle) {
      const t = tasks.get(lifecycle[1]);
      if (t) {
        if (lifecycle[2] === 'start') {
          t.status = 'TASK_STATUS_RUNNING'; t.startedAt = Date.now();
          scheduleAutoComplete(t);
        } else if (lifecycle[2] === 'cancel') t.status = 'TASK_STATUS_CANCELLED';
        else if (lifecycle[2] === 'pause') t.status = 'TASK_STATUS_PAUSED';
        else if (lifecycle[2] === 'resume') { t.status = 'TASK_STATUS_RUNNING'; if (!t.startedAt) t.startedAt = Date.now() - 1500; }
      }
      return send(200, {});
    }
    const mSnap = url.pathname.match(/^\/v1\/tasks\/([^/]+)\/snapshot$/);
    if (req.method === 'GET' && mSnap) {
      const t = tasks.get(mSnap[1]);
      // 真实网关口径：不存在的任务返回 404（插件 bindTask/轮询依赖它识别"任务已删除/归档"）
      if (!t) return send(404, { error: `NotFound: task ${mSnap[1]} not found` });
      const status = t.status;
      const elapsed = t && t.startedAt ? Date.now() - t.startedAt : 0;
      const statuses = status === 'TASK_STATUS_COMPLETED' ? STAGE_SEQ.map(() => 'STAGE_STATUS_COMPLETED') : stageStatuses(elapsed);
      const stages = STAGE_SEQ.map((s, i) => ({
        ...s, status: statuses[i],
        started_at: elapsed > 0 ? new Date(Math.max(t.startedAt, 0)).toISOString() : null,
        completed_at: statuses[i] === 'STAGE_STATUS_COMPLETED' ? new Date(Math.max(t.startedAt, 0) + 500).toISOString() : null,
        error_message: '',
      }));
      // 游标口径：logs_after 只回严格更大的 log_id；ai_cursor>0 视为已收齐（不再重发正文）
      const logsAfter = Number(url.searchParams.get('logs_after') ?? '0');
      const aiCursor = Number(url.searchParams.get('ai_cursor') ?? '0');
      const logs = LOGS.filter((l) => Number(l.log_id) > logsAfter);
      const body = {
        task: { task_id: mSnap[1], project_id: 'mock-proj-001', scan_mode: 'SCAN_MODE_AI_ONLY', sast_tools: [], status, stages: [], error_message: '' },
        progress: { overall_percent: status === 'TASK_STATUS_COMPLETED' ? 100 : Math.min(90, 20 + Math.round(elapsed / 40)), stages },
        logs: { logs },
      };
      if (status === 'TASK_STATUS_PENDING') {
        body.progress.overall_percent = 0;
      } else if (aiCursor <= 0) {
        body.ai = { chunk: AI_CHUNK_B64, next_cursor: Buffer.byteLength(AI_TEXT, 'utf8'), complete: true, total_bytes: Buffer.byteLength(AI_TEXT, 'utf8') };
      } else {
        body.ai = { chunk: '', next_cursor: aiCursor, complete: true, total_bytes: Buffer.byteLength(AI_TEXT, 'utf8') };
      }
      return send(200, body);
    }
    // 扫描预检（listTools 连通性探针）：真实网关必有此路由
    if (req.method === 'GET' && url.pathname === '/v1/tools') {
      return send(200, { tools: [] });
    }    if (req.method === 'GET' && url.pathname === '/v1/findings') {
      return send(200, { findings: FINDINGS, pagination: { next_cursor: '', has_next: false, total: FINDINGS.length } });
    }
    return send(404, { message: `mock: no route ${req.method} ${url.pathname}` });
  });
});

const port = Number(process.argv[2] || 8080);
// 自动完成窗口也可用第 3 个参数覆盖（env 在部分任务包装器下不可达）：node mockGateway.js 8080 15000
const autoCompleteMs = Number(process.argv[3]) || AUTO_COMPLETE_MS;
console.log(`[mockGateway] listening on http://127.0.0.1:${port}（auto-complete ${autoCompleteMs}ms）`);
server.listen(port, '127.0.0.1', () => { /* listen log 已在上方输出 */ });
