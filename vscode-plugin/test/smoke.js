// 真实链路冒烟（不 mock）：登录→列项目→上传zip→建任务→启动→快照轮询→（终态后）列findings。
// 用法: node test/smoke.js [serverUrl] ；需平台 docker-compose 起着（gateway :8080）。
const Module = require('module');
const path = require('path');
const orig = Module._resolveFilename;
Module._resolveFilename = function (request, ...args) {
  if (request === 'vscode') return path.join(__dirname, 'mocks', 'vscode.js');
  return orig.call(this, request, ...args);
};

const AdmZip = require('adm-zip');
const { CodeAuditClient } = require('../out/apiClient.js');

const store = {
  access: '',
  refresh: '',
  getAccessToken: () => store.access,
  getRefreshToken: () => store.refresh,
  setTokens: (a, r) => { store.access = a; if (r) store.refresh = r; },
  clear: () => { store.access = ''; store.refresh = ''; },
};

async function main() {
  const base = process.argv[2] || 'http://localhost:8080';
  const c = new CodeAuditClient(base, store);
  const step = (m) => console.log(`[smoke] ${m}`);

  step(`login ${base}`);
  const login = await c.login('admin', 'admin');
  if (!login.access_token) throw new Error('no access_token');
  step(`login OK, expires_in_s=${login.expires_in_s}`);

  step('list projects');
  const projects = await c.listProjects();
  step(`projects: ${projects.length}`);
  let project = projects[0];

  // 上传最小 zip（含一个带明显弱点的文件，便于 SAST/AI 出 findings）
  const zip = new AdmZip();
  zip.addFile('vuln_demo.py', Buffer.from(
    'import sqlite3\n\ndef get_user(user_id):\n    conn = sqlite3.connect("app.db")\n    cursor = conn.cursor()\n    # SQL injection: string concatenation\n    query = "SELECT * FROM users WHERE id = " + user_id\n    cursor.execute(query)\n    return cursor.fetchone()\n'));
  const upload = await c.uploadArchive(new Blob([zip.toBuffer()]));
  step(`upload OK dir=${upload.dir} files=${upload.files}`);

  if (!project) {
    throw new Error('平台无项目：请先在控制台创建项目后再跑冒烟');
  }

  step(`create task project=${project.project_id}`);
  const task = await c.createTask(project.project_id, 'SCAN_MODE_AI_ONLY', [], { project_path: upload.dir });
  step(`task created ${task.task_id}`);
  await c.startTask(task.task_id);
  step('task started');

  // 快照轮询至终态（上限 5 分钟，纯SAST应远快于此；07 超时矩阵 Task→SAST 20m 为服务端兜底）
  const deadline = Date.now() + 5 * 60_000;
  let snap;
  for (;;) {
    snap = await c.taskSnapshot(task.task_id);
    process.stdout.write(`\r[smoke] status=${snap.task.status} pct=${snap.progress?.overall_percent ?? '?'}   `);
    if (['TASK_STATUS_COMPLETED', 'TASK_STATUS_CANCELLED', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_DEAD'].includes(snap.task.status)) break;
    if (Date.now() > deadline) throw new Error('smoke timeout waiting terminal status');
    await new Promise((r) => setTimeout(r, 3000));
  }
  console.log();
  step(`terminal status=${snap.task.status}`);
  if (snap.task.status === 'TASK_STATUS_COMPLETED') {
    const findings = await c.listFindings(task.task_id);
    step(`findings: ${findings.length}`);
    for (const f of findings.slice(0, 5)) {
      console.log(`  - ${f.severity} ${f.title} @${f.location?.file_path ?? '?'}:${f.location?.start_line ?? '?'} tool=${f.source_tool}`);
    }
  }
  step('SMOKE PASS');
}

main().catch((e) => {
  console.error(`[smoke] FAIL: ${e.message}`);
  process.exit(1);
});
