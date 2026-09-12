// doScan 入口分支（验收 A19）：增量扫描的入口询问行为胶水层锁定。
// 覆盖：有基线 QuickPick 三选（增量/全量/Esc 取消）、无基线直全量零询问、
//      取消零残留（无上传/无建任务/互斥复位）、无变更预检（modal，仅建议）。
// createTask 请求体的载荷契约见 apiClient.test.ts「createTask 增量载荷」；本文件锁分支路由与触发条件。
// git 锚点经 setRepoProviderForTest 注入桩（与 gitAnchor.test.ts 同款），afterEach 复位不外溢。
import * as assert from 'assert';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as vscode from 'vscode';
import { activate } from '../src/extension';
import { setRepoProviderForTest } from '../src/gitAnchor';
import type { GitAnchor } from '../src/types';

// ───────────────────────────── 基础设施（与 extension.test.ts 同款形态） ─────────────────────────────

const m = () => (globalThis as any).__vsMock;
const realFetch = globalThis.fetch;
const tmps: string[] = [];
let wsInstances: any[] = [];

class WsStub {
  onopen?: () => void;
  onclose?: (ev?: { code?: number; reason?: string }) => void;
  onerror?: (ev?: { message?: string }) => void;
  onmessage?: (ev: { data: string }) => void;
  constructor(public url: string) { wsInstances.push(this); }
  close(): void { /* 桩：不建立真实连接 */ }
}

type RouteHandler = (url: string, init?: RequestInit) => unknown;

/** 可脚本化的 fetch 桩：按注册顺序匹配 `METHOD pathname?search`；返回对象→200 JSON，Response 原样透传 */
class FetchScript {
  calls: { method: string; url: string; path: string; body: unknown }[] = [];
  private routes: { re: RegExp; h: RouteHandler }[] = [];
  on(re: RegExp, h: RouteHandler): this { this.routes.push({ re, h }); return this; }
  callsTo(re: RegExp) { return this.calls.filter((c) => re.test(`${c.method} ${c.path}`)); }
  install(): this {
    const script = this;
    (globalThis as any).fetch = async (input: unknown, init?: RequestInit) => {
      const url = String(input);
      const u = new URL(url);
      const pathWithQuery = `${u.pathname}${u.search}`;
      const method = (init?.method ?? 'GET') as string;
      let body: unknown = init?.body;
      if (typeof body === 'string') { try { body = JSON.parse(body); } catch { /* 非 JSON 原文保留 */ } }
      script.calls.push({ method, url, path: pathWithQuery, body });
      for (const { re, h } of script.routes) {
        if (re.test(`${method} ${pathWithQuery}`)) {
          const out = await h(url, init);
          if (out instanceof Response) return out;
          return new Response(JSON.stringify(out ?? {}), { status: 200, headers: { 'Content-Type': 'application/json' } });
        }
      }
      throw new Error(`unexpected fetch(routes=${script.routes.length}): ${method} ${pathWithQuery}`);
    };
    return this;
  }
}

interface BootOpts {
  files?: Record<string, string>;
  config?: Record<string, unknown>;
  secrets?: Record<string, string>;
  buttons?: unknown[];
}

function boot(opts: BootOpts = {}): { root: string; globalStorage: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ca-scan-ws-'));
  const globalStorage = fs.mkdtempSync(path.join(os.tmpdir(), 'ca-scan-gs-'));
  tmps.push(root, globalStorage);
  for (const [rel, content] of Object.entries(opts.files ?? {})) {
    const abs = path.join(root, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, content, 'utf-8');
  }
  m().reset({
    root,
    globalStorage,
    config: opts.config,
    secrets: opts.secrets,
    buttons: opts.buttons,
  });
  activate(m().createContext());
  return { root, globalStorage };
}

async function until(cond: () => boolean, ms = 4000): Promise<void> {
  const start = Date.now();
  const check = (): boolean => { try { return cond(); } catch { return false; } };
  while (!check()) {
    if (Date.now() - start > ms) throw new Error('until 超时');
    await new Promise((r) => setTimeout(r, 15));
  }
}

// ───────────────────────────── 数据工厂 ─────────────────────────────

const TASK = 'gw-fresh01-ffffffffffffffff';
const BASE_TASK_ID = 'gw-base0001-bbbbbbbbbbbbbbbb';
const BASELINE_CREATED = '2026-09-01T08:30:00Z';

const baselineSummary = (gitAnchor?: GitAnchor) => ({
  task_id: BASE_TASK_ID,
  project_id: 'p1',
  scan_mode: 'SCAN_MODE_PARALLEL',
  sast_tools: [],
  status: 'TASK_STATUS_COMPLETED',
  created_at: BASELINE_CREATED,
  updated_at: null,
  error_message: '',
  ...(gitAnchor ? { git_anchor: gitAnchor } : {}),
});

const snapshot = (taskId: string, status: string, percent: number) => ({
  task: { task_id: taskId, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status, stages: [], error_message: '' },
  progress: { task_id: taskId, status, overall_percent: percent, stages: [] },
  logs: null,
  ai: null,
});

const page = (findings: unknown[]) => ({ findings, pagination: { next_cursor: '', has_next: false, total: findings.length } });

const cleanAnchor = (commit: string): GitAnchor => ({ commit, branch: 'main', dirty: false, remote: 'https://git.example/x.git' });

/** vscode.git 桩：HEAD.commit + 可选脏文件（路径须在工作区根内，relPath 才非空） */
const fakeRepo = (opts: { commit: string; dirtyFiles?: string[] }) => ({
  state: {
    HEAD: { commit: opts.commit, name: 'main' },
    workingTreeChanges: (opts.dirtyFiles ?? []).map((f) => ({ uri: vscode.Uri.file(f), status: 7 })),
    indexChanges: [],
    remotes: [{ name: 'origin', fetchUrl: 'https://git.example/x.git' }],
  },
  log: async () => [{ hash: opts.commit }],
});

/**
 * 平台路由：boot 恢复查询（GET /v1/tasks 第 1 发）返回空防误绑；
 * doScan 基线探测（第 2 发）返回 baselineTasks()。其余按扫描全链就绪态。
 */
function scriptWithBaseline(script: FetchScript, baselineTasks: () => unknown[]): void {
  let listCalls = 0;
  script.on(/GET \/v1\/tasks\?/, () => {
    listCalls++;
    return listCalls === 1 ? { tasks: [] } : { tasks: baselineTasks() };
  });
  script.on(/GET \/v1\/tools/, () => ({ tools: [] })); // doScan 连通性前置探测
  script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
  script.on(/GET \/v1\/findings/, () => page([]));
  script.on(/POST \/v1\/uploads\/archive/, () => ({ upload_id: 'up1', file_id: 'obj-1', size_bytes: 100 }));
  script.on(/POST \/v1\/tasks\/[\w-]+\/start/, () => ({}));
  script.on(/POST \/v1\/tasks$/, () => ({ task_id: TASK, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_PENDING', stages: [], error_message: '' }));
}

const messages = () => m().state().messages as { kind: string; msg: string; args?: unknown[] }[];
const hasMsg = (kind: string, frag = '') => messages().some((x) => x.kind === kind && x.msg.includes(frag));
const quickPickCalls = (): { items: any[]; opts?: { placeHolder?: string } }[] => m().state().quickPickCalls;
const uploadCalls = (script: FetchScript) => script.callsTo(/POST \/v1\/uploads\/archive/).length;
const createCalls = (script: FetchScript) => script.callsTo(/POST \/v1\/tasks$/).length;

/** 任务已启动的用例收尾：投递 COMPLETED 终态帧释放互斥，避免悬挂定时器外溢。
 *  全量终态文案是「CodeAudit 扫描完成：…」 */
async function finishTask(frag = '扫描完成：0 条发现'): Promise<void> {
  wsInstances[0]?.onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
  await until(() => hasMsg('info', frag));
}

const BOOT = {
  files: { 'a.py': 'line1\nold line\nline3\n' },
  config: { projectId: 'p1', minPackFiles: 1 },
  secrets: { 'codeaudit.refresh': 'r' },
};

// ───────────────────────────── 用例 ─────────────────────────────

describe('doScan 入口分支（验收 A19）', () => {
  beforeEach(() => {
    wsInstances = [];
    (globalThis as any).WebSocket = WsStub;
  });
  afterEach(() => {
    setRepoProviderForTest(async () => null); // git 桩复位：不外溢到后续用例/文件
    (globalThis as any).fetch = realFetch;
    for (const t of tmps.splice(0)) fs.rmSync(t, { recursive: true, force: true });
  });

  it('A19.1 有基线 → QuickPick 出现「增量扫描（基于 <id>·时间）」与「全量扫描」两项', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary()]);
    boot(BOOT);
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    // guardCmd 返回 void：executeCommand 不等待 doScan，用询问出现作为同步点
    await until(() => quickPickCalls().length === 1);
    await m().flush(); // pickQueue 空=Esc，等取消早退收敛
    const qps = quickPickCalls();
    assert.strictEqual(qps.length, 1, '基线存在时必须询问扫描方式');
    const [inc, full] = qps[0].items;
    assert.strictEqual(qps[0].items.length, 2, '三选=增量/全量/Esc，item 恰两项');
    assert.ok(inc.label.startsWith('增量扫描（基于 gw-base0'), `实际 ${inc.label}`);
    assert.ok(inc.label.includes('2026-09-01 08:3'), '基线时间应进标签（created_at 格式化）');
    assert.strictEqual(inc.incremental, true);
    assert.strictEqual(full.label, '全量扫描');
    assert.strictEqual(full.incremental, false);
    assert.ok(String(qps[0].opts?.placeHolder ?? '').includes('Esc 取消'), '占位文案明示 Esc 可取消');
    assert.strictEqual(inc.description, '', '非 git 工作区无变更预告行（布局不塌陷）');
    // pickQueue 空=Esc：不得走到上传/建任务
    assert.strictEqual(uploadCalls(script), 0);
    assert.strictEqual(createCalls(script), 0);
  });

  it('A19.2 选中增量 → createTask 请求体携带 incremental:true + git_anchor/diff_hint', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary()]);
    const { root } = boot(BOOT);
    setRepoProviderForTest(async () => fakeRepo({ commit: 'abc1234567890', dirtyFiles: [path.join(root, 'b.py')] }));
    await m().flush();
    m().state().pickQueue = [{ label: '增量扫描（基于 gw-base0…）', incremental: true }];
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => createCalls(script) === 1);
    const body = script.callsTo(/POST \/v1\/tasks$/)[0].body as Record<string, any>;
    assert.strictEqual(body.incremental, true, '增量选择必须落进请求体');
    assert.strictEqual(body.git_anchor?.commit, 'abc1234567890', 'git 锚点随增量载荷下发');
    assert.strictEqual(body.git_anchor?.dirty, true);
    assert.strictEqual(body.diff_hint, 'M\tb.py', '变更清单以 name-status 风格下发');
    assert.ok(!('baseline_task_id' in body), '自动选定基线：显式 baseline_task_id 不携带（ADR-225 契约）');
    wsInstances[0]?.onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => m().state().contexts['codeaudit.hasTask'] === false, 4000);
    // R58 锁定：终态收尾后完成口径通知必须可达（曾因 clearTaskUi 置 progress=null
    // 后复查守卫恒真早退而死代码）
    const done = m().state().messages.find((x: { msg: string }) => /CodeAudit 增量扫描完成/.test(x.msg));
    assert.ok(done, '增量完成口径通知必须显示（R58）');
    // 计数值取自平台快照（fixture 快照无增量元数据/锚点 → 全 0 属正常）；
    // 非零值与 @ 短 hash 的形态由真实快照承载，GUI 层（A20.1）验收
    assert.match(done.msg, /变更 \d+ · 删除 \d+ · 继承 \d+ · 新发现 \d+/);
  });

  it('A19.3 选中全量 → createTask 请求体零增量键（即使锚点已采到，回归口径与 apiClient.test 一致）', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary()]);
    boot(BOOT);
    setRepoProviderForTest(async () => fakeRepo({ commit: 'abc1234567890' })); // 锚点存在但不该下发
    await m().flush();
    m().state().pickQueue = [{ label: '全量扫描', incremental: false }];
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => createCalls(script) === 1);
    const body = script.callsTo(/POST \/v1\/tasks$/)[0].body as Record<string, unknown>;
    assert.ok(!('incremental' in body), '全量不得带 incremental');
    assert.ok(!('git_anchor' in body), '全量不得带 git_anchor');
    assert.ok(!('diff_hint' in body), '全量不得带 diff_hint');
    assert.ok(!('baseline_task_id' in body), '全量不得带 baseline_task_id');
    await finishTask();
  });

  it('A19.4 选中取消（Esc）→ 零上传零建任务，互斥复位可立即再次发起', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary()]);
    boot(BOOT);
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace'); // pickQueue 空 = Esc
    await until(() => quickPickCalls().length === 1);
    await m().flush(); // 取消早退收敛
    assert.strictEqual(uploadCalls(script), 0, '取消不得上传');
    assert.strictEqual(createCalls(script), 0, '取消不得建任务');
    assert.ok(!hasMsg('error'), '取消不是错误，无错误通知');
    assert.ok(!hasMsg('warn', '代码审计进行中'), '首次发起未被互斥拦截');
    // finally 复位收敛：第二次发起要能重新走到询问（scanning 残留的话会直接警告进行中且不再弹）
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => quickPickCalls().length === 2);
    await m().flush();
    assert.strictEqual(uploadCalls(script), 0, '第二次仍取消：依旧零上传');
    assert.strictEqual(createCalls(script), 0, '第二次仍取消：依旧零建任务');
  });

  it('A19.5 无基线（listTasks 空）→ 零询问直全量：QuickPick 未出现、任务创建发出', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => []);
    boot(BOOT);
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => createCalls(script) === 1); // 决策点已过：建任务已发出
    assert.strictEqual(quickPickCalls().length, 0, '无基线零询问');
    assert.strictEqual(uploadCalls(script), 1);
    const body = script.callsTo(/POST \/v1\/tasks$/)[0].body as Record<string, unknown>;
    assert.ok(!('incremental' in body), '直全量：请求体零增量键');
    await finishTask();
  });

  it('A19.5b 历史任务全非 COMPLETED → 视为无基线，同样直全量零询问', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [{ ...baselineSummary(), status: 'TASK_STATUS_RUNNING' }]);
    boot(BOOT);
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => createCalls(script) === 1);
    assert.strictEqual(quickPickCalls().length, 0, '非完成态基线不触发询问');
    const body = script.callsTo(/POST \/v1\/tasks$/)[0].body as Record<string, unknown>;
    assert.ok(!('incremental' in body));
    await finishTask();
  });

  it('A19.6 无变更预检：锚点一致且双方干净 → modal 提示；默认不放行（零上传零建任务）', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary(cleanAnchor('abc1234567890'))]);
    boot(BOOT);
    setRepoProviderForTest(async () => fakeRepo({ commit: 'abc1234567890' })); // 与基线同 commit 且干净
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace'); // buttonQueue 空 = 未点「仍然扫描」
    await until(() => messages().some((x) => x.msg.includes('无变更'))); // 预检决策点
    await m().flush(); // 未确认 → 取消早退收敛
    const entry = messages().find((x) => x.kind === 'info' && x.msg.includes('无变更'));
    assert.ok(entry, '应弹「无变更」提示');
    assert.ok(entry.msg.includes('代码较上次扫描'), `实际 ${entry.msg}`);
    assert.strictEqual(entry.args?.[0] && (entry.args[0] as { modal?: boolean }).modal, true, '预检是 modal 弹窗');
    assert.ok(entry.args?.includes('仍然扫描'), '提供「仍然扫描」按钮');
    assert.strictEqual(uploadCalls(script), 0, '未确认前不得上传');
    assert.strictEqual(createCalls(script), 0, '未确认前不得建任务');
    assert.strictEqual(quickPickCalls().length, 0, '预检先于扫描方式询问');
  });

  it('A19.6b 预检只是建议：点「仍然扫描」→ 放行到扫描方式询问', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary(cleanAnchor('abc1234567890'))]);
    boot({ ...BOOT, buttons: ['仍然扫描'] });
    setRepoProviderForTest(async () => fakeRepo({ commit: 'abc1234567890' }));
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => quickPickCalls().length === 1);
    await m().flush();
    assert.ok(hasMsg('info', '无变更'), '预检提示出现');
    assert.strictEqual(uploadCalls(script), 0, '仍停在方式询问（Esc）：零上传');
    assert.strictEqual(createCalls(script), 0);
  });

  it('A19.6c 预检不误报：commit 相同但当前工作区脏 → 不提示无变更，直接询问扫描方式', async () => {
    const script = new FetchScript().install();
    scriptWithBaseline(script, () => [baselineSummary(cleanAnchor('abc1234567890'))]); // 基线干净
    const { root } = boot(BOOT);
    setRepoProviderForTest(async () => fakeRepo({ commit: 'abc1234567890', dirtyFiles: [path.join(root, 'b.py')] })); // 当前脏
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => quickPickCalls().length === 1); // 决策点已过（预检在询问之前）
    assert.ok(!messages().some((x) => x.msg.includes('无变更')), 'dirty 内容可能不同：不得提示无变更');
    assert.strictEqual(uploadCalls(script), 0, '仍停在方式询问（Esc）：零上传');
  });

  it('B5-2 增量口径不跨任务串用：增量任务未收尾即切绑他任务，其完成弹全量口径（不误弹"增量扫描完成"）', async () => {
    const script = new FetchScript().install();
    const TASK_B = 'gw-hist222-222222222222222';
    const summaryB = { task_id: TASK_B, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_RUNNING', created_at: '', updated_at: null, error_message: '' };
    let listCalls = 0;
    script.on(/GET \/v1\/tasks\?/, () => {
      listCalls++;
      // 1=boot 恢复兜底（空）、2=doScan 基线探测（给基线→增量询问）、3=selectTask（给任务 B）
      return listCalls === 2 ? { tasks: [baselineSummary()] } : listCalls === 3 ? { tasks: [summaryB] } : { tasks: [] };
    });
    script.on(/GET \/v1\/tools/, () => ({ tools: [] }));
    script.on(/GET \/v1\/tasks\/gw-fresh01[\w-]*\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_RUNNING', 20)); // A 保持运行
    script.on(/GET \/v1\/tasks\/gw-hist222[\w-]*\/snapshot/, () => snapshot(TASK_B, 'TASK_STATUS_RUNNING', 60));
    script.on(/GET \/v1\/findings/, () => page([]));
    script.on(/POST \/v1\/uploads\/archive/, () => ({ upload_id: 'up1', file_id: 'obj-1', size_bytes: 100 }));
    script.on(/POST \/v1\/tasks\/[\w-]+\/start/, () => ({}));
    script.on(/POST \/v1\/tasks$/, () => ({ task_id: TASK, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_PENDING', stages: [], error_message: '' }));
    boot(BOOT);
    await m().flush();
    m().state().pickQueue = [{ label: '增量扫描（基于 gw-base0…）', incremental: true }];
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    // 增量任务 A 运行中即切绑历史任务 B（A 非终态 → B2-5 确认门）
    m().state().pickQueue = [{ label: 'B', description: '', task: summaryB }];
    m().state().buttonQueue = ['确认切换'];
    void vscode.commands.executeCommand('codeaudit.selectTask');
    await until(() => hasMsg('info', `已绑定任务 ${TASK_B.slice(0, 8)}`), 4000);
    // B（从未走过增量选择）终态完成：必须是全量口径通知——A 残留的 incrementalRequested
    // 若不被入口复位，会让这里误弹「增量扫描完成」（B5-2）
    wsInstances[1].onmessage?.({ data: JSON.stringify(snapshot(TASK_B, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'), 4000);
    assert.ok(!messages().some((x) => /增量扫描完成/.test(x.msg)), '非增量任务的完成通知不得带增量口径');
  });

  it('增量元数据拉取失败：完成通知如实标注统计不可得，不展示误导性「变更 0」（回归锁）', async () => {
    const script = new FetchScript().install();
    let listCalls = 0;
    let snapCalls = 0;
    script.on(/GET \/v1\/tasks\?/, () => {
      listCalls++;
      // 1=boot 恢复兜底（空）、2=doScan 基线探测（给基线→增量询问）
      return listCalls === 2 ? { tasks: [baselineSummary()] } : { tasks: [] };
    });
    script.on(/GET \/v1\/tools/, () => ({ tools: [] }));
    // 第 1 发=watcher 首轮兜底轮询（RUNNING）；其后=terminal 收尾的增量元数据拉取 → 500
    script.on(/GET \/v1\/tasks\/gw-fresh01[\w-]*\/snapshot/, () => {
      snapCalls++;
      return snapCalls === 1
        ? snapshot(TASK, 'TASK_STATUS_RUNNING', 20)
        : new Response(JSON.stringify({ error: 'boom' }), { status: 500, headers: { 'Content-Type': 'application/json' } });
    });
    script.on(/GET \/v1\/findings/, () => page([]));
    script.on(/POST \/v1\/uploads\/archive/, () => ({ upload_id: 'up1', file_id: 'obj-1', size_bytes: 100 }));
    script.on(/POST \/v1\/tasks\/[\w-]+\/start/, () => ({}));
    script.on(/POST \/v1\/tasks$/, () => ({ task_id: TASK, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_PENDING', stages: [], error_message: '' }));
    boot(BOOT);
    await m().flush();
    m().state().pickQueue = [{ label: '增量扫描（基于 gw-base0…）', incremental: true }];
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    // 增量任务完成：元数据（变更/删除数）拉取 500 → 通知不得展示「变更 0 · 删除 0」（误导为真无变更）
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => messages().some((x) => /增量扫描完成/.test(x.msg)), 4000);
    const inc = messages().find((x) => /增量扫描完成/.test(x.msg))!;
    assert.ok(!/变更 0/.test(inc.msg), '元数据不可得时不得展示「变更 0」（会被读成"确认无变更"）');
    assert.ok(/统计/.test(inc.msg) && /失败|不可得/.test(inc.msg), '如实标注增量统计拉取失败');
  });
});
