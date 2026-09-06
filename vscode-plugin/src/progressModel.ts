// 任务进度模型（纯逻辑，可单测）：把网关 WS 帧 / 轮询聚合口（ADR-189 task/progress/
// logs/ai 四路同构 JSON）增量归并为单一 ProgressState，并构建进度树节点数据。
// extension 层只做 TreeItem/Webview 胶水，全部状态演进与视图数据在此。
//
// protojson 口径（网关 EmitUnpopulated + UseProtoNames）：
//   - int64 字段（next_cursor/total_bytes/ts_ms）序列化为十进制字符串——一律经
//     Number() 归一，两种形态都容忍；
//   - bytes 字段（ai.chunk）为 base64 串，解码为 utf-8 渲染文本追加；
//   - Timestamp 为 ISO 串（可能带 9 位小数秒），解析时截到毫秒。
//
// 增量语义：
//   - logs 按 log_id 去重追加（WS 重连不带游标时服务端从头重发，去重兜底）；
//   - ai 按字节游标连续性追加：next_cursor 严格大于已见游标且 chunk 与已见内容
//     前后衔接（chunkStart = next_cursor - chunk.length ≤ aiCursor）才追加；
//     从 0 重发（重连场景）则整体重置——文本始终对应 [0, aiCursor) 连续区间。
import type { TaskLogEntry, TaskSnapshot, TaskStage } from './types';

export interface ProgressState {
  taskId: string;
  status: string;
  percent: number; // progress.overall_percent（无 progress 帧时按阶段完成度估算）
  stages: TaskStage[]; // 展示口径：progress.stages 优先，回退 task.stages
  logs: TaskLogEntry[]; // 去重后的全量（截断见 LOG_CAP）
  lastLogId: string; // 增量游标（WS 重连续订 / 轮询参数）
  aiText: string; // 累计 AI 交互上下文渲染文本
  aiCursor: number; // 字节游标
  aiComplete: boolean;
  aiTotalBytes: number;
  wsLive: boolean;
  updatedAt: number; // 最后一次有效演进的时刻（ms）
  version: number; // 内容版本号——树/Webview 只在变化时重渲染
  uploadSizeChecked?: boolean; // 沙箱收包字节数已核对（一次性校验防空项目白审"0 发现"）
}

const LOG_CAP = 500; // 与服务端环形缓存同容量（ADR-167 logCapPerTask）
const AI_TEXT_CAP = 512 * 1024; // webview 渲染上限：超限保尾（进度视图场景只需尾部）

export const asNumber = (v: string | number | undefined | null): number => {
  const n = Number(v ?? 0);
  return Number.isFinite(n) ? n : 0;
};

/** protojson Timestamp（可能 9 位小数秒）→ epoch ms；非法/缺失返回 null */
export function parseTsMs(ts: string | null | undefined): number | null {
  if (!ts) return null;
  const ms = Date.parse(ts.length > 23 ? `${ts.slice(0, 23)}Z` : ts);
  return Number.isFinite(ms) ? ms : null;
}

export function createProgressState(taskId: string): ProgressState {
  return {
    taskId,
    status: '',
    percent: 0,
    stages: [],
    logs: [],
    lastLogId: '',
    aiText: '',
    aiCursor: 0,
    aiComplete: false,
    aiTotalBytes: 0,
    wsLive: false,
    updatedAt: 0,
    version: 0,
  };
}

/**
 * AI 正文字节游标追加（纯函数）。返回 null = 丢弃该帧（游标未推进）。
 * 规则：
 *   - next_cursor <= cursor：已见内容，丢弃；
 *   - chunk 与已见区间衔接（chunkStart ≤ cursor ≤ next_cursor）：追加 chunk 尾部
 *     （cursor 之后的部分）——覆盖"从 0 重发但 next_cursor 更大"的罕见重排；
 *   - chunkStart > cursor（跳段）或 cursor === 0：整体重置为 chunk（从 0 的完整重发）。
 */
export function appendAiChunk(
  text: string,
  cursor: number,
  chunkText: string,
  nextCursor: number,
): { text: string; cursor: number } | null {
  if (nextCursor <= cursor && cursor !== 0) return null;
  const chunkStart = nextCursor - Buffer.byteLength(chunkText, 'utf8');
  if (cursor === 0 || chunkStart > cursor) {
    return { text: chunkText, cursor: nextCursor };
  }
  // 衔接：只取 cursor 之后新增的字节（按 utf-8 边界切；服务端 chunk 为完整 utf-8 文本，
  // 跳过前缀字节后仍从字符边界起步）
  const skip = cursor - chunkStart;
  let off = 0;
  let rest = chunkText;
  while (off < skip) {
    // 找到覆盖 skip 偏移的字符边界（多字节字符不撕裂）
    const step = Buffer.byteLength(rest[0] ?? '', 'utf8') || 1;
    if (off + step > skip) break;
    off += step;
    rest = rest.slice(1);
  }
  return { text: text + rest, cursor: nextCursor };
}

/** 无 progress.overall_percent 的旧帧：按阶段完成度估算（完成+跳过占比，运行中算一半） */
export function estimatePercent(stages: TaskStage[]): number {
  if (stages.length === 0) return 0;
  let units = 0;
  for (const s of stages) {
    if (s.status === 'STAGE_STATUS_COMPLETED' || s.status === 'STAGE_STATUS_SKIPPED') units += 1;
    else if (s.status === 'STAGE_STATUS_RUNNING') units += 0.5;
  }
  return Math.min(100, Math.round((units / stages.length) * 100));
}

/**
 * 沙箱收包校验（防空包白审）：dsh-agent 沙箱日志"项目打包完成 …（N 字节）"是
 * 沙箱实际拿到的项目包大小。项目目录打 tar.gz 属重打包，字节数与上传 zip 不相等
 * 属正常，故按"近空包下限"判废而非相等比对——空目录 tar.gz 恒为 ~32B（实测签名），
 * 含真实文件的项目包 ≥ 数百字节（单个 tar 头即 512B + gzip 帧开销）。
 * 返回 null = 日志中尚无打包完成行；否则返回解析到的字节数与是否低于下限。
 */
export const EMPTY_PACK_FLOOR = 64; // 空包签名 32B 的两倍安全余量

export function sandboxPackCheck(
  logs: { message?: string }[],
  expectedUploadBytes: number,
): { received: number; tooSmall: boolean } | null {
  const hit = [...logs].reverse().find((l) => /打包完成/.test(l.message ?? ''));
  if (!hit) return null;
  const m = /（(\d+) 字节）/.exec(hit.message ?? '');
  const received = m ? Number(m[1]) : 0;
  const floor = Math.max(EMPTY_PACK_FLOOR, Math.floor(expectedUploadBytes / 100));
  return { received, tooSmall: received < floor };
}

/**
 * log_id 序比较（服务端为十进制序号串，跨位数时字典序失效——"9" > "10"）：
 * 双方均为纯数字时数值比较，否则回退字典序。
 */
export function logIdAfter(a: string, b: string): boolean {
  if (/^\d+$/.test(a) && /^\d+$/.test(b)) return Number(a) > Number(b);
  return a > b;
}

/**
 * 归并一帧（WS snapshot 帧或轮询聚合响应，形状同构）。就地更新 state 并返回它。
 * 内容有实际演进时 version++（调用方据此决定是否重渲染）。
 */
export function applyFrame(state: ProgressState, frame: TaskSnapshot): ProgressState {
  const before = `${state.status}|${state.percent}|${state.stages.map((s) => `${s.stage_id}:${s.status}`).join(',')}|${state.lastLogId}|${state.aiCursor}|${state.aiComplete}`;
  state.status = frame.task.status ?? state.status;
  state.stages = frame.progress?.stages && frame.progress.stages.length > 0
    ? frame.progress.stages
    : frame.task.stages ?? [];
  const raw = frame.progress?.overall_percent;
  state.percent = typeof raw === 'number' && raw > 0
    ? Math.min(100, Math.round(raw))
    : estimatePercent(state.stages);

  for (const entry of frame.logs?.logs ?? []) {
    if (!entry || !logIdAfter(entry.log_id, state.lastLogId)) continue; // 增量去重
    state.logs.push(entry);
    state.lastLogId = entry.log_id;
  }
  if (state.logs.length > LOG_CAP) state.logs = state.logs.slice(-LOG_CAP);

  const ai = frame.ai;
  if (ai && typeof ai.chunk === 'string' && ai.chunk.length > 0) {
    const next = asNumber(ai.next_cursor);
    const merged = appendAiChunk(state.aiText, state.aiCursor, Buffer.from(ai.chunk, 'base64').toString('utf8'), next);
    if (merged) {
      state.aiCursor = merged.cursor;
      state.aiText = merged.text.length > AI_TEXT_CAP ? merged.text.slice(-AI_TEXT_CAP) : merged.text;
    }
  }
  if (ai) {
    state.aiTotalBytes = asNumber(ai.total_bytes);
    if (ai.complete) state.aiComplete = true;
  }

  const after = `${state.status}|${state.percent}|${state.stages.map((s) => `${s.stage_id}:${s.status}`).join(',')}|${state.lastLogId}|${state.aiCursor}|${state.aiComplete}`;
  if (before !== after) {
    state.version++;
    state.updatedAt = Date.now();
  }
  return state;
}

// —— 进度树节点数据（extension 胶水层映射为 vscode.TreeItem） ————————————

export type ProgressNodeKind = 'task' | 'stage' | 'ai' | 'log';

export interface ProgressNode {
  kind: ProgressNodeKind;
  id: string;
  label: string;
  description?: string;
  icon: string; // codicon 名
  tooltip?: string;
  contextValue?: string;
  /** markdown 折叠无必要：扁平列表，无子节点 */
}

const STAGE_LABELS: Record<string, string> = {
  STAGE_TYPE_UNSPECIFIED: '未分类阶段',
  STAGE_TYPE_CODE_ANALYSIS: '代码分析',
  STAGE_TYPE_SAST_SCAN: 'SAST 扫描',
  STAGE_TYPE_AI_INFERENCE: 'AI 推理',
  STAGE_TYPE_RESULT_FUSION: '结果融合',
  STAGE_TYPE_REPORT_GENERATION: '报告生成',
  STAGE_TYPE_AI_REVIEW: 'AI 审核（旧模式D）',
};

const STAGE_ICONS: Record<string, string> = {
  STAGE_STATUS_PENDING: 'clock',
  STAGE_STATUS_RUNNING: 'sync',
  STAGE_STATUS_COMPLETED: 'check',
  STAGE_STATUS_FAILED: 'error',
  STAGE_STATUS_SKIPPED: 'skip-forward',
  STAGE_STATUS_UNSPECIFIED: 'circle',
};

const STATUS_LABELS: Record<string, string> = {
  TASK_STATUS_PENDING: '待启动',
  TASK_STATUS_RUNNING: '运行中',
  TASK_STATUS_PAUSED: '已暂停',
  TASK_STATUS_COMPLETED: '已完成',
  TASK_STATUS_CANCELLED: '已取消',
  TASK_STATUS_TIMEOUT: '已超时',
  TASK_STATUS_DEAD: '异常终止',
  TASK_STATUS_FAILED: '失败',
};

export const stageLabel = (type: string, stageId: string): string => STAGE_LABELS[type] ?? stageId;

export function taskStatusLabel(status: string): string {
  return STATUS_LABELS[status] ?? status;
}

export function fmtDuration(ms: number): string {
  if (ms < 0) return '—';
  const s = Math.round(ms / 100) / 10;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  const m = Math.floor(s / 60);
  const r = Math.round(s - m * 60);
  if (m < 60) return `${m}m${r > 0 ? ` ${r}s` : ''}`;
  return `${Math.floor(m / 60)}h${m % 60}m`;
}

export function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(2)} MB`;
}

/**
 * 构建进度树扁平节点列表：任务头（状态+百分比+连接方式）→ 各阶段（图标/时长/错误
 * tooltip）→ AI 交互上下文入口（字节量/流式状态，点击打开 webview）→ 失败摘要。
 */
export function buildProgressItems(state: ProgressState): ProgressNode[] {
  const nodes: ProgressNode[] = [];
  const live = state.wsLive ? 'WS 实时' : '轮询';
  nodes.push({
    kind: 'task',
    id: 'task',
    label: `任务 ${state.taskId.slice(0, 8)}`,
    description: `${taskStatusLabel(state.status)} · ${state.percent}% · ${live}`,
    icon: state.status === 'TASK_STATUS_COMPLETED' ? 'pass' : state.status === 'TASK_STATUS_PAUSED' ? 'debug-pause' : state.status === 'TASK_STATUS_RUNNING' ? 'shield' : 'terminal',
    tooltip: `任务 ${state.taskId}\n状态：${taskStatusLabel(state.status)}（${state.status}）\n总进度：${state.percent}%\n连接：${live}\n点击状态栏或"CodeAudit: 查看 AI 交互上下文"可看实时正文`,
    contextValue: 'task',
  });

  for (const s of state.stages) {
    const start = parseTsMs(s.started_at);
    const end = parseTsMs(s.completed_at) ?? (s.status === 'STAGE_STATUS_RUNNING' ? Date.now() : null);
    const dur = start !== null && end !== null ? fmtDuration(end - start) : undefined;
    const statusZh: Record<string, string> = {
      STAGE_STATUS_PENDING: '等待中',
      STAGE_STATUS_RUNNING: '进行中',
      STAGE_STATUS_COMPLETED: '完成',
      STAGE_STATUS_FAILED: '失败',
      STAGE_STATUS_SKIPPED: '跳过',
      STAGE_STATUS_UNSPECIFIED: '—',
    };
    nodes.push({
      kind: 'stage',
      id: s.stage_id,
      label: stageLabel(s.type, s.stage_id),
      description: [statusZh[s.status] ?? s.status, dur].filter(Boolean).join(' · '),
      icon: STAGE_ICONS[s.status] ?? 'circle',
      tooltip: [
        `阶段：${stageLabel(s.type, s.stage_id)}`,
        `stage_id：${s.stage_id}`,
        `状态：${statusZh[s.status] ?? s.status}`,
        dur ? `耗时：${dur}` : null,
        s.error_message ? `错误：${s.error_message}` : null,
      ].filter(Boolean).join('\n'),
      contextValue: s.status === 'STAGE_STATUS_FAILED' ? 'stageFailed' : 'stage',
    });
  }

  const hasAi = state.aiCursor > 0 || state.aiTotalBytes > 0 || state.aiComplete;
  nodes.push({
    kind: 'ai',
    id: 'ai',
    label: 'AI 交互上下文',
    description: !hasAi
      ? '暂无内容'
      : `${fmtBytes(Math.max(state.aiCursor, state.aiTotalBytes))} · ${
          state.aiComplete
            ? '已收束'
            : state.status === 'TASK_STATUS_PAUSED'
              ? '已暂停（平台排空推理缓冲后静止，恢复后继续）'
              : '流式接收中'
        }`,
    icon: 'robot',
    tooltip: hasAi
      ? `DSH 沙箱会话的实时渲染正文（累计 ${fmtBytes(state.aiTotalBytes)}）。\n点击打开实时视图。`
      : '该任务尚无 AI 交互内容（纯 SAST 任务或 AI 阶段未开始）。任务运行后此处可实时查看。',
    contextValue: 'ai',
  });

  if (state.status === 'TASK_STATUS_DEAD' || state.status === 'TASK_STATUS_TIMEOUT') {
    nodes.push({
      kind: 'log',
      id: 'failure',
      label: '任务异常终止，详见任务日志',
      description: state.logs.length > 0 ? `最近：${state.logs[state.logs.length - 1].message.slice(0, 60)}` : undefined,
      icon: 'warning',
      contextValue: 'failure',
    });
  }
  return nodes;
}
