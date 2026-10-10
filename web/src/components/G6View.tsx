// G6View — @antv/g6 v5 画布载体。
// 节点=html 卡片（完全 DOM 自由度：状态色边框/状态条/三行文本）；布局=computeLRLayout
// 纯函数（LR 分层，位置 localStorage 按任务持久，拖拽落盘）；behaviors=drag-element
// （v5 名；v4 的 drag-node 静默无效=拖拽失效根因）/zoom-canvas/drag-canvas；
// >15 节点 minimap；点节点回调 onSelect、点画布空白 onDeselect、onApiReady 暴露
// fitView/zoomBy/refresh/getNodes。jsdom 无 canvas：逻辑下沉纯函数，视觉由 GUI 门禁实证。
import { useEffect, useMemo, useRef, useState } from 'react'
import type { TaskGraphNode } from '../api/types'
import { GRAPH_NODE_STATUS_COLOR, GRAPH_NODE_STATUS_TEXT } from '../dict/tokens'
import { batchIndexOf, computeLRLayout, graphLayoutKey, type LayoutPositions } from '../utils/graphChannels'

export interface G6ViewApi {
  fitView: () => void
  zoomBy: (factor: number) => void
}

const statusText = (s: string) => GRAPH_NODE_STATUS_TEXT[s] ?? s

/** HTML 转义（G6 html 节点绕过 React 转义直进 DOM——intent 源自 planner 模型读
 * 不可信扫描目标后的输出（提示注入回显面），id/type 同转义防御性兜底）。 */
function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;')
}

/** 卡片 HTML（内联样式——html 节点不经 React/CSS 隔离；title 属性带 intent 全文）。
 *  导出供转义回归测试直测（jsdom 无 canvas，G6 渲染面测不到字符串层）。 */
export function cardHTML(n: TaskGraphNode, selected: boolean, triggers: { seq: number }[]): string {
  const color = GRAPH_NODE_STATUS_COLOR[n.status] ?? GRAPH_NODE_STATUS_COLOR.declared
  const batch = batchIndexOf(n.declared_seq, triggers)
  const head = [
    `<span style="font-weight:700;font-size:13px;color:#24292f;">${escapeHtml(n.id)}</span>`,
    `<span style="font-size:11px;color:#57606a;">${escapeHtml(n.type)}</span>`,
    `<span style="font-size:11px;font-weight:600;color:${color};margin-left:auto;">${statusText(n.status)}</span>`,
  ].join('')
  const meta = [
    n.attempts > 1 ? `第 ${n.attempts} 次尝试` : '',
    batch > 0 ? `计划 #${batch}` : '',
    n.holder === 'human' ? '人类持有' : '',
  ].filter(Boolean).join(' · ')
  const intentRaw = n.intent.length > 24 ? n.intent.slice(0, 24) + '…' : n.intent
  return `
    <div style="width:240px;height:66px;box-sizing:border-box;background:#fff;border:2px solid ${color};border-left:6px solid ${color};border-radius:8px;padding:6px 10px 6px 12px;font-family:system-ui,-apple-system,'Segoe UI',sans-serif;overflow:hidden;${selected ? 'box-shadow:0 0 0 3px rgba(9,105,218,0.35);' : ''}">
      <div style="display:flex;gap:6px;align-items:baseline;">${head}</div>
      <div style="font-size:11px;color:#57606a;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;">${meta || '&nbsp;'}</div>
      <div style="font-size:11px;color:#24292f;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;" title="${escapeHtml(n.intent)}">${escapeHtml(intentRaw)}</div>
    </div>`
}

export default function G6View({ nodes, triggers, taskId, selected, onSelect, onDeselect, onApiReady }: {
  nodes: TaskGraphNode[]
  triggers: { seq: number }[]
  taskId: string
  selected: string | null
  onSelect: (nodeId: string) => void
  /** 点画布空白（弹层第三关闭路径）。 */
  onDeselect?: () => void
  onApiReady?: (api: G6ViewApi) => void
}) {
  const containerRef = useRef<HTMLDivElement>(null)
  const graphRef = useRef<any>(null)
  // 视口记忆（跨重建持久——ref 而非历史版本的组件体 let：后者每 render 新绑定，
  // cleanup 写旧绑定/setup 读新绑定=永远读到初值；0f43e36 连同本恢复整段丢失）
  const zoomRef = useRef(1)
  const [loadError, setLoadError] = useState(false)
  // 内容签名门控：5s 轮询每拍新对象引用——按语义签名判“真变化”，未变不重建
  // （视口/拖拽中位置/选中高亮全保；重建仅发生在节点集或状态真变时）。
  const signature = useMemo(
    () => nodes.map((n) => `${n.id}|${n.status}|${n.attempts}|${n.holder ?? ''}|${n.declared_seq ?? ''}|${n.from.join('+')}`).join(';')
      + '#' + triggers.map((t) => t.seq).join(','),
    [nodes, triggers],
  )
  const nodesRef = useRef(nodes)
  nodesRef.current = nodes
  const triggersRef = useRef(triggers)
  triggersRef.current = triggers
  const selectedRef = useRef(selected)
  selectedRef.current = selected
  const onDeselectRef = useRef(onDeselect)
  onDeselectRef.current = onDeselect
  const onSelectRef = useRef(onSelect)
  onSelectRef.current = onSelect

  useEffect(() => {
    let disposed = false
    let graph: any = null
    void (async () => {
      let Graph: any
      try {
        ({ Graph } = await import('@antv/g6'))
      } catch {
        if (!disposed) setLoadError(true)
        return
      }
      if (disposed || !containerRef.current) return
      const nodes = nodesRef.current
      const triggers = triggersRef.current
      let saved: LayoutPositions | null = null
      try { saved = JSON.parse(localStorage.getItem(graphLayoutKey(taskId)) ?? 'null') } catch { saved = null }
      const positions = computeLRLayout(nodes.map((n) => ({ id: n.id, from: n.from })), saved)

      const edges: { id: string; source: string; target: string }[] = []
      const seen = new Set<string>()
      for (const n of nodes) {
        for (const dep of n.from) {
          const id = `${dep}->${n.id}`
          if (!seen.has(id)) { seen.add(id); edges.push({ id, source: dep, target: n.id }) }
        }
      }
      graph = new Graph({
        container: containerRef.current,
        autoFit: 'view',
        padding: 40,
        behaviors: ['drag-canvas', 'zoom-canvas', 'drag-element'],
        plugins: nodes.length > 15 ? [{ type: 'minimap', size: [160, 96] } as any] : [],
        node: {
          type: 'html',
          style: {
            size: [240, 66],
            innerHTML: (d: { id: string }) => cardHTML(
              nodes.find((x) => x.id === d.id) ?? ({ id: d.id, intent: '', from: [], status: 'declared', type: '', attempts: 0 } as unknown as TaskGraphNode),
              d.id === selectedRef.current,
              triggers,
            ),
          },
        },
        edge: { style: { stroke: '#94a3b8', lineWidth: 1.5, endArrow: true } },
        data: {
          nodes: nodes.map((n) => {
            const pos = positions[n.id] ?? { x: 0, y: 0 }
            return { id: n.id, style: { x: pos.x, y: pos.y } }
          }),
          edges,
        },
      })
      graph.on('node:click', (evt: any) => {
        const id = evt.target?.id ?? evt.id
        if (typeof id === 'string') onSelectRef.current(id)
      })
      graph.on('canvas:click', () => onDeselectRef.current?.())
      graph.on('node:dragend', () => {
        const g = graph as any
        if (typeof g?.getNodeData !== 'function') return
        const out: Record<string, { x: number; y: number }> = {}
        for (const n of g.getNodeData() as Array<{ id: string; style?: { x?: number; y?: number } }>) {
          if (n.style?.x != null && n.style?.y != null) out[n.id] = { x: n.style.x, y: n.style.y }
        }
        try { localStorage.setItem(graphLayoutKey(taskId), JSON.stringify(out)) } catch { /* 尽力而为 */ }
      })
      await graph.render()
      // 视口回填：非首次重建沿用用户缩放（autoFit:'view' 每次重建复位——运行中
      // 图任务节点状态每翻转即丢缩放，5s 轮询下"可缩放"交互基本不可用）
      const prevZoom = zoomRef.current
      try {
        const g = graph as unknown as { setZoom?: (z: number, center?: unknown) => Promise<unknown> }
        if (prevZoom > 0 && prevZoom !== 1 && g.setZoom) await g.setZoom(prevZoom)
      } catch { /* 视口 API 面差异=尽力而为 */ }
      graphRef.current = graph
      onApiReady?.({
        fitView: () => { void graph?.fitView() },
        zoomBy: (f: number) => { void graph?.zoomBy?.(f) },
      })
    })()
    return () => {
      disposed = true
      try {
        zoomRef.current = (graph as unknown as { getZoom?: () => number } | null)?.getZoom?.() ?? zoomRef.current
      } catch { /* 尽力而为 */ }
      graph?.destroy()
      graphRef.current = null
    }
    // 签名门控：语义未变零重建（视口/拖拽/选中保持）
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature, taskId])

  if (loadError) {
    return (
      <div style={{ display: 'flex', height: '100%', alignItems: 'center', justifyContent: 'center', flexDirection: 'column', gap: 8 }}>
        <span style={{ color: '#57606a' }}>图引擎加载失败（网络/chunk 缺失）</span>
        <button onClick={() => setLoadError(false)} style={{ padding: '2px 12px', cursor: 'pointer' }}>重试</button>
      </div>
    )
  }

  return <div ref={containerRef} style={{ width: '100%', height: '100%' }} data-testid="g6-container" />
}
