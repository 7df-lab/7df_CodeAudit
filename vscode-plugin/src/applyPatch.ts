// 完整 PatchParser 移植——平台 diff_patch（apply_patch 语法，ADR-183）的消费入口。
// 源（逐字对标）：cline-main sdk/packages/core/src/extensions/tools/executors/
// apply-patch-parser.ts（PatchParser/peek/findContext）与 apply-patch.ts
// （normalizePatchInput/applyChunks/patchToChanges/computePatchChanges）。
// 与 Cline 的唯一结构性差异：文件内容由调用方以 Record<path, content> 注入
// （插件从编辑器缓冲区取当前真相），本模块零 fs 访问、完全可单测；
// 路径安全（工作区禁闭）由 extension 层负责——服务端 NormalizeDiffPatch 已挡一道。
//
// 语义要点（锚定判定与 b4aa6ab 的 diffParse 引擎同源，差异仅在输入格式）：
//   1. Update 段语法：@@ 后可带"定义行"（defStr）——先按 canonicalize 全等/trim 全等把游标
//      推进到该行之后（仅 trim 命中计 fuzz+1；平台 @@ 锚点行随真实文件行逐字重建，逐字
//      命中不计 fuzz——对齐 fixpatch.go 产出契约：文件未被改动时校验补丁实测 fuzz=0），
//      再锚定 hunk 上下文块。
//      上下文块构成：空格前缀=上下文行、-=删除行（并入待锚定块，必须与文件逐字对应）、
//      +=新增行（不参与锚定）；无 +/-/空格 前缀的行按上下文行容错（peek 补空格）。裸 @@ 行仅作分段。
//   2. 顺序游标 + first-hit 四级容错（0/1/100/1000）与 diffParse.findContext 一致；
//      *** End of File 追加 eof 语义：先尝试文件末尾锚定（自 lines.length - context.length
//      起扫），未命中再回退全文扫描，命中计 fuzz+10000（"应在末尾却不在末尾"仍可应用但需透明标注）。
//   3. 失败语义：任一 hunk 上下文未命中 → warning（路径/hunk 序号/最高相似度/≤200 字符预览）
//      → computePatchChanges 抛 DiffError，整个补丁拒绝，绝不部分应用、绝不静默错切。
//   4. 段级校验：Update 要求文件已存在、Add 要求不存在、同文件重复段拒绝；
//      Move to 语法完整移植（平台侧对含 Move to 的补丁在产出即拒绝，此处为防御性支持）。
//   5. 行尾：块运算在 LF 空间进行（模型只产 LF 补丁），输出按各文件自身 EOL 还原
//      （CRLF 文件不会被整体改写为 LF）；Add 新文件按平台 EOL（win32 为 CRLF）。
import * as os from 'os';
import { canonicalize } from './diffParse';

export const PATCH_MARKERS = {
  BEGIN: '*** Begin Patch',
  END: '*** End Patch',
  ADD: '*** Add File: ',
  UPDATE: '*** Update File: ',
  DELETE: '*** Delete File: ',
  MOVE: '*** Move to: ',
  SECTION: '@@',
  END_FILE: '*** End of File',
} as const;

export const BASH_WRAPPERS = ['%%bash', 'apply_patch', 'EOF', '```'] as const;

export enum PatchActionType {
  ADD = 'add',
  DELETE = 'delete',
  UPDATE = 'update',
}

export interface PatchChunk {
  origIndex: number; // 相对本次 Update 段上下文块起点的 0-based 偏移；锚定后 += 锚点
  delLines: string[];
  insLines: string[];
}

export interface PatchAction {
  type: PatchActionType;
  newFile?: string; // Add 段的全部内容
  chunks: PatchChunk[];
  movePath?: string;
}

export interface PatchWarning {
  path: string;
  chunkIndex?: number;
  message: string;
  context?: string;
}

export interface Patch {
  actions: Record<string, PatchAction>;
  warnings?: PatchWarning[];
}

export class DiffError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'DiffError';
  }
}

// —— PatchParser（对标 cline apply-patch-parser.ts PatchParser） ————————————

export class PatchParser {
  private patch: Patch = { actions: {}, warnings: [] };
  private index = 0;
  private fuzz = 0;
  private currentPath?: string;

  constructor(
    private readonly lines: string[],
    private readonly currentFiles: Record<string, string>,
  ) {}

  parse(): { patch: Patch; fuzz: number } {
    this.skipBeginSentinel();

    while (this.hasMoreLines() && !this.isEndMarker()) {
      this.parseNextAction();
    }

    if (this.patch.warnings?.length === 0) {
      delete this.patch.warnings;
    }

    return { patch: this.patch, fuzz: this.fuzz };
  }

  private addWarning(warning: PatchWarning): void {
    if (!this.patch.warnings) {
      this.patch.warnings = [];
    }
    this.patch.warnings.push(warning);
  }

  private skipBeginSentinel(): void {
    if (this.lines[this.index]?.startsWith(PATCH_MARKERS.BEGIN)) {
      this.index++;
    }
  }

  private hasMoreLines(): boolean {
    return this.index < this.lines.length;
  }

  private isEndMarker(): boolean {
    return this.lines[this.index]?.startsWith(PATCH_MARKERS.END) ?? false;
  }

  private parseNextAction(): void {
    const line = this.lines[this.index];
    if (line?.startsWith(PATCH_MARKERS.UPDATE)) {
      this.parseUpdate(line.substring(PATCH_MARKERS.UPDATE.length).trim());
      return;
    }
    if (line?.startsWith(PATCH_MARKERS.DELETE)) {
      this.parseDelete(line.substring(PATCH_MARKERS.DELETE.length).trim());
      return;
    }
    if (line?.startsWith(PATCH_MARKERS.ADD)) {
      this.parseAdd(line.substring(PATCH_MARKERS.ADD.length).trim());
      return;
    }
    throw new DiffError(`Unknown line while parsing: ${line}`);
  }

  private checkDuplicate(path: string, operation: string): void {
    if (path in this.patch.actions) {
      throw new DiffError(`Duplicate ${operation} for file: ${path}`);
    }
  }

  private parseUpdate(path: string): void {
    this.checkDuplicate(path, 'update');
    this.currentPath = path;

    this.index++;
    const movePath = this.lines[this.index]?.startsWith(PATCH_MARKERS.MOVE)
      ? (this.lines[this.index++] ?? '')
          .substring(PATCH_MARKERS.MOVE.length)
          .trim()
      : undefined;

    if (!(path in this.currentFiles)) {
      throw new DiffError(`Update File Error: Missing File: ${path}`);
    }

    const text = this.currentFiles[path] ?? '';
    const action = this.parseUpdateFile(text, path);
    action.movePath = movePath;
    this.patch.actions[path] = action;
    this.currentPath = undefined;
  }

  private parseUpdateFile(text: string, path: string): PatchAction {
    const action: PatchAction = { type: PatchActionType.UPDATE, chunks: [] };
    const fileLines = text.split('\n');
    let index = 0;

    const stopMarkers = [
      PATCH_MARKERS.END,
      PATCH_MARKERS.UPDATE,
      PATCH_MARKERS.DELETE,
      PATCH_MARKERS.ADD,
      PATCH_MARKERS.END_FILE,
    ];

    // 本轮必须至少消费一行补丁，否则下轮在原地无限循环（同步阻塞扩展宿主）：
    // 空行使 peek 原地 break、空上下文的 findContext 返回 start、endPatchIndex
    // 等于原位置——三者全不前进。正常迭代 peek 至少越过已读行，停在原地即语法坏，
    // 诚实拒绝（DiffError → 整个补丁拒绝）而非挂死。
    let lastPatchIndex = -1;
    while (
      !stopMarkers.some((marker) =>
        this.lines[this.index]?.startsWith(marker.trim()),
      )
    ) {
      if (this.index === lastPatchIndex) {
        throw new DiffError(
          `Invalid Line (parser made no progress):\n${this.lines[this.index] ?? ''}`,
        );
      }
      lastPatchIndex = this.index;
      const currentLine = this.lines[this.index];
      const defStr = currentLine?.startsWith('@@ ')
        ? currentLine.substring(3)
        : undefined;
      const sectionStr = currentLine === '@@' ? currentLine : undefined;
      let hadDefStr = false;

      if (defStr !== undefined || sectionStr !== undefined) {
        this.index++;
      } else if (index !== 0) {
        throw new DiffError(`Invalid Line:\n${this.lines[this.index]}`);
      }

      if (defStr?.trim()) {
        hadDefStr = true;
        // defStr 匹配三级（前两级与后级的顺序保证 Cline 行为不变）：
        //   1) 文件行全等 canonTrim —— Cline 原级（defStr 无缩进时即精确命中，fuzz+0）
        //   2) 文件行全等 canonExact —— 平台契约必要偏离：fixpatch.go 的 @@ 锚点行随真实
        //      文件行逐字重建（含缩进），缩进锚点逐字对应时保持 fuzz=0（Cline 原版此处
        //      trim 命中会计 fuzz+1；口径与依据见 ADR-183 补遗）
        //   3) trim 全等 —— Cline 原级（缩进漂移，fuzz+1）
        const canonTrim = canonicalize(defStr.trim());
        const canonExact = canonicalize(defStr);
        for (let i = index; i < fileLines.length; i++) {
          const fileLine = fileLines[i];
          if (
            fileLine &&
            (canonicalize(fileLine) === canonTrim ||
              canonicalize(fileLine) === canonExact ||
              canonicalize(fileLine.trim()) === canonTrim)
          ) {
            index = i + 1;
            if (
              canonicalize(fileLine.trim()) === canonTrim &&
              canonicalize(fileLine) !== canonTrim &&
              canonicalize(fileLine) !== canonExact
            ) {
              this.fuzz++;
            }
            break;
          }
        }
      }

      const [nextChunkContext, chunks, endPatchIndex, eof] = peek(
        this.lines,
        this.index,
      );
      const [newIndex, fuzz, similarity] = findContext(
        fileLines,
        nextChunkContext,
        index,
        eof,
      );

      // 平台 fixpatch.go 逐行顺序锚定层：AI 生成的"跳跃 hunk"（@@ 定义行/上下文与
      // delete 行之间隔有未列入补丁的代码）整段连续锚定必然失败或只能相似度命中
      // （相似度命中会错位——实测把 import 插进方法体）。fixpatch.go 的重建语义是
      // 逐行向下扫描首个逐字命中（允许间隙，平台校验文件未改动时 fuzz=0 即据此），
      // 此处对齐：oldLines 逐行独立锚定，全部逐字命中则按绝对行号重组 chunks。
      if (!eof && hadDefStr && (newIndex === -1 || fuzz >= 1000)) {
        let anchored = tryLineByLineAnchor(fileLines, nextChunkContext, chunks, index);
        if (!anchored && index > 0) {
          // 回溯：@@ 段乱序（远端段在前）时顺序游标已越过目标区域，逐行锚定
          // 从文件头重试（升序+重叠校验仍由调用方兜底）
          anchored = tryLineByLineAnchor(fileLines, nextChunkContext, chunks, 0);
        }
        if (anchored) {
          for (const chunk of anchored.chunks) action.chunks.push(chunk);
          index = anchored.nextIndex;
          this.index = endPatchIndex;
          continue;
        }
        // 跳跃 hunk 逐字锚定失败：拒绝该段（warning 汇总后整体拒绝），绝不相似度错位
        this.addWarning({
          path: this.currentPath || path,
          chunkIndex: action.chunks.length,
          message: `Jump hunk (@@ defStr) could not be anchored verbatim line-by-line (similarity: ${similarity.toFixed(2)}). Chunk skipped.`,
          context: nextChunkContext.join('\n').slice(0, 200),
        });
        this.index = endPatchIndex;
        continue;
      }

      if (newIndex === -1 && !eof) {
        // 回溯锚定：@@ 段在补丁中乱序（远端段写在前）时，顺序游标已越过目标区域，
        // 常规扫描必然失配——从文件头重扫一次（defStr 也一并回溯）。消歧优先级
        // 不变：顺序命中优先，回溯只是失败后的最后手段；成功后仍受 applyChunks
        // 升序+重叠校验兜底，重叠补丁照样整体拒绝。
        const rewindFrom = (() => {
          if (!defStr?.trim()) return 0;
          const canonTrimR = canonicalize(defStr.trim());
          const canonExactR = canonicalize(defStr);
          for (let i = 0; i < fileLines.length; i++) {
            const fileLine = fileLines[i];
            if (
              fileLine &&
              (canonicalize(fileLine) === canonTrimR ||
                canonicalize(fileLine) === canonExactR ||
                canonicalize(fileLine.trim()) === canonTrimR)
            ) {
              return i + 1;
            }
          }
          return 0;
        })();
        const [rewoundIndex, rewoundFuzz] = findContext(fileLines, nextChunkContext, rewindFrom, false);
        if (rewoundIndex !== -1) {
          this.fuzz += rewoundFuzz;
          for (const chunk of chunks) {
            chunk.origIndex += rewoundIndex;
            action.chunks.push(chunk);
          }
          index = rewoundIndex + nextChunkContext.length;
          this.index = endPatchIndex;
          continue;
        }
      }

      if (newIndex === -1) {
        const contextText = nextChunkContext.join('\n');
        this.addWarning({
          path: this.currentPath || path,
          chunkIndex: action.chunks.length,
          message: `Could not find matching context (similarity: ${similarity.toFixed(2)}). Chunk skipped.`,
          context:
            contextText.length > 200
              ? `${contextText.substring(0, 200)}...`
              : contextText,
        });
        this.index = endPatchIndex;
      } else {
        this.fuzz += fuzz;
        for (const chunk of chunks) {
          chunk.origIndex += newIndex;
          action.chunks.push(chunk);
        }
        index = newIndex + nextChunkContext.length;
        this.index = endPatchIndex;
      }
    }

    // 乱序 @@ 段（回溯锚定后）chunks 按文件位置升序——applyChunks 的递增校验
    // 只接受升序；真重叠段仍会被其 currentIndex>origIndex 校验整体拒绝
    action.chunks.sort((a, b) => a.origIndex - b.origIndex);
    return action;
  }

  private parseDelete(path: string): void {
    this.checkDuplicate(path, 'delete');
    if (!(path in this.currentFiles)) {
      throw new DiffError(`Delete File Error: Missing File: ${path}`);
    }
    this.patch.actions[path] = { type: PatchActionType.DELETE, chunks: [] };
    this.index++;
  }

  private parseAdd(path: string): void {
    this.checkDuplicate(path, 'add');
    if (path in this.currentFiles) {
      throw new DiffError(`Add File Error: File already exists: ${path}`);
    }

    this.index++;
    const lines: string[] = [];
    const stopMarkers = [
      PATCH_MARKERS.END,
      PATCH_MARKERS.UPDATE,
      PATCH_MARKERS.DELETE,
      PATCH_MARKERS.ADD,
    ];

    while (
      this.hasMoreLines() &&
      !stopMarkers.some((marker) =>
        this.lines[this.index]?.startsWith(marker.trim()),
      )
    ) {
      // Cline 同款越界防御（hasMoreLines 已保证不可达，保留以逐字对标）
      const line = this.lines[this.index++] as string | undefined;
      if (line === undefined) {
        break;
      }
      if (!line.startsWith('+')) {
        throw new DiffError(`Invalid Add File line (missing '+'): ${line}`);
      }
      lines.push(line.substring(1));
    }

    this.patch.actions[path] = {
      type: PatchActionType.ADD,
      newFile: lines.join('\n'),
      chunks: [],
    };
  }
}

// —— findContext（对标 cline apply-patch-parser.ts findContext，含 eof 语义）———

function calculateSimilarity(str1: string, str2: string): number {
  const longer = str1.length > str2.length ? str1 : str2;
  const shorter = str1.length > str2.length ? str2 : str1;
  if (longer.length === 0) {
    return 1;
  }
  const editDistance = levenshteinDistance(shorter, longer);
  return (longer.length - editDistance) / longer.length;
}

function levenshteinDistance(str1: string, str2: string): number {
  const rows = str2.length + 1;
  const cols = str1.length + 1;
  const matrix = new Array<number>(rows * cols).fill(0);
  const at = (r: number, c: number): number => matrix[r * cols + c] ?? 0;
  const set = (r: number, c: number, value: number): void => {
    matrix[r * cols + c] = value;
  };

  for (let i = 0; i <= str2.length; i++) set(i, 0, i);
  for (let j = 0; j <= str1.length; j++) set(0, j, j);

  for (let i = 1; i <= str2.length; i++) {
    for (let j = 1; j <= str1.length; j++) {
      if (str2[i - 1] === str1[j - 1]) {
        set(i, j, at(i - 1, j - 1));
      } else {
        set(i, j, 1 + Math.min(at(i - 1, j - 1), at(i, j - 1), at(i - 1, j)));
      }
    }
  }

  return at(str2.length, str1.length);
}

/**
 * 顺序扫描取首个命中，四级容错（0 精确 / 1 trimEnd / 100 trim / 1000 相似度≥0.66）。
 * eof=true（hunk 带 *** End of File）：先自 lines.length - context.length 起扫（末尾锚定），
 * 未命中回退自 start 全文扫描，命中 fuzz+10000。返回 [锚点|-1, fuzz, 最高相似度]。
 */
function findContext(
  lines: string[],
  context: string[],
  start: number,
  eof: boolean,
): [number, number, number] {
  if (context.length === 0) {
    return [start, 0, 1];
  }

  let bestSimilarity = 0;
  const findCore = (startIdx: number): [number, number, number] => {
    const canonicalContext = canonicalize(context.join('\n'));

    for (let i = startIdx; i < lines.length; i++) {
      const segment = canonicalize(
        lines.slice(i, i + context.length).join('\n'),
      );
      if (segment === canonicalContext) {
        return [i, 0, 1];
      }
      const similarity = calculateSimilarity(segment, canonicalContext);
      if (similarity > bestSimilarity) {
        bestSimilarity = similarity;
      }
    }

    for (let i = startIdx; i < lines.length; i++) {
      const segment = canonicalize(
        lines
          .slice(i, i + context.length)
          .map((line) => line.trimEnd())
          .join('\n'),
      );
      const canonicalTrimmed = canonicalize(
        context.map((line) => line.trimEnd()).join('\n'),
      );
      if (segment === canonicalTrimmed) {
        return [i, 1, 1];
      }
    }

    for (let i = startIdx; i < lines.length; i++) {
      const segment = canonicalize(
        lines
          .slice(i, i + context.length)
          .map((line) => line.trim())
          .join('\n'),
      );
      const canonicalTrimmed = canonicalize(
        context.map((line) => line.trim()).join('\n'),
      );
      if (segment === canonicalTrimmed) {
        return [i, 100, 1];
      }
    }

    const similarityThreshold = 0.66;
    for (let i = startIdx; i < lines.length; i++) {
      const segment = canonicalize(
        lines.slice(i, i + context.length).join('\n'),
      );
      const similarity = calculateSimilarity(segment, canonicalContext);
      if (similarity >= similarityThreshold) {
        return [i, 1000, similarity];
      }
      if (similarity > bestSimilarity) {
        bestSimilarity = similarity;
      }
    }

    return [-1, 0, bestSimilarity];
  };

  if (eof) {
    let [newIndex, fuzz, similarity] = findCore(lines.length - context.length);
    if (newIndex !== -1) {
      return [newIndex, fuzz, similarity];
    }
    [newIndex, fuzz, similarity] = findCore(start);
    return [newIndex, fuzz + 10000, similarity];
  }

  return findCore(start);
}

// —— peek（对标 cline apply-patch-parser.ts peek） ————————————————

type PeekResult = [string[], PatchChunk[], number, boolean];

/**
 * 从 patch 行 initialIndex 起读出一个 hunk 的形状（不含 @@ 行）：
 * 返回 [上下文块（上下文行+删除行，须与文件逐字对应）, 变更 chunks, 停止位置, 是否 End of File]。
 * 无前缀行按上下文行容错（补空格）；'***' 单独行终止；其余 '***' 前缀行视为语法错误。
 */
function peek(lines: string[], initialIndex: number): PeekResult {
  let index = initialIndex;
  const old: string[] = [];
  let delLines: string[] = [];
  let insLines: string[] = [];
  const chunks: PatchChunk[] = [];
  let mode: 'keep' | 'add' | 'delete' = 'keep';

  const stopMarkers = [
    '@@',
    PATCH_MARKERS.END,
    PATCH_MARKERS.UPDATE,
    PATCH_MARKERS.DELETE,
    PATCH_MARKERS.ADD,
    PATCH_MARKERS.END_FILE,
  ];

  while (index < lines.length) {
    const sourceLine = lines[index];
    if (
      !sourceLine ||
      stopMarkers.some((marker) => sourceLine.startsWith(marker.trim()))
    ) {
      break;
    }
    if (sourceLine === '***') {
      break;
    }
    if (sourceLine.startsWith('***')) {
      throw new DiffError(`Invalid line: ${sourceLine}`);
    }

    index++;
    const previousMode: 'keep' | 'add' | 'delete' = mode;
    let line = sourceLine;

    if (line[0] === '+') {
      mode = 'add';
    } else if (line[0] === '-') {
      mode = 'delete';
    } else if (line[0] === ' ') {
      mode = 'keep';
    } else {
      mode = 'keep';
      line = ` ${line}`;
    }

    line = line.slice(1);

    if (mode === 'keep' && previousMode !== mode) {
      if (insLines.length || delLines.length) {
        chunks.push({
          origIndex: old.length - delLines.length,
          delLines,
          insLines,
        });
      }
      delLines = [];
      insLines = [];
    }

    if (mode === 'delete') {
      delLines.push(line);
      old.push(line);
    } else if (mode === 'add') {
      insLines.push(line);
    } else {
      old.push(line);
    }
  }

  if (insLines.length || delLines.length) {
    chunks.push({
      origIndex: old.length - delLines.length,
      delLines,
      insLines,
    });
  }

  if (index < lines.length && lines[index] === PATCH_MARKERS.END_FILE) {
    index++;
    return [old, chunks, index, true];
  }

  return [old, chunks, index, false];
}

// —— 输入归一（对标 cline apply-patch.ts normalizePatchInput） ————————————

/**
 * 逐行顺序锚定（平台 fixpatch.go 重建语义）：
 * oldLines（context + delete 行，按补丁顺序）每行从游标起独立向下扫描首个
 * canonical 全等命中（允许行间间隙），全部命中后按文件绝对行号重组 chunks。
 * - 任一行未命中 → null（调用方走相似度降级/拒绝）；
 * - 同一 chunk 的 delete 行在文件中不连续 → null（间隙拆分会改变 insLines 语义，保守拒绝）；
 * - 全部逐字命中 → fuzz 不增（与平台"文件未改动时 fuzz=0"口径一致）。
 * 返回的 chunks.origIndex 为文件绝对行号，升序，可直接交 applyChunks。
 */
function tryLineByLineAnchor(
  fileLines: string[],
  oldLines: string[],
  chunks: PatchChunk[],
  start: number,
): { chunks: PatchChunk[]; nextIndex: number } | null {
  const fileIdx: number[] = [];
  let cursor = start;
  for (const line of oldLines) {
    const target = canonicalize(line);
    let found = -1;
    for (let i = cursor; i < fileLines.length; i++) {
      if (canonicalize(fileLines[i] ?? '') === target) {
        found = i;
        break;
      }
    }
    if (found === -1) return null;
    fileIdx.push(found);
    cursor = found + 1;
  }

  const out: PatchChunk[] = [];
  for (const chunk of chunks) {
    const s = chunk.origIndex; // del 段起点在 oldLines 中的下标
    if (chunk.delLines.length > 0) {
      const absStart = fileIdx[s];
      const absEnd = fileIdx[s + chunk.delLines.length - 1];
      if (absStart === undefined || absEnd === undefined) return null;
      if (absEnd - absStart + 1 !== chunk.delLines.length) return null; // del 行跨间隙
      out.push({ origIndex: absStart, delLines: chunk.delLines, insLines: chunk.insLines });
    } else {
      // 纯插入：插在锚定游标处（chunk.origIndex 此时指向 oldLines 中锚点位置或末尾）
      const anchor = s < fileIdx.length ? fileIdx[s] : cursor;
      out.push({ origIndex: anchor, delLines: [], insLines: chunk.insLines });
    }
  }
  // 升序校验（applyChunks 的递增契约）
  for (let i = 1; i < out.length; i++) {
    const prev = out[i - 1];
    const cur = out[i];
    if (cur.origIndex < (prev.origIndex ?? 0) + (prev?.delLines.length ?? 0)) return null;
  }
  // 文件游标推进到最后一行被锚定的 oldLine 之后（含上下文与删除行）——
  // 只推到末个 chunk 的 origIndex 会让同段后续 @@ 从已消费区域重新扫描
  const nextIndex = fileIdx.length > 0 ? fileIdx[fileIdx.length - 1] + 1 : start;
  return { chunks: out, nextIndex };
}

function splitPatchInputLines(input: string): string[] {
  return input.split('\n').map((line) => line.replace(/\r$/, ''));
}

function isWrapperLine(line: string): boolean {
  if (line.trim() === '') {
    return false;
  }
  return BASH_WRAPPERS.some((wrapper) => line.startsWith(wrapper));
}

function trimWrapperLines(lines: string[]): string[] {
  let start = 0;
  let end = lines.length;

  while (start < end && isWrapperLine(lines[start] ?? '')) {
    start++;
  }

  while (end > start && isWrapperLine(lines[end - 1] ?? '')) {
    end--;
  }

  return lines.slice(start, end);
}

function normalizePatchInput(input: string): { lines: string[] } {
  const rawLines = splitPatchInputLines(input);
  const beginIndex = rawLines.findIndex((line) =>
    line.startsWith(PATCH_MARKERS.BEGIN),
  );
  let endIndex = -1;
  for (let i = rawLines.length - 1; i >= 0; i--) {
    if (rawLines[i]?.startsWith(PATCH_MARKERS.END)) {
      endIndex = i;
      break;
    }
  }

  if (beginIndex !== -1 || endIndex !== -1) {
    if (beginIndex === -1 || endIndex === -1 || endIndex < beginIndex) {
      throw new DiffError(
        'Invalid patch text - incomplete sentinels. Try breaking it into smaller patches.',
      );
    }
    const lines = rawLines.slice(beginIndex, endIndex + 1);
    return { lines };
  }

  // 自由格式（无 sentinel）：剥掉遗留 shell 包装后补 Begin/End sentinel
  const stripped = trimWrapperLines(rawLines);
  while (stripped.length > 0 && stripped[0] === '') {
    stripped.shift();
  }
  while (stripped.length > 0 && stripped[stripped.length - 1] === '') {
    stripped.pop();
  }

  const lines = [PATCH_MARKERS.BEGIN, ...stripped, PATCH_MARKERS.END];
  return { lines };
}

function extractFilesForOperations(
  lines: readonly string[],
  markers: readonly string[],
): string[] {
  const files = new Set<string>();

  for (const line of lines) {
    for (const marker of markers) {
      if (line.startsWith(marker)) {
        files.add(line.substring(marker.length).trim());
        break;
      }
    }
  }

  return [...files];
}

/** 补丁中引用的"须已存在"文件（Update/Delete 段路径）——调用方据此预载当前内容 */
export function listUpdatedFiles(patchText: string): string[] {
  const { lines } = normalizePatchInput(patchText);
  return extractFilesForOperations(lines, [
    PATCH_MARKERS.UPDATE,
    PATCH_MARKERS.DELETE,
  ]);
}

// —— 应用（对标 cline apply-patch.ts computePatchChanges，内存版） ————————

export interface PatchFileChange {
  type: PatchActionType;
  oldContent?: string;
  newContent?: string;
  movePath?: string;
}

type LineEnding = '\r\n' | '\n';

// 任意位置残留 \r\n 即判 CRLF：把此前版本工具插入的 LF 行拉回文件自身 EOL
function detectLineEnding(content: string): LineEnding {
  return content.includes('\r\n') ? '\r\n' : '\n';
}

function normalizeLineEndings(text: string, eol: LineEnding): string {
  return text.split(/\r\n|\n/).join(eol);
}

// 新文件无既有 EOL 可保：win32 上取平台 EOL（除非内容自带 \r 已自选行尾）
function normalizeNewFileLineEndings(content: string): string {
  if (os.EOL === '\r\n' && !content.includes('\r')) {
    return content.replaceAll('\n', '\r\n');
  }
  return content;
}

function applyChunks(
  content: string,
  chunks: PatchChunk[],
  filePath: string,
): string {
  if (chunks.length === 0) {
    return content;
  }

  const lines = content.split('\n');
  const result: string[] = [];
  let currentIndex = 0;

  for (const chunk of chunks) {
    if (chunk.origIndex > lines.length) {
      throw new DiffError(
        `${filePath}: chunk.origIndex ${chunk.origIndex} > lines.length ${lines.length}`,
      );
    }
    if (currentIndex > chunk.origIndex) {
      throw new DiffError(
        `${filePath}: currentIndex ${currentIndex} > chunk.origIndex ${chunk.origIndex}`,
      );
    }
    result.push(...lines.slice(currentIndex, chunk.origIndex));
    result.push(...chunk.insLines);
    currentIndex = chunk.origIndex + chunk.delLines.length;
  }

  result.push(...lines.slice(currentIndex));
  return result.join('\n');
}

function patchToChanges(
  patch: Patch,
  loaded: { files: Record<string, string>; eols: Record<string, LineEnding> },
): Record<string, PatchFileChange> {
  const changes: Record<string, PatchFileChange> = {};
  const originalFiles = loaded.files;
  // 块运算在 LF 空间完成；输出按各文件自身 EOL 还原，CRLF 文件不被整体改写
  const withFileEol = (
    filePath: string,
    content: string | undefined,
  ): string | undefined =>
    content === undefined
      ? undefined
      : normalizeLineEndings(content, loaded.eols[filePath] ?? '\n');

  for (const [filePath, action] of Object.entries(patch.actions)) {
    switch (action.type) {
      case PatchActionType.DELETE:
        changes[filePath] = {
          type: PatchActionType.DELETE,
          oldContent: withFileEol(filePath, originalFiles[filePath]),
        };
        break;
      case PatchActionType.ADD:
        if (action.newFile === undefined) {
          throw new DiffError('ADD action without file content');
        }
        changes[filePath] = {
          type: PatchActionType.ADD,
          newContent: normalizeNewFileLineEndings(action.newFile),
        };
        break;
      case PatchActionType.UPDATE:
        changes[filePath] = {
          type: PatchActionType.UPDATE,
          oldContent: withFileEol(filePath, originalFiles[filePath]),
          newContent: withFileEol(
            filePath,
            applyChunks(originalFiles[filePath] ?? '', action.chunks, filePath),
          ),
          movePath: action.movePath,
        };
        break;
    }
  }

  return changes;
}

/** 把失配 warning 列表格式化为用户可读文本（对标 Cline formatSkippedHunkFailure） */
export function formatPatchWarnings(warnings: readonly PatchWarning[]): string {
  const lines = [
    `补丁无法锚定到当前文件内容（${warnings.length} 个 hunk 失配），已整体拒绝应用：`,
  ];
  for (const warning of warnings) {
    const hunkNumber =
      warning.chunkIndex === undefined
        ? 'unknown'
        : String(warning.chunkIndex + 1);
    lines.push(
      `${warning.path}: hunk ${hunkNumber}: 上下文未命中（相似度 ${
        /similarity: ([\d.]+)/.exec(warning.message)?.[1] ?? '?'
      }）`,
    );
    if (warning.context) {
      lines.push(`上下文预览:\n${warning.context}`);
    }
  }
  return lines.join('\n');
}

/**
 * 解析 apply_patch 文本并计算逐文件变更（不落盘）。currentFiles 由调用方注入
 * （Update/Delete 段路径必须存在，否则 DiffError）。任一 hunk 上下文未命中 →
 * DiffError(formatPatchWarnings)，整个补丁拒绝。
 */
export function computePatchChanges(
  patchText: string,
  currentFiles: Record<string, string>,
): { changes: Record<string, PatchFileChange>; fuzz: number } {
  const { lines } = normalizePatchInput(patchText);
  const files: Record<string, string> = {};
  const eols: Record<string, LineEnding> = {};
  for (const [p, content] of Object.entries(currentFiles)) {
    files[p] = content.replace(/\r\n/g, '\n');
    eols[p] = detectLineEnding(content);
  }

  const parser = new PatchParser(lines, files);
  const { patch, fuzz } = parser.parse();
  if (patch.warnings && patch.warnings.length > 0) {
    throw new DiffError(formatPatchWarnings(patch.warnings));
  }

  return { changes: patchToChanges(patch, { files, eols }), fuzz };
}
