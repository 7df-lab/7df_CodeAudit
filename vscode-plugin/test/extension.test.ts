// extension.ts 胶水层行为测试：用 test/mocks/vscode.js 内存桩 + 可控 fetch/WebSocket
// 驱动 activate() 全流程（修复/回滚/扫描/恢复/安全禁闭/宿主交互）。
// 依据：docs/external-interfaces.md §4-5、docs/internal-interfaces.md §9、docs/data-flows.md §4。
// 历史上该文件零测试覆盖（tsconfig.test.json 曾显式排除），是修复/回滚类缺陷的最大盲区。
import * as assert from 'assert';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as vscode from 'vscode';
import { activate } from '../src/extension';
import type { UnifiedFinding } from '../src/types';

// ───────────────────────────── 基础设施 ─────────────────────────────

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

function fakeWebview() {
  const posted: any[] = [];
  let recv: ((x: unknown) => void) | null = null;
  let htmlCount = 0;
  const view: any = {
    visible: true,
    show: () => undefined,
    onDidChangeVisibility: () => ({ dispose() {} }),
    onDidDispose: () => undefined,
    webview: {
      options: {},
      onDidReceiveMessage: (cb: (x: unknown) => void) => { recv = cb; },
      postMessage: (x: unknown) => { posted.push(x); return Promise.resolve(true); },
    },
  };
  let html = '';
  Object.defineProperty(view.webview, 'html', {
    get: () => html,
    set: (v: string) => { htmlCount++; html = v; },
  });
  return { view, posted, htmlCount: () => htmlCount, dispatch: (x: unknown) => recv?.(x) };
}

interface BootOpts {
  files?: Record<string, string>;
  config?: Record<string, unknown>;
  secrets?: Record<string, string>;
  workspaceState?: Record<string, string>;
  registry?: unknown[];
  inputs?: unknown[];
  picks?: unknown[];
  buttons?: unknown[];
}

function boot(opts: BootOpts = {}): { root: string; globalStorage: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ca-ws-'));
  const globalStorage = fs.mkdtempSync(path.join(os.tmpdir(), 'ca-gs-'));
  tmps.push(root, globalStorage);
  for (const [rel, content] of Object.entries(opts.files ?? {})) {
    const abs = path.join(root, rel);
    fs.mkdirSync(path.dirname(abs), { recursive: true });
    fs.writeFileSync(abs, content, 'utf-8');
  }
  if (opts.registry) {
    fs.writeFileSync(path.join(globalStorage, 'fix-registry.json'), JSON.stringify(opts.registry), 'utf-8');
  }
  m().reset({
    root,
    globalStorage,
    config: opts.config,
    secrets: opts.secrets,
    workspaceState: opts.workspaceState,
    inputs: opts.inputs,
    picks: opts.picks,
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

const TASK = 'gw-abc12345-1234567890abcdef';

const finding = (over: Partial<UnifiedFinding> = {}): UnifiedFinding => ({
  finding_id: 'f1',
  task_id: TASK,
  project_id: 'p1',
  source_tool: 'semgrep',
  source_rule_id: 'R1',
  cwe_id: 'CWE-89',
  title: 'SQL 注入',
  description: '拼接构造 SQL',
  severity: 'SEVERITY_HIGH',
  confidence: 0.8,
  ai_verdict: 'AI_VERDICT_LIKELY_TRUE',
  ai_confidence: 0.95,
  ai_reasoning: 'r',
  ai_fix_suggestion: '',
  diff_patch: '',
  location: { file_path: 'a.py', start_line: 2 },
  dedup_group: '',
  is_unique: true,
  ...over,
});

const snapshot = (taskId: string, status: string, percent: number, logs?: unknown[]) => ({
  task: { task_id: taskId, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status, stages: [], error_message: '' },
  progress: { task_id: taskId, status, overall_percent: percent, stages: [] },
  logs: logs ? { logs } : null,
  ai: null,
});

const page = (findings: UnifiedFinding[]) => ({ findings, pagination: { next_cursor: '', has_next: false, total: findings.length } });

const UPDATE_PATCH = ['*** Begin Patch', '*** Update File: a.py', '@@', '-old line', '+new line', '*** End Patch'].join('\n');
const FENCE = (rel: string, oldLine: string, newLine: string) =>
  `建议如下：\n\`\`\`diff\n--- a/${rel}\n+++ b/${rel}\n@@ -1,3 +1,3 @@\n line1\n-${oldLine}\n+${newLine}\n line3\n\`\`\`\n说明文字`;

// ───────────────────────────── 断言助手 ─────────────────────────────

const diskRead = (root: string, rel: string) => fs.readFileSync(path.join(root, rel), 'utf-8');
const messages = () => m().state().messages as { kind: string; msg: string }[];
const hasMsg = (kind: string, frag = '') => messages().some((x) => x.kind === kind && x.msg.includes(frag));
const registry = (globalStorage: string): any[] => {
  const p = path.join(globalStorage, 'fix-registry.json');
  return fs.existsSync(p) ? JSON.parse(fs.readFileSync(p, 'utf-8')) : [];
};
const checkpointIds = (globalStorage: string): string[] => {
  const dir = path.join(globalStorage, 'checkpoints');
  return fs.existsSync(dir) ? fs.readdirSync(dir).filter((d) => d.startsWith('cp-')) : [];
};

/** 平台路由：restoreLastTask 兜底查询 + snapshot/findings/uploads/create/start（按需覆写） */
function scriptScan(script: FetchScript, opts: {
  findings?: UnifiedFinding[];
  upload?: unknown;
  uploadHandler?: RouteHandler;
  snapshots?: RouteHandler;
} = {}): void {
  script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
  script.on(/GET \/v1\/tools/, () => ({ tools: [] })); // doScan 连通性前置探测
  script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, opts.snapshots ?? (() => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)));
  script.on(/GET \/v1\/findings/, () => page(opts.findings ?? []));
  script.on(/POST \/v1\/uploads\/archive/, opts.uploadHandler ?? (() => opts.upload ?? { upload_id: 'up1', file_id: 'obj-1', size_bytes: 100 }));
  script.on(/POST \/v1\/tasks\/[\w-]+\/start/, () => ({}));
  script.on(/POST \/v1\/tasks$/, () => ({ task_id: TASK, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_PENDING', stages: [], error_message: '' }));
}

/** 带已完成历史任务的工作区（恢复链路就绪态）：findings 已渲染 */
async function bootRestored(): Promise<{ root: string; globalStorage: string }> {
  const script = new FetchScript().install();
  script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
  script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
  script.on(/GET \/v1\/findings/, () => page([
    finding(),
    finding({ finding_id: 'f2', title: 'XSS', cwe_id: 'CWE-79', location: { file_path: 'a.py', start_line: 3 } }),
  ]));
  const ws = boot({
    files: { 'a.py': 'line1\nold line\nline3\n' },
    secrets: { 'codeaudit.refresh': 'r' },
    workspaceState: { 'codeaudit.lastTaskId': TASK },
  });
  await m().flush();
  await until(() => (m().state().diagnosticsCollections[0]?.all.length ?? 0) > 0);
  return ws;
}

// ───────────────────────────── 用例 ─────────────────────────────

describe('extension.ts 胶水层（内存桩行为测试）', () => {
  beforeEach(() => {
    wsInstances = [];
    (globalThis as any).WebSocket = WsStub;
  });
  afterEach(() => {
    (globalThis as any).fetch = realFetch;
    for (const t of tmps.splice(0)) fs.rmSync(t, { recursive: true, force: true });
  });

  // ── 启动与登录 ──

  it('桩 Uri.joinPath file 产物跟随宿主平台形态且本机 fs 可读（双环境回归锁）', async () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ca-joinpath-'));
    try {
      fs.writeFileSync(path.join(root, 'a.py'), 'x', 'utf-8');
      const uri = vscode.Uri.joinPath(vscode.Uri.file(root), 'a.py');
      assert.strictEqual(uri.fsPath, path.join(root, 'a.py'), 'fsPath 按宿主平台 path.join 拼接（Windows 产反斜杠形态 / POSIX 产正斜杠形态）');
      assert.strictEqual(fs.readFileSync(uri.fsPath, 'utf-8'), 'x', '产物路径可被本机 fs 直接读取（applyMachinePatch 预读链路依赖）');
      if (path.sep !== '\\') {
        assert.ok(!uri.fsPath.includes('\\'), 'POSIX 宿主不得产出反斜杠路径（win32.join 硬编码会让 fs.readFileSync 恒 ENOENT）');
      } else {
        assert.ok(uri.fsPath.includes('\\'), 'Windows 宿主产物须为反斜杠形态（真实 VS Code fsPath 语义）');
      }
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it('boot：未登录 → loggedIn=false，状态栏提示登录', async () => {
    boot();
    await m().flush();
    assert.strictEqual(m().state().contexts['codeaudit.loggedIn'], false);
    assert.ok(m().state().statusBars.some((i: any) => String(i.text).includes('未登录')));
  });

  it('登录成功：serverUrl 落配置、loggedIn=true、信息通知', async () => {
    const script = new FetchScript().install();
    script.on(/POST \/v1\/auth\/login/, () => ({ access_token: 'A', refresh_token: 'R', expires_in_s: 1800 }));
    boot({ inputs: ['http://gw:8080/', 'admin', 'pw'] });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.login');
    assert.strictEqual(m().state().contexts['codeaudit.loggedIn'], true);
    assert.ok(hasMsg('info', '登录成功'));
    assert.strictEqual(m().state().config.serverUrl, 'http://gw:8080/');
    assert.ok(script.calls.some((c) => c.method === 'POST' && /\/v1\/auth\/login/.test(c.url)));
  });

  it('登录空网关地址被拒：警告、不写配置、零登录请求（回归锁）', async () => {
    const script = new FetchScript().install();
    boot({ inputs: ['  ', 'admin', 'pw'] });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.login');
    await until(() => hasMsg('warn', '网关地址不能为空'));
    assert.strictEqual(script.calls.length, 0, '空地址在发起任何请求前拦截');
    assert.notStrictEqual(m().state().config.serverUrl, '  ', '配置未被空值覆写');
  });

  it('登录失败：错误通知，loggedIn 保持 false', async () => {
    new FetchScript().install().on(/POST \/v1\/auth\/login/, () => new Response('{"error":"bad"}', { status: 401 }));
    boot({ inputs: ['http://gw:8080', 'admin', 'pw'] });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.login');
    assert.ok(hasMsg('error', '登录失败'));
    assert.strictEqual(m().state().contexts['codeaudit.loggedIn'], false);
  });

  it('登录成功（expires_in_s 字符串形态，B8 protojson int64）：有效期按数值归一展示', async () => {
    const script = new FetchScript().install();
    script.on(/POST \/v1\/auth\/login/, () => ({ access_token: 'A', refresh_token: 'R', expires_in_s: '1800' }));
    boot({ inputs: ['http://gw:8080', 'admin', 'pw'] });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.login');
    assert.ok(hasMsg('info', '有效期 30min'), `expires_in_s='1800' 必须归一为 30min（实际 ${messages().map((x) => x.msg).join(' | ')}）`);
    assert.strictEqual(m().state().contexts['codeaudit.loggedIn'], true);
  });

  it('主动登出静默：清凭据但不弹"登录会话已失效"，保留"已退出"提示（回归锁 B5-2）', async () => {
    const script = new FetchScript().install();
    script.on(/POST \/v1\/auth\/logout/, () => ({}));
    boot({ secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    assert.strictEqual(m().state().contexts['codeaudit.loggedIn'], true, '前置：凭据在→已登录态');
    await vscode.commands.executeCommand('codeaudit.logout');
    await until(() => !m().state().secrets.has('codeaudit.refresh')); // clear 已收敛
    await m().flush();
    assert.ok(hasMsg('info', '已退出 CodeAudit 登录'), '登出自己的提示保留');
    assert.ok(!hasMsg('warn', '登录会话已失效'), '主动登出不得触发会话失效告警（B5-2）');
    assert.strictEqual(m().state().contexts['codeaudit.loggedIn'], false);
    assert.strictEqual(script.callsTo(/POST \/v1\/auth\/logout/).length, 1, '平台登出请求照发');
  });

  // ── 扫描链路 ──

  it('doScan 全链：upload_file_id 契约下发 + 终态拉取发现并渲染（诊断/树/状态栏）', async () => {
    const script = new FetchScript().install();
    scriptScan(script, { findings: [finding(), finding({ finding_id: 'f2', title: 'XSS', cwe_id: 'CWE-79' })] });
    boot({
      files: { 'a.py': 'line1\nold line\nline3\n', 'lib/util.py': 'x = 1\n' },
      config: { projectId: 'p1', minPackFiles: 1 },
      secrets: { 'codeaudit.refresh': 'r' },
    });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('info', '代码审计已开始'));
    const create = script.callsTo(/POST \/v1\/tasks$/)[0];
    assert.strictEqual((create.body as any).config.upload_file_id, 'obj-1', '新平台契约：桶内对象锚点走 upload_file_id');
    assert.ok(!('project_path' in (create.body as any).config), '不得误带 project_path（32B 空包历史事故，regressions.md #6）');
    await until(() => hasMsg('info', '扫描完成：2 条发现'));
    const s = m().state();
    const all = s.diagnosticsCollections[0].all as [unknown, any[]][];
    assert.strictEqual(all.length, 1, 'a.py 一组诊断');
    assert.strictEqual(all[0][1].length, 2);
    assert.strictEqual(all[0][1][0].source, 'CodeAudit');
    const tree = s.treeProviders['codeaudit.findings'];
    assert.ok(tree.roots.length > 0, 'findings 应构建侧栏树');
    assert.ok(s.statusBars.some((i: any) => String(i.text).includes('2 发现')));
    assert.strictEqual(s.contexts['codeaudit.taskRunning'], false, '终态后互斥释放');
    assert.ok(s.statusBars.some((i: any) => i.command === 'codeaudit.scanWorkspace' && String(i.text).includes('扫描')), '空闲快捷按钮复位');
  });

  it('doScan：file_id/dir 都缺 → 中止且不建任务（回归锁：空引用下发空包链路）', async () => {
    const script = new FetchScript().install();
    scriptScan(script, { upload: { upload_id: 'up1' } });
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('error', '上传响应缺少 file_id/dir'));
    assert.strictEqual(script.callsTo(/POST \/v1\/tasks$/).length, 0);
  });

  it('doScan：minPackFiles 阈值中止（默认 10，工作区仅 1 文件），不发起上传（回归锁：防空包）', async () => {
    const script = new FetchScript().install();
    scriptScan(script);
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1' }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('error', '打包清单仅 1 个文件'));
    assert.strictEqual(script.callsTo(/uploads/).length, 0);
  });

  it('打包清单命中 20000 上限：弹警告建议收紧 excludes；未确认（Esc）→ 零上传零建任务且互斥复位（回归锁 B5-2）', async () => {
    const script = new FetchScript().install();
    scriptScan(script, { findings: [] });
    const { root } = boot({ config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    // mock findFiles 按 max 截断：恰好 20000 个文件 → 命中上限（清单被截断）
    for (let i = 0; i < 20000; i++) fs.writeFileSync(path.join(root, `f${i}.txt`), '');
    await m().flush();
    const capWarns = () => messages().filter((x) => x.kind === 'warn' && x.msg.includes('20000 个文件上限')).length;
    void vscode.commands.executeCommand('codeaudit.scanWorkspace'); // buttonQueue 空 = Esc 取消
    await until(() => capWarns() === 1, 8000);
    await m().flush(); // 取消早退收敛
    assert.ok(messages().some((x) => x.kind === 'warn' && x.msg.includes('收紧 codeaudit.excludeGlobs')), '警告须给出收紧 excludes 的出路');
    assert.strictEqual(script.callsTo(/POST \/v1\/uploads\/archive/).length, 0, '未确认不得上传');
    assert.strictEqual(script.callsTo(/POST \/v1\/tasks$/).length, 0, '未确认不得建任务');
    // 互斥复位：再次发起必须重新走到上限警告（残留会直接被"进行中"拦截且不再弹）
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => capWarns() === 2, 8000);
    await m().flush();
    assert.strictEqual(script.callsTo(/uploads/).length, 0, '两次取消：依旧零上传');
  }).timeout(20000);

  it('打包清单命中 20000 上限：确认「继续打包」→ 按当前清单走完上传/建任务（B5-2）', async () => {
    const script = new FetchScript().install();
    scriptScan(script, { findings: [] });
    const { root } = boot({ config: { projectId: 'p1', minPackFiles: 1 }, buttons: ['继续打包'], secrets: { 'codeaudit.refresh': 'r' } });
    for (let i = 0; i < 20000; i++) fs.writeFileSync(path.join(root, `f${i}.txt`), '');
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => script.callsTo(/POST \/v1\/uploads\/archive/).length === 1, 15000);
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
  }).timeout(30000);

  it('doScan：未登录 → 警告，无网络请求', async () => {
    const script = new FetchScript().install();
    scriptScan(script);
    boot({ files: { 'a.py': 'x\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    assert.ok(hasMsg('warn', '请先执行 CodeAudit: 登录平台'));
    assert.strictEqual(script.calls.length, 0);
  });

  it('扫描互斥：进行中重复发起被拒绝（回归锁：重复消耗平台沙箱）', async () => {
    const script = new FetchScript().install();
    let release!: (v: unknown) => void;
    const gate = new Promise((r) => { release = r; });
    scriptScan(script, { uploadHandler: () => gate });
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    const first = vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => script.callsTo(/uploads/).length === 1);
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    assert.ok(hasMsg('warn', '代码审计进行中'));
    release({ upload_id: 'up', file_id: 'obj' });
    await first;
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
  });

  it('扫描互斥竞态：第一次卡在连通性探测（listTools 往返）中时第二次发起即被拒，仅一次上传（回归锁 B2-1 双上传）', async () => {
    const script = new FetchScript().install();
    let releaseTools!: (v: unknown) => void;
    const gate = new Promise((r) => { releaseTools = r; });
    let toolsCalls = 0;
    script.on(/GET \/v1\/tools/, () => {
      toolsCalls++;
      return toolsCalls === 1 ? gate : { tools: [] };
    });
    scriptScan(script, { findings: [] });
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    void vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => toolsCalls === 1, 4000); // 第一次卡在 listTools 网络往返中（旧实现此刻 scanning 仍为 false）
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    assert.ok(hasMsg('warn', '代码审计进行中'), '并发第二发必须被互斥拒绝');
    assert.strictEqual(script.callsTo(/POST \/v1\/uploads\/archive/).length, 0, '第二发不得进入上传');
    releaseTools({ tools: [] });
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    assert.strictEqual(script.callsTo(/POST \/v1\/uploads\/archive/).length, 1, '全链只允许一次上传');
    assert.strictEqual(toolsCalls, 1, '被拒的第二发不得重复探测连通性');
    // 投递 COMPLETED 终态收尾，避免悬挂定时器
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
  });

  it('扫描早退复位互斥：连通性失败（503）后再发起可正常走完整链（回归锁 B2-1 早退复位）', async () => {
    const script = new FetchScript().install();
    let toolsCalls = 0;
    script.on(/GET \/v1\/tools/, () => {
      toolsCalls++;
      return toolsCalls === 1 ? new Response('{"error":"unavailable"}', { status: 503 }) : { tools: [] };
    });
    scriptScan(script, { findings: [] });
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('error', '无法连接平台'));
    // 早退必须复位互斥：第二次发起要能重新探测并走完整链路（若某条早退路径漏复位，此处卡死）
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    assert.strictEqual(toolsCalls, 2);
    assert.strictEqual(script.callsTo(/POST \/v1\/uploads\/archive/).length, 1);
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
  });

  it('沙箱收包校验：空包（32B）→ 立即 cancelTask + 明确归因（回归锁：空包白审 0 发现）', async () => {
    const script = new FetchScript().install();
    let snaps = 0;
    scriptScan(script, {
      snapshots: () => {
        snaps++;
        return snaps === 1
          ? snapshot(TASK, 'TASK_STATUS_RUNNING', 5, [{ log_id: '1', task_id: TASK, ts_ms: '1', level: 'TASK_LOG_LEVEL_INFO', source: 'sandbox', message: '项目打包完成 /tmp/x（32 字节）' }])
          : snapshot(TASK, 'TASK_STATUS_COMPLETED', 100);
      },
    });
    script.on(/POST \/v1\/tasks\/[\w-]+\/cancel/, () => ({}));
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('error', '沙箱收到的项目包仅 32'));
    assert.strictEqual(script.callsTo(/POST \/v1\/tasks\/[\w-]+\/cancel/).length, 1, '应立即取消任务且只判一次');
    // 投递 COMPLETED 终态帧收尾，避免悬挂定时器
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
  });

  it('旧任务终态收尾 TOCTOU：listFindings await 期间切绑其他任务，恢复执行后复查归属放弃收尾（回归锁 B2-2）', async () => {
    const script = new FetchScript().install();
    const TASK_B = 'gw-old9999-bbbbbbbbbbbbbbbb';
    const summaryB = { task_id: TASK_B, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_COMPLETED', created_at: '', updated_at: null, error_message: '' };
    const findingB = finding({ finding_id: 'fb', task_id: TASK_B, title: 'B 任务发现' });
    let releaseFindingsA!: (v: unknown) => void;
    const gate = new Promise((r) => { releaseFindingsA = r; });
    // 注册顺序即匹配优先级：任务专属路由先于 scriptScan 的通配路由
    script.on(/GET \/v1\/findings\?task_id=gw-old9999/, () => page([findingB]));
    script.on(/GET \/v1\/findings\?task_id=gw-abc12345/, () => gate); // 任务 A 的收尾拉取挂起
    script.on(/GET \/v1\/tasks\/gw-old9999[\w-]*\/snapshot/, () => snapshot(TASK_B, 'TASK_STATUS_COMPLETED', 100));
    let listTasksCalls = 0;
    script.on(/GET \/v1\/tasks\?/, () => {
      listTasksCalls++;
      // 1=boot 恢复兜底查询、2=doScan 增量基线探测：都给空；3=selectTask：给任务 B
      //（若第 2 次就给 B，扫描会弹增量/全量 QuickPick 把链路挂住）
      return listTasksCalls === 3 ? { tasks: [summaryB] } : { tasks: [] };
    });
    scriptScan(script, { findings: [] });
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    // watcher 首轮轮询即得 COMPLETED → terminal → 收尾卡在 listFindings(A)
    await until(() => script.callsTo(/GET \/v1\/findings\?task_id=gw-abc12345/).length === 1, 4000);
    // 挂起窗口内切绑任务 B（A 终态、无扫描发起 → 不触发 B2-5 确认门）
    m().state().pickQueue = [{ label: 'B', description: '', task: summaryB }];
    await vscode.commands.executeCommand('codeaudit.selectTask');
    await until(() => hasMsg('info', '已绑定任务 gw-old99'), 4000);
    // 释放 gate：旧任务收尾恢复执行——await 后复查归属失败，必须整体放弃
    releaseFindingsA(page([finding({ finding_id: 'fa' })]));
    await m().flush();
    await m().flush();
    assert.ok(!hasMsg('info', 'CodeAudit 扫描完成'), '旧任务收尾不得再发完成通知/渲染旧结果');
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK_B, '绑定指针不得被旧任务收尾夺回');
    assert.strictEqual(m().state().contexts['codeaudit.hasTask'], true, '旧任务收尾不得 clearTaskUi 误清新任务界面');
    const roots = m().state().treeProviders['codeaudit.findings'].roots;
    const findingNode = roots.find((n: any) => n.kind === 'finding');
    assert.ok(findingNode, '结果树仍是新绑定任务 B 的发现');
    assert.strictEqual(findingNode.finding.finding_id, 'fb', '树上发现是新任务的 fb（不得被旧任务 fa 覆盖）');
  });

  it('旧 watcher 在途 404/迟到的 WS 1011：新任务 progress 不被标 DEAD、互斥不被误释（回归锁 B5-1）', async () => {
    const script = new FetchScript().install();
    const TASK_A = 'gw-oldaaaa-aaaaaaaaaaaaaaaa';
    const TASK_C = 'gw-newcccc-cccccccccccccccc';
    const summaryC = { task_id: TASK_C, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_PENDING', stages: [], error_message: '' };
    // 任务 A：第 1 发=boot bindTask 快照（RUNNING 立即回）；第 2 发=watcher A 首轮轮询（挂起，稍后 404）
    let snapA = 0;
    let releaseSnapA!: (v: unknown) => void;
    const gateA = new Promise((r) => { releaseSnapA = r; });
    script.on(/GET \/v1\/tasks\/gw-oldaaaa[\w-]*\/snapshot/, () => {
      snapA++;
      return snapA === 1 ? snapshot(TASK_A, 'TASK_STATUS_RUNNING', 42) : gateA;
    });
    script.on(/GET \/v1\/tasks\/gw-newcccc[\w-]*\/snapshot/, () => snapshot(TASK_C, 'TASK_STATUS_RUNNING', 10));
    script.on(/GET \/v1\/findings/, () => page([]));
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] })); // 无基线：doScan 直全量零询问
    script.on(/GET \/v1\/tools/, () => ({ tools: [] }));
    script.on(/POST \/v1\/uploads\/archive/, () => ({ upload_id: 'up1', file_id: 'obj-1', size_bytes: 100 }));
    script.on(/POST \/v1\/tasks\/[\w-]+\/start/, () => ({}));
    script.on(/POST \/v1\/tasks$/, () => summaryC);
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK_A } });
    await m().flush();
    // boot 已绑定运行中的任务 A（watcher A 活跃），其首轮轮询在途（gateA 挂起）
    await until(() => snapA === 2, 4000);
    assert.strictEqual(wsInstances.length, 1, 'watcher A 已建立');
    // 在途窗口内发起新扫描：watchTask 替换并 close 旧 watcher A（scanning 保持占用）
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('info', '代码审计已开始'), 4000);
    assert.strictEqual(wsInstances.length, 2, 'watcher A→C 已替换');
    // 挑衅 1：旧 watcher 的在途轮询以 404 not found 返回（旧任务被平台删除）
    releaseSnapA(new Response(JSON.stringify({ error: 'task not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } }));
    await m().flush();
    await m().flush();
    // 挑衅 2：迟到的 WS 1011 close 帧（服务端已发出的删除通知，close() 后仍送达）
    wsInstances[0].onclose?.({ code: 1011, reason: `task ${TASK_A} not found` });
    await m().flush();
    await m().flush();
    assert.ok(!hasMsg('warn', '已在平台删除或归档'), '旧任务的 onTaskGone 不得弹"已删除"警告');
    assert.strictEqual(m().state().contexts['codeaudit.taskRunning'], true, '新任务运行态上下文不得被误清');
    assert.ok(m().state().statusBars.some((i: any) => String(i.text).includes('10%')), '新任务进度仍为 10%（不得被标 DEAD 回落空闲/发现态）');
    // scanning 互斥未被误释：再次发起扫描必须仍被"进行中"拦截（误释会双沙箱消耗）
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => hasMsg('warn', '代码审计进行中'), 2000);
    assert.strictEqual(script.callsTo(/POST \/v1\/uploads\/archive/).length, 1, '互斥生效：第二发不得再上传');
    // 收尾：任务 C COMPLETED，释放互斥与定时器
    wsInstances[1].onmessage?.({ data: JSON.stringify(snapshot(TASK_C, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
  });

  // ── 修复主路径（apply_patch 机器补丁）──

  it('机器补丁 Update：落盘 + checkpoint + 登记 + diff 审阅 + 已修复通知', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ diff_patch: UPDATE_PATCH }));
    await until(() => diskRead(root, 'a.py') === 'line1\nnew line\nline3\n');
    assert.ok(hasMsg('info', 'AI 已修复'));
    assert.ok(m().state().executed.some((e: any) => e.id === 'vscode.diff'), '应打开修复前后 diff 审阅');
    const cps = checkpointIds(globalStorage);
    assert.strictEqual(cps.length, 1);
    const manifest = JSON.parse(fs.readFileSync(path.join(globalStorage, 'checkpoints', cps[0], 'manifest.json'), 'utf-8'));
    assert.ok(manifest[path.join(root, 'a.py')], 'manifest 以绝对路径为键');
    const recs = registry(globalStorage);
    assert.strictEqual(recs.length, 1);
    assert.strictEqual(recs[0].state, 'applied');
    assert.strictEqual(recs[0].findingId, 'f1');
    assert.strictEqual(recs[0].checkpointId, cps[0]);
    assert.deepStrictEqual(recs[0].files, [path.join(root, 'a.py')]);
  });

  it('路径禁闭：.. 逃逸与绝对路径整体拒绝——工作区不变、无 checkpoint、无登记（回归锁：补丁路径逃逸）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ diff_patch: UPDATE_PATCH.replace('File: a.py', 'File: ../outside.py') }));
    await until(() => hasMsg('error', '越界路径'));
    assert.strictEqual(diskRead(root, 'a.py'), 'line1\nold line\nline3\n');
    assert.strictEqual(checkpointIds(globalStorage).length, 0);
    assert.strictEqual(registry(globalStorage).length, 0);
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'f2', diff_patch: UPDATE_PATCH.replace('File: a.py', 'File: /etc/passwd') }));
    await until(() => messages().filter((x) => x.kind === 'error' && x.msg.includes('越界路径')).length >= 2);
    assert.ok(!fs.existsSync(path.join(path.dirname(root), 'outside.py')), '工作区外未被写');
    assert.strictEqual(registry(globalStorage).length, 0);
  });

  it('Add 目标已存在 → 拒绝覆盖（回归锁：覆盖既有文件）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'x\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ diff_patch: '*** Begin Patch\n*** Add File: a.py\n+hi\n*** End Patch' }));
    await until(() => hasMsg('error', '拒绝覆盖'));
    assert.strictEqual(diskRead(root, 'a.py'), 'x\n');
    assert.strictEqual(checkpointIds(globalStorage).length, 0);
  });

  it('Update 引用不存在的文件 → 整体拒绝（Missing File），无 checkpoint', async () => {
    const { globalStorage } = boot({ files: {} });
    const patch = '*** Begin Patch\n*** Update File: ghost.py\n@@\n-old\n+new\n*** End Patch';
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ diff_patch: patch }));
    await until(() => hasMsg('error', 'Missing File'));
    assert.strictEqual(checkpointIds(globalStorage).length, 0);
  });

  it('Delete+Add 多段补丁：应用后按发现回滚逐字节还原、登记翻 rolledback、可重新应用（回归锁：回滚不再可用）', async () => {
    const { root, globalStorage } = boot({ files: { 'old.py': 'old content\n' } });
    const patch = '*** Begin Patch\n*** Delete File: old.py\n*** Add File: fresh.py\n+new content\n*** End Patch';
    const f = finding({ diff_patch: patch });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', f);
    await until(() => diskRead(root, 'fresh.py') === 'new content');
    await until(() => !fs.existsSync(path.join(root, 'old.py')));
    // 树节点包装入口（asFinding 解包）驱动回滚
    await vscode.commands.executeCommand('codeaudit.rollbackFix', { kind: 'finding', finding: f });
    await until(() => diskRead(root, 'old.py') === 'old content\n', 4000);
    assert.ok(!fs.existsSync(path.join(root, 'fresh.py')), 'Add 目标回滚=删除');
    assert.strictEqual(registry(globalStorage)[0].state, 'rolledback');
    await vscode.commands.executeCommand('codeaudit.fixFinding', f);
    await until(() => diskRead(root, 'fresh.py') === 'new content');
    assert.strictEqual(registry(globalStorage)[0].state, 'applied');
    assert.strictEqual(checkpointIds(globalStorage).length, 2, '重新应用生成新 checkpoint');
  });

  it('Move to：源删目标建；目标已存在拒绝；回滚还原源、删除目标', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    const patch = '*** Begin Patch\n*** Update File: a.py\n*** Move to: b.py\n@@\n-old line\n+moved line\n*** End Patch';
    const f = finding({ diff_patch: patch });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', f);
    await until(() => diskRead(root, 'b.py') === 'line1\nmoved line\nline3\n');
    assert.ok(!fs.existsSync(path.join(root, 'a.py')), 'Move 源被删除');
    await vscode.commands.executeCommand('codeaudit.rollbackFix', f);
    await until(() => diskRead(root, 'a.py') === 'line1\nold line\nline3\n');
    assert.ok(!fs.existsSync(path.join(root, 'b.py')), 'Move 目标回滚=删除');
    assert.strictEqual(registry(globalStorage)[0].state, 'rolledback');

    // Move 目标已存在 → 拒绝覆盖
    boot({ files: { 'a.py': 'line1\nold line\nline3\n', 'b.py': 'exists\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'f3', diff_patch: patch }));
    await until(() => hasMsg('error', '拒绝覆盖'));
    assert.strictEqual(diskRead(root, 'a.py'), 'line1\nold line\nline3\n');
  });

  // ── 兜底路径（ai_fix_suggestion ```diff 围栏）──

  it('兜底路径：围栏 unified diff 应用 + 登记', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ diff_patch: '', ai_fix_suggestion: FENCE('a.py', 'old line', 'fixed line') }));
    await until(() => diskRead(root, 'a.py') === 'line1\nfixed line\nline3\n');
    assert.ok(hasMsg('info', 'AI 已修复'));
    assert.strictEqual(registry(globalStorage).length, 1);
  });

  it('兜底路径：save 失败 → 显式报错、磁盘不变、不登记（回归锁：修复未落盘即丢）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    m().state().saveFailOnce = true;
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ diff_patch: '', ai_fix_suggestion: FENCE('a.py', 'old line', 'fixed line') }));
    await until(() => hasMsg('error', '修复写入'));
    assert.strictEqual(diskRead(root, 'a.py'), 'line1\nold line\nline3\n');
    assert.strictEqual(registry(globalStorage).length, 0);
  });

  it('修复互斥：第一发卡在 fs 落盘时第二发被整体拒绝，完成后复位可再修（回归锁 B2-6 fixing 并发）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'x\n' } });
    const realWriteFile = fs.promises.writeFile.bind(fs.promises) as (...args: any[]) => any;
    let releaseWrite!: () => void;
    const gate = new Promise<void>((r) => { releaseWrite = r; });
    (fs.promises as any).writeFile = async (p: any, ...rest: any[]) => {
      if (String(p).endsWith('new1.py')) await gate; // 只挂起第一发补丁的落盘
      return realWriteFile(p, ...rest);
    };
    try {
      await m().flush();
      void vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'fx1', diff_patch: '*** Begin Patch\n*** Add File: new1.py\n+one\n*** End Patch' }));
      await until(() => checkpointIds(globalStorage).length === 1, 4000); // fx1 已建 checkpoint、卡在 fs 落盘
      await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'fx2', diff_patch: '*** Begin Patch\n*** Add File: new2.py\n+two\n*** End Patch' }));
      await until(() => hasMsg('error', '另一个修复正在应用中'), 4000);
      assert.ok(!fs.existsSync(path.join(root, 'new2.py')), '并发第二发不得改盘');
      assert.strictEqual(checkpointIds(globalStorage).length, 1, '并发第二发不得建 checkpoint');
      releaseWrite();
      await until(() => diskRead(root, 'new1.py') === 'one', 4000);
      await until(() => hasMsg('info', 'AI 已修复'), 4000);
      // 互斥复位：第三发正常应用
      await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'fx3', diff_patch: '*** Begin Patch\n*** Add File: new3.py\n+three\n*** End Patch' }));
      await until(() => diskRead(root, 'new3.py') === 'three', 4000);
    } finally {
      (fs.promises as any).writeFile = realWriteFile;
    }
  });

  it('写盘互斥扩展：修复卡在 fs 落盘时，回滚（rollbackFix/rollbackFixes）与围栏兜底修复均被拒、不改盘（回归锁 B2-6 扩展）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    // 预置一个已应用修复（f1 @ a.py）供回滚入口
    const f1 = finding({ diff_patch: UPDATE_PATCH });
    await vscode.commands.executeCommand('codeaudit.fixFinding', f1);
    await until(() => diskRead(root, 'a.py') === 'line1\nnew line\nline3\n');
    const fixedContent = 'line1\nnew line\nline3\n';
    // 挂起第二发修复（Add gated.py）的 fs 落盘 → 互斥占用中
    const realWriteFile = fs.promises.writeFile.bind(fs.promises) as (...args: any[]) => any;
    let releaseWrite!: () => void;
    const gate = new Promise<void>((r) => { releaseWrite = r; });
    (fs.promises as any).writeFile = async (p: any, ...rest: any[]) => {
      if (String(p).endsWith('gated.py')) await gate;
      return realWriteFile(p, ...rest);
    };
    try {
      void vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'fx1', diff_patch: '*** Begin Patch\n*** Add File: gated.py\n+one\n*** End Patch' }));
      await until(() => checkpointIds(globalStorage).length === 2, 4000); // f1 已建 1 个，fx1 建第 2 个后卡在 fs 落盘
      const rejections = () => messages().filter((x) => x.msg.includes('正在写盘')).length;
      // 回滚 f1：被拒——磁盘不动、登记仍 applied
      await vscode.commands.executeCommand('codeaudit.rollbackFix', f1);
      await until(() => rejections() >= 1, 4000);
      assert.strictEqual(diskRead(root, 'a.py'), fixedContent, '互斥期间回滚不得改盘（与修复交叠会互相踩 checkpoint/登记）');
      assert.strictEqual(registry(globalStorage)[0].state, 'applied', '登记不被翻 rolledback');
      // 回滚最近批量（rollbackFixes）：同样被拒
      await vscode.commands.executeCommand('codeaudit.rollbackFixes');
      await until(() => rejections() >= 2, 4000);
      assert.strictEqual(diskRead(root, 'a.py'), fixedContent);
      // 围栏兜底修复（改 a.py）：改盘段被拒
      await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'sug1', diff_patch: '', ai_fix_suggestion: FENCE('a.py', 'new line', 'replaced line') }));
      await until(() => rejections() >= 3, 4000);
      assert.strictEqual(diskRead(root, 'a.py'), fixedContent, '互斥期间兜底修复不得改盘');
      assert.strictEqual(registry(globalStorage).length, 1, '被拒路径不新增登记');
      // 释放后第一发完成，互斥复位
      releaseWrite();
      await until(() => diskRead(root, 'gated.py') === 'one', 4000);
      await until(() => hasMsg('info', 'AI 已修复'), 4000);
    } finally {
      (fs.promises as any).writeFile = realWriteFile;
    }
  });

  it('写盘互斥扩展：无登记兜底回滚（rollbackFixes→restoreLatest）同样被拒（回归锁 B2-6 扩展兜底分支）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'x\n' } });
    await m().flush();
    // 挂起一发修复：checkpoint 已建、登记未写（fs 成功后才登记）→ rollbackFixes 走无登记兜底
    const realWriteFile = fs.promises.writeFile.bind(fs.promises) as (...args: any[]) => any;
    let releaseWrite!: () => void;
    const gate = new Promise<void>((r) => { releaseWrite = r; });
    (fs.promises as any).writeFile = async (p: any, ...rest: any[]) => {
      if (String(p).endsWith('gated2.py')) await gate;
      return realWriteFile(p, ...rest);
    };
    try {
      void vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'fx9', diff_patch: '*** Begin Patch\n*** Add File: gated2.py\n+one\n*** End Patch' }));
      await until(() => checkpointIds(globalStorage).length === 1, 4000); // fx9 卡在 fs 落盘（无登记）
      await vscode.commands.executeCommand('codeaudit.rollbackFixes');
      await until(() => messages().some((x) => x.msg.includes('正在写盘')), 4000);
      assert.ok(!messages().some((x) => x.msg.includes('已回滚')), '互斥期间兜底回滚不得写盘（不得弹出回滚通知）');
      assert.ok(!fs.existsSync(path.join(root, 'gated2.py')), '挂起中的修复目标未被回滚删除');
      // 释放后第一发正常完成
      releaseWrite();
      await until(() => diskRead(root, 'gated2.py') === 'one', 4000);
    } finally {
      (fs.promises as any).writeFile = realWriteFile;
    }
  });

  it('fs 落盘失败还原：Update 文档缓冲区经 WorkspaceEdit 回写旧内容（缓冲区+磁盘同还原，回归锁 B2-6 脏缓冲区）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    const realWriteFile = fs.promises.writeFile.bind(fs.promises) as (...args: any[]) => any;
    (fs.promises as any).writeFile = async (p: any, ...rest: any[]) => {
      if (String(p).endsWith('new.txt')) throw new Error('EBUSY: device busy');
      return realWriteFile(p, ...rest);
    };
    try {
      await m().flush();
      const patch = '*** Begin Patch\n*** Update File: a.py\n@@\n-old line\n+new line\n*** Add File: new.txt\n+hello\n*** End Patch';
      await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'fx4', diff_patch: patch }));
      await until(() => hasMsg('error', '补丁落盘失败，已还原已执行部分'), 4000);
      assert.strictEqual(diskRead(root, 'a.py'), 'line1\nold line\nline3\n', '磁盘还原');
      const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(path.join(root, 'a.py')));
      assert.strictEqual(doc.getText(), 'line1\nold line\nline3\n', '缓冲区同步还原（不得残留补丁新内容）');
      assert.ok(!fs.existsSync(path.join(root, 'new.txt')));
      assert.strictEqual(registry(globalStorage).length, 0, '失败不登记');
    } finally {
      (fs.promises as any).writeFile = realWriteFile;
    }
  });

  it('诚实降级：无建议 → 警告；建议无围栏 → 警告；工作区不变（不伪造补丁）', async () => {
    const { root } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'n1', diff_patch: '', ai_fix_suggestion: '' }));
    assert.ok(hasMsg('warn', '平台暂无 AI 修复建议'));
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'n2', diff_patch: '', ai_fix_suggestion: '请手动改成参数化查询（自然语言，无围栏）' }));
    assert.ok(hasMsg('warn', '暂无法自动修复'));
    assert.strictEqual(diskRead(root, 'a.py'), 'line1\nold line\nline3\n');
  });

  // ── 回滚 ──

  it('回滚被 applyEdit 拒绝 → checkpoint 未消耗、登记保持 applied（回归锁：静默消耗 checkpoint）', async () => {
    const { root, globalStorage } = boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    const f = finding({ diff_patch: UPDATE_PATCH });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.fixFinding', f);
    await until(() => diskRead(root, 'a.py') === 'line1\nnew line\nline3\n');
    m().state().applyEditOk = false;
    await vscode.commands.executeCommand('codeaudit.rollbackFix', f);
    await until(() => hasMsg('error', 'checkpoint 未消耗'));
    assert.strictEqual(registry(globalStorage)[0].state, 'applied');
    assert.strictEqual(diskRead(root, 'a.py'), 'line1\nnew line\nline3\n');
  });

  // ── 低风险批量 ──

  it('低风险批量：候选筛选（HIGH/低置信/无补丁/已回滚全部排除）+ 多选应用不翻案', async () => {
    const script = new FetchScript().install();
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
    const candidates = [
      finding({ finding_id: 'f-low1', severity: 'SEVERITY_LOW', ai_confidence: 0.95, diff_patch: UPDATE_PATCH }),
      finding({ finding_id: 'f-high', severity: 'SEVERITY_HIGH', ai_confidence: 0.99, diff_patch: UPDATE_PATCH }),
      finding({ finding_id: 'f-notconfident', severity: 'SEVERITY_LOW', ai_confidence: 0.5, diff_patch: UPDATE_PATCH }),
      finding({ finding_id: 'f-nopatch', severity: 'SEVERITY_LOW', ai_confidence: 0.95, diff_patch: '' }),
      finding({ finding_id: 'f-rolled', severity: 'SEVERITY_LOW', ai_confidence: 0.95, diff_patch: UPDATE_PATCH }),
    ];
    script.on(/GET \/v1\/findings/, () => page(candidates));
    const { root, globalStorage } = boot({
      files: { 'a.py': 'line1\nold line\nline3\n' },
      secrets: { 'codeaudit.refresh': 'r' },
      workspaceState: { 'codeaudit.lastTaskId': TASK },
      registry: [{ findingId: 'f-rolled', label: '已回滚项', checkpointId: 'cp-x', files: [], appliedAt: 1, state: 'rolledback' }],
    });
    await m().flush();
    await until(() => (m().state().diagnosticsCollections[0]?.all.length ?? 0) > 0);
    m().state().pickQueue = [[{ label: 'x', description: 'y', finding: candidates[0] }]];
    await vscode.commands.executeCommand('codeaudit.applyLowRiskFixes');
    await until(() => hasMsg('info', '已应用 1 条低风险修复'));
    const qp = m().state().quickPickCalls.at(-1);
    assert.strictEqual(qp.items.length, 1, '候选仅 f-low1');
    assert.strictEqual(qp.items[0].finding.finding_id, 'f-low1');
    assert.strictEqual(qp.opts.canPickMany, true);
    assert.strictEqual(diskRead(root, 'a.py'), 'line1\nnew line\nline3\n');
    assert.ok(registry(globalStorage).some((r: any) => r.findingId === 'f-low1' && r.state === 'applied'));
    assert.ok(registry(globalStorage).some((r: any) => r.findingId === 'f-rolled' && r.state === 'rolledback'), '用户回滚过的不被翻案');
  });

  it('低风险批量：单条意外异常跳过继续，不中断其余候选（回归锁 B2-6 逐条容错）', async () => {
    const script = new FetchScript().install();
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
    const cands = [
      finding({ finding_id: 'fl1', severity: 'SEVERITY_LOW', ai_confidence: 0.95, diff_patch: UPDATE_PATCH }),
      finding({ finding_id: 'fl2', severity: 'SEVERITY_LOW', ai_confidence: 0.95, diff_patch: UPDATE_PATCH }),
    ];
    script.on(/GET \/v1\/findings/, () => page(cands));
    const { root, globalStorage } = boot({
      files: { 'a.py': 'line1\nold line\nline3\n' },
      secrets: { 'codeaudit.refresh': 'r' },
      workspaceState: { 'codeaudit.lastTaskId': TASK },
    });
    await m().flush();
    await until(() => (m().state().diagnosticsCollections[0]?.all.length ?? 0) > 0);
    const realApplyEdit = (vscode.workspace as any).applyEdit;
    let editCalls = 0;
    (vscode.workspace as any).applyEdit = async (...a: any[]) => {
      editCalls++;
      if (editCalls === 1) throw new Error('boom-edit'); // 第一条修复在 applyEdit 处意外抛错
      return realApplyEdit.apply(vscode.workspace, a);
    };
    try {
      // canPickMany：桩的 pickQueue 每格是"一次选择的结果"，多选 = 选中项数组
      m().state().pickQueue = [cands.map((f) => ({ label: 'x', description: 'y', finding: f }))];
      await vscode.commands.executeCommand('codeaudit.applyLowRiskFixes');
      await until(() => hasMsg('info', '已应用 1 条低风险修复'), 4000);
      assert.ok(hasMsg('info', '1 条补丁校验未过已跳过'), '单条异常计入跳过，不中断循环');
      assert.strictEqual(diskRead(root, 'a.py'), 'line1\nnew line\nline3\n', '第二条正常应用');
      assert.deepStrictEqual(registry(globalStorage).map((r: any) => r.findingId), ['fl2'], '只有第二条登记');
    } finally {
      (vscode.workspace as any).applyEdit = realApplyEdit;
    }
  });

  // ── 恢复链路 ──

  it('恢复链路：重启后绑定上次任务，从平台重建结果（绝不重扫、无通知打扰）', async () => {
    await bootRestored();
    const s = m().state();
    assert.strictEqual(s.messages.length, 0, '静默恢复');
    assert.strictEqual(s.contexts['codeaudit.hasTask'], true);
    assert.strictEqual(s.contexts['codeaudit.taskRunning'], false);
    const all = s.diagnosticsCollections[0].all as [unknown, any[]][];
    assert.strictEqual(all[0][1].length, 2);
    assert.ok(s.treeProviders['codeaudit.findings'].roots.length > 0);
    assert.ok(s.statusBars.some((i: any) => String(i.text).includes('2 发现')));
    assert.strictEqual(s.workspaceState.get('codeaudit.lastTaskId'), TASK);
  });

  it('恢复链路：非终态任务自动续订快照流，WS 暂停/终态帧驱动 UI（回归锁：重启后进度冻结）', async () => {
    const script = new FetchScript().install();
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_RUNNING', 42));
    script.on(/GET \/v1\/findings/, () => page([finding()]));
    boot({ files: { 'a.py': 'x\n' }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    await until(() => wsInstances.length === 1, 4000);
    assert.strictEqual(m().state().messages.length, 0, 'bindTask silent');
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_PAUSED', 42)) });
    await until(() => m().state().contexts['codeaudit.taskPaused'] === true);
    const resumeBtn = m().state().statusBars.find((i: any) => i.command === 'codeaudit.resumeScan');
    assert.ok(resumeBtn && String(resumeBtn.text).includes('恢复'), '暂停态快捷按钮切恢复');
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：1 条发现'));
    assert.strictEqual(m().state().contexts['codeaudit.taskPaused'], false);
  });

  it('恢复链路：上次任务已被平台删除 → 清 lastTaskId、静默跳过（回归锁：恢复死任务）', async () => {
    const script = new FetchScript().install();
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => new Response(JSON.stringify({ error: 'task gw-x not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } }));
    boot({ files: { 'a.py': 'x\n' }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    await until(() => script.callsTo(/snapshot/).length >= 1);
    await m().flush();
    assert.strictEqual(m().state().workspaceState.size, 0, '绑定指针应被清除');
    assert.strictEqual(m().state().messages.length, 0, '静默路径');
    assert.strictEqual(m().state().diagnosticsCollections[0].all.length, 0);
  });

  it('刷新兜底：本地无任务 → 绑定平台该项目最近完成任务并拉结果', async () => {
    const script = new FetchScript().install();
    let listCalls = 0;
    script.on(/GET \/v1\/tasks\?/, () => {
      listCalls++;
      return listCalls === 1
        ? { tasks: [] } // boot 时的 restoreLastTask 查询：暂无任务
        : { tasks: [{ task_id: TASK, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_COMPLETED', created_at: '2026-09-01T00:00:00Z', updated_at: null, error_message: '' }] };
    });
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
    script.on(/GET \/v1\/findings/, () => page([finding()]));
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1' }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    await until(() => listCalls >= 1);
    await vscode.commands.executeCommand('codeaudit.refreshFindings');
    await until(() => hasMsg('info', '已绑定任务'));
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK);
  });

  it('切换任务守卫：运行中任务切绑需确认——取消不动、确认后切换（回归锁 B2-5）', async () => {
    const script = new FetchScript().install();
    const TASK_B = 'gw-old9999-bbbbbbbbbbbbbbbb';
    const summaryB = { task_id: TASK_B, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_COMPLETED', created_at: '', updated_at: null, error_message: '' };
    script.on(/GET \/v1\/tasks\/gw-old9999[\w-]*\/snapshot/, () => snapshot(TASK_B, 'TASK_STATUS_COMPLETED', 100));
    script.on(/GET \/v1\/findings\?task_id=gw-old9999/, () => page([finding({ finding_id: 'fb', task_id: TASK_B })]));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_RUNNING', 42)); // A 运行中 → watcher 活跃
    script.on(/GET \/v1\/findings/, () => page([finding()]));
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [summaryB] }));
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1' }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    await until(() => wsInstances.length === 1, 4000); // A 的 watcher 已活跃
    // 取消路径：确认框点 Esc（buttonQueue 空 → undefined）
    m().state().pickQueue = [{ label: 'B', description: '', task: summaryB }];
    m().state().buttonQueue = [undefined];
    await vscode.commands.executeCommand('codeaudit.selectTask');
    await until(() => hasMsg('warn', '确认切换'));
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK, '取消后绑定不动');
    assert.strictEqual(script.callsTo(/gw-old9999[\w-]*\/snapshot/).length, 0, '取消后不得拉目标任务快照');
    // 确认路径：点「确认切换」→ 正常切绑
    m().state().pickQueue = [{ label: 'B', description: '', task: summaryB }];
    m().state().buttonQueue = ['确认切换'];
    await vscode.commands.executeCommand('codeaudit.selectTask');
    await until(() => hasMsg('info', '已绑定任务 gw-old99'), 4000);
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK_B);
  });

  it('绑定任务 findings 拉取失败 → 回滚绑定态：进度/lastTaskId/结果树复原（回归锁 B2-5 半绑定状态）', async () => {
    const script = new FetchScript().install();
    const TASK_B = 'gw-old9999-bbbbbbbbbbbbbbbb';
    const summaryB = { task_id: TASK_B, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_COMPLETED', created_at: '', updated_at: null, error_message: '' };
    script.on(/GET \/v1\/tasks\/gw-old9999[\w-]*\/snapshot/, () => snapshot(TASK_B, 'TASK_STATUS_COMPLETED', 100));
    script.on(/GET \/v1\/findings\?task_id=gw-old9999/, () => new Response('{"error":"internal"}', { status: 500 })); // findings 拉取失败
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)); // boot 绑定 A
    script.on(/GET \/v1\/findings/, () => page([finding()]));
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [summaryB] }));
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1' }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    await until(() => (m().state().diagnosticsCollections[0]?.all.length ?? 0) > 0); // A 的发现已渲染
    m().state().pickQueue = [{ label: 'B', description: '', task: summaryB }];
    await vscode.commands.executeCommand('codeaudit.selectTask');
    await until(() => hasMsg('error', '绑定任务失败'), 4000);
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK, '绑定指针复原到原任务');
    assert.strictEqual(m().state().contexts['codeaudit.hasTask'], true);
    const roots = m().state().treeProviders['codeaudit.findings'].roots;
    const findingNode = roots.find((n: any) => n.kind === 'finding');
    assert.ok(findingNode, '结果树复原为任务 A 的发现（不留半绑定空态）');
    assert.strictEqual(findingNode.finding.finding_id, 'f1', '树上发现仍是原任务 A 的 f1');
    assert.ok(m().state().statusBars.some((i: any) => String(i.text).includes('1 发现')), '状态栏复原为 A 的发现计数');
  });

  it('切换绑定到已删除任务（snapshot 404）：旧任务绑定保持，旧任务终态收尾不被吞（回归锁：404 清 lastTaskId 致收尾死锁）', async () => {
    const script = new FetchScript().install();
    const TASK_B = 'gw-dead0000-bbbbbbbbbbbbbbbb';
    const summaryB = { task_id: TASK_B, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_COMPLETED', created_at: '', updated_at: null, error_message: '' };
    // B 的快照 404（listTasks 结果瞬态过期：列出后、绑定前被平台删除）
    script.on(/GET \/v1\/tasks\/gw-dead0000[\w-]*\/snapshot/, () => new Response(JSON.stringify({ error: 'task gw-dead0000 not found' }), { status: 404, headers: { 'Content-Type': 'application/json' } }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_RUNNING', 42)); // A 运行中 → watcher 活跃
    script.on(/GET \/v1\/findings/, () => page([finding()]));
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [summaryB] }));
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1' }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    await until(() => wsInstances.length === 1, 4000); // A 的 watcher 已活跃（bindTask 对非终态任务续订）
    // 确认切换 → B 404 → 警告 + 旧绑定原样保留（内存与持久化指针都不动）
    m().state().pickQueue = [{ label: 'B', description: '', task: summaryB }];
    m().state().buttonQueue = ['确认切换'];
    await vscode.commands.executeCommand('codeaudit.selectTask');
    await until(() => hasMsg('warn', '未切换绑定'), 4000);
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK, '旧任务绑定指针不得被清（清空会吞掉 A 的终态收尾 → scanning 死锁）');
    // A 随后完成：terminal 归属守卫（taskId===lastTaskId）通过，正常收尾拉取结果
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：1 条发现'), 4000);
    assert.ok((m().state().diagnosticsCollections[0]?.all.length ?? 0) > 0, 'A 的发现已渲染');
  });

  it('绑定项目后自动同步：拉取该项目最近完成任务并渲染发现（回归锁：绑定即见平台已有风险）', async () => {
    const script = new FetchScript().install();
    const project = { project_id: 'p1', name: '平台项目A', repo_url: '', default_branch: '', default_scan_mode: '', created_at: null };
    script.on(/GET \/v1\/projects/, () => ({ projects: [project], pagination: { next_cursor: '', has_next: false, total: 1 } }));
    script.on(/GET \/v1\/tasks\?/, () => ({
      tasks: [{ task_id: TASK, project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_COMPLETED', created_at: '2026-09-10T00:00:00Z', updated_at: null, error_message: '' }],
    }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
    script.on(/GET \/v1\/findings/, () => page([finding()]));
    // 未配置 projectId：boot 的 restoreLastTask 兜底不触发（同步只能来自绑定动作本身）
    boot({ files: { 'a.py': 'x\n' }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    m().state().pickQueue = [{ label: '平台项目A', description: 'p1', project }];
    await vscode.commands.executeCommand('codeaudit.selectProject');
    await until(() => hasMsg('info', '已绑定项目'), 4000);
    await until(() => (m().state().diagnosticsCollections[0]?.all.length ?? 0) > 0, 4000);
    assert.ok(hasMsg('info', '已绑定任务'), '同步到最近完成任务并带轻通知');
    assert.strictEqual(m().state().workspaceState.get('codeaudit.lastTaskId'), TASK, '绑定指针落最近完成任务');
    assert.strictEqual(m().state().config.projectId, 'p1', '工作区配置写入');
  });

  it('绑定项目自动同步：项目无完成任务 → 静默零打扰（不弹任务通知、不建绑定指针）', async () => {
    const script = new FetchScript().install();
    const project = { project_id: 'p1', name: '平台项目A', repo_url: '', default_branch: '', default_scan_mode: '', created_at: null };
    script.on(/GET \/v1\/projects/, () => ({ projects: [project], pagination: { next_cursor: '', has_next: false, total: 1 } }));
    script.on(/GET \/v1\/tasks\?/, () => ({
      tasks: [{ task_id: 'gw-running-1111111111111111', project_id: 'p1', scan_mode: 'SCAN_MODE_PARALLEL', sast_tools: [], status: 'TASK_STATUS_RUNNING', created_at: '2026-09-10T00:00:00Z', updated_at: null, error_message: '' }],
    }));
    boot({ files: { 'a.py': 'x\n' }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    m().state().pickQueue = [{ label: '平台项目A', description: 'p1', project }];
    await vscode.commands.executeCommand('codeaudit.selectProject');
    await until(() => hasMsg('info', '已绑定项目'), 4000);
    await until(() => script.callsTo(/GET \/v1\/tasks\?/).length >= 1, 4000); // 同步探测已发生
    await new Promise((r) => setTimeout(r, 50));
    assert.ok(!messages().some((x) => /已绑定任务/.test(x.msg)), '无完成任务不得弹任务通知');
    assert.strictEqual(m().state().workspaceState.has('codeaudit.lastTaskId'), false, '不建绑定指针');
    assert.strictEqual((m().state().diagnosticsCollections[0]?.all.length ?? 0), 0, '面板保持空');
  });

  // ── AI 上下文视图 ──

  it('AI 上下文视图：整页只渲染 null 态+任务态两次，其余走增量 postMessage（回归锁：整页重载毁滚动位置）', async () => {
    const script = new FetchScript().install();
    let snaps = 0;
    scriptScan(script, {
      snapshots: () => {
        snaps++;
        return snapshot(TASK, snaps === 1 ? 'TASK_STATUS_RUNNING' : 'TASK_STATUS_COMPLETED', snaps === 1 ? 40 : 100);
      },
    });
    boot({ files: { 'a.py': 'x\n' }, config: { projectId: 'p1', minPackFiles: 1 }, secrets: { 'codeaudit.refresh': 'r' } });
    await m().flush();
    const aiProvider = m().state().webviewProviders['codeaudit.aiContext'];
    const fake = fakeWebview();
    aiProvider.resolveWebviewView(fake.view);
    await vscode.commands.executeCommand('codeaudit.scanWorkspace');
    await until(() => wsInstances.length >= 1 && hasMsg('info', '代码审计已开始'));
    // COMPLETED 终态经 WS 帧投递（轮询兜底为 10s，测试不等真实时钟）
    wsInstances[0].onmessage?.({ data: JSON.stringify(snapshot(TASK, 'TASK_STATUS_COMPLETED', 100)) });
    await until(() => hasMsg('info', '扫描完成：0 条发现'));
    assert.ok(fake.htmlCount() <= 2, `整页渲染只应有 null 态与任务态两次（实际 ${fake.htmlCount()}）`);
    assert.ok(fake.posted.length >= 2, '运行期应持续走增量 postMessage');
    const upd = fake.posted.find((x: any) => x.type === 'update');
    assert.ok(upd && typeof upd.percent === 'number' && 'logsHtml' in upd && 'aiHtml' in upd && 'chipsHtml' in upd);
  });

  // ── 宿主交互 ──

  it('URI 深度链接：scan/selectTask 分发到命令；未知动作不炸', async () => {
    boot({ files: { 'a.py': 'x\n' } });
    await m().flush();
    const handler = m().state().uriHandler as { handleUri(uri: unknown): void };
    await handler.handleUri(vscode.Uri.parse('vscode://codeaudit.codeaudit-vscode/scan'));
    assert.ok(m().state().executed.some((e: any) => e.id === 'codeaudit.scanWorkspace'));
    await handler.handleUri(vscode.Uri.parse('vscode://codeaudit.codeaudit-vscode/selectTask'));
    assert.ok(m().state().executed.some((e: any) => e.id === 'codeaudit.selectTask'));
    await handler.handleUri(vscode.Uri.parse('vscode://codeaudit.codeaudit-vscode/bogus'));
    assert.ok(!hasMsg('error'), '未知动作仅日志告警');
  });

  it('编辑器灯泡：诊断相交 + 文件匹配 → fixFinding QuickFix', async () => {
    const { root } = await bootRestored();
    const provider = m().state().codeActionProviders[0];
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(path.join(root, 'a.py')));
    const actions = provider.provideCodeActions(doc, new vscode.Range(1, 0, 1, 6), { diagnostics: [] }, undefined as never);
    assert.ok(actions.length >= 1, '至少一个 QuickFix');
    const fix = actions.find((a: any) => a.command?.command === 'codeaudit.fixFinding');
    assert.ok(fix, '包含 AI 修复动作');
    assert.strictEqual(fix.command.arguments[0].finding_id, 'f1');
    const none = provider.provideCodeActions(doc, new vscode.Range(99, 0, 99, 1), { diagnostics: [] }, undefined as never);
    assert.strictEqual(none.length, 0, '不相交 range 无动作');
  });

  it('行号校准贯通：修复行漂移后灯泡按校准行命中、打开位置跳校准行（回归锁：校准只进诊断不进反查/跳转）', async () => {
    const script = new FetchScript().install();
    const HEAD_INSERT_PATCH = '*** Begin Patch\n*** Update File: a.py\n@@\n+l0a\n+l0b\n l1\n l2\n*** End Patch';
    script.on(/GET \/v1\/tasks\?/, () => ({ tasks: [] }));
    script.on(/GET \/v1\/tasks\/[\w-]+\/snapshot/, () => snapshot(TASK, 'TASK_STATUS_COMPLETED', 100));
    script.on(/GET \/v1\/findings/, () => page([
      finding(),
      finding({ finding_id: 'f2', title: 'XSS', cwe_id: 'CWE-79', location: { file_path: 'a.py', start_line: 5 } }),
      finding({ finding_id: 'f3', title: '头部插入修复', location: { file_path: 'a.py', start_line: 1 }, diff_patch: HEAD_INSERT_PATCH }),
    ]));
    const { root } = boot({ files: { 'a.py': 'l1\nl2\nl3\nl4\nl5\n' }, secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    // mock 的 collection.all 为 [uri, diags[]] 条目数组：a.py 一个条目、其内 3 条诊断
    await until(() => (m().state().diagnosticsCollections[0]?.all.length ?? 0) === 1);
    // 应用 f3 修复：文件头插入 2 行 → trackedLines 迁移 f1 2→4、f2 5→7（1-based）
    await vscode.commands.executeCommand('codeaudit.fixFinding', finding({ finding_id: 'f3', title: '头部插入修复', location: { file_path: 'a.py', start_line: 1 }, diff_patch: HEAD_INSERT_PATCH }));
    await until(() => diskRead(root, 'a.py') === 'l0a\nl0b\nl1\nl2\nl3\nl4\nl5\n');
    const diags = (m().state().diagnosticsCollections[0].all as [string, unknown[]][])[0][1] as { message: string; range: { start: { line: number } } }[];
    const f1Diag = diags.find((d: any) => String(d.message).includes('SQL 注入'));
    assert.ok(f1Diag, 'f1 诊断在列');
    assert.strictEqual(f1Diag.range.start.line, 3, '诊断行号已按校准迁移（2+2-1=3）');
    // 灯泡反查与诊断同源：校准行命中 f1；漂移后的原始行不再命中（不误导）
    const provider = m().state().codeActionProviders[0];
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(path.join(root, 'a.py')));
    const actions = provider.provideCodeActions(doc, new vscode.Range(3, 0, 3, 6), { diagnostics: [] }, undefined as never);
    const fix = actions.find((a: any) => a.command?.command === 'codeaudit.fixFinding');
    assert.ok(fix, '校准行灯泡不消失');
    assert.strictEqual(fix.command.arguments[0].finding_id, 'f1', '反查命中正确发现');
    const stale = provider.provideCodeActions(doc, new vscode.Range(1, 0, 1, 6), { diagnostics: [] }, undefined as never);
    assert.strictEqual(stale.length, 0, '漂移后的原始行（f1 旧位 2-1=1）无灯泡');
    // 打开位置：f2 跳到校准行 7-1=6（原始 5-1=4 已漂移）
    await vscode.commands.executeCommand('codeaudit.openFinding', finding({ finding_id: 'f2', title: 'XSS', cwe_id: 'CWE-79', location: { file_path: 'a.py', start_line: 5 } }));
    const shown = m().state().shownDocs;
    assert.strictEqual(shown[shown.length - 1].opts.selection.start.line, 6, '跳转行=校准行而非扫描原始行');
  });

  it('漏洞详情 webview：resolve 渲染 + postMessage 动作回传分发（fix/rollback/openLocation）', async () => {
    boot({ files: { 'a.py': 'line1\nold line\nline3\n' } });
    await m().flush();
    const provider = m().state().webviewProviders['codeaudit.findingDetail'];
    const fake = fakeWebview();
    provider.resolveWebviewView(fake.view);
    assert.ok(String(fake.view.webview.html).includes('点击任意漏洞'), '空态渲染');
    await vscode.commands.executeCommand('codeaudit.openFinding', finding());
    await until(() => String(provider.current.finding?.finding_id ?? '').length > 0);
    assert.ok(String(fake.view.webview.html).includes('SQL 注入'), '详情渲染选中发现');
    assert.strictEqual(m().state().contexts['codeaudit.findingDetail'], true);
    assert.strictEqual(m().state().shownDocs.length, 1, '打开位置跳转编辑器');
    fake.dispatch({ action: 'openLocation' });
    await until(() => m().state().shownDocs.length === 2);
    fake.dispatch({ action: 'fix' });
    await until(() => hasMsg('warn', '平台暂无 AI 修复建议'));
    fake.dispatch({ action: 'rollback' });
    await until(() => hasMsg('info', '当前没有已应用的修复'));
  });

  it('清空本地结果：诊断与树清空、状态栏回空闲（平台数据不动）', async () => {
    await bootRestored();
    await vscode.commands.executeCommand('codeaudit.clearFindings');
    const s = m().state();
    assert.strictEqual(s.diagnosticsCollections[0].all.length, 0);
    assert.strictEqual(s.treeProviders['codeaudit.findings'].roots.length, 0);
    assert.ok(hasMsg('info', '已清空本地扫描结果'));
    assert.ok(s.statusBars.some((i: any) => String(i.text).includes('空闲')));
  });

  it('打开控制台：consoleUrl 缺省从 serverUrl 剥离端口推导同主机 :80 + 任务路由；显式配置优先', async () => {
    boot({ secrets: { 'codeaudit.refresh': 'r' }, workspaceState: { 'codeaudit.lastTaskId': TASK } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.openConsole');
    let opened = m().state().openedExternal;
    assert.strictEqual(opened.length, 1);
    assert.ok(opened[0].toString().startsWith('http://localhost/tasks/'), `实际 ${opened[0].toString()}`);
    assert.ok(!opened[0].toString().includes(':8080'), `网关端口必须被剥离，实际 ${opened[0].toString()}`);
    m().state().config.consoleUrl = 'https://console.example.com';
    await vscode.commands.executeCommand('codeaudit.openConsole');
    opened = m().state().openedExternal;
    assert.strictEqual(opened[1].toString(), `https://console.example.com/tasks/${TASK}`);
  });

  it('复制命令：finding_id / 文件路径写剪贴板（树节点包装与 finding 两种入参）', async () => {
    boot({ files: { 'a.py': 'x\n' } });
    await m().flush();
    await vscode.commands.executeCommand('codeaudit.copyFindingId', finding());
    assert.ok(m().state().clipboard.includes('f1'));
    await vscode.commands.executeCommand('codeaudit.copyFilePath', { kind: 'file', path: 'src/a.py' });
    assert.ok(m().state().clipboard.includes('src/a.py'));
    await vscode.commands.executeCommand('codeaudit.copyFilePath', finding({ location: { file_path: 'a.py', start_line: 2 } }));
    assert.ok(m().state().clipboard.includes('a.py'));
  });
});
