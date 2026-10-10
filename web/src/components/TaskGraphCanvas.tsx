// TaskGraphCanvas — 图执行视图 G6 画布（graph-ui-channels 批3，2026-09-23；
// Q3 裁决=@antv/G6 v5；plan §2 批3）。设计立场翻转记录：2026-09-22 推翻
// 本视图 v1 的"拒绝 DAG 气泡图"立场（platform-graph-web-exposure.md 同批回写）。
//   - 真图：边=节点 from[] 真实拓扑连线；布局=computeLRLayout 纯函数（层=最长依赖
//     路径，层内纵排；自研而非 G6 布局引擎——确定性/可单测/位置可持久化合并）；
//   - 交互：拖节点/滚轮缩放/画布平移内建 behaviors；>15 节点挂 minimap；
//   - 位置持久化：localStorage 按任务（graphLayoutKey），已手摆原样保留、新节点
//     自动落位（computeLRLayout）；拖拽结束落盘；
//   - 节点视觉：状态色描边（GRAPH_NODE_STATUS_COLOR SSOT）+类型角标+attempts 阶梯
//     +holder 徽标+计划批次角标（Q5A 增强）+选中高亮；
//   - 空态纪律：无节点整块不渲染（非图任务由调用方不挂载）。
// jsdom 无 canvas：本组件 G6 交互面经 mock 测试+GUI 门禁（批5）实证。
import { DownloadOutlined, InboxOutlined, ZoomInOutlined, ZoomOutOutlined, ExpandOutlined } from '@ant-design/icons';
import { Button, Card, Modal, Tag, Tooltip, Typography, message } from 'antd';
import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api/client';
import type { TaskGraph, TaskGraphNode } from '../api/types';
import { GRAPH_NODE_STATUS_COLOR, GRAPH_NODE_STATUS_TEXT, GRAPH_NODE_TYPE_COLOR, MONO_FONT } from '../dict/tokens';
import { batchIndexOf, segmentFlatByMarkers } from '../utils/graphChannels';
import { parseTimeline, TimelineBody } from './aiTimeline';
const G6View = lazy(() => import('./G6View'));

export default function TaskGraphCanvas({ taskId, running, channels, flatText, flatComplete, onSelectNode, onReady, onDownloadLog }: {
  taskId: string;
  running: boolean;
  /** 通道态（批3 吸收后的 per-node 流；旧任务 undefined=回落标记切段） */
  channels?: Record<string, { text: string; complete: boolean }>;
  /** 旧任务 flat 文本（无通道纪元的图任务——标记切段回落） */
  flatText?: string;
  flatComplete?: boolean;
  /** 选中变化回调（弹层由 TaskDetailPage 挂载——Q9B 盖信息栏半页）；携带节点
   * 对象（弹层元数据锚）；图数据刷新时对已选节点重发（状态/attempts 实时）。 */
  onSelectNode?: (node: TaskGraphNode | null) => void;
  /** 暴露图面即时刷新（审查 P2-2：弹层操作成功→立即拉 /graph，不等 5s 拍）与
   *  最新图节点序（Q10 下载拼接的声明序排序源——/graph nodes 数组序=seq 因果序）。 */
  onReady?: (api: { refresh: () => void; getNodes: () => TaskGraphNode[] }) => void;
  /** Q10（裁决保留项）：图任务的"下载完整日志"——通道按声明序拼接（含旧任务
   *  回落段与任务级序言），工具栏入口。 */
  onDownloadLog?: () => void;
}) {
  const [selected, setSelected] = useState<string | null>(null);
  const { data, refetch } = useGraphQuery(taskId, running);
  const [zonedOpen, setZonedOpen] = useState(false);
  const g6ApiRef = useRef<{ fitView: () => void; zoomBy: (f: number) => void } | null>(null);

  const nodes = data?.nodes ?? [];
  const nodesRef = useRef(nodes);
  nodesRef.current = nodes;

  const select = useCallback((id: string | null) => {
    setSelected(id);
    onSelectNode?.(id ? (nodes.find((n) => n.id === id) ?? null) : null);
  }, [onSelectNode, nodes]);
  useEffect(() => {
    onReady?.({
      refresh: () => void refetch(),
      getNodes: () => nodesRef.current, // ref 始终指向最新渲染的节点序（Q10 排序源）
    });
  }, [onReady, refetch]);

  // 刷新时对已选节点重发（弹层拿到最新状态/holder/attempts）
  useEffect(() => {
    if (selected && data) {
      const fresh = data.nodes.find((n) => n.id === selected);
      if (fresh) onSelectNode?.(fresh);
    }
  }, [data, selected, onSelectNode]);

  const triggers = useMemo(() => data?.plan_triggers ?? [], [data]);
  const counts = useMemo(() => {
    const c = { open: 0, running: 0, done: 0 };
    for (const n of nodes) {
      if (n.status === 'declared') c.open += 1;
      else if (n.status === 'running') c.running += 1;
      else c.done += 1;
    }
    return c;
  }, [nodes]);

  if (!data) return null; // 图面未达：整块不渲染（非图任务由页面不挂载）
  if (nodes.length === 0) {
    // 图任务零节点（规划未始/事件未达）：诚实空态卡占主面板位（Q1——AI 面板位已让渡）
    return (
      <Card size="small" title="图执行视图" styles={{ body: { padding: 16 } }}>
        <Typography.Text type="secondary">图规划中——首个节点声明后呈现拓扑画布。</Typography.Text>
      </Card>
    );
  }

  const headRight = data.converged
    ? '已收敛'
    : `未决 ${counts.open} · 运行 ${counts.running} · 已结 ${counts.done}`;

  return (
    <Card
      size="small"
      title={<span style={{ fontSize: 18 }}>图执行视图 <Typography.Text type="secondary" style={{ fontSize: 14, fontWeight: 400 }}>（节点=真实拓扑连线，可拖动/缩放/平移）</Typography.Text></span>}
      extra={
        <span style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>{headRight}</Typography.Text>
          <Tooltip title="放大">
            <Button size="small" icon={<ZoomInOutlined />} onClick={() => g6ApiRef.current?.zoomBy(1.2)} />
          </Tooltip>
          <Tooltip title="缩小">
            <Button size="small" icon={<ZoomOutOutlined />} onClick={() => g6ApiRef.current?.zoomBy(1 / 1.2)} />
          </Tooltip>
          <Tooltip title="适配视口">
            <Button size="small" icon={<ExpandOutlined />} onClick={() => g6ApiRef.current?.fitView()} />
          </Tooltip>
          <Tooltip title="分区汇总时间线（计划批次带内按声明序，每节点一区域）">
            <Button size="small" icon={<InboxOutlined />} onClick={() => setZonedOpen(true)}>分区汇总</Button>
          </Tooltip>
          {onDownloadLog && (
            <Tooltip title="下载完整日志（通道按声明序拼接；旧任务=flat 全文）">
              <Button size="small" icon={<DownloadOutlined />} onClick={onDownloadLog} disabled={nodes.length === 0}>下载完整日志</Button>
            </Tooltip>
          )}
        </span>
      }
      styles={{ body: { padding: 8 } }}
    >
      {/* Q5A 批次图例（卡头）：计划批次角标的可读性锚（batchIndexOf 语义同分区 Modal） */}
      {triggers.length > 0 && (
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 6 }}>
          <Typography.Text type="secondary" style={{ fontSize: 11 }}>计划批次：</Typography.Text>
          {triggers.map((tr, i) => (
            <Tooltip key={i} title={tr.detail}>
              <Tag style={{ fontSize: 11, marginInlineEnd: 0 }}>计划 #{i + 1} · {tr.trigger}</Tag>
            </Tooltip>
          ))}
        </div>
      )}
      {data.paused && (
        <Typography.Text type="warning" style={{ display: 'block', marginBottom: 6, fontSize: 12 }}>
          已挂起（{data.pause_reason || 'task_pause'}）{data.open_node_ids?.length ? ` · 未决节点 ${data.open_node_ids.length}` : ''}——恢复后按节点边界续跑
        </Typography.Text>
      )}
      <div style={{ height: 'calc(100vh - 300px)', minHeight: 320, position: 'relative' }}>
        <Suspense fallback={<Typography.Text type="secondary">图引擎加载中…</Typography.Text>}>
          <GraphBoundary>
            <G6View
              nodes={nodes}
              triggers={triggers}
              taskId={taskId}
              selected={selected}
              onSelect={select}
              onDeselect={() => select(null)}
              onApiReady={(apiObj) => { g6ApiRef.current = apiObj; }}
            />
          </GraphBoundary>
        </Suspense>
      </div>
      <Modal
        title={<span style={{ fontSize: 18 }}>图执行分区汇总 <Typography.Text type="secondary" style={{ fontSize: 14, fontWeight: 400 }}>（计划批次带内按声明序；每节点一区域，区域内只递增）</Typography.Text></span>}
        open={zonedOpen}
        onCancel={() => setZonedOpen(false)}
        footer={null}
        width="90vw"
        styles={{ content: { height: '86vh', display: 'flex', flexDirection: 'column' }, body: { flex: 1, minHeight: 0, overflowY: 'auto' } }}
      >
        <GraphZones nodes={nodes} triggers={triggers} channels={channels} flatText={flatText} flatComplete={flatComplete} />
      </Modal>
    </Card>
  );
}

// useGraphQuery — 既有 /graph 查询。
function useGraphQuery(taskId: string, running: boolean) {
  const [data, setData] = useState<TaskGraph | null>(null);
  useEffect(() => {
    let alive = true;
    const load = async () => {
      try {
        const r = await api.get(`/v1/tasks/${taskId}/graph`);
        if (alive) setData(r.data as TaskGraph);
      } catch { /* 尽力而为：图面缺失不阻塞页面 */ }
    };
    void load();
    if (!running) return () => { alive = false; };
    const t = window.setInterval(load, 5000);
    return () => { alive = false; window.clearInterval(t); };
  }, [taskId, running]);
  return { data, refetch: () => void api.get(`/v1/tasks/${taskId}/graph`).then((r) => setData(r.data as TaskGraph)).catch(() => {}) };
}

// GraphZones — 分区汇总内容（Q10A+Q13A）：批次带分组，节点区域渲染通道时间线；
// 旧任务通道缺失=标记切段回落（诚实注记）。
function GraphZones({ nodes, triggers, channels, flatText, flatComplete }: {
  nodes: TaskGraphNode[];
  triggers: { seq: number; trigger: string; detail: string }[];
  channels?: Record<string, { text: string; complete: boolean }>;
  flatText?: string;
  flatComplete?: boolean;
}) {
  const legacy = !channels; // 旧任务回落模式（通道纪元前）
  const fallback = useMemo(
    () => (channels ? null : segmentFlatByMarkers(flatText ?? '')),
    [channels, flatText],
  );
  const byBatch = useMemo(() => {
    const groups: { label: string | null; nodes: TaskGraphNode[] }[] = [{ label: null, nodes: [] }];
    triggers.forEach((tr, i) => {
      groups.push({ label: `计划 #${i + 1} · ${tr.trigger}`, nodes: [] });
    });
    for (const n of nodes) groups[Math.min(batchIndexOf(n.declared_seq, triggers), groups.length - 1)].nodes.push(n);
    return groups.filter((g) => g.label !== null || g.nodes.length > 0);
  }, [nodes, triggers]);
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
      {!channels && fallback && fallback.preamble && (
        <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>任务级序言（标记前内容）：<pre style={{ whiteSpace: 'pre-wrap' }}>{fallback.preamble}</pre></Typography.Paragraph>
      )}
      {byBatch.map((g, gi) => (
        <div key={gi}>
          {g.label && <Typography.Text strong style={{ fontSize: 13, display: 'block', marginBottom: 4 }}>{g.label}</Typography.Text>}
          {g.nodes.map((n) => {
            const body = channels?.[n.id]?.text ?? fallback?.segments[n.id] ?? '';
            return (
              <div key={n.id} style={{ border: `2px solid ${GRAPH_NODE_STATUS_COLOR[n.status] ?? GRAPH_NODE_STATUS_COLOR.declared}`, borderRadius: 6, padding: '6px 10px', marginBottom: 8 }}>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 4 }}>
                  <span style={{ fontFamily: MONO_FONT, fontSize: 13, fontWeight: 700 }}>{n.id}</span>
                  <span style={{ fontSize: 11, color: '#57606a' }}>{n.type}</span>
                  <span style={{ fontSize: 11, color: GRAPH_NODE_STATUS_COLOR[n.status] ?? '#57606a' }}>{GRAPH_NODE_STATUS_TEXT[n.status] ?? n.status}</span>
                  {body === '' && <Typography.Text type="secondary" style={{ fontSize: 11 }}>（无关联交互内容）</Typography.Text>}
                </div>
                {body !== '' && <ZoneBody text={body} complete={channels?.[n.id]?.complete ?? flatComplete ?? false} />}
              </div>
            );
          })}
        </div>
      ))}
    </div>
  );
}

// ZoneBody — 通道文本经共享时间线解析渲染（aiTimeline 抽取层；懒加载组件内联避环）。
function ZoneBody({ text, complete }: { text: string; complete: boolean }) {
  const entries = useMemo(() => parseTimeline(text), [text]);
  return <div style={{ background: '#0D1117', borderRadius: 4, padding: '8px 12px' }}><TimelineBody entries={entries} complete={complete} /></div>;
}


// GraphBoundary — 懒加载模块级失败兜底（审查 P2-5：lazy reject 不击穿整页）。
import { Component, type ReactNode } from 'react';
class GraphBoundaryInner extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  render() {
    if (this.state.failed) {
      return <Typography.Text type="secondary">图视图加载失败——刷新页面重试。</Typography.Text>;
    }
    return this.props.children;
  }
}
function GraphBoundary({ children }: { children: ReactNode }) {
  return <GraphBoundaryInner>{children}</GraphBoundaryInner>;
}
