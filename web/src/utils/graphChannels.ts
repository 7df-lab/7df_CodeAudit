// graphChannels — 图任务通道的纯函数层：
// 通道吸收（字节游标单调/保尾）、旧任务标记切段回落、G6 布局位置持久化合并。
// 全部纯函数（vitest 直测；G6/jsdom 不可测面由组件 mock + GUI 门禁补位）。
import type { AILogChannel } from '../api/types';

// Granularity — 到达粒度值域（b4 契约的前端单一事实源：'turn'|'token'）。
export type Granularity = 'turn' | 'token';

// ===== 通道吸收 =====

export interface ChannelState {
  text: string;
  cursor: number; // 服务端口径字节游标（单调）
  complete: boolean;
  total: number;
  /** b4：到达粒度（通道帧元数据归一化；undefined=未声明——回落 agent 判别） */
  granularity?: Granularity;
  /** 半字节残余（P-31 同款：engine readChannel 按 maxBytes 字节切界，多字节字符
   *  可跨通道块撕裂——stream 解码器按节点持久持有残余字节，下块续解） */
  pending?: Uint8Array;
}

export type ChannelMap = Record<string, ChannelState>;

// MAX_CHARS_PER_CHANNEL — 单通道保尾上限（对齐 B4-3 保尾纪律：客户端已渲染历史
// 可丢，游标不受影响）。总量上限由调用方（TaskDetailPage）按 1M 纪律约束。
const MAX_CHARS_PER_CHANNEL = 256_000;

// absorbChannels — 帧内 channels 增量并入状态表（游标单调防重放；base64→utf-8
// 解码与主面 aiText 同款保尾）。返回新表（引用变更驱动渲染）。
// normalizeGranularity — protojson enum 名 → 前端粒度值（b4）。未知/UNSPECIFIED/
// 缺失=undefined（诚实：前端回落 agent 判别过渡案——盘回落档与旧后端）。
export function normalizeGranularity(raw?: string): Granularity | undefined {
  if (raw === 'AI_LOG_GRANULARITY_TOKEN') return 'token';
  if (raw === 'AI_LOG_GRANULARITY_TURN') return 'turn';
  return undefined;
}

export function absorbChannels(prev: ChannelMap, chans: AILogChannel[] | undefined): ChannelMap {
  if (!chans || chans.length === 0) return prev;
  let next: ChannelMap | null = null; // 惰性拷贝：无实质变化返回原引用（不触发渲染）
  for (const c of chans) {
    const cur = Number(c.next_cursor ?? 0);
    const total = Number(c.total_bytes ?? 0);
    const st = (next ?? prev)[c.node_id] ?? { text: '', cursor: 0, complete: false, total: 0 };
    if (cur > st.cursor && c.chunk) {
      // P-31 同款流式解码：残余字节（半截多字节字符）跨块续解——engine readChannel
      // 按 maxBytes 字节切界，中文正文的 utf-8 多字节字符高概率被切在块边界。
      const bin = atob(c.chunk);
      let bytes = Uint8Array.from(bin, (ch) => ch.charCodeAt(0));
      // R35/P2-1（同 TaskDetailPage 主面）：快照在途时 WS 帧已推进游标的交叠块
      // 裁掉已交付前缀（node_cursor 同为请求时刻快照），防通道文本重复拼接。
      const start = cur - bytes.length;
      if (start >= 0 && start < st.cursor) bytes = bytes.subarray(st.cursor - start);
      const merged = st.pending && st.pending.length > 0
        ? new Uint8Array([...st.pending, ...bytes])
        : bytes;
      // 裁尾：从尾部回溯界定不完整多字节序列的首字节（ascii 0xxx/2B 0xc0 前缀/
      // 3B 0xe0/4B 0xf0），残余字节留 pending 下块续解。
      let keep = 0;
      for (let i = merged.length - 1; i >= 0 && i >= merged.length - 4; i--) {
        const b = merged[i];
        if ((b & 0xc0) === 0x80) { keep++; continue; } // 续字节 10xxxxxx
        const need = b >= 0xf0 ? 4 : b >= 0xe0 ? 3 : b >= 0xc0 ? 2 : 0;
        const have = merged.length - i;
        keep = need > 0 && have < need ? have : 0; // 完整序列必须归零（续字节累计不清零=裁掉合法尾字）
        break; // 首字节（或 ascii）即定界
      }
      const decoded = decodeBytes(merged.subarray(0, merged.length - keep));
      const pendingBytes = keep > 0 ? merged.slice(merged.length - keep) : undefined;
      next ??= { ...prev };
      next[c.node_id] = {
        text: (st.text + decoded).slice(-MAX_CHARS_PER_CHANNEL),
        cursor: cur,
        complete: c.complete || st.complete,
        total,
        granularity: normalizeGranularity(c.granularity) ?? st.granularity,
        pending: pendingBytes,
      };
    } else if (c.complete && !st.complete || (total !== st.total && total > 0)) {
      next ??= { ...prev };
      next[c.node_id] = { ...st, complete: c.complete || st.complete, total: total || st.total };
    }
  }
  return next ?? prev;
}

// utf8Decoder — 模块级单例（2026-09-22 等价简化批：非流式 decode 无状态，实例
// 复用与每次新建行为等价）。
const utf8Decoder = new TextDecoder('utf-8');

// decodeBytes — utf-8 字节 → 文本（非流式：调用方已裁尾残余字节——absorbChannels
// 的 pending 机制承接跨块多字节，逐段 decode 无 U+FFFD）。失败返回空（留痕纪律）。
function decodeBytes(bytes: Uint8Array): string {
  try {
    return utf8Decoder.decode(bytes);
  } catch {
    return '';
  }
}

// channelQuery — 已吸收通道 → 重连/轮询 node_cursor 查询参数值（"n001:123,n002:0"
// 形态；与网关 parseNodeCursors 对齐）。
export function channelQuery(map: ChannelMap): string {
  const ids = Object.keys(map).sort();
  if (ids.length === 0) return '';
  return ids.map((id) => `${id}:${map[id].cursor}`).join(',');
}

// ===== 旧任务回落：flat 流标记切段（无通道的图任务）=====

// segmentFlatByMarkers — 把任务级 flat AI 文本按节点标记切段（2026-09-22 实证双锚：
// 骨架行 "🤖 [子任务 g-<id>] 任务（N 字节）" 行首 + 正文 "# 图节点任务（<id> · <type>）"）。
// 段归属=最近一次标记；交错骨架生命周期行（启动/回合结束/空闲）按 g-id 识别，
// 不入正文、归入该节点的事件行前缀（诚实保留痕迹）；标记前的前导=任务级序言（''）。
// 已知边界（plan §6 审4）：无标记注入/收笔归开放段——通道纪元前的最佳努力回落。
export function segmentFlatByMarkers(flat: string): { preamble: string; segments: Record<string, string> } {
  const segments: Record<string, string> = {};
  const lines = flat.split('\n');
  let current = ''; // ''=序言
  const buckets: Record<string, string[]> = { '': [] as string[] };
  const push = (owner: string, line: string) => {
    (buckets[owner] ??= []).push(line);
  };
  for (const line of lines) {
    const skeleton = line.match(/^🤖 \[子任务 (g-[A-Za-z0-9_-]+)\] 任务（\d+ 字节）$/);
    if (skeleton) {
      current = skeleton[1].slice(2); // g-n001 → n001
      push(current, line);
      continue;
    }
    const marker = line.match(/^# 图节点任务（([A-Za-z0-9_-]+) · \w+）/);
    if (marker) {
      current = marker[1];
      // 骨架行已归段，正文标记行归段（避免重复则跳过：标记行通常紧跟骨架行）
      if (!/^🤖 /.test(line)) push(current, line);
      continue;
    }
    // 交错骨架生命周期行：归其 g-id 对应节点（不污染当前正文段）
    const lifecycle = line.match(/^🤖 \[子任务 (g-[A-Za-z0-9_-]+)\] (启动|回合结束|空闲|❌.*)$/);
    if (lifecycle) {
      push(lifecycle[1].slice(2), line);
      continue;
    }
    push(current, line);
  }
  for (const [owner, ls] of Object.entries(buckets)) {
    if (owner === '') continue;
    segments[owner] = ls.join('\n');
  }
  return { preamble: (buckets[''] ?? []).join('\n'), segments };
}

// ===== G6 布局位置持久化 =====

export type LayoutPositions = Record<string, { x: number; y: number }>;

// graphLayoutKey — 位置持久化 localStorage 键（按任务）。
export function graphLayoutKey(taskId: string): string {
  return `codeaudit.task_graph_layout_${taskId}`;
}

// 计划批次归属（与旧 TaskGraphCard groupBatches 同源语义——批次带内分区排序 Q13A）：
// 节点按 declared_seq 归入"seq 小于声明的最后一次触发"批次；默认→批次 0（种子带）。
export function batchIndexOf(
  declaredSeq: number | null | undefined,
  triggers: { seq: number }[],
): number {
  let idx = 0;
  const sorted = [...triggers].sort((a, b) => a.seq - b.seq);
  sorted.forEach((tr, i) => {
    if (declaredSeq != null && declaredSeq > tr.seq) idx = i + 1;
  });
  return idx;
}

// computeLRLayout — LR 分层布局（自研纯函数：奥卡姆——层=最长依赖路径，层内纵排；
// 确定性、可单测、位置可持久化合并；G6 只做渲染与交互，不跑布局引擎）。
// 环防御：访问中节点按已在层计数截断（图声明序=seq 因果序，环是异常输入）。
export function computeLRLayout(
  nodes: { id: string; from: string[] }[],
  saved: LayoutPositions | null,
): LayoutPositions {
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const layerCache = new Map<string, number>();
  const visiting = new Set<string>();
  const layerOf = (id: string): number => {
    if (layerCache.has(id)) return layerCache.get(id)!;
    if (visiting.has(id)) return 0; // 环截断
    visiting.add(id);
    const n = byId.get(id);
    let layer = 0;
    if (n) {
      for (const dep of n.from) {
        if (byId.has(dep)) layer = Math.max(layer, layerOf(dep) + 1);
      }
    }
    visiting.delete(id);
    layerCache.set(id, layer);
    return layer;
  };
  const layers = new Map<number, string[]>();
  for (const n of nodes) {
    const l = layerOf(n.id);
    (layers.get(l) ?? layers.set(l, []).get(l)!).push(n.id);
  }
  const positions: LayoutPositions = {};
  const X_STEP = 340;
  const Y_STEP = 130;
  for (const [layer, ids] of layers) {
    ids.forEach((id, i) => {
      if (saved && saved[id] !== undefined) {
        positions[id] = saved[id]; // 已手摆原样保留
      } else {
        positions[id] = { x: layer * X_STEP, y: i * Y_STEP };
      }
    });
  }
  return positions;
}
