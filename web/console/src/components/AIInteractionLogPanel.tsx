// AI 交互日志面板（ADR-168 补遗②；ADR-170 受控展示组件；ADR-173 时间线风格）：
// 沙箱内 DSH↔模型 bridge 交互的人性化渲染。展示风格对齐 DSH web 版（RunAgentTab）
// 的分类时间线——任务指令/模型思考/工具调用/模型回复 分色条目，思考流可折叠。
// 后端把 SSE 流量转成带类型标记的中文行（原始帧仅落盘 .sse.log），前端把标记行
// 解析为时间线条目；解析纯前端，磁盘日志格式不变。
import { Button, Card, Tag, Tooltip, Typography } from 'antd';
import { useEffect, useMemo, useRef } from 'react';

type EntryKind = 'turn' | 'meta' | 'reasoning' | 'assistant' | 'tool';

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
  tool: { border: '#d29922', label: '调用工具', labelColor: '#d29922', bodyColor: '#c9d1d9' },
};

const MAX_ENTRIES = 400; // 大日志只渲染尾部（完整内容走下载），防万条节点卡死

// 首行即元信息标记（[任务]/[用量]/── 回合结束/══ 会话开始 等）→ meta 条目
function metaLine(line: string): boolean {
  return (
    line.startsWith('══') ||
    line.startsWith('──') ||
    line.startsWith('▶') ||
    line.startsWith('■') ||
    /^\[[^\]]+\]/.test(line)
  );
}

function startKind(line: string): { kind: EntryKind; label: string } | null {
  if (line.startsWith('💭 [思考]')) return { kind: 'reasoning', label: '模型思考' };
  if (line.startsWith('✍ [输出]')) return { kind: 'assistant', label: '模型回复' };
  if (line.startsWith('🔧 [工具调用]')) return { kind: 'tool', label: '调用工具' };
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
      cur = { kind: start.kind, label: start.label, body: start.kind === 'turn' ? '' : '' };
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

export default function AIInteractionLogPanel(
  { text, totalBytes, complete, onRefresh, refreshing, live }:
  { text: string; totalBytes: number; complete: boolean; onRefresh: () => void; refreshing: boolean; live?: boolean },
) {
  const boxRef = useRef<HTMLDivElement>(null);
  const stickBottom = useRef(true);

  const entries = useMemo(() => parseTimeline(text), [text]);
  const hidden = Math.max(0, entries.length - MAX_ENTRIES);
  const shown = entries.slice(-MAX_ENTRIES);

  // 新内容到达时自动滚底；用户向上翻阅时停止跟随
  useEffect(() => {
    const box = boxRef.current;
    if (box && stickBottom.current) {
      box.scrollTop = box.scrollHeight;
    }
  }, [text]);

  const onScroll = () => {
    const box = boxRef.current;
    if (!box) return;
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

  return (
    <Card
      title={
        <span>
          AI 交互日志{' '}
          <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
            （沙箱内 DSH ↔ 模型交互，DSH web 版时间线风格；原始帧落盘 .sse.log）
          </Typography.Text>
        </span>
      }
      style={{ marginTop: 16 }}
      extra={
        <div onClick={(e) => e.stopPropagation()}>
          <span style={{ marginRight: 12 }}>
            <Tag>{totalBytes > 0 ? `${(totalBytes / 1024).toFixed(1)} KB` : '0 KB'}</Tag>
            {complete ? (
              <Tag color="success">已收束</Tag>
            ) : (
              <Tag color="processing">实时接收中</Tag>
            )}
          </span>
          <Tooltip title={complete ? '刷新' : live ? 'WebSocket 推流在线（1s 内到达即刷新）' : '随快照每 10 秒自动增量拉取'}>
            <Button size="small" onClick={onRefresh} loading={refreshing} style={{ marginRight: 8 }}>
              刷新
            </Button>
          </Tooltip>
          <Button size="small" onClick={download} disabled={text.length === 0}>
            下载完整日志
          </Button>
        </div>
      }
      styles={{ body: { padding: 0 } }}
    >
      <div
        ref={boxRef}
        onScroll={onScroll}
        style={{
          background: '#0d1117',
          padding: '12px 16px',
          maxHeight: 360,
          overflowY: 'auto',
        }}
        data-testid="ai-interaction-log-box"
      >
        {text.length === 0 ? (
          <span style={{ fontSize: 12, color: '#8b949e' }}>
            暂无交互日志——沙箱 AI 分析启动后，这里将以时间线实时呈现交互：回合、任务下发、模型思考、工具调用与模型回复；收束后可下载完整日志。
          </span>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            {hidden > 0 && (
              <div style={{ fontSize: 12, color: '#8b949e' }}>（前面还有 {hidden} 条未显示——完整内容请下载日志）</div>
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
                    <details>
                      <summary style={{ cursor: 'pointer', fontSize: 12, color: st.labelColor }}>
                        💭 模型思考
                      </summary>
                      <pre
                        style={{
                          margin: '2px 0 0',
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
                    </details>
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
    </Card>
  );
}
