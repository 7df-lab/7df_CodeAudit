// 任务详情（14号 §3.3 ②）。ADR-188：左右两栏——AI 交互日志
// 内联常驻（吸顶），另一侧=任务概况（信息+计量+报告摘要合并首卡）/阶段点轨/执行日志/发现 Tabs。
// 2026-09-22：AI 交互日志默认在右栏；页首"互换左右栏"按钮可切换，
// 偏好持久化（localStorage）。交换经 flex-direction 实现（DOM 顺序不变）——两栏子树
// 不重挂，WS 流/发现列表展开行/滚动位置等运行态零丢失。
// 2026-10-09（用户指令：产出视图被挤到看不到/Agent 段大量空白）：信息栏卡片重设计——
// 首卡去 Descriptions 网格改 inline 元数据条（操作按钮上移 Card extra，消灭按钮行
// 尾部空白与 Agent 行空格）；计量 inline 并入（空态从整卡压成一行）；阶段 Steps 换
// TaskStageRail 单行点轨；执行日志可折叠（终态默认收起）；产出视图 minHeight 320
// 成为信息栏锚点（发现表稳居视口内）。
// 快照供给：WS 推流在线时帧驱动（ADR-188 起 250ms 聚合近实时），断线回退 10s 轮询（终态自停）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { SwapOutlined } from '@ant-design/icons';
import { Alert, Button, Card, Divider, Popconfirm, Space, Tabs, Tag, Tooltip, Typography, message } from 'antd';
import { useEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api, apiErrorMessage, errStatus, getAccessToken, getReportContent, listAgents, listAllProjects, openReportWindow, pollIntervalMs } from '../../api/client';
import { MONO_FONT } from '../../dict/tokens';
import type { ScanTask, TaskLogEntry, TaskGraphNode, TaskSnapshot, UnifiedFinding } from '../../api/types';

import FindingsPage, { isDegradedFinding } from '../findings/FindingsPage';
import FusionView from '../views/FusionView';
import ReviewView from '../views/ReviewView';
import TaskLogPanel, { MAX_LOG_ROWS } from '../../components/TaskLogPanel';
import TaskStageRail from '../../components/TaskStageRail';
import { AgentSwitchModal } from '../../components/AgentSwitchModal';
import AIInteractionLogPanel from '../../components/AIInteractionLogPanel';
import TaskUsageCard from '../../components/TaskUsageCard';
// graph-ui-channels 批3/批4：泳道视图退役（Q7A）——图面板=真图画布
// （G6）占主面板位（图任务），节点弹层盖信息栏半页（Q9B），通道吸收见 absorbSnapshot。
import TaskGraphCanvas from '../../components/TaskGraphCanvas';
import GraphNodePopup from '../../components/GraphNodePopup';
import { absorbChannels, channelQuery, normalizeGranularity, segmentFlatByMarkers, type ChannelMap, type Granularity } from '../../utils/graphChannels';
import { SCAN_MODE, TASK_STATUS, reportFileExt, zh } from '../../dict';
import { STATUS_COLOR } from '../../dict/tokens';

import PageHeader from '../../components/PageHeader';
import { PageLoading } from '../../components/states';
import { actionLabel, allowedActions, dispatchAction, isTerminal, type TaskAction } from '../../tasks/stateMachine';
import { usePageTitle } from '../../hooks/usePageTitle';

// proto bytes（protojson base64）→ utf-8 原文（AI 交互日志增量）。
// B5(P3-b)：解码改流式——服务端日志块按任意字节偏移切（256KB maxBytes），多字节字符
// （中文为主）可跨块边界；每帧独立 decode 会产生 U+FFFD 并随 aiText/下载产物持久化。
// chunk 全部经 absorbSnapshot 单路按游标顺序到达，decoder 实例随组件（=任务）生命周期。
// 左右栏交换偏好持久化键。值白名单 'left'/'right'，
// 其余（含默认/脏值）一律回落新默认 right（AI 交互日志在右）。
const AI_SIDE_KEY = 'codeaudit.task_detail_ai_side';

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

// 任务概况首卡的 inline 元数据项：次级标签+同行值——替代
// Descriptions 网格（3 字段 2 列必有空格，Agent 行右侧大片空白的根因）。
function MetaItem({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div style={{ display: 'flex', alignItems: 'baseline', gap: 6, minWidth: 0 }}>
      <Typography.Text type="secondary" style={{ fontSize: 12, flexShrink: 0 }}>{label}</Typography.Text>
      <div style={{ fontSize: 13, minWidth: 0 }}>{children}</div>
    </div>
  );
}

// 报告初步判断的数字优先统计对（数字 14 加粗在前、次级小标签随后）——原
// Descriptions column=4 的紧凑替代。
function SummaryStat({ label, value }: { label: string; value: number }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'baseline', gap: 4, whiteSpace: 'nowrap' }}>
      <Typography.Text strong style={{ fontSize: 14 }}>{value}</Typography.Text>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>{label}</Typography.Text>
    </span>
  );
}

export default function TaskDetailPage({ taskId }: { taskId: string }) {
  const qc = useQueryClient();

  // ADR-172: WebSocket 推送在线时轮询暂停；ADR-188: 服务端 250ms 聚合推帧（近实时），
  // 断线回退本轮询器（保底语义不变）。日志/AI 游标在 refs 中累进。
  const logAfterRef = useRef('');
  const aiCursorRef = useRef(0);
  const aiDecoderRef = useRef<TextDecoder | null>(null); // B5(P3-b)：流式 UTF-8 解码器（跨块残余字节）
  const [logRows, setLogRows] = useState<TaskLogEntry[]>([]);
  const [aiText, setAiText] = useState('');
  const [aiMeta, setAiMeta] = useState<{ complete: boolean; total: number; granularity?: Granularity }>({ complete: false, total: 0 });
  const wsLiveRef = useRef(false);
  const [wsLive, setWsLive] = useState(false);

  // graph-ui-channels 批3：图任务逐节点通道（游标吸收/保尾）；选中节点=弹层锚
  const channelsRef = useRef<ChannelMap>({});
  const [aiChannels, setAiChannels] = useState<ChannelMap>({});
  const [selectedGraphNode, setSelectedGraphNode] = useState<TaskGraphNode | null>(null);
  // 批4（审查 P2-1/P2-2）：信息栏容器 ref（开弹层滚顶——absolute 弹层锚定滚动原点）
  // 与图面即时刷新句柄（弹层操作成功→立即拉 /graph，不等 5s 轮询拍）。
  const infoColRef = useRef<HTMLDivElement>(null);
  const graphRefreshRef = useRef<() => void>(() => {});
  // Q10：图任务"下载完整日志"——通道按声明序拼接（声明序键=图节点的 seq 声明次序，
  // 由 /graph nodes 数组序承载），旧任务回落段/任务级序言一并入文；batch 复用 a[download]。
  const graphNodesRef = useRef<TaskGraphNode[]>([]);
  const downloadGraphLog = () => {
    const ordered = graphNodesRef.current;
    const parts: string[] = [];
    if (!hasChannels && legacySegments?.preamble) {
      parts.push('══ 任务级序言 ══\n' + legacySegments.preamble);
    }
    for (const n of ordered) {
      const body = nodeBody(n.id);
      if (body) parts.push(`══ 节点会话 g-${n.id}（${n.type} · ${n.status}）══\n${body}`);
    }
    const blob = new Blob([parts.join('\n\n')], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `ai-interaction.${taskId}.channels.log`;
    a.click();
    URL.revokeObjectURL(url);
  };
  useEffect(() => {
    if (selectedGraphNode) infoColRef.current?.scrollTo({ top: 0 });
  }, [selectedGraphNode]);

  // 左右栏交换：AI 交互日志默认在右；按钮切回左。
  // 首读白名单解析（非 'left' 即默认 right），写入随切换同帧完成。
  const [aiSide, setAiSide] = useState<'left' | 'right'>(() =>
    (globalThis.localStorage?.getItem(AI_SIDE_KEY) === 'left' ? 'left' : 'right'));
  const swapColumns = () => {
    setAiSide((prev) => {
      const next = prev === 'right' ? 'left' : 'right';
      globalThis.localStorage?.setItem(AI_SIDE_KEY, next);
      return next;
    });
  };
  const aiRight = aiSide === 'right';

  // 快照增量吸收：轮询响应与 WS 帧（ADR-172 同构 JSON）共用一条路径。
  // log_id 去重 + AI 游标单调：轮询与 WS 游标各自独立（服务端连接游标自订阅位起算），
  // 首帧/重连交叠时此处兜底，杜绝重复行。
  // B4-3（审计修复）保尾上限：超长任务的执行日志/AI 正文此前无界累积（万条日志行/数 MB
  // 文本拖垮标签页）。日志保尾 MAX_LOG_ROWS（1000）条、AI 正文保尾 1M 字符，均留最新侧；
  // 游标不受影响（logAfter/aiCursor 是服务端口径，丢弃的只是客户端已渲染历史）。
  // 完整内容下载入口延后：服务端无日志全量导出端点，暂不做（TaskLogPanel 顶部如实提示
  // 截断）；AI 侧既有"下载完整日志"按钮下载的是保尾后的尾部文本，不另做全量入口。
  const seenLogIdsRef = useRef<Set<string>>(new Set());
  // A5c：终态复活重续臂——auto-retry/handoff 经终态翻转
  // （FAILED→QUEUED→RUNNING）后 WS 已按终态收束不重连，观测降级为 10s 轮询；
  // 检测终态→非终态翻转时递增 wsEpoch 重挂推流（常态 pause/resume 不触发——
  // PAUSED 非终态）。
  const prevStatusRef = useRef<string>('');
  const [wsEpoch, setWsEpoch] = useState(0);
  const MAX_AI_TEXT_CHARS = 1_000_000;
  const absorbSnapshot = (d: TaskSnapshot) => {
    const st = d.task?.status ?? '';
    const prevSt = prevStatusRef.current;
    prevStatusRef.current = st;
    if (prevSt && isTerminal(prevSt) && st && !isTerminal(st)) {
      setWsEpoch((e) => e + 1); // 终态复活：重挂 WS（流式保真恢复）
    }
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
    const chunkB64 = d.ai?.chunk ?? '';
    if (nextCursor > aiCursorRef.current && chunkB64 !== '') {
      // R35/P2-1（增量吸收幂等）：REST 快照在途时 WS 帧可能已推进游标——仅比
      // 块尾挡不住"chunk 起点 < 本地游标"的交叠块（快照按请求时刻游标起读），
      // 整块 append=字节重复进正文。字节级裁掉已交付前缀（decoder 流态恰停在
      // 本地游标处，裁后字节续解语义正确——多字节字符跨界也不劈）。
      const bin = atob(chunkB64);
      const raw = Uint8Array.from(bin, (c) => c.charCodeAt(0));
      const start = nextCursor - raw.length;
      // start<0 = 帧游标与块字节长不符线契约（next_cursor=start+len）——不裁
      // （防坏帧腐坏正文），整块按原语义吸收。
      const bytes = start >= 0 && start < aiCursorRef.current
        ? raw.subarray(aiCursorRef.current - start) : raw;
      aiCursorRef.current = nextCursor;
      aiDecoderRef.current ??= new TextDecoder('utf-8');
      setAiText((prev) => (prev + aiDecoderRef.current!.decode(bytes, { stream: true })).slice(-MAX_AI_TEXT_CHARS));
    }
    // A2：元数据守护——重连首帧常携带零值 ai（无增量可推），
    // 无条件冲刷会把 complete/total 打回 false/0（头部「0 KB」+收束态闪烁+归档
    // 折叠面抖动）。仅在该帧携带任何 ai 信号（增量/收束/总量/粒度）时更新。
    const aiHasSignal = !!d.ai && (
      (d.ai.chunk ?? '') !== '' || !!d.ai.complete
      || Number(d.ai.total_bytes ?? 0) > 0
      || !!normalizeGranularity(d.ai.granularity)
      || !!d.ai.channels?.some((c) => !!c.granularity)
    );
    if (aiHasSignal) {
      setAiMeta({
        complete: !!d.ai?.complete, total: Number(d.ai?.total_bytes ?? 0),
        // b4：主面字段优先；通道帧内声明补位（同条目同方言，取首个非未声明帧）
        granularity: normalizeGranularity(d.ai?.granularity)
          ?? normalizeGranularity(d.ai?.channels?.find((c) => c.granularity)?.granularity),
      });
    }
    // graph-ui-channels 批3：通道增量（游标单调防重放；非图任务恒空=no-op）
    const nextChannels = absorbChannels(channelsRef.current, d.ai?.channels);
    if (nextChannels !== channelsRef.current) {
      channelsRef.current = nextChannels;
      setAiChannels(nextChannels);
    }
  };

  const { data: snap, isError: taskError, error: taskErr, refetch: taskRetry, isFetching: snapFetching } = useQuery({
    queryKey: ['task-snapshot', taskId],
    retry: false, // NotFound 如实终态（内存存储重启清除任务——报告中心可能存在此类旧链）
    queryFn: async () => {
      const r = await api.get(`/v1/tasks/${taskId}/snapshot`, {
        params: {
          ...(logAfterRef.current ? { logs_after: logAfterRef.current } : {}),
          ...(aiCursorRef.current > 0 ? { ai_cursor: aiCursorRef.current } : {}),
          ...(Object.keys(channelsRef.current).length > 0 ? { node_cursor: channelQuery(channelsRef.current) } : {}),
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
          ...(Object.keys(channelsRef.current).length > 0 ? { node_cursor: channelQuery(channelsRef.current) } : {}),
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
      // 风险项实时呈现（切片 7 复批 II）：任务运行中，快照帧节流失效 findings 缓存
      // ——Agent/SAST 每批提交落库后，前端发现列表在下一失效窗内刷新（不再等
      // 任务终态一次性体现）。5s 节流防秒级帧洪泛；终态仍走既有收束路径。
      let findingsInvalidateAt = 0;
      ws.onmessage = (ev: MessageEvent<string>) => {
        lastFrameAt = Date.now();
        try {
          const d = JSON.parse(ev.data) as TaskSnapshot & { type?: string };
          if (d.type !== 'snapshot' || !d.task) return;
          absorbSnapshot(d);
          qc.setQueryData(['task-snapshot', taskId], d);
          if (!isTerminal(d.task.status) && Date.now() - findingsInvalidateAt > 5000) {
            findingsInvalidateAt = Date.now();
            void qc.invalidateQueries({ queryKey: ['findings', taskId] });
          }
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
          // 401 会经 axios 拦截器单飞刷新，下轮重连即用新 token（gw-f6a3523 实证链）。
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
  }, [taskId, wsEpoch]); // wsEpoch：终态复活重续臂（A5c）
  const task: ScanTask | undefined = snap?.task;
  const isTerminalQuery = !!task && isTerminal(task.status);
  const isCompletedTask = !!task && task.status === 'TASK_STATUS_COMPLETED';
  // graph-ui-channels 批3：图任务=图面板占主面板位（Q1/Q2A——互换按钮/偏好键沿用）
  const isGraphTask = !!task && task.config?.execution === 'graph';
  const hasChannels = Object.keys(aiChannels).length > 0;
  // 旧图任务回落：flat 流标记切段（plan §6 审4——无通道纪元的最佳努力）
  const legacySegments = useMemo(
    () => (isGraphTask && !hasChannels && aiText ? segmentFlatByMarkers(aiText) : null),
    [isGraphTask, hasChannels, aiText],
  );

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['task-snapshot', taskId] });
    // B5: 旧键 ['tasks'] 是死失效（任务列表真实键=['tasks-page',…]，前缀失配恒 no-op，
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
  // 通道/回落双路径的节点正文取值（downloadGraphLog 与弹层 text 共用——纯表达式无副作用）
  const nodeBody = (id: string) => (hasChannels ? (aiChannels[id]?.text ?? '') : (legacySegments?.segments[id] ?? ''));

  const [switchOpen, setSwitchOpen] = useState(false); // TP20-T2 切换 Agent 入口
  const act = useMutation({
    mutationFn: async (a: TaskAction) => dispatchAction(task!, a),
    onSuccess: () => {
      message.success('操作已提交');
      invalidate();
    },
    onError: (e) => message.error(`操作被拒绝${errStatus(e) ? `（HTTP ${errStatus(e)}）` : ''}：${apiErrorMessage(e)}`),
  });

  // 降级可见性：AI 降级（RuleScan 兜底）时任务仍 COMPLETED、
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
  // Agent（AI 阶段）实名显示：task.agent 空语义=部署默认——
  // 占位文案改为实际默认 Agent 名（/v1/agents 的 is_default 条目；清单加载失败回落占位）
  const { data: agentsList } = useQuery({
    queryKey: ['agents'],
    queryFn: listAgents,
    staleTime: 60_000,
  });
  const defaultAgentName = task ? (task.agent || agentsList?.agents.find((a) => a.is_default)?.id) : undefined;
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
  // v0.1.2：概况计数改吃实时统计（findings 表两轴桶）。
  // 此前数据源=报告 JSON 快照（生成时点冻结），人工裁决回写后数字永不动——把快照
  // 放进实时视图是口径错位。报告页维持快照口径（两种口径各归其位）；本端点不可用
  // （旧栈 404）时回落报告快照并标注口径。
  const taskStats = useQuery({
    queryKey: ['task-stats', taskId],
    queryFn: async () =>
      (await api.get(`/v1/tasks/${taskId}/stats`)).data as {
        total: number; by_verdict: Record<string, number>;
      },
    enabled: isTerminalQuery && isCompletedTask,
    retry: false,
  });
  // 2026-10-08 报告人性化批：JSON 档改跳站内阅读视图 /reports/:id（审计句+严重度光谱
  // +逐条代码证据），与报告中心同口；HTML 排版成品维持沙箱窗渲染（B3-2 通道不变）
  const navigateToReport = useNavigate();
  const viewReport = () => {
    if (!latestReport) return;
    getReportContent(latestReport.report_id).then(({ format, content }) => {
      if (format === 'json') {
        navigateToReport(`/reports/${latestReport.report_id}`);
        return;
      }
      // 审计 B3-2 纵深防御：HTML 统一经 openReportWindow 的 sandboxed iframe 渲染
      // （脚本全灭，CSP meta 前置双保险，注入收口在 client.ts）
      const opened = openReportWindow(content, 'text/html');
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

  // 页面标题（设计整改 P1）——必须在全部早退 return 之前调用（条件钩子会崩渲染）
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
          margin——设计整改 P1 起由 PageHeader 全站统一归零；右栏视口封顶高度 126 保持 */}
      <PageHeader
        title={<>任务 {task.task_id} <Tag color={STATUS_COLOR[task.status]}>{zh(TASK_STATUS, task.status)}</Tag></>}
        extra={
          <Tooltip title="交换左右两栏位置（AI 交互日志在左/右之间切换，偏好自动记住）">
            <Button icon={<SwapOutlined />} onClick={swapColumns}>互换左右栏</Button>
          </Tooltip>
        }
      />

      {/* ADR-188（人类指令 2026-09-03）：左右两栏——AI 交互日志（50%，吸顶随滚常驻），
          另一侧=其余信息。min-width:0 防 flex 子元素内容把 50% 宽度撑破（长 token/URL 溢出）。
          2026-09-09 布局改版（人类指令）：右侧固定一页高（视口封顶、内部滚动）——任务信息
          精简（去掉重试次数/进度/查看报告/重新生成报告）、阶段时间线横排紧凑、执行日志
          压缩高度、产出视图（发现/融合 Tabs）占满剩余空间并框内滚动，为发现列表让出稳定
          可视面积。
          2026-09-12 布局调整（用户指令）：左栏高度与右栏总高一致（fill 撑满）；任务信息卡
          与报告初步判断合并为右侧首卡；执行日志可视高度放大到 ≥10 行。 */}
      {/* 设计整改 P2: 窄屏（<1200px）双栏折叠为上下堆叠——纯 CSS 媒体查询覆盖，桌面
          （≥1200px）50/50+视口封顶+吸顶布局是 2026-09-09/09-12 人类指令钉死，零变更。
          2026-09-22（人类指令）：AI 交互日志默认在右栏、页首按钮可互换——方向经下面的
          .task-detail-cols 基础规则（源序在媒体查询后，桌面态生效）；媒体查询同步加
          !important（inline 方案会以更高优先级压过 column 破坏窄屏折叠，故弃）。 */}
      <style data-testid="task-detail-layout-style">{`
        @media (max-width: 1199px) {
          .task-detail-cols { flex-direction: column !important; }
          .task-detail-cols > * { width: 100% !important; height: auto !important; }
          /* 批4（审查 P2-1）：position 重置只钉吸顶首列——信息列保持 relative（节点弹层
             绝对定位锚，窄屏堆叠态弹层几何不失控） */
          .task-detail-cols > *:first-child { position: static !important; }
        }
        .task-detail-cols { flex-direction: ${aiRight ? 'row-reverse' : 'row'}; }
        /* 产出视图（2026-09-13 用户报障"两白框中间漏出黑色页面"根治）：原结构=卡体作滚动
           容器+tabBar sticky 吸顶——卡体顶部 padding 形成一条缝隙，滚动时经过的内容（发现
           详情里的深色源码面板/Alert）在缝隙里可见，视觉上"内容从两框之间漏出"。改为
           Tab 条常驻、内容区（content-holder）自滚动：无吸顶即无缝隙。 */
        .output-tabs { flex: 1; min-height: 0; display: flex; flex-direction: column; }
        .output-tabs > .ant-tabs-content-holder { flex: 1; min-height: 0; overflow-y: auto; }
      `}</style>
      <div className="task-detail-cols" style={{ display: 'flex', gap: 16, alignItems: 'flex-start' }}>
        {/* DOM 首栏=主面板（AI 交互日志或图执行画布——列属性 sticky/50% 与内容绑定，
            永不与任务列互换 JSX 位置）；视觉左/右由上方 .task-detail-cols 的
            flex-direction 决定（2026-09-22 互换按钮；图面板继承主面板身份 Q2A）。 */}
        <div style={{ width: '50%', minWidth: 0, position: 'sticky', top: 16, height: 'calc(100vh - 126px)' }}>
          {isGraphTask ? (
            /* graph-ui-channels 批3/4（Q1）：图任务=真图画布占位；通道态经 props 供
               分区汇总/弹层共用；选中节点回调携带节点对象（弹层元数据锚）。 */
            <TaskGraphCanvas
              taskId={taskId}
              running={!isTerminal(task.status)}
              channels={hasChannels ? aiChannels : undefined}
              flatText={aiText}
              flatComplete={aiMeta.complete}
              onSelectNode={setSelectedGraphNode}
              onDownloadLog={downloadGraphLog} /* Q10：3378095 漏传接线——按钮渲染条件 onDownloadLog&& 恒 false=图任务下载入口断链（D1 断言族 G5 抓获） */
              onReady={({ refresh, getNodes }) => {
                graphRefreshRef.current = refresh;
                graphNodesRef.current = getNodes();
              }}
            />
          ) : (
            <AIInteractionLogPanel
              text={aiText}
              totalBytes={aiMeta.total}
              complete={aiMeta.complete}
              onRefresh={taskRetry}
              refreshing={snapFetching}
              live={wsLive}
              degraded={aiDegradedSnapshot}
              agent={task.agent}
              granularity={aiMeta.granularity}
              fill
            />
          )}
        </div>

        <div ref={infoColRef} style={{
          width: '50%', minWidth: 0,
          height: 'calc(100vh - 126px)',
          position: 'relative', /* 批4：节点弹层（Q9B 盖本栏半页）的绝对定位锚 */
          display: 'flex', flexDirection: 'column', gap: 12,
          /* 完成态内容可超出视口（计量/图执行视图/产出视图叠加）——列级纵向滚动
             兜底（2026-09-20 修复：overflow hidden 会把末位"产出视图"整卡裁出
             视口且无滚动可达）。视口封顶口径不变（126px 页头除外）。 */
          overflowY: 'auto',
        }}>
          {/* 任务概况首卡（2026-10-09 重设计）：元数据 inline 条 + 操作按钮在 Card
              extra（原独立按钮行 ~600px 尾部空白根除）+ 计量 inline 并入 + 报告摘要
              数字优先统计对。视口内常驻内容从 ~215px（信息卡+空计量卡）压到 ~120px。 */}
          <Card
            size="small"
            title="任务概况"
            styles={{ body: { padding: '10px 16px 12px' } }}
            extra={
              <Space size="small" wrap>
                {actions.map((a) =>
                  // 交互审计 Top4-①：cancel（取消 RUNNING 任务，不可逆）与 retry 同口径
                  // 加确认——此前全站危险操作唯一漏网（删项目/删 Provider/停用用户均有确认）
                  a === 'retry' || a === 'cancel' ? (
                    <Popconfirm
                      key={a}
                      title={a === 'cancel' ? '确认取消该任务？取消后不可恢复' : '确认人工重试该任务？'}
                      onConfirm={() => act.mutate(a)}
                    >
                      <Button size="small" type={a === 'retry' ? 'primary' : 'default'}>{actionLabel(a)}</Button>
                    </Popconfirm>
                  ) : (
                    <Button key={a} size="small" type={a === 'start' ? 'primary' : 'default'} onClick={() => act.mutate(a)}>
                      {actionLabel(a)}
                    </Button>
                  ),
                )}
                {(task.status === 'TASK_STATUS_RUNNING' ||
                  task.status === 'TASK_STATUS_PAUSED' ||
                  task.status === 'TASK_STATUS_QUEUED') && (
                  /* TP20-T2 切换 Agent：RUNNING=回合边界暂停并切换（pause?agent=）；
                     PAUSED/QUEUED=直接换笔（SetTaskAgent）。入口在页面层判态——
                     ALLOWED_ACTIONS 端点白名单语义不变（计划 §3.1）。 */
                  <Button size="small" onClick={() => setSwitchOpen(true)}>
                    {task.status === 'TASK_STATUS_RUNNING' ? '暂停并切换 Agent' : '切换 Agent'}
                  </Button>
                )}
                {isTerminal(task.status) && task.status === 'TASK_STATUS_COMPLETED' && task.scan_mode === 'SCAN_MODE_COMPARE' && (
                  /* ADR-182 模式相关视图：D→对比（报告入口=下方报告摘要区与报告中心深链） */
                  <Link to={`/tasks/${taskId}/comparison`}><Button size="small">对比视图</Button></Link>
                )}
              </Space>
            }
          >
            <div style={{ display: 'flex', flexWrap: 'wrap', columnGap: 24, rowGap: 4 }}>
              <MetaItem label="项目">
                <Link to={`/projects/${task.project_id}`} title={task.project_id}>{projectName || task.project_id}</Link>
              </MetaItem>
              <MetaItem label="模式">{zh(SCAN_MODE, task.scan_mode)}</MetaItem>
              <MetaItem label="Agent（AI 阶段）">
                <span style={{ fontFamily: MONO_FONT }}>{task.agent || defaultAgentName || '部署默认'}</span>
                {task.agent && task.agent !== 'dsh' && <Tag style={{ marginLeft: 4 }}>{task.agent}</Tag>}
              </MetaItem>
            </div>
            {/* 任务计量（platform-graph-web-exposure §2.2 P0）：inline 并入首卡——
                tokens 合计/分模型/缓存读占比；无用量=诚实空态一行（不独占卡位） */}
            <TaskUsageCard taskId={taskId} variant="inline" active={!isTerminal(task.status)} />
            {task.error_message && (
              <Alert type="error" showIcon style={{ marginTop: 8 }} message={task.error_message} />
            )}
            {/* ADR-150: 复核概况内联（此前需跳报告中心再点在线查看，重复呆板）。
                2026-09-12 并入任务信息首卡；2026-10-09 改数字优先统计对；
                v0.1.2 数据源改实时 stats（裁决回写即联动），旧栈无端点回落报告快照。 */}
            {isTerminalQuery && isCompletedTask && (taskStats.data || reportSummary) && (
              <>
                <Divider style={{ margin: '10px 0 8px' }} />
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 8 }}>
                  <div style={{ display: 'flex', alignItems: 'baseline', flexWrap: 'wrap', columnGap: 16, rowGap: 4 }}>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {taskStats.data ? '复核概况（实时）' : '报告初步判断（快照）'}
                    </Typography.Text>
                    {taskStats.data ? (
                      <>
                        <SummaryStat label="发现总数" value={taskStats.data.total ?? 0} />
                        <SummaryStat label="确认为真" value={taskStats.data.by_verdict?.TRUE_DEFINITE ?? 0} />
                        <SummaryStat label="误报" value={taskStats.data.by_verdict?.FALSE_DEFINITE ?? 0} />
                        <SummaryStat label="未复核" value={taskStats.data.by_verdict?.NEEDS_REVIEW ?? 0} />
                      </>
                    ) : (
                      <>
                        <SummaryStat label="发现总数" value={reportSummary?.total_findings ?? 0} />
                        <SummaryStat label="确认为真" value={reportSummary?.true_positives ?? 0} />
                        <SummaryStat label="误报" value={reportSummary?.false_positives ?? 0} />
                        <SummaryStat label="未复核" value={reportSummary?.not_reviewed ?? 0} />
                      </>
                    )}
                  </div>
                  <Space size="small">
                    <Button size="small" onClick={viewReport}>在线查看完整报告</Button>
                    <Button size="small" onClick={downloadReport}>下载</Button>
                    <Link to="/reports">报告中心</Link>
                  </Space>
                </div>
              </>
            )}
          </Card>

          <DegradedNotice taskId={taskId} />

          {/* 阶段时间线（2026-10-09 重设计）：antd horizontal Steps → TaskStageRail
              单行点轨——状态语义不变（含 SKIPPED"已跳过"诚实副行与降级/错误标注），
              占高 ~140px → ~60px，为下方产出视图让位。 */}
          <Card size="small" title="阶段时间线" styles={{ body: { padding: '8px 16px' } }}>
            <TaskStageRail stages={task.stages ?? []} />
          </Card>

          {/* graph-ui-channels 批4（Q7A）：旧泳道视图退役——图执行视图迁主面板位
              （真图画布）；本栏不再挂图卡（阶段时间线与执行日志之间留白归零）。 */}

          {/* ADR-167/170/172: 执行日志——快照轮询或 WS 推流（live 徽标）增量下发；
              maxHeight 250 承 2026-09-12 用户指令（≥10 行）不变。2026-10-09 重设计：
              可折叠——终态默认收起（错误/警告徽标常驻头部，可见性不丢），为产出视图
              让位；运行态保持展开。 */}
          <TaskLogPanel logs={logRows} terminal={isTerminal(task.status)} onRefresh={taskRetry} refreshing={snapFetching} live={wsLive} maxHeight={250} collapsible defaultCollapsed={isTerminal(task.status)} />

          {isTerminal(task.status) && (
            <Card size="small" title="产出视图" style={{
              flex: 1, minHeight: 320, /* 2026-10-09：120→320——信息栏锚点，发现表稳居视口 */
              display: 'flex', flexDirection: 'column', overflow: 'hidden',
            }} styles={{ body: {
              flex: 1, minHeight: 0,
              display: 'flex', flexDirection: 'column', overflow: 'hidden',
              /* 上下留白收进内部：Tab 条常驻贴顶（原卡体自滚动+sticky 吸顶结构已废，见 style 注） */
              padding: '0 16px 12px',
            } }}>
              <Tabs
                className="output-tabs"
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
          {/* graph-ui-channels 批4（Q9B）：节点弹层盖本栏半页——内容=该节点通道实时流
              （旧任务=标记切段回落）；操作区=注入/认领/释放（迁自泳道）。 */}
          {isGraphTask && selectedGraphNode && (
            <GraphNodePopup
              node={selectedGraphNode}
              taskId={taskId}
              text={nodeBody(selectedGraphNode.id)}
              complete={hasChannels ? (aiChannels[selectedGraphNode.id]?.complete ?? false) : aiMeta.complete}
              legacy={!hasChannels}
              granularity={
                (hasChannels
                  ? Object.values(aiChannels).find((c) => c.granularity)?.granularity
                  : aiMeta.granularity)
                  // 契约未声明（盘回落档/旧后端）=回落 agent 判别过渡案（b4 后退役路径）
                  ?? (task.agent && task.agent !== 'dsh' ? 'turn' : 'token')
              }
              onClose={() => setSelectedGraphNode(null)}
              onChanged={() => graphRefreshRef.current()}
            />
          )}
        </div>
      </div>

      {/* TP20-T2 切换 Agent Modal：清单=/v1/agents（同创建页源）；当前项禁用+「当前」；
          能力摘要四行如实；失败=整体禁用+错误说明（不回落硬编码） */}
      <AgentSwitchModal
        open={switchOpen}
        taskId={taskId}
        mode={task.status === 'TASK_STATUS_RUNNING' ? 'pause-switch' : 'switch'}
        currentAgent={task.agent || ''}
        onClose={() => setSwitchOpen(false)}
        onDone={invalidate}
      />
    </div>
  );
}
