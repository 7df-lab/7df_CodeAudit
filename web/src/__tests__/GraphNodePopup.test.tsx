// GraphNodePopup（graph-ui-channels 批4，Q9B）：节点元数据头/通道时间线正文/
// 操作区（注入/认领/释放——W3/W4 规则迁移）/旧任务回落注记/Esc 关闭。
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import GraphNodePopup from '../components/GraphNodePopup';
import type { TaskGraphNode } from '../api/types';
import { useFakeGateway } from '../testsupport/fakeGateway';

const NODE: TaskGraphNode = {
  id: 'n003', type: 'executor', intent: '测一下', declared_by: 'planner', from: ['n001'],
  status: 'running', attempts: 2, started_at: '2026-09-23T10:00:00Z', ended_at: '',
  result_ref: '', declared_seq: 5, holder: 'human', lease_until: '2026-09-23T11:00:00Z',
};

const gateway = useFakeGateway({
  'POST /v1/tasks/:taskId/graph/nodes/:nodeId/message': () => ({ accepted: true, node_status: 'NODE_RUNNING' }),
});

function renderPopup(props?: Partial<Parameters<typeof GraphNodePopup>[0]>) {
  return render(
    <GraphNodePopup node={NODE} taskId="t-1" text="" complete={false}
      onClose={() => {}} onChanged={() => {}} {...props} />,
  );
}

describe('GraphNodePopup', () => {
  it('头部元数据：id/类型/状态/attempts/人类持有租约', () => {
    renderPopup();
    expect(screen.getByText('n003')).toBeTruthy();
    expect(screen.getByText('executor')).toBeTruthy();
    expect(screen.getByText('运行中')).toBeTruthy();
    expect(screen.getByText('第 2 次尝试')).toBeTruthy();
    expect(screen.getByText('人类持有')).toBeTruthy();
  });

  it('正文空态→通道内容实时渲染（时间线解析）', () => {
    const { rerender } = renderPopup({ text: '' });
    expect(screen.getByText(/该节点暂无交互内容/)).toBeTruthy();
    rerender(
      <GraphNodePopup node={NODE} taskId="t-1" text="📋 [任务下发]（10 字节）\n正文段" complete={false}
        onClose={() => {}} onChanged={() => {}} />,
    );
    expect(screen.getByText(/正文段/)).toBeTruthy();
  });

  it('旧任务回落注记（legacy 标签）', () => {
    renderPopup({ legacy: true, text: 'x' });
    expect(screen.getByText('旧任务回落')).toBeTruthy();
  });

  it('注入操作：输入→按钮可点→成功回执+输入清空', async () => {
    renderPopup({ text: 'x' });
    const input = screen.getByPlaceholderText(/向 n003 追加消息/);
    fireEvent.change(input, { target: { value: '重点核查注入面' } });
    fireEvent.click(screen.getByRole('button', { name: /注 入|注入/ }));
    await waitFor(() => expect(screen.getByText(/已注入 n003/)).toBeTruthy());
    expect((input as HTMLInputElement).value).toBe('');
  });

  it('人类持有→释放按钮在场、认领隐藏（W3/W4 迁移规则）', () => {
    renderPopup({ text: 'x' });
    expect(screen.getByRole('button', { name: /释 放|释放/ })).toBeTruthy();
    expect(screen.queryByRole('button', { name: /^认 领$|^认领$/ })).toBeNull();
  });

  it('Esc 关闭回调', () => {
    const onClose = vi.fn();
    renderPopup({ text: 'x', onClose });
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });
});
