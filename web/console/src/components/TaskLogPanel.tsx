// 任务执行日志面板（ADR-167；ADR-170 改为受控展示组件）：流水线事件可视化。
// 数据由详情页的聚合快照轮询（单一轮询器）下发，本组件不再自拉——4 轮询器并 1 后
// 请求速率 20/min 级，远低于 07 §7 单用户 50/min 限流（此前 429 冻结页面的根因）。
import { Button, Card, Segmented, Tag, Tooltip, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { TaskLogEntry } from '../api/types';

const LEVEL_COLOR: Record<string, string> = {
  TASK_LOG_LEVEL_INFO: '#8b949e',
  TASK_LOG_LEVEL_WARN: '#d29922',
  TASK_LOG_LEVEL_ERROR: '#f85149',
};
const LEVEL_ZH: Record<string, string> = {
  TASK_LOG_LEVEL_INFO: 'INFO',
  TASK_LOG_LEVEL_WARN: 'WARN',
  TASK_LOG_LEVEL_ERROR: 'ERROR',
};

type Filter = 'all' | 'warn' | 'error';
const FILTER_LEVELS: Record<Filter, string[]> = {
  all: [],
  warn: ['TASK_LOG_LEVEL_WARN', 'TASK_LOG_LEVEL_ERROR'],
  error: ['TASK_LOG_LEVEL_ERROR'],
};

function hhmmss(tsMs: number | string) {
  // proto int64 经 protojson 到前端是字符串（ADR-167 实测 NaN 回归），统一 Number 化
  const d = new Date(Number(tsMs));
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export default function TaskLogPanel(
  { logs, terminal, onRefresh, refreshing, live }:
  { logs: TaskLogEntry[]; terminal: boolean; onRefresh: () => void; refreshing: boolean; live?: boolean },
) {
  const [filter, setFilter] = useState<Filter>('all');
  const boxRef = useRef<HTMLDivElement>(null);
  const stickBottom = useRef(true);

  const shown = logs.filter((e) => FILTER_LEVELS[filter].length === 0 || FILTER_LEVELS[filter].includes(e.level));
  const warnCount = logs.filter((e) => e.level === 'TASK_LOG_LEVEL_WARN').length;
  const errCount = logs.filter((e) => e.level === 'TASK_LOG_LEVEL_ERROR').length;

  // 新日志到达时自动滚底；用户向上翻阅时停止跟随（stickBottom 惰性跟随）
  useEffect(() => {
    const box = boxRef.current;
    if (box && stickBottom.current) {
      box.scrollTop = box.scrollHeight;
    }
  }, [shown.length]);

  const onScroll = () => {
    const box = boxRef.current;
    if (!box) return;
    stickBottom.current = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  };

  return (
    <Card
      title={
        <span>
          执行日志{' '}
          <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
            （沙箱生命周期 / 降级链 / 挖掘统计）
          </Typography.Text>
        </span>
      }
      style={{ marginTop: 16 }}
      extra={
        <div onClick={(e) => e.stopPropagation()}>
          <span style={{ marginRight: 12 }}>
            {live && <Tag color="processing" style={{ marginRight: 4 }}>WS 秒级推送</Tag>}
            <Tag>{logs.length} 条</Tag>
            {warnCount > 0 && <Tag color="warning">警告 {warnCount}</Tag>}
            {errCount > 0 && <Tag color="error">错误 {errCount}</Tag>}
          </span>
          <Segmented
            size="small"
            value={filter}
            onChange={(v) => setFilter(v as Filter)}
            options={[
              { label: '全部', value: 'all' },
              { label: '警告+', value: 'warn' },
              { label: '仅错误', value: 'error' },
            ]}
            style={{ marginRight: 8 }}
          />
          <Tooltip title={terminal ? '刷新' : live ? 'WebSocket 推流在线（1s 内到达即刷新）' : '随快照每 3 秒自动刷新'}>
            <Button size="small" onClick={onRefresh} loading={refreshing}>
              刷新
            </Button>
          </Tooltip>
        </div>
      }
      styles={{ body: { padding: 0 } }}
    >
      <div
        ref={boxRef}
        onScroll={onScroll}
        style={{
          background: '#0d1117',
          color: '#c9d1d9',
          fontFamily: 'SFMono-Regular, Consolas, "Liberation Mono", Menlo, monospace',
          fontSize: 12,
          lineHeight: '20px',
          padding: '12px 16px',
          maxHeight: 360,
          overflowY: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
        }}
        data-testid="task-log-box"
      >
        {logs.length === 0 ? (
          <span style={{ fontSize: 12, color: '#8b949e' }}>
            暂无执行日志——任务启动后，流水线事件（沙箱创建/就绪/DSH 执行/降级链决策）将在此实时出现。
          </span>
        ) : (
          shown.map((e) => (
            <div key={e.log_id}>
              <span style={{ color: '#8b949e' }}>[{hhmmss(e.ts_ms)}]</span>{' '}
              <span style={{ color: LEVEL_COLOR[e.level] ?? '#8b949e', fontWeight: 600 }}>
                {LEVEL_ZH[e.level] ?? e.level}
              </span>{' '}
              <span style={{ color: '#58a6ff' }}>[{e.source}]</span> {e.message}
            </div>
          ))
        )}
      </div>
    </Card>
  );
}
