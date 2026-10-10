// GraphNodePopup — 图节点 AI 交互弹层（graph-ui-channels 批4，2026-09-23；Q9B 裁决：
// 盖页面另一半=信息卡栏，滑入、×/Esc/点图空白关闭、点其他节点即切换）。
// 内容三层：头部=节点元数据（id/类型/状态/attempts/起止/holder+租约/result_ref）；
// 正文=该节点通道实时流（通道纪元）或标记切段回落（旧任务，诚实注记）——共享
// aiTimeline 渲染层；底部操作区=v0.5 人机共编辑三件（注入/认领/释放——迁自泳道
// NodeRow，W3/W4 规则保留：worker 持有隐藏认领、计数文案口径）。
import { Button, Input, Tag, Tooltip, Typography, message } from 'antd';
import type { Granularity } from '../utils/graphChannels';
import { useEffect, useMemo, useRef, useState } from 'react';
import { apiErrorMessage, claimGraphNode, injectGraphNodeMessage, releaseGraphNode } from '../api/client';
import type { TaskGraphNode } from '../api/types';
import { GRAPH_NODE_STATUS_COLOR, GRAPH_NODE_STATUS_TEXT, GRAPH_NODE_TYPE_COLOR, MONO_FONT } from '../dict/tokens';
import { EVIDENCE } from '../theme/evidence';
import { parseTimeline, TimelineBody } from './aiTimeline';

// localHMS — RFC3339 → 本地 hh:mm:ss（审查 P2-7：与阶段时间线 hhmmss 同口径，禁 UTC 截串）
function localHMS(ts: string): string {
  const d = new Date(ts);
  return isNaN(d.getTime()) ? '' : d.toLocaleTimeString('zh-CN', { hour12: false });
}

export default function GraphNodePopup({ node, taskId, text, complete, legacy, granularity = 'token', onClose, onChanged }: {
  node: TaskGraphNode;
  taskId: string;
  /** 该节点的通道文本（通道纪元）或旧任务回落切段文本 */
  text: string;
  complete: boolean;
  /** 旧任务回落（无通道纪元）——诚实注记 */
  legacy?: boolean;
  /** 审2/审6 裁决落地（前端诚实面）：token=逐字实时；turn=整回合到达（maka/acp 型 agent）。
   *  b4 已兑现契约字段化（AILogChannel.granularity，proto enum AI_LOG_GRANULARITY_*）——
   *  值经 TaskDetailPage 从通道/主面元数据归一传入；UNSPECIFIED/缺失=回落 agent 判别。 */
  granularity?: Granularity;
  onClose: () => void;
  onChanged: () => void; // 操作成功后刷新（图状态轮询拍频外的即时拉取）
}) {
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);

  // 新内容到达自动滚底；用户上翻停止跟随（与 AIInteractionLogPanel 同语义）
  const stick = useRef(true);
  useEffect(() => {
    if (boxRef.current && stick.current) boxRef.current.scrollTop = boxRef.current.scrollHeight;
  }, [text]);

  // Esc 关闭（Q9B 裁决）
  useEffect(() => {
    const h = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
    window.addEventListener('keydown', h);
    return () => window.removeEventListener('keydown', h);
  }, [onClose]);

  const runWithBusy = async (fn: () => Promise<string>) => {
    setBusy(true);
    try {
      message.success(await fn());
      onChanged();
    } catch (e) {
      message.error(apiErrorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const send = () => {
    if (!msg.trim()) return;
    return runWithBusy(async () => {
      await injectGraphNodeMessage(taskId, node.id, msg.trim());
      setMsg('');
      return `已注入 ${node.id}（下回合边界生效）`;
    });
  };

  const entries = useMemo(() => parseTimeline(text), [text]);
  const color = GRAPH_NODE_STATUS_COLOR[node.status] ?? GRAPH_NODE_STATUS_COLOR.declared;

  return (
    <div
      data-testid="graph-node-popup"
      style={{
        position: 'absolute', inset: 0, zIndex: 30,
        display: 'flex', flexDirection: 'column',
        background: '#fff', boxShadow: '-8px 0 24px rgba(0,0,0,0.18)',
        borderRadius: 8, overflow: 'hidden',
        animation: 'gnp-slide-in 160ms ease-out', // Q9"滑出"裁决：信息栏半页自左滑入
      }}
    >
      <style>{`@keyframes gnp-slide-in { from { transform: translateX(-24px); opacity: 0; } to { transform: translateX(0); opacity: 1; } }`}</style>
      {/* 头部：节点元数据 */}
      <div style={{ padding: '10px 14px', borderBottom: `1px solid ${EVIDENCE.border}`, display: 'flex', flexWrap: 'wrap', gap: 8, alignItems: 'center' }}>
        <span style={{ fontFamily: MONO_FONT, fontSize: 15, fontWeight: 700 }}>{node.id}</span>
        <Tag color={GRAPH_NODE_TYPE_COLOR[node.type]} style={{ marginInlineEnd: 0 }}>{node.type}</Tag>
        <Tag color={color} style={{ marginInlineEnd: 0 }}>{GRAPH_NODE_STATUS_TEXT[node.status] ?? node.status}</Tag>
        {node.attempts > 0 && <Tag style={{ marginInlineEnd: 0 }}>第 {node.attempts} 次尝试</Tag>}
        {node.holder && (
          <Tooltip title={node.lease_until ? `认领租约至 ${node.lease_until}` : `持有者 ${node.holder}`}>
            <Tag color={node.holder === 'human' ? 'gold' : 'blue'} style={{ marginInlineEnd: 0 }}>
              {node.holder === 'human' ? '人类持有' : 'worker'}
            </Tag>
          </Tooltip>
        )}
        {node.started_at && <Typography.Text type="secondary" style={{ fontSize: 12 }}>开始 {localHMS(node.started_at)}</Typography.Text>}
        {node.ended_at && node.started_at && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            · 耗时 {Math.max(1, Math.round((new Date(node.ended_at).getTime() - new Date(node.started_at).getTime()) / 1000))}s
          </Typography.Text>
        )}
        {node.result_ref && (
          <Tooltip title={node.result_ref}>
            <Typography.Text ellipsis style={{ fontSize: 12, maxWidth: 260, fontFamily: MONO_FONT }}>{node.result_ref}</Typography.Text>
          </Tooltip>
        )}
        <span style={{ flex: 1 }} />
        {legacy && <Tooltip title="该任务早于通道纪元——按节点标记切段回落（无锚注入/收笔可能归入相邻段，plan §6 审4 边界）"><Tag>旧任务回落</Tag></Tooltip>}
        <Button size="small" onClick={onClose}>✕ 关闭</Button>
      </div>

      {/* 正文：通道/回落段实时流 */}
      <div
        ref={boxRef}
        onScroll={(e) => {
          const b = e.currentTarget;
          stick.current = b.scrollHeight - b.scrollTop - b.clientHeight < 40;
        }}
        style={{ flex: 1, minHeight: 0, overflowY: 'auto', background: EVIDENCE.bg, padding: '10px 14px' }}
      >
        {granularity === 'turn' && (
          <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>
            该 agent 以整回合返回结论（无逐字流）——内容在回合收束后一次性到达。
          </Typography.Text>
        )}
        {text === '' ? (
          <Typography.Text type="secondary">
            {granularity === 'turn'
              ? '该节点暂无交互内容——回合收束后一次性到达。'
              : '该节点暂无交互内容——运行中节点的通道内容将实时到达。'}
          </Typography.Text>
        ) : (
          <TimelineBody entries={entries} complete={complete} />
        )}
      </div>

      {/* 底部操作区：注入/认领/释放（v0.5 人机共编辑，迁自泳道 NodeRow） */}
      <div style={{ padding: '8px 14px', borderTop: `1px solid ${EVIDENCE.border}`, display: 'flex', gap: 6, alignItems: 'center' }}>
        <Input
          size="small"
          placeholder={`向 ${node.id} 追加消息（下回合生效）`}
          value={msg}
          disabled={busy}
          onChange={(e) => setMsg(e.target.value)}
          onPressEnter={send}
          style={{ maxWidth: 360 }}
        />
        <Button size="small" type="primary" ghost loading={busy} disabled={!msg.trim()} onClick={send}>注入</Button>
        {node.holder === '' && (
          <Button size="small" disabled={busy} onClick={() => runWithBusy(async () => {
            const r = await claimGraphNode(taskId, node.id);
            return `已认领（租约至 ${r.lease_until}）`;
          })}>认领</Button>
        )}
        {node.holder === 'human' && (
          <Button size="small" danger ghost loading={busy} onClick={() => runWithBusy(async () => {
            await releaseGraphNode(taskId, node.id);
            return `已释放 ${node.id}`;
          })}>释放</Button>
        )}
      </div>
    </div>
  );
}
