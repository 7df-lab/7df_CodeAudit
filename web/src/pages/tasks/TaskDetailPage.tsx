// 任务详情（14号 §3.3 ②）。ADR-188：左右两栏——左=AI 交互日志
// 内联常驻（吸顶），右=任务信息+报告摘要（合并首卡）/阶段时间线/执行日志/发现 Tabs。
// 快照供给：WS 推流在线时帧驱动（ADR-188 起 250ms 聚合近实时），断线回退 10s 轮询（终态自停）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Alert, Button, Card, Descriptions, Divider, Popconfirm, Space, Steps, Tabs, Tag, Typography, message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { api, getAccessToken, getReportContent, listAllProjects, openReportWindow, pollIntervalMs } from '../../api/client';
import type { ScanTask, TaskLogEntry, TaskSnapshot, TaskStage, UnifiedFinding } from '../../api/types';

import FindingsPage, { isDegradedFinding } from '../findings/FindingsPage';
import FusionView from '../views/FusionView';
import ReviewView from '../views/ReviewView';
import TaskLogPanel, { MAX_LOG_ROWS } from '../../components/TaskLogPanel';
import AIInteractionLogPanel from '../../components/AIInteractionLogPanel';
import { SCAN_MODE, STAGE_TYPE, TASK_STATUS, reportFileExt, zh } from '../../dict';
import { STATUS_COLOR } from '../../dict/tokens';
import PageHeader from '../../components/PageHeader';
import { PageLoading } from '../../components/states';
import { actionLabel, allowedActions, dispatchAction, isTerminal, type TaskAction } from '../../tasks/stateMachine';
import { usePageTitle } from '../../hooks/usePageTitle';

// proto bytes（protojson base64）→ utf-8 原文（AI 交互日志增量）。
// (P3-b)：解码改流式——服务端日志块按任意字节偏移切（256KB maxBytes），多字节字符
// （中文为主）可跨块边界；每帧独立 decode 会产生 U+FFFD 并随 aiText/下载产物持久化。
// chunk 全部经 absorbSnapshot 单路按游标顺序到达，decoder 实例随组件（=任务）生命周期。
function decodeAiChunk(decoder: TextDecoder, b64: string): string {
  if (!b64) return '';
  const bin = atob(b64);
  const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
  return decoder.decode(bytes, { stream: true });
}

const STEP_STATUS: Record<string, 'wait' | 'process' | 'finish' | 'error'> = {
  STAGE_STATUS_PENDING: 'wait',
  STAGE_STATUS_RUNNING: 'process',
  STAGE_STATUS_COMPLETED: 'finish',
  STAGE_STATUS_FAILED: 'error',
  STAGE_STATUS_SKIPPED: 'finish',
};

// proto Timestamp（protojson RFC3339 字符串）→ 本地 hh:mm:ss
function hhmmss(ts: string | null | undefined): string {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d.getTime())) return '';
  return d.toLocaleTimeString('zh-CN', { hour12: false });
}

// 阶段时间（ADR-181）：RUNNING 显示开始时刻（进行中的中间态），终态显示起止与耗时
function stageTimeText(st: TaskStage): string {
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

// 降级警示（R56）：隔离订阅 findings 缓存——缓存更新仅重渲染本组件，
// 不连带发现表格（展开行定位稳定性）。
function DegradedNotice({ taskId }: { taskId: string }) {
  const findingsCache = useQuery({
    queryKey: ['findings', taskId],
    enabled: false,
    queryFn: (): { pages: { findings: UnifiedFinding[] }[] } => ({ pages: [] }),
  });
  const degraded = (findingsCache.data?.pages ?? []).some((pg) => (pg.findings ?? []).some(isDegradedFinding));
  if (!degraded) return null;
  return (
    <Alert
      type="warning"
      showIcon
      message="AI 推理未生效——沙箱不可达，已由内置规则引擎（RuleScan）兜底，全部发现标记为需人工复核"
    />
  );
}

export default function TaskDetailPage({ taskId }: { taskId: string }) {
  const qc = useQueryClient();

  // ADR-172: WebSocket 推送在线时轮询暂停；ADR-188: 服务端 250ms 聚合推帧（近实时），
  // 断线回退本轮询器（保底语义不变）。日志/AI 游标在 refs 中累进。
  const logAfterRef = useRef('');
  const aiCursorRef = useRef(0);
  const aiDecoderRef = useRef<TextDecoder | null>(null); // (P3-b)：流式 UTF-8 解码器（跨块残余字节）
  const [logRows, setLogRows] = useState<TaskLogEntry[]>([]);
  const [aiText, setAiText] = useState('');
  const [aiMeta, setAiMeta] = useState({ complete: false, total: 0 });
  const wsLiveRef = useRef(false);
  const [wsLive, setWsLive] = useState(false);

  // 快照增量吸收：轮询响应与 WS 帧（ADR-172 同构 JSON）共用一条路径。
  // log_id 去重 + AI 游标单调：轮询与 WS 游标各自独立（服务端连接游标自订阅位起算），
  // 首帧/重连交叠时此处兜底，杜绝重复行。
  // （审计修复）保尾上限：超长任务的执行日志/AI 正文此前无界累积（万条日志行/数 MB
  // 文本拖垮标签页）。日志保尾 MAX_LOG_ROWS（1000）条、AI 正文保尾 1M 字符，均留最新侧；
  // 游标不受影响（logAfter/aiCursor 是服务端口径，丢弃的只是客户端已渲染历史）。
  // 完整内容下载入口延后：服务端无日志全量导出端点，暂不做（TaskLogPanel 顶部如实提示
  // 截断）；AI 侧既有"下载完整日志"按钮下载的是保尾后的尾部文本，不另做全量入口。
  const seenLogIdsRef = useRef<Set<string>>(new Set());
  const MAX_AI_TEXT_CHARS = 1_000_000;
  const absorbSnapshot = (d: TaskSnapshot) => {
    const newLogs = (d.logs?.logs ?? []).filter((l) => !seenLogIdsRef.current.has(l.log_id));
    if (newLogs.length > 0) {
      for (const l of newLogs) seenLogIdsRef.current.add(l.log_id);
      logAfterRef.current = newLogs[newLogs.length - 1].log_id;
      setLogRows((prev) => {
        const next = [...prev, ...newLogs].slice(-MAX_LOG_ROWS);
        // 去重集同步收敛到窗口内 id——防 Set 随任务时长无界增长（保尾的另一半）
        if (next.length === MAX_LOG_ROWS) {
          seenLogIdsRef.current = new Set(next.map((l) => l.log_id));
        }
        return next;
      });
    }
    const nextCursor = Number(d.ai?.next_cursor ?? 0);
    if (nextCursor > aiCursorRef.current && (d.ai?.chunk ?? '') !== '') {
      aiCursorRef.current = nextCursor;
      aiDecoderRef.current ??= new TextDecoder('utf-8');
      setAiText((prev) => (prev + decodeAiChunk(aiDecoderRef.current!, d.ai!.chunk)).slice(-MAX_AI_TEXT_CHARS));
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
      return pollIntervalMs(10_000); // WS 断线回退 10s/次（限流余量进一步扩大）
    },
  });

  // ADR-172: WebSocket 推送——ADR-188 起服务端 250ms 聚合推帧（近实时，网关 /v1/tasks/{id}/ws），
  // 帧到达即入缓存/增量面板；断线回退轮询并每 5s 重连，任务终态收束后不再重连。
  useEffect(() => {
    let closed = false;
    let retryTimer: number | undefined;
    let settled = false;
    let ws: WebSocket | undefined; // 提到 effect 作用域：卸载时才能关闭连接
    // 半开看门狗：TCP 静默断链时浏览器不触发 onclose，wsLive 恒真 → 轮询永久停用、
    // 页面冻结到手动刷新。45s 无任何数据帧且任务未收束 → 强制 close，走 onclose 的
    // 快照兜底 + 5s 重连（服务端 20s 无条件 ping 浏览器不可见，客户端只能以数据帧
    // 有无判活；静默期误杀的代价只是一次快照拉取+重连）。
    let lastFrameAt = Date.now();
    const watchdog = window.setInterval(() => {
      if (closed || settled) return;
      const sock = ws;
      if (!sock || sock.readyState !== WebSocket.OPEN) return;
      if (Date.now() - lastFrameAt > 45_000) sock.close();
    }, 5000);
    const connect = () => {
      if (closed || settled) return;
      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
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
      const socket = ws;
      socket.onopen = () => {
        lastFrameAt = Date.now(); // 新连接重置看门狗窗口
        if (!closed) {
          wsLiveRef.current = true;
          setWsLive(true);
        }
      };
      ws.onmessage = (ev: MessageEvent<string>) => {
        lastFrameAt = Date.now();
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
            socket.close();
          }
        } catch {
          /* 单帧异常不致断流 */
        }
      };
      socket.onclose = () => {
        wsLiveRef.current = false;
        if (!closed) setWsLive(false);
        if (!closed && !settled) {
          // 断线窗口即时回填：重连前后端游标已越过帧的内容只能经快照兜底，
          // 此前等 5s 重连（或最坏 10s 轮询拍）——长任务中途断流即观测空白；
          // 若 access token 已过期（WS 在线期无 REST 调用无续期），本次快照
          // 401 会经 axios 拦截器单飞刷新，下轮重连即用新 token（实证链）。
          void qc.refetchQueries({ queryKey: ['task-snapshot', taskId] });
          retryTimer = window.setTimeout(connect, 5000);
        }
      };
      socket.onerror = () => socket.close();
    };
    connect();
    return () => {
      closed = true;
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
      window.clearInterval(watchdog);
      // 离开页面必须断开推流——此前只置标志不 close，连接留在原地持续收 250ms 帧，
      // 旧挂载的 onmessage 继续写查询缓存，反复进出任务页连接累积（泄漏）
      ws?.close();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskId]);
  const task: ScanTask | undefined = snap?.task;
  const isTerminalQuery = !!task && isTerminal(task.status);
  const isCompletedTask = !!task && task.status === 'TASK_STATUS_COMPLETED';

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['task-snapshot', taskId] });
    // 旧键 ['tasks'] 是死失效（任务列表真实键=['tasks-page',…]，前缀失配恒 no-op，
    // 恰是 G-02b 封的"死键失效"模式新形态）；列表 staleTime=0 靠重挂载自愈才未显形。
    qc.invalidateQueries({ queryKey: ['tasks-page'] });
  };

  // 收束即补拉：发现/融合/审核列表在"终态帧早于发现落库"的异常序列（如长任务被
  // 对账器误判超时后阶段仍收敛）下会以空列表入缓存且不再触发——终态+AI 收束帧
  // 到达时统一失效一次，晚到数据不再需要手动刷新页面。
  const settled = isTerminalQuery && !!snap?.ai?.complete;
  const settledInvalidatedRef = useRef(false);
  useEffect(() => {
    if (!settled || settledInvalidatedRef.current) return;
    settledInvalidatedRef.current = true;
    qc.invalidateQueries({ queryKey: ['findings', taskId] });
    qc.invalidateQueries({ queryKey: ['fusion-findings', taskId] });
    qc.invalidateQueries({ queryKey: ['review-findings', taskId] });
  }, [settled, taskId, qc]);

  const act = useMutation({
    mutationFn: async (a: TaskAction) => dispatchAction(task!, a),
    onSuccess: () => {
      message.success('操作已提交');
      invalidate();
    },
    onError: (e) => message.error(`操作被拒绝：${(e as Error).message}`),
  });

  // 降级可见性（2026-09-11 用户报障）：AI 降级（RuleScan 兜底）时任务仍 COMPLETED、
  // 阶段时间线绿色对勾，用户无从得知 AI 推理未生效。此处 enabled:false 只订阅
  // ['findings', taskId] 缓存（数据由内嵌 FindingsPage 拉取，零重复请求），发现级
  // 降级痕迹警示已下沉到 <DegradedNotice>（隔离订阅）——findings 缓存更新不再
  // 连带本页（含发现表格/展开行）重渲染，定位器/滚动位置保持稳定（R56 报障修复）。
  // 面板空态归因用非订阅快照（挂载时点读一次，不建立缓存依赖）。
  const aiDegradedSnapshot = (() => {
    const cached = qc.getQueryData<{ pages: { findings: UnifiedFinding[] }[] }>(['findings', taskId]);
    return (cached?.pages ?? []).some((pg) => (pg.findings ?? []).some(isDegradedFinding));
  })();

  // ADR-150: 报告初步判断内联——拉取本任务最新报告并解析 summary，不再绕行报告中心
  const { data: taskReports } = useQuery({
    queryKey: ['task-reports', taskId],
    enabled: isCompletedTask,
    queryFn: async () => (await api.get('/v1/reports', { params: { task_id: taskId } })).data as {
      reports: { report_id: string; format: string }[]; // B5-P1-1: 枚举名字符串（非数值）
    },
  });
  // 2026-09-09 GUI 评审: 头部信息卡显示项目名称（而非裸项目 ID）
  const { data: projectsIndex } = useQuery({
    queryKey: ['projects-index'],
    queryFn: async () => ({ projects: await listAllProjects() }), // B5-P2-7: 全量翻页（200 被服务端钳 100）
    staleTime: 60_000,
  });
  const projectName = projectsIndex?.projects.find((p) => p.project_id === task?.project_id)?.name;
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
      // 纵深防御：报告内容统一经 openReportWindow 的
      // sandboxed iframe 渲染（脚本全灭，CSP meta 前置双保险，注入统一收口在 client.ts）；
      // JSON 分支保持转义 <pre> 文本。
      // (P3-a)：弹窗被拦（异步后 user activation 失效）——显式提示而非静默
      const opened = format === 'html'
        ? openReportWindow(content, 'text/html')
        : openReportWindow('<pre style="font-size:13px;white-space:pre-wrap">' +
          JSON.stringify(JSON.parse(content), null, 2).replace(/[<>&]/g,
            (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;' }[c] || c)) + '</pre>', 'text/html');
      if (!opened) message.warning('弹出窗口被浏览器拦截，请允许弹出窗口后重试');
    }).catch(() => message.error('打开失败'));
  };
  const downloadReport = async () => {
    if (!latestReport) return;
    try {
      const resp = await api.get(`/v1/reports/${latestReport.report_id}/download`, { responseType: 'blob' });
      const url = URL.createObjectURL(resp.data as Blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `${latestReport.report_id}.${reportFileExt(latestReport.format)}`; // 按格式给扩展名（此前恒 .bin）
      a.click();
      URL.revokeObjectURL(url);
    } catch {
      message.error('下载失败');
    }
  };

  // 页面标题——必须在全部早退 return 之前调用（条件钩子会崩渲染）
  usePageTitle(task ? `任务 ${task.task_id.slice(-6)}` : '任务详情');

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
        description="报告中心的旧条目可能指向已清理的任务——报告文件本身仍在。"
        action={<Button onClick={() => window.history.back()}>返回</Button>} />
    );
  }
  if (!task) return <PageLoading />;
  const actions = allowedActions(task.status);

  return (
    <div>
      {/* 2026-09-12 间距修复（人类反馈"导航栏与内容间空白太多"）曾在此手工置零 Title
          margin—— 起由 PageHeader 全站统一归零；右栏视口封顶高度 126 保持 */}
      <PageHeader
        title={<>任务 {task.task_id} <Tag color={STATUS_COLOR[task.status]}>{zh(TASK_STATUS, task.status)}</Tag></>}
      />

      {/* ADR-188：左右两栏——左=AI 交互日志（50%，吸顶随滚常驻），
          右=其余信息。min-width:0 防 flex 子元素内容把 50% 宽度撑破（长 token/URL 溢出）。
          布局改版：右侧固定一页高（视口封顶、内部滚动）——任务信息
          精简（去掉重试次数/进度/查看报告/重新生成报告）、阶段时间线横排紧凑、执行日志
          压缩高度、产出视图（发现/融合 Tabs）占满剩余空间并框内滚动，为发现列表让出稳定
          可视面积。
          2026-09-12 布局调整（用户指令）：左栏高度与右栏总高一致（fill 撑满）；任务信息卡
          与报告初步判断合并为右侧首卡；执行日志可视高度放大到 ≥10 行。 */}
      {/*  窄屏（<1200px）双栏折叠为上下堆叠——纯 CSS 媒体查询覆盖，桌面
          （≥1200px）50/50+视口封顶+吸顶布局已定版，零变更 */}
      <style>{`
        @media (max-width: 1199px) {
          .task-detail-cols { flex-direction: column; }
          .task-detail-cols > * { width: 100% !important; height: auto !important; position: static !important; }
        }
      `}</style>
      <div className="task-detail-cols" style={{ display: 'flex', gap: 16, alignItems: 'flex-start' }}>
        <div style={{ width: '50%', minWidth: 0, position: 'sticky', top: 16, height: 'calc(100vh - 126px)' }}>
          {/* ADR-168/170/172/188: AI 交互日志——人性化渲染流增量下发；终态=最终交互日志（可下载）。
              内联时间线为主视图（不再默认折叠），整页 Modal 为辅入口（组件内）。 */}
          <AIInteractionLogPanel
            text={aiText}
            totalBytes={aiMeta.total}
            complete={aiMeta.complete}
            onRefresh={taskRetry}
            refreshing={snapFetching}
            live={wsLive}
            degraded={aiDegradedSnapshot}
            fill
          />
        </div>

        <div style={{
          width: '50%', minWidth: 0,
          height: 'calc(100vh - 126px)',
          display: 'flex', flexDirection: 'column', gap: 12,
          overflow: 'hidden',
        }}>
          <Card size="small">
            <Descriptions column={2} size="small">
              <Descriptions.Item label="项目">
                <Link to={`/projects/${task.project_id}`} title={task.project_id}>{projectName || task.project_id}</Link>
              </Descriptions.Item>
              <Descriptions.Item label="模式">{zh(SCAN_MODE, task.scan_mode)}</Descriptions.Item>
            </Descriptions>
            {task.error_message && (
              <Alert type="error" showIcon style={{ marginTop: 8 }} message={task.error_message} />
            )}
            <Space style={{ marginTop: 8 }} wrap>
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
              {isTerminal(task.status) && task.status === 'TASK_STATUS_COMPLETED' && task.scan_mode === 'SCAN_MODE_COMPARE' && (
                /* ADR-182 模式相关视图：D→对比（报告入口=下方报告摘要区与报告中心深链） */
                <Link to={`/tasks/${taskId}/comparison`}><Button>对比视图</Button></Link>
              )}
            </Space>
            {/* ADR-150: 报告初步判断内联（此前需跳报告中心再点在线查看，重复呆板）。
                2026-09-12 布局调整（用户指令）：并入任务信息卡成为右侧首卡（原独立卡取消）——
                项目/模式与最新报告摘要一目了然。 */}
            {isTerminalQuery && isCompletedTask && reportSummary && (
              <>
                <Divider style={{ margin: '12px 0 8px' }} />
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 8, marginBottom: 4 }}>
                  <Typography.Text strong>报告初步判断（最新报告摘要）</Typography.Text>
                  <Space>
                    <Button size="small" onClick={viewReport}>在线查看完整报告</Button>
                    <Button size="small" onClick={downloadReport}>下载</Button>
                    <Link to="/reports">报告中心</Link>
                  </Space>
                </div>
                <Descriptions column={4} size="small">
                  <Descriptions.Item label="发现总数">{reportSummary.total_findings ?? 0}</Descriptions.Item>
                  <Descriptions.Item label="确认为真">{reportSummary.true_positives ?? 0}</Descriptions.Item>
                  <Descriptions.Item label="误报">{reportSummary.false_positives ?? 0}</Descriptions.Item>
                  <Descriptions.Item label="未复核">{reportSummary.not_reviewed ?? 0}</Descriptions.Item>
                </Descriptions>
              </>
            )}
          </Card>

          <DegradedNotice taskId={taskId} />

          <Card size="small" title="阶段时间线">
            {task.stages?.length ? (
              <Steps
                direction="horizontal"
                size="small"
                items={task.stages.map((st) => ({
                  title: zh(STAGE_TYPE, st.type),
                  status: STEP_STATUS[st.status] ?? 'wait',
                  description: (
                    <>
                      {stageTimeText(st) && (
                        <Typography.Text type="secondary" style={{ fontSize: 12, whiteSpace: 'nowrap' }}>
                          {stageTimeText(st)}
                        </Typography.Text>
                      )}
                      {/* 引擎侧阶段 metadata 降级标志（兼容缺省：字段未透出则不渲染） */}
                      {st.metadata?.degraded === 'true' && (
                        <Typography.Text type="warning" style={{ fontSize: 12, display: 'block' }}>
                          （已降级·RuleScan）
                        </Typography.Text>
                      )}
                      {st.error_message && (
                        <Typography.Text type="danger" style={{ fontSize: 12, display: 'block' }}>
                          {st.error_message}
                        </Typography.Text>
                      )}
                    </>
                  ),
                }))}
              />
            ) : (
              <Typography.Text type="secondary">任务尚未启动（阶段在 StartTask 时注册）</Typography.Text>
            )}
          </Card>

          {/* ADR-167/170/172: 执行日志——快照轮询或 WS 推流（live 徽标）增量下发；
              2026-09-12（用户指令）可视高度放大：行高 20px+上下 padding 24px，
              maxHeight 250 保证 ≥10 行日志同时可见（原 150 仅 ~6 行） */}
          <TaskLogPanel logs={logRows} terminal={isTerminal(task.status)} onRefresh={taskRetry} refreshing={snapFetching} live={wsLive} maxHeight={250} />

          {isTerminal(task.status) && (
            <Card size="small" title="产出视图" style={{
              flex: 1, minHeight: 120,
              display: 'flex', flexDirection: 'column', overflow: 'hidden',
            }} styles={{ body: {
              flex: 1, minHeight: 0, overflowY: 'auto',
              display: 'flex', flexDirection: 'column',
            } }}>
              <Tabs
                tabBarStyle={{ position: 'sticky', top: 0, zIndex: 1, background: '#fff', marginBottom: 8 }}
                items={[
                  { key: 'findings', label: '发现', children: <FindingsPage taskId={task.task_id} /> },
                  // ADR-186: 融合视图=产出融合去重清单的模式（C 并行融合 / A 纯SAST 去重合并 / D AI增强SAST 验证后融合 / 旧B 历史兼容）
                  ...(task.scan_mode === 'SCAN_MODE_PARALLEL' ||
                     task.scan_mode === 'SCAN_MODE_SAST_ONLY' ||
                     task.scan_mode === 'SCAN_MODE_AI_ENHANCED_SAST' ||
                     task.scan_mode === 'SCAN_MODE_TRADITIONAL_FIRST'
                    ? [{ key: 'fusion', label: '融合视图', children: <FusionView taskId={task.task_id} /> }]
                    : []),
                  ...(task.scan_mode === 'SCAN_MODE_SAST_REVIEW'
                    ? [{ key: 'review', label: '审核视图（旧模式D）', children: <ReviewView taskId={task.task_id} /> }]
                    : []),
                ]}
              />
            </Card>
          )}
        </div>
      </div>

    </div>
  );
}
