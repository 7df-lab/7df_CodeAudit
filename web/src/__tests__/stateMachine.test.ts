// 04 §1 状态机展示镜像回归（T2 完成标准：向导/按钮分支全覆盖）
import { allowedActions, isTerminal, progressRefetchInterval } from '../tasks/stateMachine';

describe('allowedActions（04 §1 全状态分支）', () => {
  it('CREATED → 仅 start（审批流废除，2026-09-01 人类裁定）', () => {
    expect(allowedActions('TASK_STATUS_CREATED')).toEqual(['start']);
  });
  it('PENDING → 无动作（保留值，审批流废除后不再产生）', () => {
    expect(allowedActions('TASK_STATUS_PENDING')).toEqual([]);
  });
  it('QUEUED → start + cancel', () => {
    expect(allowedActions('TASK_STATUS_QUEUED')).toEqual(['start', 'cancel']);
  });
  it('RUNNING → 仅 cancel', () => {
    expect(allowedActions('TASK_STATUS_RUNNING')).toEqual(['pause', 'cancel']);
    expect(allowedActions('TASK_STATUS_PAUSED')).toEqual(['resume', 'cancel']); // ADR-200
  });
  it('FAILED → 无手动动作（自动重试由编排器驱动，proto L174）', () => {
    expect(allowedActions('TASK_STATUS_FAILED')).toEqual(['cancel']);
  });
  it('TIMEOUT → cancel（04 §1 任何非终态可取消）', () => {
    expect(allowedActions('TASK_STATUS_TIMEOUT')).toEqual(['cancel']);
  });
  it('DEAD → 仅人工重试（proto L863）', () => {
    expect(allowedActions('TASK_STATUS_DEAD')).toEqual(['retry', 'cancel']);
  });
  it('终态 COMPLETED/CANCELLED → 无动作', () => {
    expect(allowedActions('TASK_STATUS_COMPLETED')).toEqual([]);
    expect(allowedActions('TASK_STATUS_CANCELLED')).toEqual([]);
  });
  it('未知状态 → 无动作（服务端权威，UI 不自造）', () => {
    expect(allowedActions('TASK_STATUS_FUTURE')).toEqual([]);
  });
});

describe('isTerminal / 进度轮询（14号 §3.4 V1 口径）', () => {
  it('终态判定与 statemachine.IsTerminal 同源（四值）', () => {
    for (const s of ['TASK_STATUS_COMPLETED', 'TASK_STATUS_CANCELLED', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_DEAD']) {
      expect(isTerminal(s)).toBe(true);
    }
    expect(isTerminal('TASK_STATUS_RUNNING')).toBe(false);
  });
  it('非终态（RUNNING/QUEUED/PENDING）均 3s 轮询；终态自停（ADR-156：排队阶段进度可见）', () => {
    expect(progressRefetchInterval('TASK_STATUS_RUNNING')).toBe(3000);
    expect(progressRefetchInterval('TASK_STATUS_QUEUED')).toBe(3000);
    expect(progressRefetchInterval('TASK_STATUS_PENDING')).toBe(3000);
    expect(progressRefetchInterval('TASK_STATUS_COMPLETED')).toBe(false);
    expect(progressRefetchInterval('TASK_STATUS_DEAD')).toBe(false);
  });
});
