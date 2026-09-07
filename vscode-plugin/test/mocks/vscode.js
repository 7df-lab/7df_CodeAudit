// VS Code 内存桩：extension.ts 胶水层行为测试的驱动器。
// run.js 把 'vscode' 模块解析重定向到本文件——mocha 进程内 extension.js require('vscode')
// 得到的就是这里导出的 API（smoke/e2e 脚本同样重定向，仅满足解析，不消费这些 API）。
//
// 设计要点：
//   - 状态集中在一个 state 对象，测试经 globalThis.__vsMock.reset(over) 重建；
//   - 工作区文件与 globalStorage 都是真实临时目录（extension 用真实 node fs 写盘），
//     openTextDocument/applyEdit/save 在内存文档层模拟，语义对齐 VS Code（applyEdit 只改
//     缓冲区，save() 才落盘）；
//   - 通知/输入框/QuickPick 走脚本队列；命令/视图/webview/URI/代码动作注册全部记录，
//     供测试直接驱动；
//   - 网络与 WebSocket 不在本桩内：测试自行替换 globalThis.fetch / globalThis.WebSocket。
'use strict';
const fs = require('fs');
const path = require('path');
const os = require('os');

// ─────────────────────────── 基础类型 ───────────────────────────

class EventEmitter {
  constructor() { this._listeners = new Set(); }
  get event() {
    return (cb) => { this._listeners.add(cb); return { dispose: () => this._listeners.delete(cb) }; };
  }
  fire(x) { for (const cb of [...this._listeners]) cb(x); }
  dispose() { this._listeners.clear(); }
}

class Uri {
  constructor(parts) {
    this.scheme = parts.scheme ?? 'file';
    this.authority = parts.authority ?? '';
    this.path = parts.path ?? '';
    this.query = parts.query ?? '';
    this.fragment = parts.fragment ?? '';
    this._fsPath = parts.fsPath;
  }
  get fsPath() { return this._fsPath ?? this.path; }
  with(chg) { return new Uri({ ...this, ...chg }); }
  toString() {
    return `${this.scheme}://${this.authority}${this.path}${this.query ? '?' + this.query : ''}${this.fragment ? '#' + this.fragment : ''}`;
  }
  static file(p) { return new Uri({ scheme: 'file', path: p, fsPath: p }); }
  static from(o) { return new Uri(o ?? {}); }
  static parse(str) {
    const m = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\/([^/?#]*)([^?#]*)(\?[^#]*)?(#.*)?$/.exec(String(str));
    if (!m) return new Uri({ scheme: 'untitled', path: String(str) });
    return new Uri({
      scheme: m[1],
      authority: m[2],
      path: m[3] || '/',
      query: (m[4] ?? '').slice(1),
      fragment: (m[5] ?? '').slice(1),
    });
  }
  static joinPath(base, ...parts) {
    let p = base.path.replace(/\/+$/, '');
    for (const part of parts) p += '/' + String(part).replace(/^\/+/, '');
    const segs = [];
    for (const s of p.split('/')) {
      if (s === '' || s === '.') continue;
      if (s === '..') { segs.pop(); continue; }
      segs.push(s);
    }
    return new Uri({ scheme: base.scheme, authority: base.authority, path: '/' + segs.join('/') });
  }
}

class Position {
  constructor(line, character) { this.line = line; this.character = character; }
}

class Range {
  constructor(a, b, c, d) {
    if (typeof a === 'number') {
      this.start = new Position(a, b);
      this.end = new Position(c, d);
    } else {
      this.start = a;
      this.end = b;
    }
  }
  get isEmpty() { return this.start.line === this.end.line && this.start.character === this.end.character; }
  intersection(other) {
    if (other.end.line < this.start.line || other.start.line > this.end.line) return undefined;
    return other;
  }
  with(start, end) { return new Range(start ?? this.start, end ?? this.end); }
}

class Diagnostic {
  constructor(range, message, severity) { this.range = range; this.message = message; this.severity = severity; }
}

class MarkdownString { constructor(value) { this.value = value; } }
class ThemeIcon { constructor(id) { this.id = id; } }
class TreeItem {
  constructor(label, collapsibleState) { this.label = label; this.collapsibleState = collapsibleState; }
}
class CodeAction { constructor(title, kind) { this.title = title; this.kind = kind; } }

class WorkspaceEdit {
  constructor() { this._ops = []; }
  replace(uri, range, text) { this._ops.push({ op: 'replace', uri, range, text }); }
  insert() { /* 未消费 */ }
  delete() { /* 未消费 */ }
  deleteFile(uri, opts) { this._ops.push({ op: 'deleteFile', uri, opts }); }
  createFile() { /* 未消费 */ }
  get size() { return this._ops.length; }
  entries() { return this._ops; }
}

class DiagnosticCollection {
  constructor(name) { this.name = name; this._map = new Map(); }
  set(uri, diags) { if (diags.length === 0) { this._map.delete(uri.toString()); return; } this._map.set(uri.toString(), diags); }
  get(uri) { return this._map.get(uri.toString()); }
  has(uri) { return this._map.has(uri.toString()); }
  delete(uri) { this._map.delete(uri.toString()); }
  clear() { this._map.clear(); }
  get all() { return [...this._map.entries()]; }
  forEach(cb) { this._map.forEach(cb); }
  dispose() { this._map.clear(); }
}

// ─────────────────────────── 状态 ───────────────────────────

const DEFAULT_CONFIG = {
  serverUrl: 'http://localhost:8080',
  consoleUrl: '',
  projectId: '',
  scanMode: 'SCAN_MODE_PARALLEL',
  sastTools: [],
  excludeGlobs: ['**/node_modules/**', '**/.git/**', '**/dist/**', '**/build/**', '**/.venv/**', '**/vendor/**', '**/.vscode/**'],
  minPackFiles: 10,
  autoOpenAiContext: true,
};

let state = null;

function reset(over = {}) {
  state = {
    root: over.root ?? null,
    globalStorage: over.globalStorage ?? os.tmpdir(),
    config: { ...DEFAULT_CONFIG, ...(over.config ?? {}) },
    secrets: new Map(Object.entries(over.secrets ?? {})),
    workspaceState: new Map(Object.entries(over.workspaceState ?? {})),
    inputQueue: [...(over.inputs ?? [])],
    pickQueue: [...(over.picks ?? [])],
    buttonQueue: [...(over.buttons ?? [])],
    quickPickCalls: [],
    messages: [],
    commands: new Map(),
    executed: [],
    contexts: {},
    treeProviders: {},
    webviewProviders: {},
    contentProviders: {},
    configListeners: [],
    uriHandler: null,
    codeActionProviders: [],
    openDocs: new Map(),
    applyEditOk: over.applyEditOk ?? true,
    saveFailOnce: false,
    diagnosticsCollections: [],
    statusBars: [],
    output: [],
    clipboard: [],
    openedExternal: [],
    shownDocs: [],
    statusMsgs: [],
  };
  return state;
}
reset();

// ─────────────────────────── 文档 ───────────────────────────

class TextDocument {
  constructor(uri, content) {
    this.uri = uri;
    this._content = content;
    this.eol = content.includes('\r\n') ? 2 : 1; // EndOfLine.CRLF : LF
  }
  getText() { return this._content; }
  positionAt(offset) {
    const before = this._content.slice(0, Math.max(0, Math.min(offset, this._content.length)));
    const line = (before.match(/\n/g) ?? []).length;
    const character = offset - (before.lastIndexOf('\n') + 1);
    return new Position(line, character);
  }
  offsetAt(pos) {
    const lines = this._content.split('\n');
    let off = 0;
    for (let i = 0; i < pos.line && i < lines.length; i++) off += lines[i].length + 1;
    return off + pos.character;
  }
  async save() {
    if (state.saveFailOnce) { state.saveFailOnce = false; return false; }
    fs.writeFileSync(this.uri.fsPath, this._content, 'utf-8');
    return true;
  }
}

// ─────────────────────────── workspace ───────────────────────────

function walkFiles(dir) {
  const out = [];
  if (!dir || !fs.existsSync(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walkFiles(p));
    else if (entry.isFile()) out.push(p);
  }
  return out;
}

const workspace = {
  get workspaceFolders() {
    return state.root ? [{ uri: Uri.file(state.root), name: path.basename(state.root), index: 0 }] : [];
  },
  get name() { return state.root ? path.basename(state.root) : ''; },
  getConfiguration() {
    return {
      get(key, def) { const v = state.config[key]; return v === undefined ? def : v; },
      update(key, value) {
        if (value === undefined) delete state.config[key];
        else state.config[key] = value;
        return Promise.resolve();
      },
      has: (key) => key in state.config,
      inspect: () => undefined,
    };
  },
  async findFiles(_include, _exclude, max) {
    const all = walkFiles(state.root).map(Uri.file);
    return typeof max === 'number' ? all.slice(0, max) : all;
  },
  async openTextDocument(arg) {
    const uri = typeof arg === 'string' ? Uri.file(arg) : arg;
    const key = uri.toString();
    if (state.openDocs.has(key)) return state.openDocs.get(key);
    let content;
    if (uri.scheme === 'file') {
      try { content = fs.readFileSync(uri.fsPath, 'utf-8'); } catch { throw new Error(`无法打开文件: ${uri.fsPath}`); }
    } else if (uri.scheme === 'untitled') {
      content = '';
    } else {
      throw new Error(`openTextDocument 不支持 scheme: ${uri.scheme}`);
    }
    const doc = new TextDocument(uri, content);
    state.openDocs.set(key, doc);
    return doc;
  },
  async applyEdit(edit) {
    if (!state.applyEditOk) return false;
    for (const op of edit.entries()) {
      if (op.op === 'replace') {
        let doc = state.openDocs.get(op.uri.toString());
        if (!doc) doc = await workspace.openTextDocument(op.uri);
        const start = doc.offsetAt(op.range.start);
        const end = doc.offsetAt(op.range.end);
        doc._content = doc._content.slice(0, start) + op.text + doc._content.slice(end);
      } else if (op.op === 'deleteFile') {
        fs.rmSync(op.uri.fsPath, { force: !!(op.opts && op.opts.ignoreIfNotExists) });
        state.openDocs.delete(op.uri.toString());
      }
    }
    return true;
  },
  registerTextDocumentContentProvider(scheme, provider) {
    state.contentProviders[scheme] = provider;
    return { dispose() {} };
  },
  // serverUrl 配置热更监听（3603ccb）：测试可经 state.configListeners 手动触发
  onDidChangeConfiguration(cb) {
    state.configListeners.push(cb);
    return { dispose() {} };
  },
  fs: {
    // 回滚磁盘终验用（writeRestored 逐字节校验落盘结果）
    async readFile(uri) { return fs.readFileSync(uri.fsPath); },
    async writeFile(uri, content) { fs.writeFileSync(uri.fsPath, content); },
    async delete(uri) { fs.rmSync(uri.fsPath, { force: true }); },
  },
};

// ─────────────────────────── window ───────────────────────────

const window = {
  async showInputBox(_opts) { return state.inputQueue.length ? state.inputQueue.shift() : undefined; },
  async showQuickPick(items, opts) {
    state.quickPickCalls.push({ items, opts });
    return state.pickQueue.length ? state.pickQueue.shift() : undefined;
  },
  showInformationMessage(msg) { state.messages.push({ kind: 'info', msg: String(msg) }); return Promise.resolve(state.buttonQueue.shift()); },
  showWarningMessage(msg) { state.messages.push({ kind: 'warn', msg: String(msg) }); return Promise.resolve(state.buttonQueue.shift()); },
  showErrorMessage(msg) { state.messages.push({ kind: 'error', msg: String(msg) }); return Promise.resolve(state.buttonQueue.shift()); },
  createStatusBarItem(alignment, priority) {
    const item = {
      alignment, priority, text: '', tooltip: '', command: '',
      show() {
        const i = state.statusBars.indexOf(item);
        if (i >= 0) state.statusBars[i] = item; else state.statusBars.push(item);
      },
      hide() { const i = state.statusBars.indexOf(item); if (i >= 0) state.statusBars.splice(i, 1); },
      dispose() {},
    };
    return item;
  },
  createOutputChannel(name, _opts) {
    return {
      name,
      info: (m) => state.output.push(`[info] ${m}`),
      warn: (m) => state.output.push(`[warn] ${m}`),
      error: (m) => state.output.push(`[error] ${m}`),
      debug: (m) => state.output.push(`[debug] ${m}`),
      appendLine: (m) => state.output.push(String(m)),
      append: () => undefined,
      show: () => undefined,
      dispose: () => undefined,
    };
  },
  setStatusBarMessage(msg) { state.statusMsgs.push(String(msg)); return { dispose() {} }; },
  async showTextDocument(doc, opts) { state.shownDocs.push({ doc, opts }); return doc; },
  registerTreeDataProvider(view, provider) { state.treeProviders[view] = provider; return { dispose() {} }; },
  registerWebviewViewProvider(viewId, provider, _opts) { state.webviewProviders[viewId] = provider; return { dispose() {} }; },
  registerUriHandler(handler) { state.uriHandler = handler; return { dispose() {} }; },
};

// ─────────────────────────── 其余命名空间 ───────────────────────────

const commands = {
  registerCommand(id, cb) {
    if (state.commands.has(id)) throw new Error(`command '${id}' already registered`);
    state.commands.set(id, cb);
    return { dispose() {} };
  },
  async executeCommand(id, ...args) {
    state.executed.push({ id, args });
    if (id === 'setContext') { state.contexts[args[0]] = args[1]; return undefined; }
    const cb = state.commands.get(id);
    if (cb) return cb(...args);
    return undefined; // vscode.diff / *.focus 等宿主内建命令：仅记录
  },
};

const languages = {
  createDiagnosticCollection(name) {
    const c = new DiagnosticCollection(name);
    state.diagnosticsCollections.push(c);
    return c;
  },
  registerCodeActionsProvider(_selector, provider, _meta) {
    state.codeActionProviders.push(provider);
    return { dispose() {} };
  },
};

const env = {
  appName: 'vscode-mock',
  language: 'zh-cn',
  clipboard: {
    async writeText(t) { state.clipboard.push(t); },
    readText: async () => state.clipboard[state.clipboard.length - 1] ?? '',
  },
  async openExternal(uri) { state.openedExternal.push(uri); return true; },
};

module.exports = {
  Uri, Range, Position, Diagnostic, MarkdownString, ThemeIcon, TreeItem, CodeAction,
  WorkspaceEdit, DiagnosticCollection, EventEmitter,
  DiagnosticSeverity: { Error: 0, Warning: 1, Information: 2, Hint: 3 },
  StatusBarAlignment: { Left: 1, Right: 2 },
  TreeItemCollapsibleState: { None: 0, Collapsed: 1, Expanded: 2 },
  ConfigurationTarget: { Global: 1, Workspace: 2 },
  EndOfLine: { LF: 1, CRLF: 2 },
  CodeActionKind: { QuickFix: { value: 'quickfix' }, Empty: { value: '' } },
  ViewColumn: { Active: -1, Beside: -2 },
  window, workspace, commands, languages, env,
};

// ─────────────────────────── 测试驱动入口 ───────────────────────────

function createContext() {
  return {
    secrets: {
      async get(k) { return state.secrets.get(k); },
      async store(k, v) { state.secrets.set(k, v); },
      async delete(k) { state.secrets.delete(k); },
      keys: () => [...state.secrets.keys()],
    },
    workspaceState: {
      get(k, d) { return state.workspaceState.has(k) ? state.workspaceState.get(k) : d; },
      async update(k, v) {
        if (v === undefined) state.workspaceState.delete(k); else state.workspaceState.set(k, v);
      },
      keys: () => [...state.workspaceState.keys()],
    },
    globalStorageUri: Uri.file(state.globalStorage),
    storageUri: Uri.file(state.globalStorage),
    subscriptions: [],
  };
}

globalThis.__vsMock = {
  reset,
  state: () => state,
  createContext,
  // 两个微任务轮：覆盖 activate 内 boot().then(...) 这类异步链的就绪
  flush: () => new Promise((r) => setImmediate(() => setImmediate(r))),
};
