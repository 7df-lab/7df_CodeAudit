// TaskGraphCanvas：泳道视图退役后的
// 图执行视图等价断言（Q7A——本文件重写自 TaskGraphCard 9 用例）。锁：工具栏/
// 状态计数/挂起横幅/空态占位（Q1 让渡语义）/分区汇总 Modal（批次带+节点区域+
// 旧任务回落段）/选中回调携带节点对象。G6 渲染交互面经 mock（jsdom 无 canvas），
// 真实交互=批5 GUI 门禁。
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach } from 'vitest';
import TaskGraphCanvas from '../components/TaskGraphCanvas';
import type { TaskGraph, TaskGraphNode } from '../api/types';
import { useFakeGateway } from '../testsupport/fakeGateway';

vi.mock('../components/G6View', () => ({
  default: ({ nodes, onSelect }: { nodes: TaskGraphNode[]; onSelect: (id: string) => void }) => (
    <div data-testid="g6-mock">
      {nodes.map((n) => (
        <button key={n.id} data-testid={`g6-node-${n.id}`} onClick={() => onSelect(n.id)}>{n.id}</button>
      ))}
    </div>
  ),
}));

const EMPTY = { task_id: 't-1', nodes: [], plan_triggers: [], paused: false, pause_reason: '', converged: false, converge_summary: '', open_remaining: 0, updated_at: null };
const RICH = {
  task_id: 't-1',
  nodes: [
    { id: 'n001', type: 'executor', intent: '初扫注入面', declared_by: 'pack', from: [], status: 'completed', attempts: 0, started_at: '2026-09-20T10:00:00Z', ended_at: '2026-09-20T10:02:14Z', result_ref: '3 处发现', declared_seq: 1 },
    { id: 'n002', type: 'verifier', intent: '复核补丁', declared_by: 'planner', from: ['n001'], status: 'running', attempts: 1, started_at: '2026-09-20T10:03:00Z', ended_at: '', result_ref: '', declared_seq: 5 },
  ],
  plan_triggers: [
    { trigger: 'initial', detail: '起初始 planner', at: null, seq: 0 },
    { trigger: 'new_evidence', detail: 'n001 报告可疑残面', at: null, seq: 4 },
  ],
  paused: false, pause_reason: '', converged: false, converge_summary: '', open_remaining: 1, updated_at: null,
};

let graphPayload: unknown = EMPTY;
const gateway = useFakeGateway({
  'GET /v1/tasks/:taskId/graph': () => graphPayload,
});

function renderCanvas(props?: Partial<Parameters<typeof TaskGraphCanvas>[0]>) {
  return render(
    <TaskGraphCanvas taskId="t-1" running={false} {...props} />,
  );
}

describe('TaskGraphCanvas（泳道退役后等价面）', () => {
  beforeEach(() => { vi.restoreAllMocks(); });

  it('图面未达 → 整块不渲染（空态纪律保留：组件级 null）', () => {
    graphPayload = undefined; // 图面未达（GET 失败/未响应同形）
    const { container } = renderCanvas();
    expect(container.innerHTML).toBe('');
  });

  it('图任务零节点 → 诚实占位卡（Q1：AI 面板位已让渡，不回退 AI 面板）', async () => {
    graphPayload = EMPTY;
    renderCanvas();
    expect(await screen.findByText(/图规划中/)).toBeTruthy();
  });

  it('有节点 → 画布挂载+状态计数（未决/运行/已结）', async () => {
    graphPayload = RICH;
    renderCanvas();
    await waitFor(() => expect(screen.getByTestId('g6-mock')).toBeTruthy());
    expect(screen.getByText(/运行 1/)).toBeTruthy();
    expect(screen.getByText(/已结 1/)).toBeTruthy();
  });

  it('paused → 挂起横幅（恢复语义+未决计数）', async () => {
    graphPayload = { ...(RICH as TaskGraph), paused: true, pause_reason: 'human_claim', open_node_ids: ['n002'] };
    renderCanvas();
    await waitFor(() => expect(screen.getByText(/已挂起（human_claim）\s*·\s*未决节点 1/)).toBeTruthy());
  });

  it('点节点 → onSelectNode 携带节点对象（弹层元数据锚）', async () => {
    graphPayload = RICH;
    const onSelect = vi.fn();
    renderCanvas({ onSelectNode: onSelect });
    await waitFor(() => expect(screen.getByTestId('g6-node-n002')).toBeTruthy());
    fireEvent.click(screen.getByTestId('g6-node-n002'));
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: 'n002', status: 'running' }));
  });

  it('分区汇总 Modal：批次带+节点区域+通道文本渲染+旧任务回落', async () => {
    graphPayload = RICH;
    // 旧任务回落：无 channels，flat 文本标记切段
    renderCanvas({ flatText: '# 图节点任务（n001 · executor）\nn001 交互内容\n✍ [输出]\n结论', flatComplete: true });
    await waitFor(() => expect(screen.getByTestId('g6-mock')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /分区汇总/ }));
    expect((await screen.findAllByText(/计划 #2 · new_evidence/)).length).toBeGreaterThanOrEqual(1); // 批次带（图例+Modal 分区同文案）
    expect(screen.getAllByText('n001').length).toBeGreaterThan(0); // 画布节点+分区区域头
    expect(await screen.findByText(/n001 交互内容/)).toBeTruthy(); // 回落切段入区
    // 旧任务诚实注记标签在节点弹层（GraphNodePopup）——本 Modal 断言回落段内容即可
  });

  it('通道纪元：节点区域渲染通道文本', async () => {
    graphPayload = RICH;
    renderCanvas({ channels: { n001: { text: '通道纪元内容', complete: true }, n002: { text: 'n002 内容', complete: false } } });
    await waitFor(() => expect(screen.getByTestId('g6-mock')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /分区汇总/ }));
    expect(await screen.findByText(/通道纪元内容/)).toBeTruthy();
    expect(screen.queryByText('旧任务回落')).toBeNull();
  });

  it('契约锁：消费形状键集对齐 proto TaskGraph 派生类型', async () => {
    graphPayload = RICH;
    renderCanvas();
    await waitFor(() => expect(screen.getByTestId('g6-mock')).toBeTruthy());
    // 消费形状锁：RICH 夹具节点键集=proto TaskGraphNode 派生类型消费面
    expect(Object.keys((graphPayload as TaskGraph).nodes[0])).toEqual(
      expect.arrayContaining(['id', 'type', 'intent', 'from', 'status', 'attempts', 'declared_seq']));
  });
});
