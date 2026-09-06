// 修复流程真实链路 E2E（不 mock 平台）：上传含多个风险的文件 → AI_ONLY 扫描 →
// 取同文件多条带 diff_patch 的发现 → 顺序应用（第二条针对第一条修复后的新内容，
// 验证内容锚定不受行号漂移影响）→ 按发现回滚 → 断言文件逐字节还原 → 再次应用。
// 用法: node test/fixflow.e2e.js [serverUrl]（需平台网关在线；AI_ONLY 约 1 分钟）
const Module = require('module');
const path = require('path');
const orig = Module._resolveFilename;
Module._resolveFilename = function (request, ...args) {
  if (request === 'vscode') return path.join(__dirname, 'mocks', 'vscode.js');
  return orig.call(this, request, ...args);
};
const fs = require('fs');
const os = require('os');
const AdmZip = require('adm-zip');
const { CodeAuditClient } = require('../out/apiClient.js');
const { CheckpointStore } = require('../out/checkpoint.js');
const { FixRegistry } = require('../out/fixRegistry.js');
const { computePatchChanges, listUpdatedFiles } = require('../out/applyPatch.js');

const store = {
  access: '', refresh: '',
  getAccessToken: () => store.access,
  getRefreshToken: () => store.refresh,
  setTokens: (a, r) => { store.access = a; if (r) store.refresh = r; },
  clear: () => { store.access = ''; store.refresh = ''; },
};

const DEMO = `import sqlite3
import os

DB_PATH = "app.db"
ADMIN_PASSWORD = "SuperSecret123"

def get_user(user_id):
    conn = sqlite3.connect(DB_PATH)
    cur = conn.cursor()
    query = "SELECT * FROM users WHERE id = " + user_id
    cur.execute(query)
    return cur.fetchone()

def backup_database():
    target = "/tmp/backup.db"
    os.system("cp app.db " + target)
    return target

def delete_user(user_id):
    conn = sqlite3.connect(DB_PATH)
    cur = conn.cursor()
    cur.execute("DELETE FROM users WHERE name = " + user_id)
    conn.commit()
    conn.close()
`;

async function main() {
  const base = process.argv[2] || 'http://pve.internal:8080';
  const c = new CodeAuditClient(base, store);
  const step = (m) => console.log(`[e2e] ${new Date().toISOString().slice(11, 19)} ${m}`);
  const assert = (cond, msg) => { if (!cond) throw new Error(`断言失败：${msg}`); };

  await c.login('admin', 'admin');
  const projects = await c.listProjects();
  const project = projects[0];
  step(`login OK, project=${project.project_id}`);

  const zip = new AdmZip();
  zip.addFile('multi_vuln.py', Buffer.from(DEMO));
  const upload = await c.uploadArchive(new Blob([zip.toBuffer()]));
  // 存储桶方案（ADR-148/163）后返回桶内 zip 路径 file_path，旧平台为解压目录 dir——两者取先有的
  const projectPath = upload.file_path ?? upload.dir;
  const task = await c.createTask(project.project_id, 'SCAN_MODE_AI_ONLY', [], { project_path: projectPath });
  await c.startTask(task.task_id);
  step(`task ${task.task_id} started, waiting terminal…`);
  const deadline = Date.now() + 10 * 60_000;
  for (;;) {
    const snap = await c.taskSnapshot(task.task_id);
    if (['TASK_STATUS_COMPLETED', 'TASK_STATUS_CANCELLED', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_DEAD'].includes(snap.task.status)) {
      assert(snap.task.status === 'TASK_STATUS_COMPLETED', `任务未完成：${snap.task.status}`);
      break;
    }
    if (Date.now() > deadline) throw new Error('等待任务终态超时');
    await new Promise((r) => setTimeout(r, 3000));
  }
  const findings = await c.listFindings(task.task_id);
  step(`findings: ${findings.length}`);
  for (const f of findings) {
    step(`  - ${f.severity} ${f.title} @${f.location?.file_path}:${f.location?.start_line} patch=${f.diff_patch ? `${f.diff_patch.length}B` : '无'}`);
  }

  // —— 插件同款装配：checkpoint + registry（真实磁盘临时目录）——
  const workDir = fs.mkdtempSync(path.join(os.tmpdir(), 'fixflow-'));
  const targetFile = path.join(workDir, 'multi_vuln.py');
  fs.writeFileSync(targetFile, DEMO, 'utf-8');
  const checkpoints = new CheckpointStore(path.join(workDir, 'checkpoints'));
  const registry = new FixRegistry(path.join(workDir, 'fix-registry.json'));

  // 模拟插件：补丁路径 → 相对工作区根（此处 workDir 即根）
  const patched = findings.filter((f) => f.diff_patch && (f.location?.file_path ?? '').endsWith('multi_vuln.py'));
  assert(patched.length >= 1, `需要至少 1 条带 diff_patch 的同文件发现（实际 ${patched.length}）`);

  const applyOne = (findingId, label, patchText) => {
    const currentFiles = { 'multi_vuln.py': fs.readFileSync(targetFile, 'utf-8') };
    const files = listUpdatedFiles(patchText);
    for (const rel of files) assert(rel === 'multi_vuln.py', `非目标文件 ${rel}`);
    const computed = computePatchChanges(patchText, currentFiles); // 锚定失败在此整体拒绝
    const change = computed.changes['multi_vuln.py'];
    const cpId = checkpoints.save({ [targetFile]: currentFiles['multi_vuln.py'] });
    fs.writeFileSync(targetFile, change.newContent, 'utf-8');
    registry.recordApplied({ findingId, label, checkpointId: cpId, files: [targetFile], appliedAt: Date.now(), state: 'applied' });
    return { fuzz: computed.fuzz, cpId };
  };

  // 顺序应用（核心验证：第 N 条补丁锚定在第 N-1 条修复后的内容上）
  let applied = 0, rejected = [];
  for (const f of patched) {
    const label = f.title || f.cwe_id || f.finding_id;
    try {
      const r = applyOne(f.finding_id, label, f.diff_patch);
      applied++;
      step(`apply#${applied} 「${label}」 fuzz=${r.fuzz}（在最新内容上锚定成功）`);
    } catch (e) {
      rejected.push({ label, msg: e.message });
      step(`REJECT 「${label}」：${e.message.slice(0, 120)}（诚实拒绝，文件未被部分改写）`);
    }
  }
  assert(applied >= 1, '至少成功应用 1 条修复');
  const afterAll = fs.readFileSync(targetFile, 'utf-8');
  assert(afterAll !== DEMO, '应用后文件内容应有变化');

  // 按发现回滚（后进先出），断言逐字节还原
  let rolls = 0;
  for (;;) {
    const rec = registry.appliedRecords().sort((a, b) => b.appliedAt - a.appliedAt)[0];
    if (!rec) break;
    const restored = checkpoints.restore(rec.checkpointId);
    assert(restored && restored[targetFile] !== undefined, `checkpoint ${rec.checkpointId} 可恢复`);
    fs.writeFileSync(targetFile, restored[targetFile], 'utf-8');
    registry.markRolledback(rec.findingId);
    rolls++;
  }
  const finalContent = fs.readFileSync(targetFile, 'utf-8');
  assert(finalContent === DEMO, '全部回滚后文件必须逐字节还原');
  step(`rollback ×${rolls} → 文件逐字节还原 ✔`);

  // 回滚后重新应用第一条（可再应用语义）
  const again = patched.find((f) => !rejected.some((r) => r.label === (f.title || f.cwe_id || f.finding_id)));
  if (again) {
    applyOne(again.finding_id, again.title, again.diff_patch);
    assert(fs.readFileSync(targetFile, 'utf-8') !== DEMO, '重新应用后内容再次变化');
    step(`re-apply 「${again.title}」 ✔（回滚后可再次应用）`);
  }

  console.log('--------------------------------------------------');
  console.log(`[e2e] 结论：同文件 ${patched.length} 条修复，顺序应用成功 ${applied} 条、诚实拒绝 ${rejected.length} 条；回滚 ×${rolls} 逐字节还原；回滚后可重应用。`);
  console.log('[e2e] E2E PASS');
}

main().catch((e) => { console.error(`[e2e] FAIL: ${e.message}`); process.exit(1); });
