// TP20-T2 切换 Agent 收口（主仓库计划 §3.3）：Modal 当前项禁用+「当前」徽标/能力摘要
// 如实/端点失败=整体禁用+错误说明（不回落硬编码）/两态确认走对端点（pause?agent= 与
// POST /v1/tasks/{id}/agent）。请求形状断言用 fakeGateway requests 日志（纪律 §8-4）。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { AgentSwitchModal } from '../components/AgentSwitchModal';
import { httpError, useFakeGateway } from '../testsupport/fakeGateway';

const AGENTS = {
  agents: [
    { id: 'dsh', dialect: 'dsh-sse', is_default: true,
      tool_events: true, structured_submit: true, interaction: false, session_resume: false, conclude_semantics: true },
    { id: 'maka', dialect: 'maka-runtime-host', is_default: false,
      tool_events: false, structured_submit: false, interaction: false, session_resume: false, conclude_semantics: false },
  ],
};
let agentsFail = false;

const gateway = useFakeGateway({
  'GET /v1/agents': () => {
    if (agentsFail) throw httpError(500, { error: "runtime unavailable" });
    return AGENTS;
  },
  'POST /v1/tasks/:id/pause': { task_id: 't-1', status: 'TASK_STATUS_PAUSED' },
  'POST /v1/tasks/:id/agent': { task_id: 't-1', agent: 'maka', status: 'TASK_STATUS_PAUSED' },
});

function lastPost() {
  return gateway.requests.filter((r) => r.method === 'POST').slice(-1)[0];
}

function renderModal(props: Partial<Parameters<typeof AgentSwitchModal>[0]> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AgentSwitchModal
        open
        taskId="t-1"
        mode="switch"
        currentAgent=""
        onClose={() => {}}
        onDone={() => {}}
        {...props}
      />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  agentsFail = false;
});

describe('AgentSwitchModal（TP20-T2）', () => {
  it('当前项禁用+「当前」徽标；默认徽标；能力摘要如实', async () => {
    // currentAgent 空=部署默认=is_default 条目（dsh）
    renderModal({ currentAgent: '' });
    await waitFor(() => expect(screen.getByText('maka')).toBeTruthy());
    expect(screen.getByText('当前')).toBeTruthy();
    expect(screen.getByText('默认')).toBeTruthy();
    const dshRadio = screen.getByText('dsh').closest('label') as HTMLLabelElement;
    expect(dshRadio.className).toContain('ant-radio-wrapper-disabled');
    // 能力摘要四行如实（计划 §3.2）：maka=非结构化 → 围栏 JSON 兜底标注
    expect(screen.getByText(/结构化提交：无（围栏 JSON 兜底）/)).toBeTruthy();
    expect(screen.getByText(/工具事件：无/)).toBeTruthy();
  });

  it('端点失败=整体禁用+如实错误（不回落硬编码清单）', async () => {
    agentsFail = true;
    renderModal();
    await waitFor(() => expect(screen.getByText('Agent 清单加载失败')).toBeTruthy());
    expect(screen.queryByText('dsh')).toBeNull();
    expect(screen.queryByText('maka')).toBeNull();
    const ok = screen.getByRole('button', { name: '确认切换' });
    expect((ok as HTMLButtonElement).disabled).toBe(true);
  });

  it('PAUSED 态确认走 POST /v1/tasks/{id}/agent（body.agent）', async () => {
    renderModal({ mode: 'switch' });
    await waitFor(() => expect(screen.getByText('maka')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '确认切换' }));
    await waitFor(() => expect(lastPost()).toBeTruthy());
    expect(lastPost()!.url).toBe('/v1/tasks/t-1/agent');
    expect((lastPost()!.body as { agent: string }).agent).toBe('maka');
  });

  it('RUNNING 态标题与说明文案；确认走 pause?agent=（回合边界换手）', async () => {
    renderModal({ mode: 'pause-switch' });
    await waitFor(() => expect(screen.getByText('暂停并切换 Agent')).toBeTruthy());
    expect(screen.getByText(/任务将在当前回合边界暂停/)).toBeTruthy();
    // 清单加载完成才可确认（加载期确认钮禁用——降级语义的一部分）
    await waitFor(() => expect(screen.getByText('maka')).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: '确认切换' }));
    await waitFor(() => expect(lastPost()).toBeTruthy());
    expect(lastPost()!.url).toBe('/v1/tasks/t-1/pause');
    expect(lastPost()!.query).toContain('agent=maka');
  });

  it('指定 currentAgent 时对应项禁用（非默认项为当前）', async () => {
    renderModal({ currentAgent: 'maka' });
    await waitFor(() => expect(screen.getByText('dsh')).toBeTruthy());
    const makaRadio = screen.getByText('maka').closest('label') as HTMLLabelElement;
    expect(makaRadio.className).toContain('ant-radio-wrapper-disabled');
  });
});
