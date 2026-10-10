// graphChannels 纯函数回归：
// 通道吸收（游标单调/保尾/查询参数）、旧任务标记切段回落（双锚+交错骨架行）、
// 位置持久键、LR 分层（层序/保存位置保留/环防御）、批次归属。
import { describe, expect, it } from 'vitest';
import {
  absorbChannels, batchIndexOf, channelQuery, computeLRLayout, graphLayoutKey,
  normalizeGranularity, segmentFlatByMarkers,
} from '../utils/graphChannels';
import type { AILogChannel } from '../api/types';

const b64 = (s: string) => btoa(unescape(encodeURIComponent(s)));

describe('absorbChannels（批3 通道吸收）', () => {
  it('游标单调：帧增量并入，重放帧被忽略', () => {
    const f1: AILogChannel[] = [{ node_id: 'n001', chunk: b64('第一段'), next_cursor: 9, complete: false, total_bytes: 9 }];
    const m1 = absorbChannels({}, f1);
    expect(m1.n001.text).toBe('第一段');
    expect(m1.n001.cursor).toBe(9);
    // 重放（同游标）零变化；新段并入
    const m2 = absorbChannels(m1, f1);
    expect(m2).toBe(m1); // 引用不变（无变化不触发渲染）
    // R35/P2-1 修正：next_cursor 与线契约一致（=前游标+块字节长；原 15 为算术
    // 松弛值——start=6<9 会误触交叠裁剪砍掉 3 字节，恰证裁剪判据按契约工作）
    const m3 = absorbChannels(m2, [{ node_id: 'n001', chunk: b64('第二段'), next_cursor: 18, complete: true, total_bytes: 18 }]);
    expect(m3.n001.text).toBe('第一段第二段');
    expect(m3.n001.complete).toBe(true);
  });

  it('节点隔离 + complete/total 元数据帧（chunk 空）推进终态位', () => {
    const m = absorbChannels(absorbChannels({}, [
      { node_id: 'n001', chunk: b64('A'), next_cursor: 1, complete: false, total_bytes: 1 },
    ]), [
      { node_id: 'n001', chunk: '', next_cursor: 1, complete: true, total_bytes: 1 }, // 收束元数据帧
      { node_id: 'n002', chunk: '', next_cursor: 0, complete: false, total_bytes: 0 }, // 空通道不出场
    ]);
    expect(m.n001.complete).toBe(true);
    expect(m.n002).toBeUndefined();
  });

  it('channelQuery — 已吸收通道 → 重连查询参数（字母序 id:cursor 逗号串）', () => {
    expect(channelQuery({})).toBe('');
    const m = absorbChannels({}, [
      { node_id: 'n002', chunk: b64('x'), next_cursor: 1, complete: false, total_bytes: 1 },
      { node_id: 'n001', chunk: b64('y'), next_cursor: 2, complete: false, total_bytes: 2 },
    ]);
    expect(channelQuery(m)).toBe('n001:2,n002:1');
  });
});

describe('segmentFlatByMarkers（旧任务回落，plan §6 审4）', () => {
  it('双锚切段：骨架行与正文标记同效，段间内容归最近标记节点', () => {
    const flat = [
      '══ DSH 会话开始（bridge）══',
      '🤖 [子任务 g-n001] 任务（100 字节）',
      '# 图节点任务（n001 · executor）',
      'n001 的任务正文',
      '💭 [思考]',
      '🤖 [子任务 g-n002] 任务（80 字节）',
      '# 图节点任务（n002 · planner）',
      'n002 的任务正文',
    ].join('\n');
    const { preamble, segments } = segmentFlatByMarkers(flat);
    expect(preamble).toContain('══ DSH 会话开始');
    expect(segments.n001).toContain('n001 的任务正文');
    expect(segments.n001).toContain('# 图节点任务（n001');
    expect(segments.n002).toContain('n002 的任务正文');
    expect(segments.n001).not.toContain('n002');
  });

  it('交错骨架生命周期行归其 g-id 节点（不污染当前正文段）', () => {
    const flat = [
      '# 图节点任务（n003 · executor）',
      'n003 正文开头',
      '🤖 [子任务 g-n002] 回合结束',
      '🤖 [子任务 g-n002] 空闲',
      'n003 正文续',
    ].join('\n');
    const { segments } = segmentFlatByMarkers(flat);
    expect(segments.n003).toContain('n003 正文开头');
    expect(segments.n003).toContain('n003 正文续');
    expect(segments.n003).not.toContain('n002'); // 生命周期行已被抽走
    expect(segments.n002).toContain('回合结束');
  });

  it('无任何标记 → 全部为序言，segments 空', () => {
    const { preamble, segments } = segmentFlatByMarkers('普通任务日志\n无标记');
    expect(preamble).toContain('普通任务日志');
    expect(Object.keys(segments)).toHaveLength(0);
  });
});

describe('布局持久化与 LR 分层', () => {
  it('computeLRLayout — 层=最长依赖路径（LR）；保存位置原样保留', () => {
    const nodes = [
      { id: 'a', from: [] },
      { id: 'b', from: ['a'] },
      { id: 'c', from: ['a'] },
      { id: 'd', from: ['b', 'c'] }, // 层 2
    ];
    const pos = computeLRLayout(nodes, { d: { x: 999, y: 999 } });
    expect(pos.a.x).toBe(0);
    expect(pos.b.x).toBe(pos.c.x); // 同层
    expect(pos.b.x).toBeGreaterThan(pos.a.x);
    expect(pos.d).toEqual({ x: 999, y: 999 }); // 手摆不动
    // b/c 同层纵排分离
    expect(pos.b.y).not.toBe(pos.c.y);
  });

  it('computeLRLayout — 环防御（异常输入不挂死）', () => {
    const pos = computeLRLayout([
      { id: 'x', from: ['y'] },
      { id: 'y', from: ['x'] },
    ], null);
    expect(pos.x).toBeDefined();
    expect(pos.y).toBeDefined();
  });

  it('graphLayoutKey — 按任务持久键（沿 codeaudit.* 先例）', () => {
    expect(graphLayoutKey('t-1')).toBe('codeaudit.task_graph_layout_t-1');
  });
});

describe('batchIndexOf（批次归属，Q13A）', () => {
  it('declared_seq 归入"seq 小于声明的最后一次触发"批次；默认批次 0', () => {
    const triggers = [{ seq: 10 }, { seq: 30 }];
    expect(batchIndexOf(5, triggers)).toBe(0);
    expect(batchIndexOf(15, triggers)).toBe(1);
    expect(batchIndexOf(99, triggers)).toBe(2);
    expect(batchIndexOf(null, triggers)).toBe(0);
  });
});


// ===== P-31 复发锁：多字节字符跨通道块撕裂=====
// engine readChannel 按 maxBytes 字节切界——中文（3B utf-8）被切在块边界时，
// 旧整段 decode 产生 U+FFFD 并随 aiChannels 持久化；流式 pending 续解修复。
describe('absorbChannels 多字节跨块（P-31 复发锁）', () => {
  it('中文字符被字节切块 → pending 续解无 U+FFFD', () => {
    // "发现注入" 4 汉字 × 3B = 12B;切成 7+5（第 3 字中间断开）
    const full = new TextEncoder().encode('发现注入');
    const part1 = full.slice(0, 7);
    const part2 = full.slice(7);
    const toB64 = (u: Uint8Array) => btoa(String.fromCharCode(...u));
    const m1 = absorbChannels({}, [
      { node_id: 'n001', chunk: toB64(part1), next_cursor: part1.length, complete: false, total_bytes: full.length },
    ]);
    // 第一段:前两字完整,第三字半截(不渲染 U+FFFD)
    expect(m1.n001.text).toBe('发现');
    const m2 = absorbChannels(m1, [
      { node_id: 'n001', chunk: toB64(part2), next_cursor: full.length, complete: true, total_bytes: full.length },
    ]);
    expect(m2.n001.text).toBe('发现注入');
    expect(m2.n001.text).not.toContain('\uFFFD');
  });

  it('ascii 无残余;混合场景(中文+ascii 尾)不误裁', () => {
    const full = new TextEncoder().encode('注入abc');
    const m1 = absorbChannels({}, [
      { node_id: 'n001', chunk: b64(new TextDecoder().decode(full)), next_cursor: full.length, complete: true, total_bytes: full.length },
    ]);
    expect(m1.n001.text).toBe('注入abc'); // 整段一次到:无撕裂无丢字
  });
});

// ===== node_cursor 请求形状锚（审查硬违规#2:契约测试列补齐）=====
describe('channelQuery 请求形状锚（E-20/E-23 node_cursor 契约）', () => {
  it('与网关 parseNodeCursors 契约:可重复/逗号多组的值形态 id:cursor', () => {
    // 契约原文(web/docs/external-interfaces.md E-20):node_cursor=<id>:<int> 可重复/逗号多组
    // 本锚锁 channelQuery 输出必为 "id:cursor" 逗号串(字母序)——形状漂移当次红。
    const m = absorbChannels({}, [
      { node_id: 'n002', chunk: b64('b'), next_cursor: 7, complete: false, total_bytes: 7 },
      { node_id: 'n001', chunk: b64('a'), next_cursor: 3, complete: true, total_bytes: 3 },
      { node_id: 'n010', chunk: b64('c'), next_cursor: 5, complete: false, total_bytes: 5 },
    ]);
    expect(channelQuery(m)).toBe('n001:3,n002:7,n010:5');
  });
});

// ===== b4：通道帧 granularity 契约（platform-acp-bridge-v1 §4 审2/审6 跨仓欠账兑现）=====
describe('granularity 契约（b4：protojson enum 名归一+帧内提取）', () => {
  it('normalizeGranularity：enum 名归一，UNSPECIFIED/未知/缺失=undefined（回落过渡案）', () => {
    expect(normalizeGranularity('AI_LOG_GRANULARITY_TOKEN')).toBe('token');
    expect(normalizeGranularity('AI_LOG_GRANULARITY_TURN')).toBe('turn');
    expect(normalizeGranularity('AI_LOG_GRANULARITY_UNSPECIFIED')).toBeUndefined();
    expect(normalizeGranularity(undefined)).toBeUndefined();
    expect(normalizeGranularity('AI_LOG_GRANULARITY_BOGUS')).toBeUndefined();
  });
  it('absorbChannels 提取帧内粒度到通道态；后续未声明帧不回退已声明值', () => {
    const m1 = absorbChannels({}, [
      { node_id: 'n001', chunk: b64('a'), next_cursor: 1, complete: false, total_bytes: 1, granularity: 'AI_LOG_GRANULARITY_TURN' },
    ]);
    expect(m1.n001.granularity).toBe('turn');
    const m2 = absorbChannels(m1, [
      { node_id: 'n001', chunk: b64('b'), next_cursor: 2, complete: true, total_bytes: 2 }, // 未声明帧（盘回落等）
    ]);
    expect(m2.n001.granularity).toBe('turn'); // 已声明值不丢
  });
  it('契约未声明（旧后端）通道态粒度=undefined（不虚构）', () => {
    const m = absorbChannels({}, [
      { node_id: 'n001', chunk: b64('a'), next_cursor: 1, complete: true, total_bytes: 1 },
    ]);
    expect(m.n001.granularity).toBeUndefined();
  });
});

// R35/P2-1（增量吸收幂等）：REST 快照在途时 WS 帧已推进游标的交叠块——
// 块尾判定（cur > st.cursor）挡不住起点落后于本地游标的块，整块 append=
// 通道文本重复拼接。锁定：交叠前缀按字节裁剪，仅追加未交付尾部。
describe('absorbChannels 交叠块幂等（R35/P2-1）', () => {
  const b64 = (s: string) => btoa(s)
  it('快照块起点落后于本地游标：裁前缀只追加尾部', () => {
    // WS 已交付游标 12（"ABCDEFGHIJKL"）；快照按游标 5 请求带回 chunk="FGHIJ..."（起点5）
    let st = absorbChannels(
      { n1: { text: 'ABCDEFGHIJKL', cursor: 12, complete: false, total: 0 } },
      [{ node_id: 'n1', chunk: b64('FGHIJKLMNOPQRS'), next_cursor: 19, complete: false, total_bytes: 19 }],
    )
    // 交叠块 [5,19)：已交付 12 → 仅追加 "MNOPQRS"
    expect(st.n1.text).toBe('ABCDEFGHIJKLMNOPQRS')
    expect(st.n1.cursor).toBe(19)
    st = absorbChannels(st, [{ node_id: 'n1', chunk: b64('TUV'), next_cursor: 22, complete: false, total_bytes: 22 }])
    expect(st.n1.text).toBe('ABCDEFGHIJKLMNOPQRSTUV')
  })
  it('正常块（起点=本地游标）行为不变', () => {
    const st = absorbChannels(
      { n1: { text: 'AB', cursor: 2, complete: false, total: 0 } },
      [{ node_id: 'n1', chunk: b64('CD'), next_cursor: 4, complete: false, total_bytes: 4 }],
    )
    expect(st.n1.text).toBe('ABCD')
  })
})
