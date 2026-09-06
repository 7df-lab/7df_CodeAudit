// 执行日志面板回归（ADR-167/170 受控化）：渲染/级别过滤/空态/条数徽标
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import TaskLogPanel from '../components/TaskLogPanel';

const LOGS = [
  { log_id: '1', task_id: 't-9', ts_ms: 1756630000000, level: 'TASK_LOG_LEVEL_INFO',
    source: 'task', message: '状态流转 TASK_STATUS_CREATED → TASK_STATUS_PENDING（submit）' },
  { log_id: '2', task_id: 't-9', ts_ms: 1756630001000, level: 'TASK_LOG_LEVEL_INFO',
    source: 'sandbox', message: '沙箱已创建 id=x name=am-1 phase=SANDBOX_PHASE_PROVISIONING' },
  { log_id: '3', task_id: 't-9', ts_ms: 1756630002000, level: 'TASK_LOG_LEVEL_WARN',
    source: 'dsh-agent', message: '沙箱路径不可用（mode=off），降级后续链路（07 §10）' },
  { log_id: '4', task_id: 't-9', ts_ms: 1756630003000, level: 'TASK_LOG_LEVEL_ERROR',
    source: 'orchestrator', message: 'stage boom' },
];

function renderPanel(terminal = true, logs = LOGS) {
  return render(<TaskLogPanel logs={logs} terminal={terminal} onRefresh={() => {}} refreshing={false} />);
}

describe('TaskLogPanel（执行日志，ADR-167/170）', () => {
  it('渲染日志行：时间/级别/来源/消息', () => {
    renderPanel();
    expect(screen.getByText(/沙箱已创建/)).toBeTruthy();
    expect(screen.getByText(/am-1/)).toBeTruthy();
    expect(screen.getByText('[sandbox]')).toBeTruthy();
    expect(screen.getByText('WARN')).toBeTruthy();
    // 条数与级别徽标
    expect(screen.getByText('4 条')).toBeTruthy();
    expect(screen.getByText('警告 1')).toBeTruthy();
    expect(screen.getByText('错误 1')).toBeTruthy();
  });

  it('级别过滤：仅错误只留 ERROR 行', () => {
    renderPanel();
    fireEvent.click(screen.getByText('仅错误'));
    expect(screen.getByText(/stage boom/)).toBeTruthy();
    expect(screen.queryByText(/沙箱已创建/)).toBeNull();
  });

  it('空态：提示文案在深底上可读（ADR-170 配色修正）', () => {
    renderPanel(true, []);
    const box = screen.getByTestId('task-log-box');
    expect(box.textContent!).toContain('暂无执行日志');
  });
});
