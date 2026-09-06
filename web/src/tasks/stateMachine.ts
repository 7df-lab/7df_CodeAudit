// 04 §1 状态机的**客户端展示镜像**：仅决定操作按钮的可见性/置灰（14号 §3.3 ②）。
// 服务端（task-service statemachine）才是转换权威——非法提交会被 FailedPrecondition 拒绝，
// UI 不自作主张放行任何此处未列出的动作。
import { api } from '../api/client';
import type { ScanTask } from '../api/types';

export type TaskAction = 'start' | 'cancel' | 'retry' | 'pause' | 'resume'; // ADR-200: 暂停/恢复（AI 交互会话回合闸门） // 2026-09-01 人类裁定：审批流（提交/批准/拒绝）废除，有创建权限即有启动权限

// 依据: 04 §1 状态机图（含 proto L174/L177 重试耗尽与 proto L863 人工重试）
export const ALLOWED_ACTIONS: Record<string, TaskAction[]> = {
  TASK_STATUS_CREATED: ['start'], // 有创建权限即有启动权限（审批流废除）
  TASK_STATUS_QUEUED: ['start', 'cancel'], // 自动重试路径的瞬态
  TASK_STATUS_RUNNING: ['pause', 'cancel'], // ADR-200: RUNNING 可暂停
  TASK_STATUS_PAUSED: ['resume', 'cancel'], // ADR-200: 暂停态可恢复/取消
  TASK_STATUS_FAILED: ['cancel'], // 后端允许 FAILED→CANCELLED（T10）；自动重试仍由编排器驱动
  TASK_STATUS_TIMEOUT: ['cancel'],
  TASK_STATUS_DEAD: ['retry', 'cancel'], // 人工重试（L863）+ 取消（T10）
  TASK_STATUS_COMPLETED: [],
  TASK_STATUS_CANCELLED: [],
};

// autoRunTask — 创建后直达启动（人类指令 2026-09-01：审批流废除+任务应自动执行）。
// 失败即上抛，状态如实停在 CREATED，可在任务页人工点启动续走。
export async function autoRunTask(taskId: string): Promise<void> {
  await api.post(`/v1/tasks/${taskId}/start`); // 审批流废除（2026-09-01）：创建→启动直达
}

export function allowedActions(status: string | undefined): TaskAction[] {
  return ALLOWED_ACTIONS[status ?? ''] ?? [];
}

// 终态（与 statemachine.IsTerminal 同源口径：COMPLETED/CANCELLED/TIMEOUT/DEAD）
export function isTerminal(status: string | undefined): boolean {
  return ['TASK_STATUS_COMPLETED', 'TASK_STATUS_CANCELLED', 'TASK_STATUS_TIMEOUT', 'TASK_STATUS_DEAD'].includes(
    status ?? '',
  );
}

// 进度轮询间隔（14号 §3.3 ②：3s，终态自停）。独立纯函数便于回归。
export const PROGRESS_POLL_MS = 3000;
// ADR-156: QUEUED 也轮询——此前仅 RUNNING 轮询，排队阶段进度恒"…"，用户感知为卡死。
// 非终态（CREATED/PENDING/QUEUED/RUNNING/FAILED 重试中）均保持轮询，终态自停。
export function progressRefetchInterval(status: string | undefined): number | false {
  if (isTerminal(status)) return false;
  return PROGRESS_POLL_MS;
}

export function actionLabel(a: TaskAction): string {
  const labels: Record<TaskAction, string> = {
    start: '启动',
    cancel: '取消',
    retry: '人工重试',
    pause: '暂停任务',
    resume: '恢复任务',
  };
  return labels[a];
}

export async function dispatchAction(task: ScanTask, a: TaskAction, reason?: string): Promise<string> {
  const id = task.task_id;
  switch (a) {
    case 'start':
      await api.post(`/v1/tasks/${id}/start`);
      break;
    case 'cancel':
      await api.post(`/v1/tasks/${id}/cancel`);
      break;
    case 'retry':
      await api.post(`/v1/tasks/${id}/retry`);
      break;
    case 'pause':
      await api.post(`/v1/tasks/${id}/pause`);
      break;
    case 'resume':
      await api.post(`/v1/tasks/${id}/resume`);
      break;
  }
  return id;
}
