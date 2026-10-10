// 任务执行日志面板（ADR-167；ADR-170 改为受控展示组件）：流水线事件可视化。
// 数据由详情页的聚合快照轮询（单一轮询器）下发，本组件不再自拉——4 轮询器并 1 后
// 请求速率 20/min 级，远低于 07 §7 单用户 50/min 限流（此前 429 冻结页面的根因）。
import { DownOutlined, UpOutlined } from '@ant-design/icons';
import { Button, Card, Segmented, Tag, Tooltip, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { TaskLogEntry } from '../api/types';
import { EVIDENCE } from '../theme/evidence';
import { MONO_FONT } from '../dict/tokens';

// B4-3（审计修复）：执行日志客户端保尾上限——任务详情页吸收快照时只保留最新 1000 条
// （超长任务无界累积会拖垮标签页）。此常量由 TaskDetailPage 消费，提示文案在此渲染。
// 完整内容下载入口延后（服务端无日志全量导出端点），顶部如实提示截断。
export const MAX_LOG_ROWS = 1000;

const LEVEL_COLOR: Record<string, string> = {
  TASK_LOG_LEVEL_INFO: EVIDENCE.textMuted,
  TASK_LOG_LEVEL_WARN: EVIDENCE.warn,
  TASK_LOG_LEVEL_ERROR: EVIDENCE.error,
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
  { logs, terminal, onRefresh, refreshing, live, maxHeight = 360, collapsible = false, defaultCollapsed = false }:
  { logs: TaskLogEntry[]; terminal: boolean; onRefresh: () => void; refreshing: boolean; live?: boolean; maxHeight?: number; collapsible?: boolean; defaultCollapsed?: boolean },
) {
  const [filter, setFilter] = useState<Filter>('all');
  // 可折叠：终态任务默认收起（头部计数/警告/错误徽标常驻
  // ——错误可见性不丢），运行态保持展开。折叠用 display:none 而非卸载：日志行留驻
  // DOM（保尾/截断提示等既有断言面不破），重新展开滚动位置不丢。
  const [collapsed, setCollapsed] = useState(defaultCollapsed);
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
          {/* B4-3：保尾窗口生效（已达上限）时如实告知截断——更早日志已不在客户端 */}
          {logs.length >= MAX_LOG_ROWS && (
            <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400, marginLeft: 8 }}>
              仅显示最近 {MAX_LOG_ROWS} 条
            </Typography.Text>
          )}
        </span>
      }
      style={{ marginTop: 0 }} /* 信息栏列 gap 统一管距（2026-10-09 起；原自带 16px 与列 gap 叠加） */
      extra={
        <div onClick={(e) => e.stopPropagation()}>
          {collapsible && (
            <Tooltip title={collapsed ? '展开执行日志' : '收起执行日志（计数/警告/错误徽标常驻此行）'}>
              <Button
                size="small"
                type="text"
                aria-label={collapsed ? '展开执行日志' : '收起执行日志'}
                icon={collapsed ? <DownOutlined /> : <UpOutlined />}
                onClick={() => setCollapsed((c) => !c)}
                style={{ marginRight: 4 }}
              />
            </Tooltip>
          )}
          <span style={{ marginRight: 12 }}>
            {live && <Tag color="processing" style={{ marginRight: 4 }}>WS 实时推送</Tag>}
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
          <Tooltip title={terminal ? '刷新' : live ? 'WebSocket 推流在线（亚秒级到达即刷新）' : '随快照每 10 秒自动刷新'}>
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
          background: EVIDENCE.bg,
          color: EVIDENCE.text,
          fontFamily: MONO_FONT,
          fontSize: 12,
          lineHeight: '20px',
          padding: '12px 16px',
          maxHeight,
          overflowY: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-all',
          ...(collapsed ? { display: 'none' } : {}),
        }}
        data-testid="task-log-box"
      >
        {logs.length === 0 ? (
          <span style={{ fontSize: 12, color: EVIDENCE.textMuted }}>
            暂无执行日志——任务启动后，流水线事件（沙箱创建/就绪/DSH 执行/降级链决策）将在此实时出现。
          </span>
        ) : (
          shown.map((e) => (
            <div key={e.log_id}>
              <span style={{ color: EVIDENCE.textMuted }}>[{hhmmss(e.ts_ms)}]</span>{' '}
              <span style={{ color: LEVEL_COLOR[e.level] ?? EVIDENCE.textMuted, fontWeight: 600 }}>
                {LEVEL_ZH[e.level] ?? e.level}
              </span>{' '}
              <span style={{ color: EVIDENCE.link }}>[{e.source}]</span> {e.message}
            </div>
          ))
        )}
      </div>
    </Card>
  );
}
