// TaskUsageCard：既有
// GET /v1/tasks/{id}/usage 端点的首个 web 消费面。锁三形态：
// 有用量→摘要行+分模型明细；空聚合→诚实空态行；计费口径=缓存读占比可算。
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import TaskUsageCard from '../components/TaskUsageCard';
import type { TaskUsageSummary } from '../api/types';
import { useFakeGateway } from '../testsupport/fakeGateway';

const EMPTY = { task_id: 't-1', models: [], totals: { model: '', calls: '0', input_tokens: '0', output_tokens: '0' } };
const RICH = {
  task_id: 't-1',
  models: [
    { model: 'glm-4.7', calls: '18', input_tokens: '41200', output_tokens: '9800', cache_read_tokens: '63400', cache_write_tokens: '8100', reasoning_tokens: '2200', total_tokens: '122500' },
    { model: 'glm-4.7-flash', calls: '6', input_tokens: '5400', output_tokens: '1200', cache_read_tokens: '2100', cache_write_tokens: '0', reasoning_tokens: '0', total_tokens: '8700' },
  ],
  // 计费输入=46600+65500+8100=120200；缓存读占比=65500/120200≈54%
  totals: { model: '', calls: '24', input_tokens: '46600', output_tokens: '11000', cache_read_tokens: '65500', cache_write_tokens: '8100', reasoning_tokens: '2200', total_tokens: '131200' },
};

let usagePayload: unknown = EMPTY;
const gateway = useFakeGateway({
  'GET /v1/tasks/:taskId/usage': () => usagePayload,
});

function renderCard() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <TaskUsageCard taskId="t-1" />
    </QueryClientProvider>,
  );
}

describe('TaskUsageCard', () => {
  it('空聚合 → 诚实空态行（不隐藏块）', async () => {
    usagePayload = EMPTY;
    renderCard();
    await waitFor(() => expect(screen.getByText(/暂无计量（任务未产生用量事件）/)).toBeTruthy());
  });

  it('有用量 → tokens 合计/模型×回合/缓存读占比摘要行', async () => {
    usagePayload = RICH;
    renderCard();
    await waitFor(() => expect(screen.getByText(/tokens 合计 131\.2k/)).toBeTruthy());
    expect(screen.getByText(/glm-4\.7 ×18/)).toBeTruthy();
    expect(screen.getByText(/缓存读 54%/)).toBeTruthy();
  });

  it('展开分模型明细 → 双模型行与数值人性化', async () => {
    usagePayload = RICH;
    renderCard();
    await waitFor(() => expect(screen.getByText(/tokens 合计/)).toBeTruthy());
    fireEvent.click(screen.getByText('分模型明细'));
    await waitFor(() => expect(screen.getByText('glm-4.7-flash')).toBeTruthy());
    // 明细数值人性化（122.5k 而非裸 122500）
    expect(screen.getByText('122.5k')).toBeTruthy();
  });

  it('请求路径与缓存键契约：/v1/tasks/{id}/usage 单口', async () => {
    usagePayload = RICH;
    renderCard();
    await waitFor(() => expect(screen.getByText(/tokens 合计/)).toBeTruthy());
    const gets = gateway.requests.filter((r) => r.method === 'GET' && r.url.includes('/usage'));
    expect(gets.length).toBeGreaterThan(0);
    expect(gets[0].url).toContain('/v1/tasks/t-1/usage');
  });
});

describe('TaskUsageCard 契约锁（§4.3）', () => {
  it('消费形状键集对齐 proto TaskUsageSummary（类型锁：字段增删须显式过此关）', () => {
    const keys: (keyof TaskUsageSummary)[] = ['task_id', 'models', 'totals'];
    expect(keys.sort()).toEqual(['models', 'task_id', 'totals']);
    // 计数口径锁：uint64 protojson 字符串化——RICH 夹具计数字段必须保持 string 形态
    const m = RICH.models[0];
    expect(typeof m.calls).toBe('string');
    expect(typeof m.input_tokens).toBe('string');
    expect(typeof (m as { cache_read_tokens?: string | number }).cache_read_tokens).toBe('string');
  });
});