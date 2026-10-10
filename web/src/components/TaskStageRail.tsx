// 阶段时间线的紧凑点轨：原 antd horizontal Steps 连描述
// 行占高 ~140px+，是"产出视图被挤出视口"的主因之一。同样的状态语义压成单行仪表轨：
//   完成=实心绿 · 运行=脉冲蓝 · 失败=实心红 · 跳过=空心灰 · 未启动=空心浅灰，
//   连段颜色承前段状态（完成路径呈绿色填充——进度一眼可读）。
// 诚实呈现不降级：跳过/降级/失败/进行中的副行常驻（"已跳过"承 2026-10-03 修复——
// 引擎回填的起止时间戳不得把"从未执行"伪装成"1s 跑完"）；完整起止耗时收进 hover
// Tooltip。零硬编码色：antd 语义 token 经 CSS 变量下发；脉冲动画尊重
// prefers-reduced-motion。
import { Tooltip, Typography, theme as antdTheme } from 'antd';
import { Fragment } from 'react';
import type { CSSProperties } from 'react';
import type { TaskStage } from '../api/types';
import { STAGE_TYPE, zh } from '../dict';

// proto Timestamp（protojson RFC3339 字符串）→ 本地 hh:mm:ss（迁自 TaskDetailPage
// ——Steps 退役后唯一消费方在本轨）
function hhmmss(ts: string | null | undefined): string {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '';
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}

// 阶段时间（ADR-181）：RUNNING 显示开始时刻（进行中的中间态），终态显示起止与耗时。
// SKIPPED 出"已跳过"——引擎早失败收敛会回填起止时间戳（R63/ADR-149b），照实显示耗时
// 会把"从未执行的阶段"伪装成"1s 跑完"
function stageTimeText(st: TaskStage): string {
  if (st.status === 'STAGE_STATUS_SKIPPED') return '已跳过';
  const start = hhmmss(st.started_at);
  const end = hhmmss(st.completed_at);
  if (st.status === 'STAGE_STATUS_RUNNING' && start) return `开始于 ${start}`;
  if (start && end) {
    const ms = new Date(st.completed_at!).getTime() - new Date(st.started_at!).getTime();
    if (!isNaN(ms)) {
      const s = Math.max(1, Math.round(ms / 1000));
      return `${start} → ${end}（耗时 ${s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60}s`}）`;
    }
  }
  return start ? `开始于 ${start}` : '';
}

type RailState = 'done' | 'process' | 'error' | 'skipped' | 'wait';
const RAIL_STATE: Record<string, RailState> = {
  STAGE_STATUS_PENDING: 'wait',
  STAGE_STATUS_RUNNING: 'process',
  STAGE_STATUS_COMPLETED: 'done',
  STAGE_STATUS_FAILED: 'error',
  // SKIPPED=空心灰点+副行"已跳过"（原 Steps wait 灰底+MinusCircle 图标的同语义替代）
  STAGE_STATUS_SKIPPED: 'skipped',
};

export default function TaskStageRail({ stages }: { stages: TaskStage[] }) {
  const { token } = antdTheme.useToken();

  if (!stages.length) {
    return <Typography.Text type="secondary">任务尚未启动（阶段在 StartTask 时注册）</Typography.Text>;
  }

  // 语义 token → CSS 变量（点/连段/文字全走 token，零硬编码色）
  const vars = {
    '--rail-primary': token.colorPrimary,
    '--rail-primary-ring': token.colorPrimaryBorder,
    '--rail-success': token.colorSuccess,
    '--rail-error': token.colorError,
    '--rail-warn': token.colorWarning,
    '--rail-border': token.colorBorder,
    '--rail-disabled': token.colorTextDisabled,
    '--rail-split': token.colorSplit,
    '--rail-text': token.colorText,
    '--rail-text-2': token.colorTextSecondary,
  } as CSSProperties;

  return (
    <div className="task-stage-rail" style={vars}>
      <style data-testid="task-stage-rail-style">{`
        .task-stage-rail { display: flex; align-items: flex-start; flex-wrap: wrap; row-gap: 6px; }
        .task-stage-rail .stage-cell { display: flex; flex-direction: column; gap: 1px; min-width: 0; }
        .task-stage-rail .stage-head { display: flex; align-items: center; gap: 6px; height: 20px; white-space: nowrap; }
        .task-stage-rail .stage-dot { width: 8px; height: 8px; border-radius: 50%; flex: none; box-sizing: border-box; }
        .task-stage-rail .stage-dot-done { background: var(--rail-success); }
        .task-stage-rail .stage-dot-process { background: var(--rail-primary); animation: rail-pulse 1.8s ease-out infinite; }
        .task-stage-rail .stage-dot-error { background: var(--rail-error); }
        .task-stage-rail .stage-dot-wait { background: transparent; border: 1.5px solid var(--rail-border); }
        .task-stage-rail .stage-dot-skipped { background: transparent; border: 1.5px solid var(--rail-disabled); }
        .task-stage-rail .stage-name { font-size: 12px; color: var(--rail-text); }
        .task-stage-rail .stage-name-process { color: var(--rail-primary); font-weight: 600; }
        .task-stage-rail .stage-name-skipped { color: var(--rail-disabled); }
        /* 连段（阶段 i 与 i+1 之间）承阶段 i 状态：完成路径=绿填充（软化透明度），进度可视化 */
        .task-stage-rail .stage-link { flex: 1 1 16px; min-width: 10px; height: 2px; border-radius: 1px; margin: 9px 8px 0; background: var(--rail-split); }
        .task-stage-rail .stage-link-done { background: var(--rail-success); opacity: 0.45; }
        .task-stage-rail .stage-link-process { background: var(--rail-primary); opacity: 0.6; }
        .task-stage-rail .stage-sub { font-size: 11px; line-height: 16px; padding-left: 14px; max-width: 190px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
        .task-stage-rail .stage-sub-time { color: var(--rail-text-2); }
        .task-stage-rail .stage-sub-skip { color: var(--rail-disabled); }
        .task-stage-rail .stage-sub-degraded { color: var(--rail-warn); }
        .task-stage-rail .stage-sub-error { color: var(--rail-error); }
        @keyframes rail-pulse {
          0% { box-shadow: 0 0 0 0 var(--rail-primary-ring); }
          70% { box-shadow: 0 0 0 5px transparent; }
          100% { box-shadow: 0 0 0 0 transparent; }
        }
        @media (prefers-reduced-motion: reduce) { .task-stage-rail .stage-dot-process { animation: none; } }
      `}</style>
      {stages.map((st, i) => {
        const state = RAIL_STATE[st.status] ?? 'wait';
        const timeText = stageTimeText(st);
        const degraded = st.metadata?.degraded === 'true';
        // 副行=须常驻的诚实信号（进行中时刻/已跳过/已降级/失败原因）；起止耗时只进 Tooltip
        const [subText, subKind]: [string, string] =
          st.status === 'STAGE_STATUS_RUNNING' && timeText ? [timeText, 'time']
          : st.status === 'STAGE_STATUS_SKIPPED' ? ['已跳过', 'skip']
          : degraded ? ['（已降级·RuleScan）', 'degraded']
          : st.error_message ? [st.error_message, 'error']
          : ['', ''];
        return (
          <Fragment key={st.stage_id || `${st.type}-${i}`}>
            <Tooltip title={
              <div>
                <div>{zh(STAGE_TYPE, st.type)}</div>
                {timeText && <div>{timeText}</div>}
                {degraded && <div>（已降级·RuleScan）</div>}
                {st.error_message && <div>{st.error_message}</div>}
              </div>
            }>
              <div className="stage-cell">
                <div className="stage-head">
                  <span className={`stage-dot stage-dot-${state}`} />
                  <span className={`stage-name${state === 'process' ? ' stage-name-process' : state === 'skipped' ? ' stage-name-skipped' : ''}`}>
                    {zh(STAGE_TYPE, st.type)}
                  </span>
                </div>
                {subText && <div className={`stage-sub stage-sub-${subKind}`}>{subText}</div>}
              </div>
            </Tooltip>
            {i < stages.length - 1 && <div className={`stage-link stage-link-${state}`} aria-hidden="true" />}
          </Fragment>
        );
      })}
    </div>
  );
}
