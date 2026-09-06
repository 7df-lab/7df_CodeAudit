// CodeAudit VS Code 插件入口（薄胶水层：全部业务逻辑在可单测的纯模块中）。
// 设计依据：2026-09-02 人类批准的插件设计方案——原生扩展 + gateway REST/WS +
// Diagnostics/CodeAction/TreeView 展示 + diff 审批落盘 + checkpoint 回滚。
import * as fs from 'fs';
import * as path from 'path';
import * as vscode from 'vscode';
import { ApiError, CodeAuditClient, type TokenStore } from './apiClient';
import { selectLowRiskFixCandidates } from './lowRiskApply';
import { computePatchChanges, listUpdatedFiles, PatchActionType, type PatchFileChange } from './applyPatch';
import { buildViewUpdate, renderAiContextHtml } from './aiContextView';
import { AiContextViewProvider } from './aiContextViewProvider';
import { CheckpointStore } from './checkpoint';
import { applyPatchToLines, buildFilePatch, extractDiffBlock, formatPatchFailures, invertFilePatch, parseUnifiedDiff, shiftLine, type FilePatch } from './diffParse';
import { mapFinding } from './diagnosticsMapper';
import { renderFindingDetailHtml, type FindingDetailAction, type FindingDetailData } from './findingDetailView';
import { FixRegistry, type FixRecord } from './fixRegistry';
import { applyFrame, buildProgressItems, createProgressState, sandboxPackCheck, taskStatusLabel, type ProgressNode, type ProgressState } from './progressModel';
import { buildTree, findingDescription, findingLabel, pickFindingAtLine, rollbackPickItems, type TreeNode } from './treeModel';
import { TaskWatcher } from './taskWatcher';
import { zipFiles } from './workspaceZip';
import type { TaskSnapshot, TaskSummary, UnifiedFinding } from './types';
import { isTerminalTaskStatus } from './types';

class SecretTokenStore implements TokenStore {
  private access = '';
  /**
   * onCleared：凭据被清（refresh 失败/登出）即会话失效——必须同步 loggedIn 上下文，
   * 否则欢迎页 when 条件卡在已登录态（状态栏由 isLoggedIn 驱动会自愈，上下文键不会）。
   */
  constructor(private secrets: vscode.SecretStorage, private readonly onCleared?: () => void) {}
  getAccessToken(): string {
    return this.access;
  }
  getRefreshToken(): string {
    // 同步接口下返回缓存；缓存由 setTokens/clear 维护
    return this.refreshCache;
  }
  private refreshCache = '';
  setTokens(access: string, refresh?: string): void {
    this.access = access;
    if (refresh) {
      this.refreshCache = refresh;
      void this.secrets.store('codeaudit.refresh', refresh);
    }
  }
  async boot(): Promise<void> {
    this.refreshCache = await this.secrets.get('codeaudit.refresh') ?? '';
  }
  clear(): void {
    const had = !!this.access || !!this.refreshCache;
    this.access = '';
    this.refreshCache = '';
    void this.secrets.delete('codeaudit.refresh');
    if (had) this.onCleared?.();
  }
}

class FindingsTreeProvider implements vscode.TreeDataProvider<TreeNode> {
  public readonly _onDidChange = new vscode.EventEmitter<void>();
  readonly onDidChangeTreeData = this._onDidChange.event;
  public roots: TreeNode[] = [];
  // 已应用修复的发现集合：发现保留在树/Problems（应用补丁 ≠ 风险记录消失），
  // 以"✔ 已修复（可回滚）"徽章 + 不同的 contextValue（内联按钮切换为回滚）呈现
  private fixedIds = new Set<string>();

  setFindings(findings: UnifiedFinding[], fixedIds: Set<string> = new Set<string>()): void {
    this.fixedIds = fixedIds;
    this.roots = buildTree(findings);
    this._onDidChange.fire();
  }

  getTreeItem(node: TreeNode): vscode.TreeItem {
    if (node.kind === 'file') {
      const item = new vscode.TreeItem(`${node.path} (${node.count})`, vscode.TreeItemCollapsibleState.Expanded);
      item.contextValue = 'file';
      item.iconPath = new vscode.ThemeIcon('file-directory');
      return item;
    }
    const f = node.finding;
    const fixed = this.fixedIds.has(f.finding_id);
    const item = new vscode.TreeItem(findingLabel(f));
    item.description = fixed ? `✔ 已修复（可回滚） · ${findingDescription(f)}` : findingDescription(f);
    item.contextValue = fixed ? 'findingFixed' : 'finding';
    item.iconPath = new vscode.ThemeIcon(
      fixed ? 'check' : f.severity === 'SEVERITY_CRITICAL' || f.severity === 'SEVERITY_HIGH' ? 'error' : f.severity === 'SEVERITY_MEDIUM' ? 'warning' : 'info',
    );
    item.command = { command: 'codeaudit.openFinding', title: '打开漏洞位置', arguments: [f] };
    return item;
  }

  getChildren(node?: TreeNode): TreeNode[] {
    if (!node) return this.roots.filter((n) => n.kind === 'file');
    if (node.kind === 'file') {
      return this.roots.filter((n) => n.kind === 'finding' && n.parentPath === node.path);
    }
    return [];
  }
}

// 任务进度树（codeaudit.progress 视图）：任务头 + 阶段 + AI 交互入口 + 失败摘要。
// 节点数据全部来自 progressModel.buildProgressItems（纯逻辑），此处只做 TreeItem 胶水。
class ProgressTreeProvider implements vscode.TreeDataProvider<ProgressNode> {
  public readonly _onDidChange = new vscode.EventEmitter<void>();
  readonly onDidChangeTreeData = this._onDidChange.event;
  public nodes: ProgressNode[] = [];

  setItems(nodes: ProgressNode[]): void {
    this.nodes = nodes;
    this._onDidChange.fire();
  }

  getTreeItem(node: ProgressNode): vscode.TreeItem {
    const item = new vscode.TreeItem(node.label);
    item.description = node.description;
    item.tooltip = node.tooltip;
    item.contextValue = node.contextValue;
    item.iconPath = new vscode.ThemeIcon(node.icon);
    if (node.kind === 'ai') {
      item.command = { command: 'codeaudit.showAiContext', title: '查看 AI 交互上下文', arguments: [] };
    }
    return item;
  }

  getChildren(): ProgressNode[] {
    return this.nodes;
  }
}

// AI 交互上下文视图（底部面板 webview view，不占编辑器空间）：胶水实现见
// aiContextViewProvider（结构化接口、不 import vscode，行为有单测回归锁）。

// 漏洞详情视图（侧栏 codeaudit 容器，与"任务进度"按 when 上下文互斥）：
// 扫描完成后点击漏洞 → 任务进度切换为漏洞详情（进度已完成，无增量价值）；
// 新扫描开始时自动切回任务进度。渲染纯函数见 findingDetailView.ts。
class FindingDetailProvider implements vscode.WebviewViewProvider {
  public view: vscode.WebviewView | null = null;
  public current: FindingDetailData = { finding: null, fixed: false };

  constructor(private readonly onAction: (action: FindingDetailAction['action'], finding: UnifiedFinding) => void) {}

  resolveWebviewView(view: vscode.WebviewView): void {
    this.view = view;
    // enableScripts：详情页操作按钮（修复/回滚/打开位置）经 postMessage 回传插件
    view.webview.options = { enableScripts: true };
    view.webview.html = renderFindingDetailHtml(this.current);
    view.webview.onDidReceiveMessage((m: unknown) => {
      const f = this.current.finding;
      if (!f) return;
      const action = (m as { type?: string; action?: FindingDetailAction['action'] })?.action;
      if (action === 'fix' || action === 'rollback' || action === 'openLocation') this.onAction(action, f);
    });
    view.onDidDispose(() => {
      this.view = null;
    });
  }

  /** 展示/刷新详情：视图未解析时用 focus 命令触发（不抢编辑器焦点无法保证，focus 语义即可） */
  set(data: FindingDetailData): void {
    this.current = data;
    if (this.view) {
      this.view.webview.html = renderFindingDetailHtml(data);
    } else {
      void vscode.commands.executeCommand('codeaudit.findingDetail.focus');
    }
  }
}

// view/item/context 菜单（inline 按钮/右键）调用命令时 VS Code 传入的是树元素 TreeNode 而非 finding 本身，需解包
const asFinding = (arg: unknown): UnifiedFinding | undefined =>
  arg && typeof arg === 'object' && 'finding' in arg ? (arg as { finding: UnifiedFinding }).finding : (arg as UnifiedFinding | undefined);

export function activate(context: vscode.ExtensionContext): void {
  const cfg = () => vscode.workspace.getConfiguration('codeaudit');
  const secrets = context.secrets;
  // 会话失效（refresh 失败/凭据被清）时同步上下文并明确告知——
  // 否则 welcome 页 when 卡在已登录态，用户对着不可达网关却看不到任何异常
  const tokens = new SecretTokenStore(secrets, () => {
    setCtx('codeaudit.loggedIn', false);
    updateStatusBar();
    log.warn('登录会话已失效（refresh token 校验失败或被清除），请重新登录');
    void vscode.window.showWarningMessage('CodeAudit 登录会话已失效，请重新执行「CodeAudit: 登录平台」');
  });
  // 连通性翻转 → 记日志 + 刷新状态栏（offline 态由状态栏呈现"已登录·网关不可达"）
  const client = new CodeAuditClient(cfg().get('serverUrl', 'http://localhost:8080'), tokens, (u, i) => fetch(u, i), {
    onStateChange: () => {
      log.warn(`网关连通性变化：${client.offline ? '不可达（离线）' : '已恢复可达'}`);
      updateStatusBar();
    },
  });
  const diagnostics = vscode.languages.createDiagnosticCollection('codeaudit');
  const tree = new FindingsTreeProvider();
  const status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
  // 状态栏扫描快捷入口：标题栏播放图标之外的第二入口，屏幕阅读器/无键鼠环境可达
  const scanStatus = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 49);
  scanStatus.text = '$(play) 扫描';
  scanStatus.tooltip = 'CodeAudit: 扫描工作区';
  scanStatus.command = 'codeaudit.scanWorkspace';
  // 诊断日志（输出面板 "CodeAudit"）：登录/扫描里程碑 + WS 连接原始事件。
  // WS 在扩展宿主内会被 VS Code 代理解析器接管（http.proxySupport），连不上时只
  // 会静默回退轮询——没有日志就无法区分"平台没推"与"本机连不上"。
  const log = vscode.window.createOutputChannel('CodeAudit', { log: true });
  const checkpointDir = path.join(context.globalStorageUri.fsPath, 'checkpoints');
  const checkpoints = new CheckpointStore(checkpointDir);
  // 修复登记：发现 → checkpoint 映射 + applied/rolledback 状态（持久化；支撑
  // "发现保留 + 随时按发现回滚/重新应用"）
  const fixRegistry = new FixRegistry(path.join(context.globalStorageUri.fsPath, 'fix-registry.json'));
  // 漏洞详情视图：点击漏洞时顶替"任务进度"（when 上下文互斥）。onAction 回调引用的
  // doFixFinding/doRollbackFix/doOpenFinding 在 activate 下方定义，回调仅由用户点击触发，安全。
  const detailView = new FindingDetailProvider((action, f) => {
    if (action === 'openLocation') void doOpenFinding(f, { preserveFocus: false });
    else if (action === 'fix') void doFixFinding(f);
    else if (action === 'rollback') void doRollbackFix(f);
  });
  // 点击漏洞（树上条目/详情里的"打开代码位置"）：跳转编辑器 + 任务进度切换为漏洞详情
  const showFindingDetail = (f: UnifiedFinding): void => {
    setCtx('codeaudit.findingDetail', true);
    detailView.set({ finding: f, fixed: fixRegistry.appliedFindingIds().has(f.finding_id) });
  };
  /** 修复状态变化后同步详情视图（正展示同一发现时刷新按钮与徽章） */
  const refreshFindingDetail = (f: UnifiedFinding): void => {
    if (detailView.current.finding?.finding_id === f.finding_id) {
      detailView.set({ finding: f, fixed: fixRegistry.appliedFindingIds().has(f.finding_id) });
    }
  };
  let currentFindings: UnifiedFinding[] = [];
  // 发现集来源（状态栏 tooltip 措辞）：本次扫描 vs 绑定的历史任务
  let findingsSource: 'scan' | 'bound' = 'scan';
  // 发现行号跟踪（findingId → 当前 1-based 行号）：修复/回滚改变文件内容后，
  // 诊断与回滚入口按跟踪行号定位，而不是永远停在扫描快照的原始行号
  const trackedLines = new Map<string, number>();
  const trackedLinesSnapshot = (): Record<string, number> => Object.fromEntries(trackedLines);
  const persistTrackedLines = (): void => {
    void context.workspaceState.update('codeaudit.trackedLines', trackedLinesSnapshot());
  };
  // 行号不可信文件（rel 路径）：整文件覆盖回滚且无 linesBefore 快照时标注
  const degradedLineFiles = new Set<string>();
  /** 该文件（补丁相对路径）上所有发现的当前跟踪行号快照（应用修复前记录） */
  const linesSnapshotForFile = (relPath: string): Record<string, number> => {
    const norm = relPath.replace(/\\/g, '/');
    const snap: Record<string, number> = {};
    for (const fd of currentFindings) {
      if ((fd.location?.file_path ?? '').replace(/\\/g, '/') !== norm) continue;
      const cur = trackedLines.get(fd.finding_id) ?? fd.location?.start_line;
      if (cur !== undefined) snap[fd.finding_id] = cur;
    }
    return snap;
  };
  /** 按行偏移表迁移该文件上所有发现的跟踪行号（补丁应用/外科回滚后调用） */
  const applyTrackedShifts = (shiftsByFile: Record<string, { start: number; delCount: number; delta: number }[]>): void => {
    for (const [abs, shifts] of Object.entries(shiftsByFile)) {
      for (const fd of currentFindings) {
        const rel = fd.location?.file_path?.replace(/\\/g, '/');
        if (!rel) continue;
        const w = resolveWsPath(rel);
        if (!w || w.abs !== abs) continue;
        const cur = trackedLines.get(fd.finding_id);
        if (cur !== undefined) trackedLines.set(fd.finding_id, shiftLine(cur, shifts));
      }
    }
    persistTrackedLines();
  };
  let watcher: TaskWatcher | null = null;
  let lastTaskId = context.workspaceState.get<string>('codeaudit.lastTaskId', '');
  // 任务进度态（WS 四路帧归并后的单一真相源；无任务时为 null）
  let progress: ProgressState | null = null;
  let cancelRequested = false; // 用户主动取消：终态提示与异常终止区分
  let phase = ''; // 任务创建前的阶段文案（打包/上传/创建），任务运行后由 percent 接管
  const progressTree = new ProgressTreeProvider();
  // AI 交互上下文：底部面板视图（不占编辑器空间）；状态源恒为 progress，渲染为纯函数
  // lastPaint = 已送达视图的内容位置（taskId + version）：同任务增量推送、换任务整页重绘
  let lastPaint: { taskId: string; version: number } | null = null;
  const aiView = new AiContextViewProvider(
    () => progress,
    (s) => renderAiContextHtml({ state: s }),
    () => {
      lastPaint = progress ? { taskId: progress.taskId, version: progress.version } : null;
    },
  );

  // boot 后凭据就位（refresh token 在 SecretStorage）：登录态 context 与状态栏需要重算，
  // 随后恢复上次任务的历史结果（依赖登录态，必须在 boot 完成后执行）
  void tokens.boot()
    .then(() => {
      setCtx('codeaudit.loggedIn', client.isLoggedIn());
      updateStatusBar();
      // 行号跟踪恢复（workspaceState 持久化）：重载后修复过的文件诊断仍按校准行号显示
      for (const [k, v] of Object.entries(context.workspaceState.get<Record<string, number>>('codeaudit.trackedLines', {}))) {
        trackedLines.set(k, v);
      }
      void restoreLastTask();
    })
    .catch((e: unknown) => log.warn(`凭据加载失败（SecretStorage 不可用？）：${(e as Error).message}`));

  const setCtx = (key: string, value: unknown): void => {
    void vscode.commands.executeCommand('setContext', key, value);
  };

  // 紧凑状态栏：图标 + 一眼可读的短态（百分比/发现数/登录态），细节进 tooltip。
  // 点击任意状态 → 打开 AI 交互上下文实时视图。
  // 百分比只对非终态任务展示：绑定的历史任务（已完成/已取消）挂百分比会让
  // 状态栏长期停在旧任务上，误导"仍有任务在跑"；终态回退发现数/空闲。
  const updateStatusBar = (): void => {
    if (!client.isLoggedIn()) {
      status.text = '$(shield) 未登录';
      status.tooltip = 'CodeAudit：未登录——点击登录，或从扫描结果面板的欢迎页进入';
      status.command = 'codeaudit.login';
    } else if (client.offline) {
      // 本地凭据存在但网关不可达：必须与"已登录"可区分，否则后端宕机时用户
      // 对着不可达网关却看到正常登录态（连不上却不报错的根因）
      status.text = '$(shield) 已登录 · 网关不可达';
      status.tooltip = 'CodeAudit：本地凭据存在，但网关无法连接。\n检查 codeaudit.serverUrl 配置与网络/代理，或点击重新登录';
      status.command = 'codeaudit.login';
    } else if (progress && !isTerminalTaskStatus(progress.status)) {
      status.text = `$(shield) ${progress.percent}%`;
      status.tooltip = new vscode.MarkdownString(
        `**CodeAudit 代码审计**\n\n任务 \`${progress.taskId.slice(0, 8)}…\` · ${progress.stages.filter((s) => s.status === 'STAGE_STATUS_RUNNING').length} 个阶段进行中\n\n点击查看 AI 交互上下文实时视图（阶段 / AI 正文 / 任务日志）`,
      );
      status.command = 'codeaudit.showAiContext';
    } else if (phase) {
      status.text = `$(shield) ${phase}`;
      status.tooltip = 'CodeAudit：正在准备扫描（打包/上传/建任务）…';
      status.command = 'codeaudit.showAiContext';
    } else {
      status.text = currentFindings.length > 0 ? `$(shield) ${currentFindings.length} 发现` : '$(shield) 空闲';
      status.tooltip = currentFindings.length > 0
        ? `CodeAudit：${findingsSource === 'scan' ? '上次扫描' : '已绑定历史任务'} ${currentFindings.length} 条发现（任务 ${lastTaskId.slice(0, 8)}）——点击查看 AI 交互上下文；扫描结果见 CodeAudit 面板`
        : 'CodeAudit：空闲——点击查看最近任务的 AI 交互上下文，或执行扫描';
      status.command = 'codeaudit.showAiContext';
    }
    status.show();
    // 扫描快捷按钮随任务态切换：空闲=发起扫描；暂停中=恢复；运行中=操作菜单
    //（暂停/取消/查看上下文汇聚一处——取消入口原先只藏在进度面板小图标，可达性差）
    if (progress && progress.status === 'TASK_STATUS_PAUSED') {
      scanStatus.text = '$(debug-start) 恢复扫描';
      scanStatus.tooltip = 'CodeAudit: 恢复扫描（任务继续推送与推理）';
      scanStatus.command = 'codeaudit.resumeScan';
    } else if (progress && !isTerminalTaskStatus(progress.status)) {
      scanStatus.text = '$(sync) 任务进行中';
      scanStatus.tooltip = 'CodeAudit: 任务操作（暂停 / 取消 / 查看 AI 交互上下文）';
      scanStatus.command = 'codeaudit.runningMenu';
    } else {
      scanStatus.text = '$(play) 扫描';
      scanStatus.tooltip = 'CodeAudit: 扫描工作区';
      scanStatus.command = 'codeaudit.scanWorkspace';
    }
    scanStatus.show();
  };

  // 任务创建前的阶段文案（doScan 各步）；任务建立后不再生效
  const setPhase = (text: string): void => {
    phase = text;
    updateStatusBar();
  };

  // 进度态演进后的统一刷新：树 + 状态栏 + （可见时）底部面板视图。
  // 视图更新分两路：同任务走 postMessage 增量（绝不整页重载——WS 推流亚秒级演进，
  // 重载会不断销毁滚动位置，用户滑不动也读不了历史；页内脚本按"贴底才跟随"策略
  // 更新 DOM）；换任务/首次才整页重绘。
  const refreshProgressUi = (): void => {
    if (!progress) return;
    progressTree.setItems(buildProgressItems(progress));
    updateStatusBar();
    const view = aiView.view;
    if (!view?.visible) return;
    if (lastPaint && lastPaint.taskId === progress.taskId) {
      if (progress.version === lastPaint.version) return;
      void view.webview.postMessage(buildViewUpdate(progress));
    } else {
      view.webview.html = renderAiContextHtml({ state: progress });
    }
    lastPaint = { taskId: progress.taskId, version: progress.version };
  };

  const clearTaskUi = (): void => {
    // 成功收尾：进度视图回欢迎态。失败/取消路径不调用本函数——保留终态（阶段/错误/
    // AI 正文）供用户检视，面板 AI 上下文视图仍可打开查看。
    progress = null;
    phase = '';
    setCtx('codeaudit.hasTask', false);
    setCtx('codeaudit.taskRunning', false);
    progressTree.setItems([]);
    updateStatusBar();
  };

  // 诊断构建单一收口：
  // - 行号取 trackedLines（修复/回滚后已校准）?? 扫描原始行号；新发现初始化跟踪；
  // - 已应用修复的发现不再显示波浪线（修复后原行内容已变，指向旧代码误导）——
  //   风险记录仍在面板/树上（✔ 已修复，可回滚），回滚后诊断按跟踪行号恢复；
  // - resetTracking：新扫描结果/切换任务的发现集是新的真相，行号跟踪清零重来；
  //   同任务刷新与修复徽章刷新不 reset（保留本地校准）。
  const renderFindings = (findings: UnifiedFinding[], opts: { resetTracking?: boolean } = {}): void => {
    currentFindings = findings;
    if (opts.resetTracking) {
      trackedLines.clear();
      degradedLineFiles.clear();
      void context.workspaceState.update('codeaudit.trackedLines', undefined);
    }
    const appliedIds = fixRegistry.appliedFindingIds();
    diagnostics.clear();
    const byFile = new Map<string, { line: number; endLine: number; rank: number; msg: string; code: string; f: UnifiedFinding }[]>();
    for (const f of findings) {
      if (appliedIds.has(f.finding_id)) continue;
      let tracked = trackedLines.get(f.finding_id) ?? f.location?.start_line;
      if (tracked === undefined) continue;
      if (!trackedLines.has(f.finding_id)) {
        trackedLines.set(f.finding_id, tracked);
      }
      // 行号不可信标注（整文件回滚且无快照，无法精确恢复）：行号仍展示，仅追加注记
      const degraded = degradedLineFiles.has(m_relOf(f) ?? '');
      const m = mapFinding(f, tracked);
      if (!m) continue;
      if (degraded) m.message += '（行号可能因 AI 修复偏移，重新扫描可校准）';
      const list = byFile.get(m.filePath) ?? [];
      list.push({ line: m.line, endLine: m.endLine, rank: m.severityRank, msg: m.message, code: m.code, f });
      byFile.set(m.filePath, list);
    }
    for (const [filePath, list] of byFile) {
      const docUri = docUriFor(filePath);
      if (!docUri) continue;
      diagnostics.set(docUri, list.map((d) => {
        const range = new vscode.Range(d.line, 0, d.endLine, 65535);
        const diag = new vscode.Diagnostic(range, d.msg, d.rank >= 3 ? vscode.DiagnosticSeverity.Error : d.rank === 2 ? vscode.DiagnosticSeverity.Warning : vscode.DiagnosticSeverity.Information);
        diag.source = 'CodeAudit';
        diag.code = d.code;
        return diag;
      }));
    }
    tree.setFindings(findings, fixRegistry.appliedFindingIds());
    void persistTrackedLines();
  };

  /** 发现工作区相对路径（归一 /），无定位返回 null */
  const m_relOf = (f: UnifiedFinding): string | null => {
    const p = f.location?.file_path?.replace(/\\/g, '/');
    return p || null;
  };

  const docUriFor = (relPath: string): vscode.Uri | null => {
    const root = vscode.workspace.workspaceFolders?.[0];
    if (!root) return null;
    return vscode.Uri.joinPath(root.uri, relPath);
  };

  // 补丁路径 → 工作区绝对路径，带禁闭校验（拒绝绝对路径与 .. 逃逸）。
  // 服务端 NormalizeDiffPatch 已挡一道（ADR-183），此处为插件侧兜底。
  const resolveWsPath = (relPath: string): { abs: string; uri: vscode.Uri } | null => {
    const root = vscode.workspace.workspaceFolders?.[0];
    if (!root) return null;
    const norm = relPath.replace(/\\/g, '/');
    if (!norm || path.posix.isAbsolute(norm) || norm.split('/').includes('..')) return null;
    const uri = vscode.Uri.joinPath(root.uri, norm);
    return { abs: uri.fsPath, uri };
  };

  // 空内容虚拟文档：Add（左侧）/ Delete（右侧）的 diff 审批视图占位
  const emptyDocUri = (key: string): vscode.Uri =>
    vscode.Uri.from({ scheme: 'codeaudit-fix', path: `/${key}`, query: Buffer.from(JSON.stringify({ content: '' }), 'utf8').toString('base64') });

  /** 绝对路径 → 工作区相对路径（posix 分隔；越界返回 null） */
  const absPathToRel = (abs: string): string | null => {
    const root = vscode.workspace.workspaceFolders?.[0];
    if (!root) return null;
    const rel = path.relative(root.uri.fsPath, abs);
    if (!rel || rel.startsWith('..')) return null;
    return rel.replace(/\\/g, '/');
  };

  // —— 登录 ——
  const doLogin = async (): Promise<void> => {
    const serverUrl = await vscode.window.showInputBox({ prompt: '平台网关地址', value: cfg().get('serverUrl', '') });
    if (serverUrl === undefined) return;
    await cfg().update('serverUrl', serverUrl, vscode.ConfigurationTarget.Global);
    client.baseUrl = serverUrl.replace(/\/+$/, '');
    const username = await vscode.window.showInputBox({ prompt: '用户名' });
    if (username === undefined) return;
    const password = await vscode.window.showInputBox({ prompt: '密码', password: true });
    if (password === undefined) return;
    try {
      const resp = await client.login(username, password);
      setCtx('codeaudit.loggedIn', true);
      updateStatusBar();
      log.info(`登录成功：${client.baseUrl}（有效期 ${Math.round(resp.expires_in_s / 60)}min）`);
      vscode.window.showInformationMessage(`CodeAudit 登录成功（有效期 ${Math.round(resp.expires_in_s / 60)}min）`);
    } catch (e) {
      log.error(`登录失败 ${client.baseUrl}：${(e as Error).message}（若本机有代理，检查 VS Code http.proxy/noProxy 设置或以无代理环境启动 VS Code）`);
      vscode.window.showErrorMessage(`CodeAudit 登录失败：${(e as Error).message}`);
    }
  };

  // —— 项目绑定 ——
  const doSelectProject = async (): Promise<void> => {
    if (!client.isLoggedIn()) {
      vscode.window.showWarningMessage('请先执行 CodeAudit: 登录平台');
      return;
    }
    let projects: Awaited<ReturnType<typeof client.listProjects>>;
    try {
      projects = await client.listProjects();
    } catch (e) {
      vscode.window.showErrorMessage(`获取项目列表失败：${(e as Error).message}`);
      return;
    }
    if (projects.length === 0) {
      vscode.window.showInformationMessage('平台暂无项目，请先在控制台创建项目');
      return;
    }
    const picked = await vscode.window.showQuickPick(
      projects.map((p) => ({ label: p.name, description: p.project_id, project: p })),
      { placeHolder: '选择要绑定到当前工作区的平台项目' },
    );
    if (!picked) return;
    await cfg().update('projectId', picked.project.project_id, vscode.ConfigurationTarget.Workspace);
    setCtx('codeaudit.boundProject', true);
    vscode.window.showInformationMessage(`已绑定项目：${picked.label}（写入工作区 .vscode/settings.json）`);
  };

  // —— 扫描 ——
  const requireReady = async (): Promise<string | null> => {
    if (!client.isLoggedIn()) {
      vscode.window.showWarningMessage('请先执行 CodeAudit: 登录平台');
      return null;
    }
    const projectId = cfg().get('projectId', '');
    if (!projectId) {
      vscode.window.showWarningMessage('当前工作区未绑定平台项目，请先执行 CodeAudit: 选择平台项目');
      return null;
    }
    return projectId;
  };

  // 扫描互斥：运行中禁止重复发起——每次扫描都会让平台创建沙箱执行任务（真实资源消耗）
  let scanning = false;

  const doScan = async (): Promise<void> => {
    if (scanning) {
      vscode.window.showWarningMessage(`代码审计进行中（任务 ${lastTaskId.slice(0, 8)}…），完成后方可再次发起；进度见状态栏`);
      return;
    }
    const projectId = await requireReady();
    if (!projectId) return;
    const root = vscode.workspace.workspaceFolders?.[0];
    if (!root) {
      vscode.window.showErrorMessage('没有打开的工作区');
      return;
    }
    // 连通性前置探测：isLoggedIn 只代表本地有凭据。网关不可达时若无此检查，
    // 用户会等打包/上传走完才看到 ECONNREFUSED（白打一次包还误以为是扫描问题）
    try {
      await client.listTools();
    } catch (e) {
      const offlineHint = client.offline ? '（网关不可达：检查 serverUrl 与网络/代理）' : '';
      vscode.window.showErrorMessage(`无法连接平台${offlineHint}：${(e as Error).message}——未发起扫描`);
      return;
    }
    scanning = true;
    setPhase('打包中…');
    try {
      const excludes = cfg().get<string[]>('excludeGlobs', []);
      const uris = await vscode.workspace.findFiles('**/*', `{${excludes.join(',')}}`, 20000);
      log.info(`打包清单：findFiles 命中 ${uris.length} 个文件（排除规则 ${excludes.length} 条，根 ${root.uri.fsPath}）`);
      const files = uris.map((u) => ({ relPath: path.relative(root.uri.fsPath, u.fsPath).replace(/\\/g, '/'), absPath: u.fsPath }));
      // 防空包：曾实测 VS Code 冷启动后短窗内 findFiles 返回不全（3900→1 个文件），
      // 空包上传会白耗平台沙箱并产出误导性"0 发现"，故低于阈值即中止
      const minFiles = cfg().get('minPackFiles', 10);
      if (files.length < minFiles) {
        throw new Error(`打包清单仅 ${files.length} 个文件（低于阈值 ${minFiles}）——疑似 VS Code 文件枚举瞬态异常，已中止上传；请稍后重试，或把 codeaudit.minPackFiles 设为 0 关闭该检查`);
      }
      const skippedFiles: string[] = [];
      const blob = await zipFiles(files, (abs) => fs.promises.readFile(abs), (rel, err) => {
        skippedFiles.push(rel);
        log.warn(`打包跳过 ${rel}：${(err as Error).message}`);
      });
      log.info(`打包完成：zip ${blob.size} 字节（清单 ${files.length} 项${skippedFiles.length > 0 ? `，跳过 ${skippedFiles.length} 项` : ''}）`);
      if (skippedFiles.length > 0) {
        void vscode.window.showWarningMessage(`打包时 ${skippedFiles.length} 个文件读取失败已跳过——这些文件不会进入本次审计（清单见 CodeAudit 输出）`);
      }
      setPhase('上传中…');
      const upload = await client.uploadArchive(blob);
      setPhase('创建任务…');
      // ADR-200 平台契约：上传返回 file_id（桶内对象锚点），经 config.upload_file_id 下发，
      // task 启动时自行从 storage 拉回解包（task-service storage 模式），沙箱拿到的才是
      // 真实项目树。旧平台返回解压目录（dir）经 config.project_path 下发。二者按响应字段
      // 二选一——把桶内对象路径当 project_path 下发会让 dsh-agent 对不存在的相对路径打包
      // 出空包（实测 32B 空 tar.gz → 沙箱白审 → 误导性"0 发现"）
      const taskConfig: Record<string, string> = {};
      if (upload.file_id) {
        taskConfig.upload_file_id = upload.file_id;
      } else if (upload.dir) {
        taskConfig.project_path = upload.dir;
      } else {
        throw new Error(`上传响应缺少 file_id/dir 引用：${JSON.stringify(upload).slice(0, 200)}`);
      }
      const task = await client.createTask(
        projectId,
        cfg().get('scanMode', 'SCAN_MODE_PARALLEL'),
        cfg().get<string[]>('sastTools', []),
        taskConfig,
      );
      lastTaskId = task.task_id;
      void context.workspaceState.update('codeaudit.lastTaskId', task.task_id);
      await client.startTask(task.task_id);
      log.info(`任务已启动：${task.task_id}（模式 ${cfg().get('scanMode', 'SCAN_MODE_PARALLEL')}）`);
      phase = ''; // 任务已建立：状态栏切到 percent 驱动
      vscode.window.showInformationMessage(`代码审计已开始（任务 ${task.task_id.slice(0, 8)}）——进度见状态栏与“CodeAudit-任务进度”面板，AI 交互上下文可实时查看`);
      watchTask(task.task_id, blob.size);
      // 扫描启动即自动展开 AI 交互上下文（codeaudit.autoOpenAiContext 可关；不抢编辑器焦点）
      if (cfg().get('autoOpenAiContext', true)) showAiContext(true);
    } catch (e) {
      scanning = false;
      phase = '';
      updateStatusBar();
      vscode.window.showErrorMessage(`扫描发起失败：${(e as Error).message}`);
    }
  };

  const watchTask = (taskId: string, expectedUploadBytes = 0, resumeState = false): void => {
    watcher?.close();
    cancelRequested = false;
    if (!resumeState) {
      // 新任务开始：侧栏从"漏洞详情"切回"任务进度"（进度实时演进重新有价值）
      setCtx('codeaudit.findingDetail', false);
      progress = createProgressState(taskId);
      lastPaint = null; // 新任务：下一次刷新整页重绘（taskId 变化亦会触发）
    }
    // resumeState=true：bindTask 恢复的非终态任务——保留已重建的历史（logs/AI 正文/进度），
    // 仅续订快照流（否则运行中/暂停中的任务重启后将永远不再更新）
    setCtx('codeaudit.hasTask', true);
    setCtx('codeaudit.taskRunning', true);
    const serverUrl = cfg().get('serverUrl', '');
    const wsHttp = serverUrl.replace(/^http/, 'ws');
    watcher = new TaskWatcher({
      taskId,
      client,
      // 重连续订带游标（ADR-189 logs_after/ai_cursor）：服务端自此起算，不重发已见内容
      wsUrl: (id, token) =>
        `${wsHttp}/v1/tasks/${id}/ws?token=${encodeURIComponent(token)}`
        + (progress && progress.lastLogId ? `&logs_after=${encodeURIComponent(progress.lastLogId)}` : '')
        + (progress && progress.aiCursor > 0 ? `&ai_cursor=${progress.aiCursor}` : ''),
      getAccessToken: () => tokens.getAccessToken(),
      // 轮询回退的增量游标：logs 按 log_id、AI 正文按字节偏移，只取新增
      cursors: () => (progress ? { logsAfter: progress.lastLogId || undefined, aiCursor: progress.aiCursor || undefined } : {}),
      setWsLive: (live) => {
        if (progress) {
          progress.wsLive = live;
          refreshProgressUi();
        }
      },
      onWsEvent: (kind, detail) => {
        if (kind === 'open') log.info(`WS 已连接（任务 ${taskId.slice(0, 8)}…）——实时推送中`);
        else if (kind === 'error') log.warn(`WS 连接失败：${detail || '(无错误详情)'}——已回退 10s 轮询（常见原因：VS Code 代理解析器劫持 ws://，检查 http.proxySupport/http.noProxy 或以无代理环境启动 VS Code）`);
        else log.info(`WS 关闭：${detail || '(正常)'}`);
      },
      onTaskGone: (detail) => {
        // 平台已删除/归档该任务：终止同步（重连死循环防护），本地状态落终态"异常终止"
        // ——否则 progress 停在非终态，状态栏仍显示"暂停扫描"（点了只会再报 404）
        scanning = false;
        setCtx('codeaudit.taskRunning', false);
        setCtx('codeaudit.taskPaused', false);
        if (progress) {
          progress.status = 'TASK_STATUS_DEAD';
          progress.wsLive = false;
          refreshProgressUi();
        }
        log.warn(`任务 ${taskId.slice(0, 8)}… 已在平台删除/归档，停止同步：${detail}`);
        updateStatusBar();
        void vscode.window.showWarningMessage(`任务 ${taskId.slice(0, 8)}… 已在平台删除或归档——已停止进度同步；本地保留的历史结果仍可查看`);
      },
    });
    watcher.on('snapshot', (snap: TaskSnapshot) => {
      // 守卫：快照必须仍属于当前绑定的任务（watcher 被替换/关闭前的在途帧不生效）
      if (!progress || progress.taskId !== taskId) return;
      applyFrame(progress, snap);
      setCtx('codeaudit.taskPaused', snap.task.status === 'TASK_STATUS_PAUSED');
      updateStatusBar();
      // 沙箱收包校验（防空包白审）：沙箱日志"项目打包完成 …（N 字节）"若低于近空包
      // 下限（空 tar.gz 实测 32B 签名），说明平台拉包/解包环节给了沙箱一个空项目——
      // 立即取消任务并明确归因，绝不放行一轮注定产出误导性"0 发现"的白审。
      // （重打包 tar.gz 与上传 zip 字节数不等属正常，故按下限判废而非相等比对。）
      if (expectedUploadBytes > 0 && !progress.uploadSizeChecked) {
        const chk = sandboxPackCheck(progress.logs, expectedUploadBytes);
        if (chk) {
          progress.uploadSizeChecked = true;
          if (chk.tooSmall) {
            log.warn(`沙箱收包异常：上传 ${expectedUploadBytes}B，沙箱项目包仅 ${chk.received}B（低于下限）——取消任务（空项目进沙箱，平台上传件拉取/解包链路故障特征）`);
            void client.cancelTask(taskId).catch(() => {});
            vscode.window.showErrorMessage(
              `扫描中止：沙箱收到的项目包仅 ${chk.received} 字节（本地上传 ${expectedUploadBytes} 字节）——疑似空项目进沙箱，本次结果不可信；请平台侧排查上传件拉取/解包环节`,
            );
          }
        }
      }
      refreshProgressUi();
    });
    watcher.on('terminal', (taskStatus: string) => {
      scanning = false; // 终态（完成/失败/超时/死亡/取消）一律释放互斥
      setCtx('codeaudit.taskRunning', false);
      setCtx('codeaudit.taskPaused', false);
      log.info(`任务终态 ${taskStatus}：logs=${progress?.logs.length ?? 0} ai=${progress?.aiCursor ?? 0}B wsLive=${progress?.wsLive ?? false}`);
      void (async () => {
        // 竞态守卫：任务已切走（lastTaskId 变更）时，在途的收尾不得覆盖新绑定任务的结果
        if (taskId !== lastTaskId) return;
        if (taskStatus === 'TASK_STATUS_CANCELLED' || (cancelRequested && taskStatus !== 'TASK_STATUS_COMPLETED')) {
          updateStatusBar();
          vscode.window.showWarningMessage('代码审计已取消（任务未完成，结果不可用）');
          return;
        }
        if (taskStatus !== 'TASK_STATUS_COMPLETED') {
          updateStatusBar();
          vscode.window.showErrorMessage(`扫描任务未完成：${taskStatus}——阶段与日志详情见“CodeAudit-任务进度”面板`);
          return;
        }
        setPhase('拉取结果…');
        try {
          findingsSource = 'scan';
          const findings = await client.listFindings(taskId);
          renderFindings(findings, { resetTracking: true }); // 新扫描结果是行号的新真相
          clearTaskUi();
          vscode.window.showInformationMessage(`CodeAudit 扫描完成：${findings.length} 条发现`);
        } catch (e) {
          clearTaskUi();
          vscode.window.showErrorMessage(`拉取结果失败：${(e as Error).message}`);
        }
      })();
    });
    watcher.start();
    refreshProgressUi();
  };

  // —— 取消 / 清空 / AI 交互上下文 ——
  const doCancelScan = async (): Promise<void> => {
    if (!watcher || !progress) {
      vscode.window.showInformationMessage('当前没有运行中的代码审计任务');
      return;
    }
    const sure = await vscode.window.showWarningMessage(
      `取消任务 ${progress.taskId.slice(0, 8)}…？已产生的阶段结果将不可用`,
      { modal: true },
      '取消任务',
    );
    if (sure !== '取消任务') return;
    try {
      await client.cancelTask(progress.taskId);
      cancelRequested = true;
      vscode.window.showInformationMessage('取消请求已发送，等待任务收束…');
    } catch (e) {
      vscode.window.showErrorMessage(`取消失败：${(e as Error).message}`);
    }
  };

  const doClearFindings = (): void => {
    renderFindings([], { resetTracking: true });
    updateStatusBar();
    vscode.window.showInformationMessage('已清空本地扫描结果（诊断与面板；平台数据不受影响）');
  };

  // 运行中任务操作菜单（状态栏"任务进行中"点击）：暂停/取消/上下文汇聚一处。
  // 原先取消只藏在任务进度面板标题小图标，且命令面板文案"取消当前任务"与
  // 状态栏"暂停扫描"不一致，用户难联想到取消能力
  const doRunningMenu = async (): Promise<void> => {
    if (!progress || isTerminalTaskStatus(progress.status)) {
      vscode.window.showInformationMessage('当前没有运行中的代码审计任务');
      return;
    }
    const picked = await vscode.window.showQuickPick(
      [
        { label: '$(debug-pause) 暂停扫描', description: '任务保持可恢复', action: 'pause' as const },
        { label: '$(debug-stop) 取消任务', description: '终止任务（结果不可用）', action: 'cancel' as const },
        { label: '$(book) 查看 AI 交互上下文', description: '阶段 / AI 正文 / 任务日志 实时视图', action: 'ai' as const },
      ],
      { placeHolder: `任务 ${progress.taskId.slice(0, 8)}… 进行中——选择操作` },
    );
    if (!picked) return;
    if (picked.action === 'pause') void doPauseScan();
    else if (picked.action === 'cancel') void doCancelScan();
    else showAiContext();
  };

  const showAiContext = (preserveFocus = false): void => {
    // 底部面板视图（不占编辑器空间）：已解析则 show（可 preserveFocus）并增量刷新
    //（保留滚动位置；webview 若被回收，show 触发重新 resolve 整页渲染当前态），
    // 未解析（面板从未打开）用 focus 命令触发解析
    if (aiView.view) {
      aiView.view.show(preserveFocus);
      aiView.postUpdate();
    } else {
      void vscode.commands.executeCommand('codeaudit.aiContext.focus');
    }
  };

  // —— 打开漏洞位置 ——
  // preserveFocus 默认 true：树/键盘路径逐条浏览时焦点不丢（Enter 后继续用键盘）；
  // 详情面板"打开代码位置"是显式跳转意图，传 false 让编辑器拿到焦点
  const doOpenFinding = async (f?: UnifiedFinding, opts?: { preserveFocus?: boolean }): Promise<void> => {
    if (!f) return;
    showFindingDetail(f);
    const uri = docUriFor(f.location?.file_path ?? '');
    if (!uri) {
      vscode.window.showInformationMessage(`该发现无工作区相对位置：${f.location?.file_path ?? '(无)'}`);
      return;
    }
    let doc: vscode.TextDocument;
    try {
      doc = await vscode.workspace.openTextDocument(uri);
    } catch {
      vscode.window.showInformationMessage(`无法打开 ${f.location?.file_path}：文件在工作区中不存在或不可读（详情视图仍可查看该发现）`);
      return;
    }
    const line = Math.max(0, (f.location?.start_line ?? 1) - 1);
    await vscode.window.showTextDocument(doc, { selection: new vscode.Range(line, 0, line, 0), preserveFocus: opts?.preserveFocus ?? true });
  };

  // —— AI 修复（自动 patch + 人工 diff 确认落盘，对标 Cline）——
  // 主路径：finding.diff_patch（apply_patch 语法，ADR-183 全链产出，服务端已按工作区
  // 逐字校验）→ 完整 PatchParser 移植（applyPatch.ts，逐字对标 Cline），支持
  // 多文件 / Add File / Delete File / Move to。
  // 兜底路径：diff_patch 置空（服务端校验拒绝，或 ADR-183 之前旧任务）→ 从
  // ai_fix_suggestion 提取 unified diff 围栏（diffParse.ts）。两者失败语义一致：
  // 任一 hunk 锚定失败 → 整体拒绝，绝不部分应用、绝不静默错切。
  const doFixFinding = async (arg?: UnifiedFinding): Promise<void> => {
    // 命令面板入口不带参数：不能静默返回（用户会以为没反应），用 QuickPick 选择目标发现
    let f = arg;
    if (!f) {
      if (currentFindings.length === 0) {
        vscode.window.showInformationMessage('尚无扫描发现，请先执行 CodeAudit: 扫描工作区');
        return;
      }
      const picked = await vscode.window.showQuickPick(
        currentFindings.map((x) => ({ label: findingLabel(x), description: findingDescription(x), finding: x })),
        { placeHolder: '选择要 AI 修复的发现' },
      );
      if (!picked) return;
      f = picked.finding;
    }
    if (!f) return;
    const label = f.title || f.cwe_id || `${f.location?.file_path ?? '?'}:${f.location?.start_line ?? '?'}`;
    if (f.diff_patch) {
      await doApplyPatchFix(f, label);
      return;
    }
    if (!f.ai_fix_suggestion) {
      // 诚实降级：平台未产出修复建议，不伪造补丁
      vscode.window.showWarningMessage(`${label}：平台暂无 AI 修复建议，无法自动修复`);
      return;
    }
    const relPath = f.location?.file_path ?? '';
    const uri = docUriFor(relPath);
    if (!uri) {
      vscode.window.showWarningMessage(`${label}：该发现无精确文件位置，无法在编辑器中应用修复`);
      return;
    }
    const diffText = extractDiffBlock(f.ai_fix_suggestion);
    if (!diffText) {
      // 兜底路径走到此处 = 无机器补丁且建议为自然语言（无 ```diff 围栏）：无法自动 patch，明确告知
      vscode.window.showWarningMessage(`${label}：无机器补丁，平台建议为自然语言描述、未包含可应用补丁，暂无法自动修复`);
      return;
    }
    const patches = parseUnifiedDiff(diffText).filter((p) => p.hunks.length > 0);
    const patch = patches.find((p) => p.oldPath === relPath.replace(/\\/g, '/')) ?? patches[0];
    if (!patch) {
      vscode.window.showWarningMessage('补丁解析失败');
      return;
    }
    let doc: vscode.TextDocument;
    try {
      doc = await vscode.workspace.openTextDocument(uri);
    } catch {
      // 诚实降级：目标文件不在工作区（已删除/路径漂移），补丁无从应用
      vscode.window.showWarningMessage(`${label}：目标文件不存在或不可读（${relPath}），无法应用补丁`);
      return;
    }
    const original = doc.getText().split(/\r?\n/);
    // 内容锚定应用（对标 Cline apply_patch）：行号漂移时按内容唯一定位；
    // 任何 hunk 锚定失败 → 整体拒绝，绝不部分应用、绝不静默错切
    const result = applyPatchToLines(original, patch);
    if (result.lines === null) {
      vscode.window.showWarningMessage(formatPatchFailures(patch.oldPath, result.failures));
      return;
    }
    const fixed = result.lines;
    const fuzzNote = result.fuzz > 0 ? ` [容错匹配 fuzz=${result.fuzz}]` : '';
    // 一键修复（无确认门）：checkpoint → 落盘 → 展示变更 diff → 告知（可随时回滚）
    const beforeContent = doc.getText();
    const cpId = checkpoints.save({ [uri.fsPath]: beforeContent });
    const edit = new vscode.WorkspaceEdit();
    const fullRange = new vscode.Range(doc.positionAt(0), doc.positionAt(doc.getText().length));
    edit.replace(uri, fullRange, fixed.join(doc.eol === vscode.EndOfLine.CRLF ? '\r\n' : '\n'));
    const ok = await vscode.workspace.applyEdit(edit);
    if (ok) {
      // applyEdit 只改内存缓冲区；不保存落盘的话关闭窗口即丢失修复，且与已创建的 checkpoint 语义不符
      if (!await doc.save()) {
        vscode.window.showErrorMessage(`修复写入 ${uri.fsPath} 失败（文件被外部修改或无权限）`);
        return;
      }
      if (cpId) {
        // 逆补丁数据（外科回滚用）+ 行偏移（诊断行号迁移）：old/new 现算
        const fixedContent = fixed.join(doc.eol === vscode.EndOfLine.CRLF ? '\r\n' : '\n');
        const bp = buildFilePatch(beforeContent, fixedContent);
        const patches: Record<string, FilePatch> | undefined =
          bp && bp.hunks.length > 0 ? { [uri.fsPath]: { oldPath: relPath, newPath: relPath, hunks: bp.hunks } } : undefined;
        fixRegistry.recordApplied({
          findingId: f.finding_id,
          label,
          checkpointId: cpId,
          files: [uri.fsPath],
          appliedAt: Date.now(),
          state: 'applied',
          patches,
          linesBefore: { [uri.fsPath]: linesSnapshotForFile(relPath) },
        });
        if (bp) applyTrackedShifts({ [uri.fsPath]: bp.shifts });
      }
      // 变更审阅视图：左=修复前快照（虚拟文档），右=当前文件（实时）
      const beforeUri = uri.with({ scheme: 'codeaudit-fix', path: `/${path.basename(uri.fsPath)}.before`, query: Buffer.from(JSON.stringify({ content: beforeContent }), 'utf8').toString('base64') });
      // diff 对照"供查看"，preserveFocus 不把光标从面板拽走；点编辑器即可编辑
      await vscode.commands.executeCommand('vscode.diff', beforeUri, uri, `AI 已修复：${label}${fuzzNote}——可在 CodeAudit 面板回滚`, { preserveFocus: true });
      // 发现保留（应用补丁 ≠ 风险记录消失），仅刷新"已修复"徽章；诊断不动（扫描快照的诚实展示）
      renderFindings(currentFindings);
      refreshFindingDetail(f);
      vscode.window.showInformationMessage(`AI 已修复：${label}${fuzzNote}——发现已标记为已修复，可在 CodeAudit 面板随时回滚`);
    } else {
      vscode.window.showErrorMessage('修复应用失败：VS Code 拒绝了编辑（applyEdit=false）');
    }
  };

  // —— 机器补丁（diff_patch，apply_patch 语法）——
  // 应用核心（手动修复与低风险自动应用共用）：解析 + 路径禁闭校验 + checkpoint +
  // 落盘 + 登记。任一校验失败 → 整体拒绝（返回原因，不弹 UI、不改盘），绝不部分
  // 应用、绝不静默错切。逐文件 diff 审阅数据一并返回：手动路径打开审阅视图，
  // 自动路径不用（避免轰炸编辑器）。
  const applyMachinePatch = async (
    f: UnifiedFinding,
    label: string,
  ): Promise<
    | { ok: true; fileCount: number; fuzz: number; saveFailures: number; saveTotal: number; diffs: { fileLabel: string; beforeUri: vscode.Uri; rightUri: vscode.Uri }[] }
    | { ok: false; reason: string }
  > => {
    let computed: { changes: Record<string, PatchFileChange>; fuzz: number };
    const docs = new Map<string, vscode.TextDocument>(); // 补丁路径 → 已打开文档（Update/Delete 段）
    try {
      for (const rel of listUpdatedFiles(f.diff_patch)) {
        const w = resolveWsPath(rel);
        if (!w) return { ok: false, reason: `补丁引用了越界路径，已整体拒绝：${rel}` };
        try {
          const doc = await vscode.workspace.openTextDocument(w.uri);
          docs.set(rel, doc); // 编辑器缓冲区内容为当前真相
        } catch {
          // 文件不存在：不入 currentFiles，解析器将以 Missing File 拒绝整个补丁
        }
      }
      const currentFiles: Record<string, string> = {};
      for (const [rel, doc] of docs) currentFiles[rel] = doc.getText();
      computed = computePatchChanges(f.diff_patch, currentFiles);
    } catch (e) {
      // DiffError：语法坏 / 文件缺失 / 重复段 / 任一 hunk 上下文失配 → 整体拒绝
      return { ok: false, reason: `机器补丁被拒绝（文件未改动）：${(e as Error).message}` };
    }
    const entries = Object.entries(computed.changes);
    if (entries.length === 0) return { ok: false, reason: '机器补丁未包含任何文件变更' };
    // 变更路径解析（禁闭校验；Add/Move 目标必须不存在——拒绝覆盖既有文件）
    type PlanEntry = { rel: string; abs: string; uri: vscode.Uri; change: PatchFileChange; doc?: vscode.TextDocument; moveAbs?: string };
    const plan: PlanEntry[] = [];
    for (const [rel, change] of entries) {
      const w = resolveWsPath(rel);
      if (!w) return { ok: false, reason: `补丁变更路径越界，已整体拒绝：${rel}` };
      let moveAbs: string | undefined;
      if (change.movePath) {
        const mw = resolveWsPath(change.movePath);
        if (!mw) return { ok: false, reason: `补丁 Move to 路径越界，已整体拒绝：${change.movePath}` };
        moveAbs = mw.abs;
      }
      const createTarget = change.type === PatchActionType.ADD ? w.abs : moveAbs;
      if (createTarget !== undefined && fs.existsSync(createTarget)) {
        return { ok: false, reason: `补丁要创建的目标文件已存在，拒绝覆盖：${change.type === PatchActionType.ADD ? rel : change.movePath}` };
      }
      plan.push({ rel, abs: w.abs, uri: w.uri, change, doc: docs.get(rel), moveAbs });
    }

    // checkpoint：落盘前创建（null = 修复前不存在，回滚时删除而非还原）
    const snapshot: Record<string, string | null> = {};
    for (const e of plan) {
      if (e.change.type === PatchActionType.UPDATE || e.change.type === PatchActionType.DELETE) snapshot[e.abs] = e.change.oldContent ?? e.doc?.getText() ?? '';
      else snapshot[e.abs] = null; // Add 目标
      if (e.moveAbs) snapshot[e.moveAbs] = null; // Move 目标
    }
    const cpId = checkpoints.save(snapshot);
    const touchedFiles = [...new Set([...Object.keys(snapshot)])];

    // 应用：Update 走 WorkspaceEdit+保存（保留撤销栈，缓冲区即真相）；
    // Add/Move 目标与 Delete/Move 源走 fs（VS Code 随磁盘变更自动刷新）。
    // 顺序即失败语义：先缓冲区（失败=零改动）→ 再 fs（失败=用 checkpoint 尽力
    // 还原已执行部分）——任何失败路径都必须兑现"整体拒绝，绝不部分应用"。
    const edit = new vscode.WorkspaceEdit();
    const toSave: vscode.TextDocument[] = [];
    const writes: { abs: string; content: string }[] = [];
    const deletes: string[] = [];
    for (const e of plan) {
      if (e.change.type === PatchActionType.UPDATE && e.change.newContent !== undefined && e.doc && !e.moveAbs) {
        edit.replace(e.uri, new vscode.Range(e.doc.positionAt(0), e.doc.positionAt(e.doc.getText().length)), e.change.newContent);
        toSave.push(e.doc);
      } else if (e.change.type === PatchActionType.UPDATE && e.change.newContent !== undefined && e.moveAbs) {
        writes.push({ abs: e.moveAbs, content: e.change.newContent });
        deletes.push(e.abs);
      } else if (e.change.type === PatchActionType.ADD && e.change.newContent !== undefined) {
        writes.push({ abs: e.abs, content: e.change.newContent });
      } else if (e.change.type === PatchActionType.DELETE) {
        deletes.push(e.abs);
      } else {
        // 不可达（解析器保证 newContent/doc 存在）；防御性整体放弃，绝不静默跳过
        return { ok: false, reason: `补丁变更缺少可应用内容，已放弃：${e.rel}` };
      }
    }
    if (toSave.length > 0 && !await vscode.workspace.applyEdit(edit)) {
      // 此时尚未发生任何 fs 变更：工作区（缓冲区+磁盘）完全未动，checkpoint 未消耗
      return { ok: false, reason: '修复应用失败：VS Code 拒绝了编辑（applyEdit=false），工作区未改动' };
    }
    try {
      for (const w of writes) {
        await fs.promises.mkdir(path.dirname(w.abs), { recursive: true });
        await fs.promises.writeFile(w.abs, w.content, 'utf8');
      }
      for (const d of deletes) await fs.promises.rm(d, { force: true });
    } catch (e) {
      // fs 阶段失败（Windows EBUSY/EACCES 等）：先用 checkpoint 把已执行的写入/删除
      // 尽力还原，再整体报败——绝不留下"半应用"状态
      for (const [abs, content] of Object.entries(snapshot)) {
        try {
          if (content === null) await fs.promises.rm(abs, { force: true });
          else await fs.promises.writeFile(abs, content, 'utf8');
        } catch {
          /* 尽力而为：主因随 reason 上报 */
        }
      }
      return { ok: false, reason: `补丁落盘失败，已还原已执行部分：${(e as Error).message}` };
    }
    let saveFailures = 0;
    const saveTotal = toSave.length;
    if (toSave.length > 0) {
      const saved = await Promise.all(toSave.map((d) => d.save()));
      saveFailures = saved.filter((s) => !s).length;
    }
    // 发现保留（应用补丁 ≠ 风险记录消失）；登记供按发现回滚
    if (cpId) {
      // 逆补丁数据 + 行偏移表：纯 Update 修复逐文件从 old/new 现算带上下文
      // hunks（buildFilePatch），支撑外科回滚（任意序回滚不覆盖同文件更晚修复）
      // 与诊断行号迁移。含 Add/Delete/Move 或差异过大时放弃（patches 缺省 →
      // 回滚自动降级 checkpoint 整文件覆盖）
      let patches: Record<string, FilePatch> | undefined;
      const patchMap: Record<string, FilePatch> = {};
      const shiftsByFile: Record<string, import('./diffParse').LineShift[]> = {};
      let patchesOk = plan.length > 0;
      for (const e of plan) {
        if (e.change.type === PatchActionType.UPDATE && e.change.oldContent !== undefined && e.change.newContent !== undefined && !e.moveAbs) {
          const bp = buildFilePatch(e.change.oldContent, e.change.newContent);
          if (!bp || bp.hunks.length === 0) {
            patchesOk = false;
            break;
          }
          patchMap[e.abs] = { oldPath: e.rel, newPath: e.rel, hunks: bp.hunks };
          shiftsByFile[e.abs] = bp.shifts;
        } else {
          patchesOk = false;
          break;
        }
      }
      if (patchesOk) patches = patchMap;
      // linesBefore 快照：该修复应用前各触及文件上所有发现的行号——整文件覆盖
      // 回滚（跳回该修复前状态）时据此精确恢复行号
      const linesBefore: Record<string, Record<string, number>> = {};
      for (const e of plan) linesBefore[e.abs] = linesSnapshotForFile(e.rel);
      fixRegistry.recordApplied({ findingId: f.finding_id, label, checkpointId: cpId, files: touchedFiles, appliedAt: Date.now(), state: 'applied', patches, linesBefore });
      if (patchesOk) applyTrackedShifts(shiftsByFile);
    }
    // 逐文件 diff 审阅数据：左=修复前快照（虚拟文档），右=当前文件（实时）。
    // 应用后展示而非应用前审批——补丁随时可回滚，审阅是事后动作而非门禁。
    const diffs = plan.map((e) => {
      const beforeContent = snapshot[e.abs] ?? '';
      const beforeUri = vscode.Uri.from({ scheme: 'codeaudit-fix', path: `/${(e.change.movePath ?? e.rel)}.before`, query: Buffer.from(JSON.stringify({ content: beforeContent }), 'utf8').toString('base64') });
      const rightUri = e.change.type === PatchActionType.DELETE ? emptyDocUri(`${e.rel}.deleted`) : e.moveAbs ? vscode.Uri.file(e.moveAbs) : e.uri;
      return { fileLabel: e.change.movePath ? `${e.rel} → ${e.change.movePath}` : e.rel, beforeUri, rightUri };
    });
    return { ok: true, fileCount: plan.length, fuzz: computed.fuzz, saveFailures, saveTotal, diffs };
  };

  const doApplyPatchFix = async (f: UnifiedFinding, label = f.title || f.cwe_id || `${f.location?.file_path ?? '?'}:${f.location?.start_line ?? '?'}`): Promise<void> => {
    const r = await applyMachinePatch(f, label);
    if (!r.ok) {
      vscode.window.showErrorMessage(r.reason);
      return;
    }
    const fuzzNote = r.fuzz > 0 ? ` [容错匹配 fuzz=${r.fuzz}]` : '';
    for (const d of r.diffs) {
      // 文件名前置：多次修复的 diff 标签在编辑器标签栏并存时可分辨
      // diff 对照"供查看"，preserveFocus 不把光标从面板拽走；点编辑器即可编辑
      await vscode.commands.executeCommand('vscode.diff', d.beforeUri, d.rightUri, `AI 已修复：${d.fileLabel} · ${label}${fuzzNote}——可在 CodeAudit 面板回滚`, { preserveFocus: true });
    }
    renderFindings(currentFindings); // 仅刷新"已修复"徽章；诊断不动（扫描快照的诚实展示）
    refreshFindingDetail(f);
    if (r.saveFailures > 0) {
      vscode.window.showWarningMessage(`已应用，但 ${r.saveFailures}/${r.saveTotal} 个文件保存失败，请手动保存`);
    }
    // 告知性通知（无操作按钮）：修复已落盘，不满意随时回退。
    // fuzz≥1000 = 相似度级锚定（可能错位——实测出现过把 import 插进方法体的案例）：
    // 升级为警告并提示审阅 diff；其余为普通告知
    const msg = `AI 已修复：${label}（${r.fileCount} 个文件${fuzzNote}）——发现已标记为已修复，可在 CodeAudit 面板随时回滚`;
    if (r.fuzz >= 1000) {
      void vscode.window.showWarningMessage(`补丁未精确锚定${fuzzNote}，请在 diff 视图确认变更位置。${msg}`);
    } else {
      vscode.window.showInformationMessage(msg);
    }
  };

  // —— 低风险批量应用（手动触发，绝不自动改盘）——
  // 入口：扫描结果面板标题栏按钮 / 命令面板。候选 = LOW/INFO 且 AI 置信度≥0.9 且带
  // 机器补丁且登记表未见过（筛选纯逻辑见 lowRiskApply.ts）；QuickPick 多选让人逐条
  // 裁决，选中项走与手动修复同一套 applyMachinePatch 核心（禁闭/整体拒绝/checkpoint/
  // 登记一致），不打开 diff 视图，汇总通知收尾；单条校验失败跳过并记日志，不中断其余。
  const doApplyLowRiskFixes = async (): Promise<void> => {
    if (currentFindings.length === 0) {
      vscode.window.showInformationMessage('尚无扫描结果，请先执行 CodeAudit: 扫描工作区');
      return;
    }
    const candidates = selectLowRiskFixCandidates(currentFindings, fixRegistry.knownFindingIds());
    if (candidates.length === 0) {
      vscode.window.showInformationMessage('没有可批量应用的低风险修复（条件：LOW/INFO · AI 置信度≥0.9 · 带机器补丁 · 未应用/未回滚过）');
      return;
    }
    const picked = await vscode.window.showQuickPick(
      candidates.map((f) => ({ label: findingLabel(f), description: findingDescription(f), finding: f })),
      { canPickMany: true, placeHolder: `勾选要应用的低风险修复（${candidates.length} 条候选）` },
    );
    if (!picked || picked.length === 0) return;
    let applied = 0;
    let skipped = 0;
    for (const p of picked) {
      const r = await applyMachinePatch(p.finding, p.label);
      if (r.ok) {
        applied++;
      } else {
        skipped++;
        log.warn(`低风险修复跳过「${p.label}」：${r.reason}`);
      }
    }
    renderFindings(currentFindings); // 刷新"✔ 已修复"徽章
    if (applied > 0) {
      vscode.window.showInformationMessage(
        `已应用 ${applied} 条低风险修复——checkpoint 已落盘，可在 CodeAudit 面板回滚`
        + (skipped > 0 ? `；${skipped} 条补丁校验未过已跳过（详见 CodeAudit 输出）` : ''),
      );
    } else if (skipped > 0) {
      vscode.window.showWarningMessage(`${skipped} 条补丁校验未过，工作区未改动（详见 CodeAudit 输出）`);
    }
  };

  // —— 回滚 ——
  // 把 checkpoint 快照写回工作区（Update/还原内容；Add/Move 目标等 null 条目 = 删除文件）。
  // 返回 null = VS Code 拒绝编辑或写入校验未过（checkpoint 未消耗，可重试）；否则返回摘要文案。
  // 写入后必须校验缓冲区与磁盘：applyEdit/save 存在"返回 true 但模型/磁盘未变"的静默
  // 空转路径（GUI 实测复现过一次），不校验就会谎报"已回滚"而工作区原样。
  const writeRestored = async (restored: Record<string, string | null>): Promise<string | null> => {
    const norm = (s: string): string => s.replace(/\r\n/g, '\n');
    const edit = new vscode.WorkspaceEdit();
    const expected: { doc: vscode.TextDocument; content: string }[] = [];
    let deletedCount = 0;
    try {
      for (const [fsPath, content] of Object.entries(restored)) {
        const uri = vscode.Uri.file(fsPath);
        if (content === null) {
          // 修复前不存在的文件（Add File / Move to 目标）：回滚 = 删除
          edit.deleteFile(uri, { ignoreIfNotExists: true });
          deletedCount++;
          continue;
        }
        const doc = await vscode.workspace.openTextDocument(uri);
        expected.push({ doc, content });
        edit.replace(uri, new vscode.Range(doc.positionAt(0), doc.positionAt(doc.getText().length)), content);
      }
    } catch (e) {
      vscode.window.showErrorMessage(`回滚失败：文件读取异常：${(e as Error).message}`);
      return null;
    }
    const bufferInSync = async (): Promise<boolean> =>
      expected.every((e) => norm(e.doc.getText()) === norm(e.content));
    if (!await vscode.workspace.applyEdit(edit) || !(await bufferInSync())) {
      // applyEdit=false 或缓冲区未变（静默空转）：重试一次，再失败则显式报错
      const retry = new vscode.WorkspaceEdit();
      for (const e of expected) {
        if (norm(e.doc.getText()) === norm(e.content)) continue;
        retry.replace(e.doc.uri, new vscode.Range(e.doc.positionAt(0), e.doc.positionAt(e.doc.getText().length)), e.content);
      }
      const retryApplied = retry.size > 0 ? await vscode.workspace.applyEdit(retry) : true;
      if (!retryApplied || !(await bufferInSync())) {
        vscode.window.showErrorMessage('回滚失败：VS Code 未把恢复内容写入缓冲区（已重试仍不一致），checkpoint 未消耗，修复状态未变更');
        return null;
      }
    }
    // 回滚同样需要显式保存，否则仅存在于未保存缓冲区
    const saved = await Promise.all(expected.map((e) => e.doc.save()));
    const summary = `已回滚 ${expected.length} 个文件${deletedCount > 0 ? `、删除 ${deletedCount} 个新增文件` : ''}`;
    if (!saved.every(Boolean)) {
      return `${summary}（${saved.filter(Boolean).length}/${expected.length} 个文件保存成功，其余请手动保存）`;
    }
    // 磁盘终验：save() 报 true 不代表字节已落盘（观测过静默 no-op）
    const decoder = new TextDecoder('utf-8');
    for (const e of expected) {
      try {
        const onDisk = decoder.decode(await vscode.workspace.fs.readFile(e.doc.uri));
        if (norm(onDisk) !== norm(e.content)) {
          vscode.window.showErrorMessage(
            `回滚未落盘：${e.doc.uri.fsPath.split(/[\\/]/).pop()}（保存报成功但磁盘内容未变）；checkpoint 未消耗，修复状态未变更`,
          );
          return null;
        }
      } catch {
        vscode.window.showErrorMessage(`回滚校验失败：无法读取 ${e.doc.uri.fsPath} 验证落盘结果；修复状态未变更`);
        return null;
      }
    }
    return summary;
  };

  // 外科回滚：有逆补丁数据的纯 Update 修复，把每文件的补丁交换 old/new 后
  // 按内容锚定应用——只撤销该修复引入的变更，同文件上更晚修复的内容原样保留
  // （任意序回滚；整文件覆盖会把更晚修复一并抹掉，多修复时被迫 LIFO）。
  // 锚定 fuzz>1（内容漂移/后续修复重叠）或文件缺失 → 返回 null 降级整文件覆盖，
  // 绝不带高 fuzz 强上——错切比覆盖更危险。
  const trySurgicalRollback = async (
    rec: FixRecord,
  ): Promise<{ restored: Record<string, string>; shiftsByFile: Record<string, import('./diffParse').LineShift[]> } | null> => {
    const patches = rec.patches;
    if (!patches || rec.files.length !== Object.keys(patches).length) return null; // 含非 Update 变更或登记不全
    const restored: Record<string, string> = {};
    const shiftsByFile: Record<string, import('./diffParse').LineShift[]> = {};
    const decoder = new TextDecoder('utf-8');
    for (const [abs, patch] of Object.entries(patches)) {
      let text: string;
      try {
        text = decoder.decode(await vscode.workspace.fs.readFile(vscode.Uri.file(abs)));
      } catch {
        return null;
      }
      const inv = invertFilePatch(patch);
      const r = applyPatchToLines(text.split(/\r?\n/), inv);
      if (r.lines === null || r.fuzz > 1) return null;
      const eol = text.includes('\r\n') ? '\r\n' : '\n';
      restored[abs] = r.lines.join(eol);
      shiftsByFile[abs] = r.applied.map((a) => ({
        start: a.start,
        delCount: inv.hunks[a.hunkIndex].oldLines.length,
        delta: inv.hunks[a.hunkIndex].newLines.length - inv.hunks[a.hunkIndex].oldLines.length,
      }));
    }
    return { restored, shiftsByFile };
  };

  // 按发现回滚：优先外科回滚（逆补丁，不影响同文件更晚修复）；无补丁数据或
  // 锚定失败则降级 checkpoint 整文件覆盖（更晚修复被覆盖时明确警告，由用户裁决）。
  // 状态翻为 rolledback（可重新应用）。
  const rollbackRecord = async (rec: FixRecord): Promise<void> => {
    const later = fixRegistry.appliedRecords().filter((r) => r.appliedAt > rec.appliedAt && r.files.some((x) => rec.files.includes(x)));
    if (rec.patches && Object.keys(rec.patches).length > 0) {
      const surgical = await trySurgicalRollback(rec);
      if (surgical) {
        const summary = await writeRestored(surgical.restored);
        if (summary === null) return;
        fixRegistry.markRolledback(rec.findingId);
        // 行号按逆补丁锚点增量迁移（比快照恢复更准：同文件后续修复的偏移被保留）；
        // 此前被标注"行号可能偏移"的文件一并解除（已精确校准）
        applyTrackedShifts(surgical.shiftsByFile);
        for (const abs of Object.keys(surgical.restored)) {
          const rel = absPathToRel(abs);
          if (rel) degradedLineFiles.delete(rel);
        }
        renderFindings(currentFindings); // 徽章解除，内联按钮回到"AI 修复"（可重新应用）
        const rolledFinding = currentFindings.find((x) => x.finding_id === rec.findingId);
        if (rolledFinding) refreshFindingDetail(rolledFinding);
        vscode.window.showInformationMessage(
          `${summary}——「${rec.label}」已回滚（外科精确撤销，同文件其他修复不受影响），可重新执行 AI 修复再次应用`,
        );
        return;
      }
      log.warn(`「${rec.label}」逆补丁锚定失败（文件内容漂移或后续修复重叠），降级为 checkpoint 整文件回滚`);
    }
    if (later.length > 0) {
      const go = await vscode.window.showWarningMessage(
        `回滚「${rec.label}」将把 ${rec.files.length} 个文件恢复到该修复前状态，同文件上更晚应用的 ${later.length} 个修复内容会被一并覆盖：${later.map((l) => l.label).join('、')}`,
        '仍要回滚',
      );
      if (go !== '仍要回滚') return;
    }
    let restored: Record<string, string | null> | null;
    try {
      restored = checkpoints.restore(rec.checkpointId);
    } catch (e) {
      vscode.window.showErrorMessage(`回滚失败：checkpoint 读取异常：${(e as Error).message}`);
      return;
    }
    if (!restored) {
      vscode.window.showErrorMessage(`回滚失败：checkpoint ${rec.checkpointId} 缺失或损坏（文件可能被清理）；修复状态未变更`);
      return;
    }
    const summary = await writeRestored(restored);
    if (summary === null) return;
    fixRegistry.markRolledback(rec.findingId);
    // 被本次恢复一并覆盖掉的更晚修复，内容已不在文件里，必须同步解除 applied，
    // 否则徽章误报"已修复"且再修复循环被卡死（回滚它还会复活已明确回滚的内容）。
    for (const l of later) fixRegistry.markRolledback(l.findingId);
    // 整文件覆盖 = 文件跳回该修复前状态：按 linesBefore 快照恢复行号（含被一并
    // 覆盖的更晚修复发现）；快照缺失（旧记录）只能标注"行号可能偏移"诚实降级
    if (rec.linesBefore) {
      for (const snap of Object.values(rec.linesBefore)) {
        for (const [fid, ln] of Object.entries(snap)) trackedLines.set(fid, ln);
      }
      persistTrackedLines();
    } else {
      for (const abs of rec.files) {
        const rel = absPathToRel(abs);
        if (rel) degradedLineFiles.add(rel);
      }
    }
    renderFindings(currentFindings); // 徽章解除，内联按钮回到"AI 修复"（可重新应用）
    const rolledFinding = currentFindings.find((x) => x.finding_id === rec.findingId);
    if (rolledFinding) refreshFindingDetail(rolledFinding);
    vscode.window.showInformationMessage(`${summary}——「${rec.label}」已回滚，可重新执行 AI 修复再次应用`);
  };

  // 命令入口"回滚此漏洞修复"（树上已修复条目的内联按钮/右键）。
  // 无参调用（命令面板）与 doFixFinding 对称：QuickPick 列出可回滚项，
  // 不再只弹"去面板找按钮"的提示
  const doRollbackFix = async (arg?: unknown): Promise<void> => {
    let f = asFinding(arg);
    if (!f) {
      const candidates = rollbackPickItems(currentFindings, fixRegistry.appliedFindingIds(), trackedLinesSnapshot());
      if (candidates.length === 0) {
        vscode.window.showInformationMessage('没有已应用的修复可回滚——修复后在 CodeAudit 面板对已修复（✔）的发现使用回滚按钮，或执行「回滚最近一次批量修复」');
        return;
      }
      const picked = await vscode.window.showQuickPick(candidates, { placeHolder: '选择要回滚的修复（将把文件恢复到该修复前状态）' });
      if (!picked) return;
      f = picked.finding;
    }
    const rec = fixRegistry.byFinding(f.finding_id);
    if (!rec || rec.state !== 'applied') {
      vscode.window.showInformationMessage(`「${f.title || f.finding_id}」当前没有已应用的修复`);
      return;
    }
    await rollbackRecord(rec);
  };

  // 回滚最近一次批量修复：优先走登记表（最近应用的记录）；无登记时兜底 restoreLatest
  const doRollback = async (): Promise<void> => {
    const rec = fixRegistry.appliedRecords().sort((a, b) => b.appliedAt - a.appliedAt)[0];
    if (rec) {
      await rollbackRecord(rec);
      return;
    }
    let restored: Record<string, string | null> | null;
    try {
      restored = checkpoints.restoreLatest();
    } catch (e) {
      vscode.window.showErrorMessage(`回滚失败：checkpoint 读取异常：${(e as Error).message}`);
      return;
    }
    if (!restored) {
      vscode.window.showInformationMessage('没有可回滚的修复 checkpoint');
      return;
    }
    const summary = await writeRestored(restored);
    if (summary !== null) vscode.window.showInformationMessage(summary);
  };

  // 绑定/切换到指定任务并拉取历史数据（不重扫）：一次快照重建进度视图终态
  // （阶段/日志/AI 交互全文）+ findings 全量。重载恢复与「切换任务」共用。
  const bindTask = async (taskId: string, opts: { silent?: boolean } = {}): Promise<void> => {
    // 切换标志须在 lastTaskId 赋值前捕获：换绑任务=新发现集（行号跟踪清零），
    // 同任务重绑定（启动恢复/刷新兜底）保留本地校准，否则重载即丢修复后行号
    const isSwitch = taskId !== lastTaskId;
    try {
      const snap = await client.taskSnapshot(taskId);
      // 切换绑定前先收口旧跟踪：旧 watcher 的 snapshot/terminal 事件不得再写进
      // 新任务的 progress（状态/日志混入）或用旧任务 findings 覆盖渲染；
      // 互斥标志同步复位，否则绑定终态任务后旧任务不终态会让 scanning 永久为 true
      watcher?.close();
      watcher = null;
      scanning = false;
      cancelRequested = false;
      progress = createProgressState(taskId);
      applyFrame(progress, snap);
      lastTaskId = taskId;
      void context.workspaceState.update('codeaudit.lastTaskId', taskId);
      setCtx('codeaudit.hasTask', true);
      setCtx('codeaudit.taskRunning', false);
      setCtx('codeaudit.taskPaused', snap.task.status === 'TASK_STATUS_PAUSED');
      progressTree.setItems(buildProgressItems(progress));
      updateStatusBar();
      refreshProgressUi();
      // 非终态任务（运行中/暂停中）重启后必须续订快照流，否则进度/暂停恢复不再可见
      if (!isTerminalTaskStatus(snap.task.status)) {
        watchTask(taskId, 0, true);
      }
      const findings = await client.listFindings(taskId);
      findingsSource = 'bound';
      renderFindings(findings, { resetTracking: isSwitch });
      // 绑定路径补状态栏刷新：上面已按旧 findings 刷过一次，拉到历史结果后
      // 不再刷的话状态栏计数停在切换前的旧任务上（刷新路径自愈、绑定路径曾漏）
      updateStatusBar();
      if (!opts.silent) {
        vscode.window.showInformationMessage(`已绑定任务 ${taskId.slice(0, 8)}：${findings.length} 条发现（历史数据，从平台拉取，未重新扫描）`);
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (/404/.test(msg) && /not found/i.test(msg)) {
        // 上次任务已被平台删除/归档：清掉本地绑定指针，避免下次启动再恢复一个 404 任务
        lastTaskId = '';
        void context.workspaceState.update('codeaudit.lastTaskId', undefined);
        log.warn(`上次任务已不存在（平台删除/归档），已清除本地绑定：${msg}`);
        return;
      }
      if (!opts.silent) vscode.window.showErrorMessage(`绑定任务失败：${msg}`);
    }
  };

  // 平台该项目最近一次已完成的任务（时间倒序第一个 COMPLETED）
  const latestCompletedTask = async (projectId: string): Promise<string | null> => {
    const tasks = await client.listTasks(projectId);
    return tasks.find((t) => t.status === 'TASK_STATUS_COMPLETED')?.task_id ?? null;
  };

  const doRefresh = async (): Promise<void> => {
    if (lastTaskId) {
      try {
        setPhase('刷新结果…');
        renderFindings(await client.listFindings(lastTaskId));
        phase = '';
        updateStatusBar();
      } catch (e) {
        phase = '';
        updateStatusBar();
        vscode.window.showErrorMessage(`刷新失败：${(e as Error).message}`);
      }
      return;
    }
    // 本地无任务记录（如首次使用/更换工作区）：兜底绑定平台该项目最近完成的任务
    const projectId = await requireReady();
    if (!projectId) return;
    try {
      const tid = await latestCompletedTask(projectId);
      if (!tid) {
        vscode.window.showInformationMessage('平台该项目暂无已完成的扫描任务，请先执行 CodeAudit: 扫描工作区');
        return;
      }
      await bindTask(tid);
    } catch (e) {
      vscode.window.showErrorMessage(`获取历史任务失败：${(e as Error).message}`);
    }
  };

  // 命令「切换任务」：列出平台该项目的历史任务（时间倒序），绑定任意一个并拉取其结果
  const doSelectTask = async (): Promise<void> => {
    const projectId = await requireReady();
    if (!projectId) return;
    let tasks: TaskSummary[];
    try {
      tasks = await client.listTasks(projectId);
    } catch (e) {
      vscode.window.showErrorMessage(`获取任务列表失败：${(e as Error).message}`);
      return;
    }
    if (tasks.length === 0) {
      vscode.window.showInformationMessage('平台该项目暂无任务，请先执行 CodeAudit: 扫描工作区');
      return;
    }
    const picked = await vscode.window.showQuickPick(
      tasks.map((t) => ({
        label: `${taskStatusLabel(t.status)} · ${t.task_id.slice(0, 8)}…`,
        description: `${t.scan_mode.replace('SCAN_MODE_', '')}${t.created_at ? ` · ${t.created_at.slice(0, 16).replace('T', ' ')}` : ''}${t.task_id === lastTaskId ? ' · 当前' : ''}`,
        task: t,
      })),
      { placeHolder: '选择要绑定的工作区任务（将拉取其历史结果，不重新扫描）' },
    );
    if (!picked) return;
    await bindTask(picked.task.task_id);
  };

  // 窗口重载/重启后自动恢复：优先 workspaceState 持久化的任务；无记录则绑定平台该项目
  // 最近完成的任务——扫描结果在平台侧持久存储，拉取即可，绝不重扫。失败仅记日志不打扰。
  const restoreLastTask = async (): Promise<void> => {
    try {
      let tid: string | null = lastTaskId;
      if (!tid) {
        const projectId = cfg().get('projectId', '');
        if (!projectId || !client.isLoggedIn()) {
          log.info('跳过任务恢复：未登录或未绑定项目');
          return;
        }
        tid = await latestCompletedTask(projectId);
      }
      if (tid) {
        log.info(`自动恢复上次任务 ${tid.slice(0, 8)}…（从平台拉取历史结果，不重扫）`);
        await bindTask(tid, { silent: true });
      }
    } catch (e) {
      log.warn(`任务自动恢复失败：${(e as Error).message}`);
    }
  };

  const doOpenConsole = (): void => {
    // 控制台是独立 Web 前端（默认与网关同主机 :4173，cookie 登录）；serverUrl 是 API 网关，
    // 浏览器直开网关路径只会拿到 401 JSON（网关只认 Authorization 头，不认 query token）
    const explicit = cfg().get<string>('consoleUrl', '').trim();
    const base = (explicit || cfg().get('serverUrl', '').replace(/:\d+(?=\/|$)/, ':4173')).replace(/\/$/, '');
    const url = lastTaskId ? `${base}/tasks/${lastTaskId}` : base;
    void vscode.env.openExternal(vscode.Uri.parse(url));
  };

  // 暂停/恢复：平台原生 pause/resume（TASK_STATUS_PAUSED 非终态，watcher 持续跟踪）
  const doPauseScan = async (): Promise<void> => {
    if (!lastTaskId || !progress) {
      vscode.window.showWarningMessage('当前没有运行中的扫描任务');
      return;
    }
    try {
      await client.pauseTask(lastTaskId);
      log.info(`扫描已暂停：${lastTaskId}`);
      vscode.window.showInformationMessage(`扫描已暂停（任务 ${lastTaskId.slice(0, 8)}）——状态栏"恢复扫描"或视图菜单可继续`);
    } catch (e) {
      vscode.window.showErrorMessage(`暂停失败：${(e as Error).message}`);
    }
  };

  const doResumeScan = async (): Promise<void> => {
    if (!lastTaskId) {
      vscode.window.showWarningMessage('当前没有可恢复的任务');
      return;
    }
    try {
      await client.resumeTask(lastTaskId);
      log.info(`扫描已恢复：${lastTaskId}`);
      vscode.window.showInformationMessage(`扫描已恢复（任务 ${lastTaskId.slice(0, 8)}）——进度实时推送已继续`);
    } catch (e) {
      vscode.window.showErrorMessage(`恢复失败：${(e as Error).message}`);
    }
  };

  // quick fix：编辑器内灯泡 → AI 修复
  class CodeAuditCodeActionProvider implements vscode.CodeActionProvider {
    provideCodeActions(doc: vscode.TextDocument, range: vscode.Range): vscode.CodeAction[] {
      const diags = diagnostics.get(doc.uri) ?? [];
      const hit = diags.find((d) => d.range.intersection(range));
      if (!hit) return [];
      // 反向映射必须按行定位：同文件多条发现时按文件取首条会把补丁应用到错误目标
      const relPath = path.relative(vscode.workspace.workspaceFolders?.[0].uri.fsPath ?? '', doc.uri.fsPath).replace(/\\/g, '/');
      const f = pickFindingAtLine(currentFindings, relPath, hit.range.start.line);
      if (!f) return [];
      const action = new vscode.CodeAction('CodeAudit: AI 修复此漏洞', vscode.CodeActionKind.QuickFix);
      action.command = { command: 'codeaudit.fixFinding', title: 'AI 修复', arguments: [f] };
      action.diagnostics = [hit];
      // Problems 面板是原生入口：补一个到漏洞详情视图的路由（详情视图默认只在
      // 点击侧栏树条目时切换，从 Problems 进来的用户没有可发现的详情路径）
      const view = new vscode.CodeAction('CodeAudit: 查看漏洞详情', vscode.CodeActionKind.QuickFix);
      view.command = { command: 'codeaudit.openFinding', title: '查看漏洞详情', arguments: [f] };
      view.diagnostics = [hit];
      return [action, view];
    }
  }

  // codeaudit-fix 虚拟文档提供者：diff 右侧内容
  context.subscriptions.push(
    vscode.workspace.registerTextDocumentContentProvider('codeaudit-fix', {
      provideTextDocumentContent(uri: vscode.Uri): string {
        try {
          const payload = JSON.parse(Buffer.from(uri.query, 'base64').toString('utf8')) as { content: string };
          return payload.content;
        } catch {
          return '';
        }
      },
    }),
  );

  // serverUrl 热更：client.baseUrl 只在构造/登录时赋值，配置变更不监听会出现
  // "REST 打旧地址、WS URL 用新地址"的分裂
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('codeaudit.serverUrl')) {
        client.baseUrl = cfg().get('serverUrl', client.baseUrl).replace(/\/+$/, '');
        log.info(`serverUrl 已热更：${client.baseUrl}`);
      }
    }),
  );

  // vscode://codeaudit.codeaudit-vscode/<command> 深度链接（scan/selectTask），无键鼠环境/外部工具可触发
  context.subscriptions.push(
    vscode.window.registerUriHandler({
      handleUri(uri: vscode.Uri): void {
        const action = (uri.path || uri.query).replace(/^\//, '').split(/[/?]/)[0] ?? '';
        log.info(`URI 触发: ${uri.toString()} -> ${action}`);
        if (action === 'scan') {
          void vscode.commands.executeCommand('codeaudit.scanWorkspace');
        } else if (action === 'selectTask') {
          void vscode.commands.executeCommand('codeaudit.selectTask');
        } else {
          log.warn(`未知的 URI 动作: ${action}`);
        }
      },
    }),
  );

  context.subscriptions.push(
    diagnostics,
    { dispose: () => tree._onDidChange.dispose() },
    { dispose: () => progressTree._onDidChange.dispose() },
    status,
    scanStatus,
    log,
    vscode.window.registerTreeDataProvider('codeaudit.findings', tree),
    vscode.window.registerTreeDataProvider('codeaudit.progress', progressTree),
    vscode.window.registerWebviewViewProvider('codeaudit.aiContext', aiView),
    vscode.window.registerWebviewViewProvider('codeaudit.findingDetail', detailView),
    vscode.languages.registerCodeActionsProvider({ scheme: 'file' }, new CodeAuditCodeActionProvider()),
    vscode.commands.registerCommand('codeaudit.login', doLogin),
    vscode.commands.registerCommand('codeaudit.logout', () => {
      client.logout().catch(() => undefined);
      setCtx('codeaudit.loggedIn', false);
      updateStatusBar();
      vscode.window.showInformationMessage('已退出 CodeAudit 登录');
    }),
    vscode.commands.registerCommand('codeaudit.selectProject', () => void doSelectProject()),
    vscode.commands.registerCommand('codeaudit.scanWorkspace', () => void doScan()),
    vscode.commands.registerCommand('codeaudit.refreshFindings', () => void doRefresh()),
    vscode.commands.registerCommand('codeaudit.selectTask', () => void doSelectTask()),
    vscode.commands.registerCommand('codeaudit.openFinding', (f?: unknown) => void doOpenFinding(asFinding(f))),
    vscode.commands.registerCommand('codeaudit.fixFinding', (f?: unknown) => void doFixFinding(asFinding(f))),
    vscode.commands.registerCommand('codeaudit.applyLowRiskFixes', () => void doApplyLowRiskFixes()),
    vscode.commands.registerCommand('codeaudit.rollbackFixes', () => void doRollback()),
    vscode.commands.registerCommand('codeaudit.rollbackFix', (arg?: unknown) => void doRollbackFix(arg)),
    vscode.commands.registerCommand('codeaudit.openConsole', doOpenConsole),
    vscode.commands.registerCommand('codeaudit.showAiContext', () => showAiContext()),
    vscode.commands.registerCommand('codeaudit.cancelScan', () => void doCancelScan()),
    vscode.commands.registerCommand('codeaudit.runningMenu', () => void doRunningMenu()),
    vscode.commands.registerCommand('codeaudit.pauseScan', () => void doPauseScan()),
    vscode.commands.registerCommand('codeaudit.resumeScan', () => void doResumeScan()),
    vscode.commands.registerCommand('codeaudit.clearFindings', () => doClearFindings()),
    vscode.commands.registerCommand('codeaudit.copyFindingId', (arg?: unknown) => {
      const f = asFinding(arg);
      if (!f) return;
      void vscode.env.clipboard.writeText(f.finding_id);
      vscode.window.setStatusBarMessage(`已复制 finding_id：${f.finding_id}`, 3000);
    }),
    vscode.commands.registerCommand('codeaudit.copyFilePath', (arg?: unknown) => {
      // view/item/context 传入 TreeNode（file 节点）或 finding；两者都取到工作区相对路径
      const p =
        arg && typeof arg === 'object' && 'path' in arg
          ? String((arg as { path: string }).path)
          : asFinding(arg)?.location?.file_path;
      if (!p) return;
      void vscode.env.clipboard.writeText(p);
      vscode.window.setStatusBarMessage(`已复制路径：${p}`, 3000);
    }),
  );

  // 欢迎页/菜单的初始 context keys（projectId 为工作区配置，同步可读）
  setCtx('codeaudit.boundProject', !!cfg().get('projectId', ''));
  updateStatusBar();
}

export function deactivate(): void {
  /* watcher 由订阅机制随扩展停用关闭 */
}
