// 纯逻辑：从 ai_fix_suggestion 文本中提取 unified diff 并解析为文件编辑。
// 【兜底路径】diff_patch（apply_patch 语法，ADR-183）已全链落地，主路径为
// applyPatch.ts 的完整 PatchParser 移植；本文件的 unified diff 围栏提取仅服务
// diff_patch 被服务端校验拒绝置空、或 ADR-183 之前旧任务的 findings——
// 无围栏块则返回 null，调用方降级为"仅展示建议"（诚实降级，不伪造补丁）。
//
// 锚定与应用的判定逻辑与 Cline apply-patch 逐字对标
// （cline-main sdk/packages/core/src/extensions/tools/executors/apply-patch-parser.ts）：
//   1. findContext：从上一锚点顺序扫描取【首个命中】，四级容错 fuzz 计分
//      0=精确（canonicalize 后全等）→ 1=仅行尾空白(trimEnd) → 100=两端空白(trim) → 1000=相似度≥0.66。
//      不做唯一性/歧义判定（Cline 靠顺序游标天然消歧）；hunk 声明行号不参与锚定。
//   2. 顺序游标：每个 hunk 锚定后 cursor = index + oldLines.length，下一 hunk 从 cursor 续扫。
//   3. 应用为 applyChunks 同款【递增校验】：chunks 按锚点升序拼接，
//      currentIndex > chunk.start 视为重叠 → 整体拒绝（防御性，顺序锚定下正常不可达）。
//   4. 失败语义（computePatchChanges 同款）：任一 hunk 上下文未命中 → 收集 warning
//      （含最高相似度与 ≤200 字符上下文预览）→ 整个补丁拒绝，绝不部分应用、绝不静默错切。
//   5. fuzz > 0 时返回给调用方向用户透明标注。
// 差异（仅输入格式）：本文件消费 unified diff（```diff 围栏兜底路径）；
// apply_patch 语法的完整 PatchParser 移植见 applyPatch.ts（主路径，逐字对标 Cline）。
// 两个模块共享 canonicalize/相似度判定（Cline 同源），锚定语义完全一致。
export interface HunkEdit {
  oldStart: number; // 1-based，补丁声明的原文件起始行（仅元数据，不参与锚定——对标 Cline 不信任行号）
  oldLines: string[]; // 被替换的原行
  newLines: string[]; // 替换后的行
}

export interface FilePatch {
  oldPath: string;
  newPath: string;
  hunks: HunkEdit[];
}

export function extractDiffBlock(suggestion: string): string | null {
  if (!suggestion) return null;
  const m = suggestion.match(/```diff\r?\n([\s\S]*?)```/);
  return m ? m[1] : null;
}

export function parseUnifiedDiff(diff: string): FilePatch[] {
  const patches: FilePatch[] = [];
  let cur: FilePatch | null = null;
  let hunk: HunkEdit | null = null;
  // split 的尾部分裂产物（"a\n" → ['a','']）不是 diff 内容；若当作空上下文行会扩张
  // oldLines，把 hunk 之后本应保留的文件行一并替换掉（吞行）。其余位置的裸 '' 仍按
  // GNU diff 惯例视为省略了前导空格的空上下文行。
  const lines = diff.split(/\r?\n/);
  if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
  for (const raw of lines) {
    if (raw.startsWith('--- ')) {
      cur = { oldPath: normalizePath(raw.slice(4)), newPath: '', hunks: [] };
      patches.push(cur);
      hunk = null;
    } else if (raw.startsWith('+++ ')) {
      if (cur) cur.newPath = normalizePath(raw.slice(4));
    } else if (raw.startsWith('@@')) {
      const m = raw.match(/@@ -(\d+)(?:,(\d+))?/);
      if (cur && m) {
        hunk = { oldStart: Number(m[1]), oldLines: [], newLines: [] };
        cur.hunks.push(hunk);
      }
    } else if (raw.startsWith('\\')) {
      // "\ No newline at end of file" 标记：不参与内容比对
      continue;
    } else if (hunk && (raw.startsWith(' ') || raw === '')) {
      hunk.oldLines.push(raw.slice(1));
      hunk.newLines.push(raw.slice(1));
    } else if (hunk && raw.startsWith('-')) {
      hunk.oldLines.push(raw.slice(1));
    } else if (hunk && raw.startsWith('+')) {
      hunk.newLines.push(raw.slice(1));
    }
  }
  return patches.filter((p) => p.oldPath || p.newPath);
}

function normalizePath(p: string): string {
  return p.trim().replace(/^a\//, '').replace(/^b\//, '').replace(/\\/g, '/');
}

// —— Cline apply-patch canonicalize：NFC + 智能标点归一 + 转义还原 ——————————

const PUNCTUATION: Record<string, string> = {
  '\u2010': '-', '\u2011': '-', '\u2012': '-', '\u2013': '-', '\u2014': '-', '\u2212': '-',
  '\u201C': '"', '\u201D': '"', '\u201E': '"', '\u00AB': '"', '\u00BB': '"',
  '\u2018': "'", '\u2019': "'", '\u201B': "'",
  '\u00A0': ' ', '\u202F': ' ',
};

export function canonicalize(s: string): string {
  return s
    .normalize('NFC')
    .replace(/./gu, (ch) => PUNCTUATION[ch] ?? ch)
    // 模型产出的补丁常带反斜杠转义的引号/反引号（JSON 惯性），归一到裸字符再比对
    .replace(/\\`/g, '`')
    .replace(/\\'/g, "'")
    .replace(/\\"/g, '"');
}

// —— Cline 同款 Levenshtein 相似度 ————————————————————————————————————————————

const SIMILARITY_THRESHOLD = 0.66;

function levenshtein(a: string, b: string): number {
  const rows = b.length + 1;
  const cols = a.length + 1;
  const m = new Array<number>(rows * cols).fill(0);
  const at = (r: number, c: number): number => m[r * cols + c] ?? 0;
  for (let i = 0; i <= b.length; i++) m[i * cols] = i;
  for (let j = 0; j <= a.length; j++) m[j] = j;
  for (let i = 1; i <= b.length; i++) {
    for (let j = 1; j <= a.length; j++) {
      m[i * cols + j] = b[i - 1] === a[j - 1]
        ? at(i - 1, j - 1)
        : 1 + Math.min(at(i - 1, j - 1), at(i, j - 1), at(i - 1, j));
    }
  }
  return at(b.length, a.length);
}

function similarity(a: string, b: string): number {
  const longer = a.length >= b.length ? a : b;
  const shorter = a.length >= b.length ? b : a;
  if (longer.length === 0) return 1;
  return (longer.length - levenshtein(shorter, longer)) / longer.length;
}

// —— Cline findContext：顺序扫描取首个命中（判定逻辑逐字对标） ——————————————————

export type AnchorFuzz = 0 | 1 | 100 | 1000; // 精确 / trimEnd / trim / 相似度（Cline 分值）

export interface ContextHit {
  index: number; // 0-based 锚点
  fuzz: AnchorFuzz;
  similarity?: number; // 仅 fuzz=1000 时携带
}

export interface ContextMiss {
  index: -1;
  bestSimilarity: number;
}

/**
 * 在 lines 中从 start 起顺序查找 context 块，取首个命中。
 * 级联与 Cline findContext 完全一致：
 *   1) canonicalize 后整块全等（fuzz 0）
 *   2) 仅 trimEnd 后全等（fuzz 1）
 *   3) 两端 trim 后全等（fuzz 100）
 *   4) 相似度 ≥0.66 首个命中（fuzz 1000）
 * 不做唯一性/歧义判定；空 context（纯插入 hunk）锚定在 start。
 */
export function findContext(lines: string[], context: string[], start: number, insertHint = 0): ContextHit | ContextMiss {
  const len = context.length;
  // 空 context（纯插入 hunk）：锚定在游标与声明行号取大。
  // 与 Cline 的必要偏离：apply_patch 语法中插入位置由前置 @@ 上下文行锚定，
  // unified diff 无此语法，只有 @@ -N,0 的行号可用；两者游标语义一致。
  if (len === 0) return { index: Math.max(start, insertHint), fuzz: 0 };
  const lastStart = lines.length - len;

  const canonCtx = canonicalize(context.join('\n'));
  for (let i = start; i <= lastStart; i++) {
    if (canonicalize(lines.slice(i, i + len).join('\n')) === canonCtx) return { index: i, fuzz: 0 };
  }

  const ctxTrimEnd = canonicalize(context.map((l) => l.trimEnd()).join('\n'));
  for (let i = start; i <= lastStart; i++) {
    if (canonicalize(lines.slice(i, i + len).map((l) => l.trimEnd()).join('\n')) === ctxTrimEnd) return { index: i, fuzz: 1 };
  }

  const ctxTrim = canonicalize(context.map((l) => l.trim()).join('\n'));
  for (let i = start; i <= lastStart; i++) {
    if (canonicalize(lines.slice(i, i + len).map((l) => l.trim()).join('\n')) === ctxTrim) return { index: i, fuzz: 100 };
  }

  let best = 0;
  for (let i = start; i <= lastStart; i++) {
    const sim = similarity(canonicalize(lines.slice(i, i + len).join('\n')), canonCtx);
    if (sim >= SIMILARITY_THRESHOLD) return { index: i, fuzz: 1000, similarity: sim };
    if (sim > best) best = sim;
  }
  return { index: -1, bestSimilarity: best };
}

// —— 应用（computePatchChanges 整体拒绝 + applyChunks 递增校验，均对标 Cline）——

export interface HunkFailure {
  hunkIndex: number;
  oldStart: number; // 补丁声明行（仅展示用）
  reason: string;
  bestSimilarity: number;
  contextPreview: string; // ≤200 字符（Cline 模式）
}

export interface PatchResult {
  /** 应用后的全部行；null = 有 hunk 锚定失败，整个补丁被拒绝（文件不变） */
  lines: string[] | null;
  applied: { hunkIndex: number; start: number; fuzz: AnchorFuzz; similarity?: number }[];
  failures: HunkFailure[];
  /** 容错总分（对标 Cline fuzz factor），>0 时调用方应向用户透明标注 */
  fuzz: number;
}

function contextPreview(lines: string[]): string {
  const t = lines.join('\n');
  return t.length > 200 ? `${t.slice(0, 200)}...` : t;
}

export interface ResolvedChunk {
  start: number; // 0-based 锚点
  insLines: string[];
  delCount: number;
}

/**
 * 按 chunks 拼接新文件——递增校验对标 Cline applyChunks：
 * chunk 起点必须落在 [currentIndex, lines.length] 内，currentIndex > chunk.start 即重叠 → 报错。
 */
export function applyResolvedChunks(fileLines: string[], chunks: ResolvedChunk[], filePath = 'file'): { lines: string[] } | { error: string } {
  const result: string[] = [];
  let currentIndex = 0;
  for (const c of chunks) {
    if (c.start > fileLines.length) {
      return { error: `${filePath}: chunk 起点越界（start=${c.start} > 行数 ${fileLines.length}）` };
    }
    if (currentIndex > c.start) {
      return { error: `${filePath}: chunk 区间重叠（currentIndex=${currentIndex} > chunk.start=${c.start}）` };
    }
    result.push(...fileLines.slice(currentIndex, c.start), ...c.insLines);
    currentIndex = c.start + c.delCount;
  }
  result.push(...fileLines.slice(currentIndex));
  return { lines: result };
}

export function applyPatchToLines(fileLines: string[], patch: FilePatch): PatchResult {
  const applied: PatchResult['applied'] = [];
  const failures: HunkFailure[] = [];
  let fuzz = 0;
  let cursor = 0; // 顺序游标：每个 hunk 从上一锚点末尾续扫（Cline 同款，天然消歧）

  const anchorAt = (h: HunkEdit, i: number, start: number): ContextHit | ContextMiss =>
    findContext(fileLines, h.oldLines, start, h.oldStart);

  // 第一轮：顺序锚定（Cline 语义，重复块消歧靠游标天然推进）
  const pending: { hunkIndex: number }[] = [];
  for (let i = 0; i < patch.hunks.length; i++) {
    const h = patch.hunks[i];
    const m = anchorAt(h, i, cursor);
    if ('bestSimilarity' in m) {
      pending.push({ hunkIndex: i });
      continue;
    }
    applied.push({ hunkIndex: i, start: m.index, fuzz: m.fuzz, ...(m.similarity !== undefined ? { similarity: m.similarity } : {}) });
    fuzz += m.fuzz;
    cursor = m.index + h.oldLines.length;
  }

  // 第二轮：失败 hunk 从文件头回溯锚定——模型输出的 hunk 可能乱序（远端 hunk 写在
  // 前面），顺序游标已越过目标区域导致必然失配；回溯是最后手段，内容命中标准不变。
  for (const p of pending) {
    const h = patch.hunks[p.hunkIndex];
    const m = anchorAt(h, p.hunkIndex, 0);
    if ('bestSimilarity' in m) {
      failures.push({
        hunkIndex: p.hunkIndex,
        oldStart: h.oldStart,
        reason: `未在文件中找到 hunk 上下文（自第 ${cursor + 1} 行起扫描，最高相似度 ${m.bestSimilarity.toFixed(2)}）`,
        bestSimilarity: m.bestSimilarity,
        contextPreview: contextPreview(h.oldLines),
      });
      continue;
    }
    applied.push({ hunkIndex: p.hunkIndex, start: m.index, fuzz: m.fuzz, ...(m.similarity !== undefined ? { similarity: m.similarity } : {}) });
    fuzz += m.fuzz;
  }

  // 整体拒绝（对标 computePatchChanges：任一 warning 即 throw）
  if (failures.length > 0) return { lines: null, applied: [], failures, fuzz };

  // 乱序 hunk 锚定后按文件位置排序再拼接（递增校验仍在：真重叠照样整体拒绝）
  applied.sort((a, b) => a.start - b.start);
  const r = applyResolvedChunks(
    fileLines,
    applied.map((a) => ({ start: a.start, insLines: patch.hunks[a.hunkIndex].newLines, delCount: patch.hunks[a.hunkIndex].oldLines.length })),
    patch.oldPath,
  );
  if ('error' in r) {
    return {
      lines: null,
      applied: [],
      failures: [{ hunkIndex: applied[applied.length - 1]?.hunkIndex ?? 0, oldStart: patch.hunks[applied[applied.length - 1]?.hunkIndex ?? 0]?.oldStart ?? 0, reason: r.error, bestSimilarity: 0, contextPreview: '' }],
      fuzz,
    };
  }
  return { lines: r.lines, applied, failures: [], fuzz };
}

/** 把失败列表格式化为用户可读的多行文本（对标 Cline formatSkippedHunkFailure） */
export function formatPatchFailures(oldPath: string, failures: HunkFailure[]): string {
  const head = `补丁无法锚定到 ${oldPath} 当前内容（${failures.length} 个 hunk 失配），已整体拒绝应用：`;
  return [head, ...failures.map((f) => `  hunk #${f.hunkIndex + 1}（声明第 ${f.oldStart} 行起）：${f.reason}`)].join('\n');
}

// —— 内容对 → 补丁（修复登记的外科回滚数据源） ————————————————————————————
// 应用修复时从 oldContent/newContent 现算带上下文的 FilePatch 存进 FixRecord；
// 回滚时 invertFilePatch 交换 old/new 后走与正向应用完全相同的锚定/整体拒绝
// 语义（applyPatchToLines），只撤销该修复引入的变更——同文件上更晚的修复
// 内容原样保留，任意序回滚成立。上下文保足锚定唯一性，fuzz>1 即降级。

/** 行偏移：before 坐标 [start, start+delCount) 被替换，净差 delta 行 */
export interface LineShift {
  start: number; // 0-based，before 坐标
  delCount: number;
  delta: number; // ins - del
}

const DIFF_CONTEXT = 3; // hunk 上下文行数（git diff 惯例）
// LCS DP 面积上限：修复补丁的变更区在 prefix/suffix 剥离后都很小；超限说明
// 内容面目全非（或超大生成文件），外科补丁数据放弃生成（回滚走整文件覆盖降级）
const DIFF_MAX_AREA = 6_000_000;

/**
 * 对比修复前后内容，产出可逆的补丁数据（hunks 带 DIFF_CONTEXT 上下文）与
 * 行偏移表。无差异返回空 hunks；差异过大（DP 面积超限）返回 null——调用方
 * 不得存 patches，回滚时降级整文件覆盖。
 */
export function buildFilePatch(before: string, after: string): { hunks: HunkEdit[]; shifts: LineShift[] } | null {
  const b = before.split(/\r\n|\n/);
  const a = after.split(/\r\n|\n/);
  // 尾部 split 产物：内容以换行结尾时末尾多一个 ''，两侧一致则不参与差异
  if (b.length > 0 && b[b.length - 1] === '' && a.length > 0 && a[a.length - 1] === '') {
    b.pop();
    a.pop();
  }
  let pre = 0;
  while (pre < b.length && pre < a.length && b[pre] === a[pre]) pre++;
  let suf = 0;
  while (suf < b.length - pre && suf < a.length - pre && b[b.length - 1 - suf] === a[a.length - 1 - suf]) suf++;
  const rb = b.slice(pre, b.length - suf);
  const ra = a.slice(pre, a.length - suf);
  if (rb.length === 0 && ra.length === 0) return { hunks: [], shifts: [] };
  if (rb.length * ra.length > DIFF_MAX_AREA) return null;

  // LCS DP（rb/ra 剥离后很小；行内容来自同文件两次快照，重复行由上下文吸收）
  const rows = ra.length + 1;
  const cols = rb.length + 1;
  const dp = new Uint32Array(rows * cols);
  for (let i = ra.length - 1; i >= 0; i--) {
    for (let j = rb.length - 1; j >= 0; j--) {
      dp[i * cols + j] = rb[j] === ra[i] ? dp[(i + 1) * cols + j + 1] + 1 : Math.max(dp[(i + 1) * cols + j], dp[i * cols + j + 1]);
    }
  }

  // 回溯生成编辑块（before/after 绝对行坐标）：纯插入（delEnd==delStart）与
  // 纯删除（insEnd==insStart）同样成块；相邻编辑行聚进同一块
  type Block = { delStart: number; delEnd: number; insStart: number; insEnd: number };
  const blocks: Block[] = [];
  let cur: Block | null = null;
  const close = (): void => {
    if (cur) {
      blocks.push(cur);
      cur = null;
    }
  };
  let i = 0;
  let j = 0;
  while (i < ra.length && j < rb.length) {
    if (rb[j] === ra[i]) {
      close();
      i++;
      j++;
    } else if (dp[(i + 1) * cols + j] >= dp[i * cols + j + 1]) {
      cur = cur ?? { delStart: pre + j, delEnd: pre + j, insStart: pre + i, insEnd: pre + i };
      cur.insEnd = pre + i + 1;
      i++;
    } else {
      cur = cur ?? { delStart: pre + j, delEnd: pre + j, insStart: pre + i, insEnd: pre + i };
      cur.delEnd = pre + j + 1;
      j++;
    }
  }
  while (i < ra.length) {
    cur = cur ?? { delStart: pre + j, delEnd: pre + j, insStart: pre + i, insEnd: pre + i };
    cur.insEnd = pre + i + 1;
    i++;
  }
  while (j < rb.length) {
    cur = cur ?? { delStart: pre + j, delEnd: pre + j, insStart: pre + i, insEnd: pre + i };
    cur.delEnd = pre + j + 1;
    j++;
  }
  close();

  const hunks: HunkEdit[] = [];
  const shifts: LineShift[] = [];
  let idx = 0;
  while (idx < blocks.length) {
    // 上下文扩展 + 重叠归并
    let end = idx;
    let bLo = Math.max(0, blocks[idx].delStart - DIFF_CONTEXT);
    let bHi = Math.min(b.length, blocks[idx].delEnd + DIFF_CONTEXT);
    let aLo = Math.max(0, blocks[idx].insStart - DIFF_CONTEXT);
    let aHi = Math.min(a.length, blocks[idx].insEnd + DIFF_CONTEXT);
    while (end + 1 < blocks.length && blocks[end + 1].delStart - DIFF_CONTEXT <= bHi) {
      end++;
      bLo = Math.min(bLo, Math.max(0, blocks[end].delStart - DIFF_CONTEXT));
      bHi = Math.min(b.length, Math.max(bHi, blocks[end].delEnd + DIFF_CONTEXT));
      aLo = Math.min(aLo, Math.max(0, blocks[end].insStart - DIFF_CONTEXT));
      aHi = Math.min(a.length, Math.max(aHi, blocks[end].insEnd + DIFF_CONTEXT));
    }
    hunks.push({ oldStart: bLo + 1, oldLines: b.slice(bLo, bHi), newLines: a.slice(aLo, aHi) });
    for (let m = idx; m <= end; m++) {
      shifts.push({ start: blocks[m].delStart, delCount: blocks[m].delEnd - blocks[m].delStart, delta: blocks[m].insEnd - blocks[m].insStart - (blocks[m].delEnd - blocks[m].delStart) });
    }
    idx = end + 1;
  }
  return { hunks, shifts };
}

/** 逆补丁：交换每个 hunk 的 old/new（锚定侧变为修复后的内容，替换回修复前内容） */
export function invertFilePatch(p: FilePatch): FilePatch {
  return {
    oldPath: p.newPath || p.oldPath,
    newPath: p.oldPath,
    hunks: p.hunks.map((h) => ({ oldStart: h.oldStart, oldLines: h.newLines, newLines: h.oldLines })),
  };
}

/** 1-based 行号按偏移表迁移：位于替换区内的行贴到替换区新起点（该行内容已不存在，仅保底）。
 * 各块区间在 before 坐标下互不重叠——条件判断恒用原始行号，delta 逐块累计，
 * 不能用迁移后的坐标判后续块（坐标系已变）。 */
export function shiftLine(line: number, shifts: LineShift[]): number {
  const orig = line - 1; // 0-based 原始行号
  let shifted = orig;
  for (const s of shifts) {
    if (orig >= s.start + s.delCount) {
      shifted += s.delta;
    } else if (orig >= s.start) {
      return s.start + 1; // 1-based 替换区新起点
    }
  }
  return shifted + 1;
}
