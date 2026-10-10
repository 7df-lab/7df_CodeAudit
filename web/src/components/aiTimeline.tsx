// aiTimeline — AI 交互时间线的共享渲染层：
// 从 AIInteractionLogPanel 原地搬出（行为零变化——该组件既有测试为 golden 锁）；
// 任务级平铺面板/图节点弹层/分区汇总共用同一解析与条目渲染。
import { useState, type CSSProperties } from 'react';
import { Button } from 'antd';
import { MONO_FONT } from '../dict/tokens';
import { EVIDENCE } from '../theme/evidence';

export const FONT = 14;
export const TITLE_FONT = 18;
export const LINE_HEIGHT = '21px';

export type EntryKind = 'turn' | 'meta' | 'reasoning' | 'assistant' | 'task' | 'agent';

export interface TimelineEntry {
  kind: EntryKind;
  label: string;
  body: string;
}

export const KIND_STYLE: Record<EntryKind, { border: string; label: string; labelColor: string; bodyColor: string }> = {
  turn: { border: EVIDENCE.link, label: '回合', labelColor: EVIDENCE.link, bodyColor: EVIDENCE.text },
  meta: { border: EVIDENCE.border, label: '事件', labelColor: EVIDENCE.textMuted, bodyColor: EVIDENCE.textMuted },
  reasoning: { border: EVIDENCE.ai, label: '模型思考', labelColor: EVIDENCE.ai, bodyColor: EVIDENCE.textMuted },
  assistant: { border: EVIDENCE.ok, label: '模型回复', labelColor: EVIDENCE.ok, bodyColor: EVIDENCE.text },
  task: { border: EVIDENCE.system, label: '任务', labelColor: EVIDENCE.system, bodyColor: EVIDENCE.text },
  agent: { border: EVIDENCE.accent, label: '子任务', labelColor: EVIDENCE.accent, bodyColor: EVIDENCE.text },
};

export const WINDOW_STEP = 400; // 大日志渐进回看步长（ADR-181：提供继续显示方式，不只靠下载）

// 元信息行（══ 会话开始 / ── 第 N 轮 / ── 回合结束 / ■ 收束 / ⚠ 退出）→ meta 条目
export function metaLine(line: string): boolean {
  return (
    line.startsWith('══') ||
    line.startsWith('──') ||
    line.startsWith('■') ||
    line.startsWith('⚠') ||
    line.startsWith('▶')
  );
}

export function startKind(line: string): { kind: EntryKind; label: string } | null {
  if (line.startsWith('💭 [思考]')) return { kind: 'reasoning', label: '模型思考' };
  if (line.startsWith('✍ [输出]')) return { kind: 'assistant', label: '模型回复' };
  // 任务下发/子任务回报：保留字节数入标签（折叠态摘要可见，ADR-181 补遗）
  const task = line.match(/^📋 \[(任务下发|子任务回报)\]（(\d+) 字节）$/);
  if (task) return { kind: 'task', label: `${task[1]}（${task[2]} 字节）` };
  if (line.startsWith('🤖 [子任务')) {
    // 骨架行两种："[子任务 xxxx] 启动"=单行事件；"[子任务 xxxx] 任务（N 字节）"=带正文块
    if (/^🤖 \[子任务 [^\]]+\] 启动$/.test(line)) return { kind: 'meta', label: line };
    return { kind: 'agent', label: line };
  }
  if (/^── 第 \d+ 轮开始 ──$/.test(line)) return { kind: 'turn', label: line };
  return null;
}

// parseTimeline — 把后端人性化流解析为 DSH web 风格时间线（纯字符串扫描，无状态）。
export function parseTimeline(text: string): TimelineEntry[] {
  const entries: TimelineEntry[] = [];
  let cur: TimelineEntry | null = null;
  const push = () => {
    if (cur) entries.push(cur);
  };
  for (const line of text.split('\n')) {
    const start = startKind(line);
    if (start) {
      push();
      cur = { kind: start.kind, label: start.label, body: '' };
      continue;
    }
    if (metaLine(line)) {
      push();
      cur = { kind: 'meta', label: line, body: '' };
      continue;
    }
    if (!cur) {
      cur = { kind: 'meta', label: '', body: line };
      continue;
    }
    cur.body += (cur.body ? '\n' : '') + line;
  }
  push();
  return entries;
}

export function ReasoningBody({ entry, archived }: { entry: TimelineEntry; archived: boolean }) {
  const st = KIND_STYLE.reasoning;
  // ADR-181：执行未收束前思考流式展开（不折叠）；归档后才允许折叠长思考。
  // 2026-09-14 用户报障：实时态此前只有裸正文（💭 [思考] 行被 parseTimeline 消费为
  // kind 标记后 label 未渲染），标题只在归档 summary 出现——两态不一致。实时态补
  // 同源标题头（无"已归档"字样，收束与否由面板顶部 Tag 表达）。
  // 标题头与正文元素两态各只持一份单源（两态样式曾手抄漂移），收束瞬间零跳变。
  const headStyle = { fontSize: FONT, color: st.labelColor, fontWeight: 600 };
  const body = <pre style={{ margin: '2px 0 0', fontFamily: MONO_FONT, fontSize: FONT, lineHeight: LINE_HEIGHT,
    whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: st.bodyColor }}>{entry.body}</pre>;
  if (!archived) {
    return (
      <>
        <div style={headStyle}>💭 {entry.label}</div>
        {body}
      </>
    );
  }
  return (
    <details>
      <summary style={{ cursor: 'pointer', ...headStyle }}>💭 {entry.label}（已归档，点击展开）</summary>
      {body}
    </details>
  );
}

// FoldableBody — 静态长文本块（任务下发/子任务回报/子任务任务，ADR-181 补遗：
// 人类反馈"任务下发也要可以支持折叠"）：默认折叠，摘要=图标+标签(含字节数)+
// 首行预览，点击展开全文。
export function FoldableBody({ entry }: { entry: TimelineEntry }) {
  const st = KIND_STYLE[entry.kind];
  const icon = entry.kind === 'task' ? '📋' : '🤖';
  const firstLine = entry.body.split('\n').find((l) => l.trim() !== '') ?? '';
  const preview = firstLine.length > 48 ? `${firstLine.slice(0, 48)}…` : firstLine;
  return (
    <details>
      <summary style={{ cursor: 'pointer', fontSize: FONT, color: st.labelColor, fontWeight: 600 }}>
        {icon} {entry.label}
        {preview && <span style={{ color: EVIDENCE.textMuted, fontWeight: 400 }}>{` · ${preview}`}</span>}
      </summary>
      <pre style={{ margin: '2px 0 0', fontFamily: MONO_FONT, fontSize: FONT, lineHeight: LINE_HEIGHT,
        whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: st.bodyColor }}>
        {entry.body}
      </pre>
    </details>
  );
}


// TimelineBody — 条目列表渲染（弹层/分区汇总共用；AIInteractionLogPanel 内联版
// 保持既有渲染不动——两处样式同源，本组件抽出时逐字对齐）。
// windowSize（审查 P2-4）：渐进回看保护——单通道可达 256K 字符（数千条目），
// 逐帧全量解析渲染会拖垮弹层/分区 Modal；默认渲染末窗 WINDOW_STEP 条+加载更早。
export function TimelineBody({ entries, complete, windowSize = WINDOW_STEP }: {
  entries: TimelineEntry[];
  complete: boolean;
  windowSize?: number;
}) {
  const [win, setWin] = useState(windowSize);
  const hidden = Math.max(0, entries.length - win);
  const shown = entries.slice(-win);
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
      {hidden > 0 && (
        <div style={{ fontSize: FONT, color: '#57606a', display: 'flex', gap: 12, alignItems: 'center' }}>
          <span>前面还有 {hidden} 条未显示</span>
          <Button size="small" style={{ fontSize: FONT }} onClick={() => setWin((w) => w + windowSize)}>
            加载更早 {Math.min(windowSize, hidden)} 条
          </Button>
          <Button size="small" style={{ fontSize: FONT }} onClick={() => setWin(entries.length)}>
            显示全部
          </Button>
        </div>
      )}
      {shown.map((e, i) => {
        const st = KIND_STYLE[e.kind];
        return (
          <div key={i} style={{
            borderLeft: `3px solid ${st.border}`,
            padding: '2px 10px',
            background: e.kind === 'reasoning' ? 'rgba(163,113,247,0.06)' : 'rgba(110,118,129,0.08)',
            borderRadius: 4,
          }}>
            {e.kind === 'reasoning' ? (
              <ReasoningBody entry={e} archived={complete} />
            ) : e.kind === 'task' || (e.kind === 'agent' && e.body) ? (
              <FoldableBody entry={e} />
            ) : (
              <>
                {e.label && <div style={{ fontSize: FONT, color: st.labelColor, fontWeight: 600 }}>{e.label}</div>}
                {e.body && (
                  <pre style={{ margin: 0, fontFamily: MONO_FONT, fontSize: FONT, lineHeight: LINE_HEIGHT,
                    whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: st.bodyColor }}>{e.body}</pre>
                )}
              </>
            )}
          </div>
        );
      })}
    </div>
  );
}
