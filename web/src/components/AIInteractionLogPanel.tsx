// AI 交互日志面板（ADR-168 补遗②；ADR-170 受控展示组件；ADR-173 时间线风格；ADR-181 交互重做；
// ADR-188 人类指令 2026-09-03：内联时间线为主视图，占据任务详情页左侧一半——不再默认折叠）。
// 沙箱内 DSH↔模型 bridge 交互的人性化渲染。后端按 ADR-181 过滤机器噪音（模型路由/
// 审批策略/系统上下文/用量/tool-call 静默）并按 sessionId 分流（主会话流式渲染、
// 子任务只出骨架行），前端把标记行解析为时间线条目；解析纯前端，磁盘日志格式不变。
//
// ADR-181 历史交互语义（保留）：
//   ② 模型思考在任务未收束（complete=false）时流式展开显示，收束归档后才折叠；
//   ③ 任务下发显示完整提示词（后端已全文下发，前端原样呈现）；
//   ④ "前面还有 N 条未显示"改为可继续加载（每次 +400 / 一键全部），不只靠下载；
//   ⑤ 整页 Modal 保留为辅入口（"整页查看"按钮）。
import { EyeOutlined } from '@ant-design/icons';
import { Button, Card, Modal, Tag, Tooltip, Typography } from 'antd';
import { useEffect, useMemo, useRef, useState } from 'react';

type EntryKind = 'turn' | 'meta' | 'reasoning' | 'assistant' | 'task' | 'agent';

interface TimelineEntry {
  kind: EntryKind;
  label: string;
  body: string;
}

const KIND_STYLE: Record<EntryKind, { border: string; label: string; labelColor: string; bodyColor: string }> = {
  turn: { border: '#58a6ff', label: '回合', labelColor: '#58a6ff', bodyColor: '#c9d1d9' },
  meta: { border: '#30363d', label: '事件', labelColor: '#8b949e', bodyColor: '#8b949e' },
  reasoning: { border: '#a371f7', label: '模型思考', labelColor: '#a371f7', bodyColor: '#8b949e' },
  assistant: { border: '#7ee787', label: '模型回复', labelColor: '#7ee787', bodyColor: '#c9d1d9' },
  task: { border: '#39c5cf', label: '任务', labelColor: '#39c5cf', bodyColor: '#c9d1d9' },
  agent: { border: '#ffa657', label: '子任务', labelColor: '#ffa657', bodyColor: '#c9d1d9' },
};

const WINDOW_STEP = 400; // 大日志渐进回看步长（ADR-181：提供继续显示方式，不只靠下载）

// 元信息行（══ 会话开始 / ── 第 N 轮 / ── 回合结束 / ■ 收束 / ⚠ 退出）→ meta 条目
function metaLine(line: string): boolean {
  return (
    line.startsWith('══') ||
    line.startsWith('──') ||
    line.startsWith('■') ||
    line.startsWith('⚠') ||
    line.startsWith('▶')
  );
}

function startKind(line: string): { kind: EntryKind; label: string } | null {
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
function parseTimeline(text: string): TimelineEntry[] {
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

function ReasoningBody({ entry, archived }: { entry: TimelineEntry; archived: boolean }) {
  const st = KIND_STYLE.reasoning;
  // ADR-181：执行未收束前思考流式展开（不折叠）；归档后才允许折叠长思考
  if (!archived) {
    return (
      <pre style={{ margin: '2px 0 0', fontFamily: 'inherit', fontSize: 12, lineHeight: '18px',
        whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: st.bodyColor }}>
        {entry.body}
      </pre>
    );
  }
  return (
    <details>
      <summary style={{ cursor: 'pointer', fontSize: 12, color: st.labelColor }}>💭 模型思考（已归档，点击展开）</summary>
      <pre style={{ margin: '2px 0 0', fontFamily: 'inherit', fontSize: 12, lineHeight: '18px',
        whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: st.bodyColor }}>
        {entry.body}
      </pre>
    </details>
  );
}

// FoldableBody — 静态长文本块（任务下发/子任务回报/子任务任务，ADR-181 补遗：
// 人类反馈"任务下发也要可以支持折叠"）：默认折叠，摘要=图标+标签(含字节数)+
// 首行预览，点击展开全文。
function FoldableBody({ entry }: { entry: TimelineEntry }) {
  const st = KIND_STYLE[entry.kind];
  const icon = entry.kind === 'task' ? '📋' : '🤖';
  const firstLine = entry.body.split('\n').find((l) => l.trim() !== '') ?? '';
  const preview = firstLine.length > 48 ? `${firstLine.slice(0, 48)}…` : firstLine;
  return (
    <details>
      <summary style={{ cursor: 'pointer', fontSize: 12, color: st.labelColor, fontWeight: 600 }}>
        {icon} {entry.label}
        {preview && <span style={{ color: '#8b949e', fontWeight: 400 }}>{` · ${preview}`}</span>}
      </summary>
      <pre style={{ margin: '2px 0 0', fontFamily: 'inherit', fontSize: 12, lineHeight: '18px',
        whiteSpace: 'pre-wrap', wordBreak: 'break-all', color: st.bodyColor }}>
        {entry.body}
      </pre>
    </details>
  );
}

export default function AIInteractionLogPanel(
  { text, totalBytes, complete, onRefresh, refreshing, live }:
  { text: string; totalBytes: number; complete: boolean; onRefresh: () => void; refreshing: boolean; live?: boolean },
) {
  // 内联与 Modal 各自持 ref——此前共用一个 boxRef，Modal 内容挂载后 ref 被抢占且关闭后
  // （antd Modal 默认不销毁）仍指向隐藏盒子：内联"自动滚底"永久失效、onScroll 读错盒子。
  const inlineBoxRef = useRef<HTMLDivElement>(null);
  const modalBoxRef = useRef<HTMLDivElement>(null);
  const stickBottom = useRef(true);
  const [modalOpen, setModalOpen] = useState(false); // ADR-188：整页 Modal 为辅入口
  const [window_, setWindow] = useState(WINDOW_STEP); // 渐进回看窗口

  const entries = useMemo(() => parseTimeline(text), [text]);
  const hidden = Math.max(0, entries.length - window_);
  const shown = entries.slice(-window_);

  // 新内容到达时自动滚底（Modal 开着滚 Modal，否则滚内联）；用户向上翻阅时停止跟随
  useEffect(() => {
    const box = modalOpen ? modalBoxRef.current : inlineBoxRef.current;
    if (box && stickBottom.current) {
      box.scrollTop = box.scrollHeight;
    }
  }, [text, modalOpen]);

  const onScroll = (e: { currentTarget: HTMLDivElement }) => {
    const box = e.currentTarget;
    stickBottom.current = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  };

  const download = () => {
    const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `ai-interaction.ai.log`;
    a.click();
    URL.revokeObjectURL(url);
  };

  // 时间线正文（内联与 Modal 共用渲染；boxId 区分 testid——内联为主视图；
  // boxRef 由调用方分别传入，杜绝共享 ref 被 Modal 抢占）
  const timeline = (boxId: string, height: string, boxRef: typeof inlineBoxRef) => (
    <div
      ref={boxRef}
      onScroll={onScroll}
      style={{
        background: '#0d1117',
        padding: '12px 16px',
        height,
        overflowY: 'auto',
        borderRadius: 4,
      }}
      data-testid={boxId}
    >
      {text.length === 0 ? (
        <span style={{ fontSize: 12, color: '#8b949e' }}>
          暂无交互日志——沙箱 AI 分析启动后，任务下发全文、模型思考、模型回复与子任务回报将在此实时滚动。
        </span>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {hidden > 0 && (
            <div style={{ fontSize: 12, color: '#8b949e', display: 'flex', gap: 12, alignItems: 'center' }}>
              <span>前面还有 {hidden} 条未显示</span>
              <Button size="small" onClick={() => setWindow((w) => w + WINDOW_STEP)}>
                加载更早 {Math.min(WINDOW_STEP, hidden)} 条
              </Button>
              <Button size="small" onClick={() => setWindow(entries.length)}>
                显示全部
              </Button>
            </div>
          )}
          {shown.map((e, i) => {
            const st = KIND_STYLE[e.kind];
            return (
              <div
                key={i}
                style={{
                  borderLeft: `3px solid ${st.border}`,
                  padding: '2px 10px',
                  background: e.kind === 'reasoning' ? 'rgba(163,113,247,0.06)' : 'rgba(110,118,129,0.08)',
                  borderRadius: 4,
                }}
              >
                {e.kind === 'reasoning' ? (
                  <ReasoningBody entry={e} archived={complete} />
                ) : e.kind === 'task' || (e.kind === 'agent' && e.body) ? (
                  <FoldableBody entry={e} />
                ) : (
                  <>
                    {e.label && (
                      <div style={{ fontSize: 12, color: st.labelColor, fontWeight: 600 }}>{e.label}</div>
                    )}
                    {e.body && (
                      <pre
                        style={{
                          margin: 0,
                          fontFamily: 'inherit',
                          fontSize: 12,
                          lineHeight: '18px',
                          whiteSpace: 'pre-wrap',
                          wordBreak: 'break-all',
                          color: st.bodyColor,
                        }}
                      >
                        {e.body}
                      </pre>
                    )}
                  </>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );

  const statusTags = (
    <span style={{ marginRight: 12 }}>
      <Tag>{totalBytes > 0 ? `${(totalBytes / 1024).toFixed(1)} KB` : '0 KB'}</Tag>
      {complete ? <Tag color="success">已收束</Tag> : <Tag color="processing">实时接收中</Tag>}
    </span>
  );

  return (
    <Card
      title={
        <span>
          AI 交互日志{' '}
          <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
            （沙箱内 DSH ↔ 模型交互时间线，实时滚动）
          </Typography.Text>
        </span>
      }
      extra={
        <div onClick={(e) => e.stopPropagation()}>
          {statusTags}
          <Tooltip title={complete ? '刷新' : live ? 'WebSocket 推流在线（亚秒级到达即刷新）' : '随快照每 10 秒自动增量拉取'}>
            <Button size="small" onClick={onRefresh} loading={refreshing} style={{ marginRight: 8 }}>
              刷新
            </Button>
          </Tooltip>
          <Button size="small" onClick={download} disabled={text.length === 0} style={{ marginRight: 8 }}>
            下载完整日志
          </Button>
          <Button size="small" icon={<EyeOutlined />} disabled={text.length === 0} onClick={() => setModalOpen(true)}>
            整页查看
          </Button>
        </div>
      }
      styles={{ body: { padding: '8px 16px' } }}
    >
      {timeline('ai-interaction-log-box', 'calc(100vh - 230px)', inlineBoxRef)}

      <Modal
        title={
          <span>
            AI 交互日志{' '}
            <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
              （沙箱内 DSH ↔ 模型交互时间线{complete ? '；已归档' : '；实时接收中'}）
            </Typography.Text>
          </span>
        }
        open={modalOpen}
        onCancel={() => setModalOpen(false)}
        footer={null}
        width="min(96vw, 1500px)"
        styles={{ body: { padding: 0 }, content: { top: 24 } }}
      >
        {timeline('ai-interaction-log-box-modal', 'calc(100vh - 140px)', modalBoxRef)}
      </Modal>
    </Card>
  );
}
