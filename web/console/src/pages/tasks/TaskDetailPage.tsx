// 任务详情（14号 §3.3 ②）：阶段时间线 + 进度轮询（3s，终态自停）+ 状态流转按钮
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Descriptions, Popconfirm, Space, Steps, Tag, Typography, message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api, getAccessToken, pollIntervalMs } from '../../api/client';
import type { ScanTask, TaskLogEntry, TaskProgress, TaskSnapshot } from '../../api/types';

type TaskLogEntryType = TaskLogEntry;
import FindingsPage from '../findings/FindingsPage';
import { getReportContent } from '../../api/client';
import FusionView from '../views/FusionView';
import ReviewView from '../views/ReviewView';
import TaskLogPanel from '../../components/TaskLogPanel';
import AIInteractionLogPanel from '../../components/AIInteractionLogPanel';
import { SCAN_MODE, STAGE_STATUS, STAGE_TYPE, TASK_STATUS, zh } from '../../dict';
import { Tabs } from 'antd';
import { actionLabel, allowedActions, dispatchAction, isTerminal, progressRefetchInterval, type TaskAction } from '../../tasks/stateMachine';

// proto bytes（protojson base64）→ utf-8 原文（AI 交互日志增量）
function b64ToText(b64: string): string {
  if (!b64) return '';
  const bin = atob(b64);
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

const STEP_STATUS: Record<string, 'wait' | 'process' | 'finish' | 'error'> = {
  STAGE_STATUS_PENDING: 'wait',
  STAGE_STATUS_RUNNING: 'process',
  STAGE_STATUS_COMPLETED: 'finish',
  STAGE_STATUS_FAILED: 'error',
  STAGE_STATUS_SKIPPED: 'finish',
};

export default function TaskDetailPage({ taskId }: { taskId: string }) {
  const qc = useQueryClient();

  // ADR-170: 聚合快照单口轮询（3s=20/min，429 退避）；ADR-172: WebSocket 秒级推送在线时
  // 轮询暂停、断线自动回退本轮询器（保底语义不变）。日志/AI 游标在 refs 中累进。
  const logAfterRef = useRef('');
  const aiCursorRef = useRef(0);
  const [logRows, setLogRows] = useState<TaskLogEntryType[]>([]);
  const [aiText, setAiText] = useState('');
  const [aiMeta, setAiMeta] = useState({ complete: false, total: 0 });
  const wsLiveRef = useRef(false);
  const [wsLive, setWsLive] = useState(false);

  // 快照增量吸收：轮询响应与 WS 帧（ADR-172 同构 JSON）共用一条路径。
  // log_id 去重 + AI 游标单调：轮询与 WS 游标各自独立（服务端连接游标自订阅位起算），
  // 首帧/重连交叠时此处兜底，杜绝重复行。
  const seenLogIdsRef = useRef<Set<string>>(new Set());
  const absorbSnapshot = (d: TaskSnapshot) => {
    const newLogs = (d.logs?.logs ?? []).filter((l) => !seenLogIdsRef.current.has(l.log_id));
    if (newLogs.length > 0) {
      for (const l of newLogs) seenLogIdsRef.current.add(l.log_id);
      logAfterRef.current = newLogs[newLogs.length - 1].log_id;
      setLogRows((prev) => [...prev, ...newLogs]);
    }
    const nextCursor = Number(d.ai?.next_cursor ?? 0);
    if (nextCursor > aiCursorRef.current && (d.ai?.chunk ?? '') !== '') {
      aiCursorRef.current = nextCursor;
      setAiText((prev) => prev + b64ToText(d.ai!.chunk));
    }
    setAiMeta({ complete: !!d.ai?.complete, total: Number(d.ai?.total_bytes ?? 0) });
  };

  const { data: snap, isError: taskError, error: taskErr, refetch: taskRetry, isFetching: snapFetching } = useQuery({
    queryKey: ['task-snapshot', taskId],
    retry: false, // NotFound 如实终态（内存存储重启清除任务——报告中心可能存在此类旧链）
    queryFn: async () => {
      const r = await api.get(`/v1/tasks/${taskId}/snapshot`, {
        params: {
          ...(logAfterRef.current ? { logs_after: logAfterRef.current } : {}),
          ...(aiCursorRef.current > 0 ? { ai_cursor: aiCursorRef.current } : {}),
        },
      });
      const d = r.data as TaskSnapshot;
      absorbSnapshot(d);
      return d;
    },
    refetchInterval: (q) => {
      if (wsLiveRef.current) return false; // WS 推流在线：轮询停（ADR-172）
      const d = q.state.data;
      if (!d?.task) return pollIntervalMs(10_000);
      const aiDone = !!d.ai?.complete && aiCursorRef.current >= Number(d.ai?.total_bytes ?? 0);
      if (isTerminal(d.task.status) && aiDone) return false; // 终态且日志收束 → 自停
      return pollIntervalMs(10_000); // WS 断线回退 10s/次（人类指令 2026-09-01；限流余量进一步扩大）
    },
  });

  // ADR-172: WebSocket 秒级推送——服务端 1s 聚合推帧（网关 /v1/tasks/{id}/ws），
  // 帧到达即入缓存/增量面板；断线回退 3s 轮询并每 5s 重连，任务终态收束后不再重连。
  useEffect(() => {
    let closed = false;
    let retryTimer: number | undefined;
    let settled = false;
    const connect = () => {
      if (closed || settled) return;
      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      let ws: WebSocket;
      try {
        const params = new URLSearchParams({
          token: getAccessToken(),
          ...(logAfterRef.current ? { logs_after: logAfterRef.current } : {}),
          ...(aiCursorRef.current > 0 ? { ai_cursor: String(aiCursorRef.current) } : {}),
        });
        ws = new WebSocket(`${proto}//${window.location.host}/v1/tasks/${taskId}/ws?${params}`);
      } catch {
        setWsLive(false); // 无 WebSocket 环境 → 轮询兜底
        return;
      }
      ws.onopen = () => {
        if (!closed) {
          wsLiveRef.current = true;
          setWsLive(true);
        }
      };
      ws.onmessage = (ev: MessageEvent<string>) => {
        try {
          const d = JSON.parse(ev.data) as TaskSnapshot & { type?: string };
          if (d.type !== 'snapshot' || !d.task) return;
          absorbSnapshot(d);
          qc.setQueryData(['task-snapshot', taskId], d);
          if (
            isTerminal(d.task.status) &&
            !!d.ai?.complete &&
            Number(d.ai?.next_cursor ?? 0) >= Number(d.ai?.total_bytes ?? 1)
          ) {
            settled = true; // 终态收束：服务端会关连接，不再重连（轮询自停条件同样满足）
            ws.close();
          }
        } catch {
          /* 单帧异常不致断流 */
        }
      };
      ws.onclose = () => {
        wsLiveRef.current = false;
        if (!closed) setWsLive(false);
        if (!closed && !settled) retryTimer = window.setTimeout(connect, 5000);
      };
      ws.onerror = () => ws.close();
    };
    connect();
    return () => {
      closed = true;
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskId]);
  const task: ScanTask | undefined = snap?.task;
  const progress: TaskProgress | undefined = snap?.progress ?? undefined;
  const isTerminalQuery = !!task && isTerminal(task.status);
  const isCompletedTask = !!task && task.status === 'TASK_STATUS_COMPLETED';

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['task-snapshot', taskId] });
    qc.invalidateQueries({ queryKey: ['tasks'] });
  };

  const act = useMutation({
    mutationFn: async (a: TaskAction) => dispatchAction(task!, a),
    onSuccess: () => {
      message.success('操作已提交');
      invalidate();
    },
    onError: (e) => message.error(`操作被拒绝：${(e as Error).message}`),
  });

  // ADR-150: 报告初步判断内联——拉取本任务最新报告并解析 summary，不再绕行报告中心
  const { data: taskReports } = useQuery({
    queryKey: ['task-reports', taskId],
    enabled: isCompletedTask,
    queryFn: async () => (await api.get('/v1/reports', { params: { task_id: taskId } })).data as {
      reports: { report_id: string; format: number }[];
    },
  });
  const latestReport = taskReports?.reports?.[0];
  const { data: reportContent } = useQuery({
    queryKey: ['report-content', latestReport?.report_id],
    enabled: !!latestReport,
    queryFn: async () => {
      const rid = latestReport?.report_id as string;
      return getReportContent(rid);
    },
  });
  const reportSummary = (() => {
    if (!reportContent || reportContent.format !== 'json') return null;
    try {
      return JSON.parse(reportContent.content).summary as Record<string, number> | null;
    } catch {
      return null;
    }
  })();
  const viewReport = () => {
    if (!latestReport) return;
    getReportContent(latestReport.report_id).then(({ format, content }) => {
      const w = window.open('', '_blank');
      if (!w) return;
      if (format === 'html') {
        w.document.write(content);
      } else {
        w.document.write('<pre style="font-size:13px;white-space:pre-wrap">' +
          JSON.stringify(JSON.parse(content), null, 2).replace(/[<>&]/g,
            (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;' }[c] || c)) + '</pre>');
      }
      w.document.close();
    }).catch(() => message.error('打开失败'));
  };
  const downloadReport = async () => {
    if (!latestReport) return;
    try {
      const resp = await api.get(`/v1/reports/${latestReport.report_id}/download`, { responseType: 'blob' });
      const url = URL.createObjectURL(resp.data as Blob);
      const a = document.createElement('a');
      a.href = url; a.download = `${latestReport.report_id}.bin`; a.click();
      URL.revokeObjectURL(url);
    } catch {
      message.error('下载失败');
    }
  };

  const regenerate = useMutation({
    mutationFn: async () => api.post(`/v1/tasks/${taskId}/report`, {}),
    onSuccess: () => {
      message.success('报告已生成——可在"查看报告"中打开');
      qc.invalidateQueries({ queryKey: ['reports'] });
      qc.invalidateQueries({ queryKey: ['reports', taskId] });
    },
  });

  if (taskError) {
    // ADR-147: 区分 404（任务已清除）与其他错误（限流/网络）——此前限流也误报"不存在"
    const status = (taskErr as { response?: { status?: number } })?.response?.status;
    if (status !== 404) {
      return (
        <Alert type="error" showIcon style={{ margin: 24 }}
          message={`加载失败（${status ?? '网络错误'}）`}
          description="服务暂不可用或请求被限流，请稍后重试。"
          action={<Button onClick={() => taskRetry()}>重试</Button>} />
      );
    }
    return (
      <Alert type="warning" showIcon style={{ margin: 24 }}
        message="任务不存在或已被清除"
        description="内存存储模式下服务重启会清除任务（演示口径）。报告中心的旧条目可能指向已清除的任务——报告文件本身仍在。"
        action={<Button onClick={() => window.history.back()}>返回</Button>} />
    );
  }
  if (!task) return <Typography.Text type="secondary">加载中…</Typography.Text>;
  const actions = allowedActions(task.status);

  return (
    <div>
      <Typography.Title level={3}>
        任务 {task.task_id} <Tag color="blue">{zh(TASK_STATUS, task.status)}</Tag>
      </Typography.Title>
      <Card style={{ marginBottom: 16 }}>
        <Descriptions column={2} size="small">
          <Descriptions.Item label="项目">{task.project_id}</Descriptions.Item>
          <Descriptions.Item label="模式">{zh(SCAN_MODE, task.scan_mode)}</Descriptions.Item>
          <Descriptions.Item label="重试次数">{task.retry_count}</Descriptions.Item>
          <Descriptions.Item label="进度">{progress ? `${progress.overall_percent.toFixed(0)}%` : '—'}</Descriptions.Item>
        </Descriptions>
        {task.error_message && (
          <Alert type="error" showIcon style={{ marginTop: 12 }} message={task.error_message} />
        )}
        <Space style={{ marginTop: 12 }} wrap>
          {actions.map((a) =>
            a === 'retry' ? (
              <Popconfirm key={a} title="确认人工重试该任务？" onConfirm={() => act.mutate(a)}>
                <Button type="primary">{actionLabel(a)}</Button>
              </Popconfirm>
            ) : (
              <Button key={a} type={a === 'start' ? 'primary' : 'default'} onClick={() => act.mutate(a)}>
                {actionLabel(a)}
              </Button>
            ),
          )}
          {isTerminal(task.status) && task.status === 'TASK_STATUS_COMPLETED' && (
            <>
              {/* 模式相关独立视图（C→对比 / D→审核）；发现与融合已内嵌下方 Tabs */}
              {task.scan_mode === 'SCAN_MODE_PARALLEL' && (
                <Link to={`/tasks/${taskId}/comparison`}><Button>对比视图</Button></Link>
              )}
              {/* 任务↔报告双向导航（ADR-142）：直达本任务报告过滤视图 */}
              <Link to={`/reports?task=${taskId}`}>
                <Button>查看报告</Button>
              </Link>
              <Popconfirm title="重新生成报告？" onConfirm={() => regenerate.mutate()}>
                <Button loading={regenerate.isPending}>重新生成报告</Button>
              </Popconfirm>
            </>
          )}
        </Space>
      </Card>

      <Card title="阶段时间线（proto TaskStage）">
        {task.stages?.length ? (
          <Steps
            direction="vertical"
            size="small"
            items={task.stages.map((st) => ({
              title: `${zh(STAGE_TYPE, st.type)}（${st.stage_id}）`,
              status: STEP_STATUS[st.status] ?? 'wait',
              description: (
                <>
                  <Tag>{zh(STAGE_STATUS, st.status)}</Tag>
                  {st.error_message && <Typography.Text type="danger">{st.error_message}</Typography.Text>}
                </>
              ),
            }))}
          />
        ) : (
          <Typography.Text type="secondary">任务尚未启动（阶段在 StartTask 时注册）</Typography.Text>
        )}
      </Card>

      {/* ADR-167/170/172: 执行日志——快照轮询或 WS 推流（live 徽标）增量下发 */}
      <TaskLogPanel logs={logRows} terminal={isTerminal(task.status)} onRefresh={taskRetry} refreshing={snapFetching} live={wsLive} />

      {/* ADR-168/170/172: AI 交互日志——人性化渲染流增量下发；终态=最终交互日志（可下载） */}
      <AIInteractionLogPanel
        text={aiText}
        totalBytes={aiMeta.total}
        complete={aiMeta.complete}
        onRefresh={taskRetry}
        refreshing={snapFetching}
        live={wsLive}
      />

      {/* ADR-150: 报告初步判断内联（此前需跳报告中心再点在线查看，重复呆板） */}
      {isTerminalQuery && isCompletedTask && reportSummary && (
        <Card title="报告初步判断（最新报告摘要）" style={{ marginTop: 16 }}
          extra={
            <Space>
              <Button size="small" onClick={viewReport}>在线查看完整报告</Button>
              <Button size="small" onClick={downloadReport}>下载</Button>
              <Link to="/reports">报告中心</Link>
            </Space>
          }>
        <Descriptions column={4} size="small">
          <Descriptions.Item label="发现总数">{reportSummary.total_findings ?? 0}</Descriptions.Item>
          <Descriptions.Item label="确认为真">{reportSummary.true_positives ?? 0}</Descriptions.Item>
          <Descriptions.Item label="误报">{reportSummary.false_positives ?? 0}</Descriptions.Item>
          <Descriptions.Item label="未复核">{reportSummary.not_reviewed ?? 0}</Descriptions.Item>
        </Descriptions>
      </Card>
      )}
      {isTerminal(task.status) && (
        <Card style={{ marginTop: 16 }}>
          <Tabs
            items={[
              { key: 'findings', label: '发现', children: <FindingsPage taskId={task.task_id} /> },
              ...(task.scan_mode === 'SCAN_MODE_TRADITIONAL_FIRST'
                ? [{ key: 'fusion', label: '融合视图', children: <FusionView taskId={task.task_id} /> }]
                : []),
              ...(task.scan_mode === 'SCAN_MODE_SAST_REVIEW'
                ? [{ key: 'review', label: '审核视图', children: <ReviewView taskId={task.task_id} /> }]
                : []),
            ]}
          />
        </Card>
      )}

    </div>
  );
}
