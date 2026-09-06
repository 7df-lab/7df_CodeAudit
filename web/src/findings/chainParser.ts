// AI 结论 Source→Sink 链路普适解析器（ADR-195）。
// 依据: 14号 §3.3 ④ 代码上下文复核——AI 结论（[DSH-sandbox] reasoning）以自由文本携带
// 跨文件链路（file:line 引用链），本模块把文本还原为结构化 hops 供点选定位；
// 不推测：role（源/汇）仅当片段含明确关键词才标注。
//
// 覆盖形态（按 gw-0fb985799a00ab4ba3995b98 实例归纳，普适于常见 LLM 输出习惯）：
//   1. path/File.ext:49          —— 带行号（含路径前缀或裸文件名）
//   2. path/File.ext:594-596     —— 行区间
//   3. 第 93-95 行 / 93-95 行     —— 中文行引用，挂接最近提及的文件
//   4. L49 / L49-L60              —— L 前缀行引用，挂接最近文件
//   5. lines 49-60 / line 49      —— 英文行引用，挂接最近文件
//   6. 标识符（153-167）           —— 全角/半角括号内纯数字区间（"publish（153-167）"类），
//                                      挂接最近文件；真实调用形如 filter(x) 含非数字不匹配

export interface ChainHop {
  path: string;            // 原文写法（可能是裸文件名或截断路径——服务端 source-file 端点回退解析）
  line?: number;
  endLine?: number;
  snippet: string;         // 引用所在原文片段（人工核对解析是否成立的第一手材料）
  role?: 'source' | 'sink'; // 仅关键词命中才标注，否则缺省
}

export interface ChainParseResult {
  hops: ChainHop[];        // 按原文出现顺序（去重）
  files: string[];         // 提及的全部文件（含无行号者），按首次出现顺序
}

// 文件 token：(目录/)?名字.扩展名——扩展名以字母开头（排除 IP/版本号 1.2.1、gateway.internal）
const FILE_REF = /((?:[\w.\-]+\/)*[\w.\-]+\.[A-Za-z][\w]{0,9})(?::(\d{1,5})(?:\s*[-–—~至]\s*(\d{1,5}))?)?/g;
// 行引用族（挂接最近文件）：
const LINE_CN = /第?\s*(\d{1,5})(?:\s*[-–—~至]\s*(\d{1,5}))?\s*行/g;                 // 第 93-95 行 / 93-95 行
const LINE_L = /(^|[^A-Za-z0-9])L(\d{1,5})(?:\s*[-–—~至]\s*L?(\d{1,5}))?(?![A-Za-z0-9])/g; // L49 / L49-L60
const LINE_EN = /(?:lines?|Lines?)\s+(\d{1,5})(?:\s*[-–—~至]\s*(\d{1,5}))?/g;       // lines 49-60
const LINE_PAREN = /[\w.\-）（\u4e00-\u9fff]+[（(]\s*(\d{1,5})(?:\s*[-–—~至]\s*(\d{1,5}))?\s*[）)]/g; // publish（153-167）

// 噪声过滤：散文缩写（"e.g." 等）形似文件名
const STOP_FILES = new Set(['e.g', 'i.e', 'etc', 'vs', 'approx', 'nov', 'dec']);
// 代码片段噪声（gw-0fb9857 实例归纳）：`Foo.Builder`（大写扩展=类引用）、
// `httpRouter.filter(`（紧跟左括号=方法调用形）不是文件路径
function isFileToken(tok: string, followedBy: string): boolean {
  if (STOP_FILES.has(tok.toLowerCase())) return false;
  if (followedBy === '(' || followedBy === '（') return false; // obj.method( 调用形
  const base = tok.split('/').pop() ?? tok;
  const dot = base.lastIndexOf('.');
  if (dot <= 0) return false; // 无扩展名或以 . 开头
  const ext = base.slice(dot + 1);
  if (!/^[a-z]+$/.test(ext)) return false; // 扩展名须全小写字母（.java/.py ✓，.Builder/.basicAuth ✗）
  if (dot < 2 && ext.length < 2) return false; // "a.b" 级噪声
  return true;
}

function snippetOf(text: string, start: number, end: number): string {
  const from = Math.max(0, start - 90);
  const to = Math.min(text.length, end + 90);
  return (from > 0 ? '…' : '') + text.slice(from, to).replace(/\s+/g, ' ').trim() + (to < text.length ? '…' : '');
}

const SOURCE_KW = /(?:source|来源|污点源|入口|用户输入|请求参数|输入点)/i;
const SINK_KW = /(?:sink|汇点|危险|暴露|端点|可达|执行点)/i;

interface Ev { offset: number; end: number; path?: string; line?: number; endLine?: number; refOnly: boolean }

export function parseChain(text?: string | null): ChainParseResult {
  if (!text) return { hops: [], files: [] };
  // 反引号代码段置空（等长占位保偏移）：`HttpBasicAuth.enable`/`httpRouter.filter(`/
  // `foo(123)` 类 属性/方法调用/伪调用 噪声主产地；真实 file:line 引用在本实例语料中
  // 均在代码段之外（含反引号内引用的极端形态属可接受损失——链路解析为最优努力口径）
  const masked = text.replace(/`[^`]*`/g, (s) => ' '.repeat(s.length));
  const events: Ev[] = [];

  // 文件 token（含可选 :line 后缀）——同时确立 currentFile 语境
  for (const m of masked.matchAll(FILE_REF)) {
    const path = m[1];
    const followedBy = masked.slice((m.index ?? 0) + m[0].length, (m.index ?? 0) + m[0].length + 1);
    if (!isFileToken(path, followedBy)) continue;
    events.push({ offset: m.index ?? 0, end: (m.index ?? 0) + m[0].length, path, line: m[2] ? Number(m[2]) : undefined, endLine: m[3] ? Number(m[3]) : undefined, refOnly: false });
  }
  // 行引用族——按最近文件挂接（refOnly，offset 排序后回填）。
  // lineGrp/endGrp 指定行号分组位置（LINE_L 前置边界组占 m[1]）
  const pushLine = (m: RegExpMatchArray, lineGrp: number, endGrp: number, grpOffset = 0) => {
    events.push({ offset: (m.index ?? 0) + grpOffset, end: (m.index ?? 0) + m[0].length, line: Number(m[lineGrp]), endLine: m[endGrp] ? Number(m[endGrp]) : undefined, refOnly: true });
  };
  for (const m of masked.matchAll(LINE_CN)) pushLine(m, 1, 2);
  for (const m of masked.matchAll(LINE_L)) pushLine(m, 2, 3, (m[1] ?? '').length);
  for (const m of masked.matchAll(LINE_EN)) pushLine(m, 1, 2);
  for (const m of masked.matchAll(LINE_PAREN)) pushLine(m, 1, 2);

  events.sort((a, b) => a.offset - b.offset);

  const hops: ChainHop[] = [];
  const files: string[] = [];
  const seen = new Set<string>();
  let currentFile: string | null = null;
  let segStart = 0;      // 当前文件段起点（该文件 token 出现处）——role 关键词归属上界
  let segFresh = true;   // 段内首跳：下界回溯到上一事件末（捕获"来源 App.py:10"式前导关键词）
  let prevEnd = 0;       // 上一事件（文件/行引用）结束位置
  for (let i = 0; i < events.length; i++) {
    const ev = events[i];
    if (!ev.refOnly) {
      currentFile = ev.path!;
      segStart = ev.offset;
      segFresh = true;
      if (!files.includes(ev.path!)) files.push(ev.path!);
      if (ev.line === undefined) { prevEnd = ev.end; continue; } // 无行号 → 进 files、不产 hop
    } else if (!currentFile) {
      continue; // 行引用先于任何文件提及 → 无挂接对象，丢弃
    }
    const line = ev.line!;
    if (!Number.isFinite(line) || line <= 0) { prevEnd = ev.end; continue; }
    const path = ev.refOnly ? currentFile : ev.path!;
    const key = `${path}|${line}|${ev.endLine ?? ''}`;
    if (!seen.has(key)) {
      seen.add(key);
      // role 归属：段内首跳取 [上一事件末→本引用末]（前导关键词），后续跳取
      // [段起点→本引用末]（同文件枚举共享语境）；引用之后的关键词归属下一语境；
      // sink 优先（更具体的危险词）
      const lower = segFresh ? prevEnd : segStart;
      const before = text.slice(lower, ev.end);
      const snippet = snippetOf(text, ev.offset, ev.end);
      hops.push({
        path, line, endLine: ev.endLine && ev.endLine > line ? ev.endLine : undefined,
        snippet,
        role: SINK_KW.test(before) ? 'sink' : SOURCE_KW.test(before) ? 'source' : undefined,
      });
    }
    segFresh = false;
    prevEnd = ev.end;
  }
  return { hops, files };
}

export function baseName(p: string): string {
  return p.split('/').pop() ?? p;
}
