// 任务计量卡：消费既有
// GET /v1/tasks/{id}/usage（engine usage_collector 归集 baton usage.recorded → PG，
// ADR-247）——此前端点在、web 零消费（暴露面复查缺口）。
// 认识论纪律：只呈现引擎已归集的事实；无用量=诚实空态行（用户需要知道"这里应该
// 有计量"），错误态不渲染（任务级 404 由快照口权威呈现，计量是附属面）。
import { useQuery } from '@tanstack/react-query';
import { Card, Collapse, Table, Typography } from 'antd';
import { api } from '../api/client';
import type { TaskModelUsage, TaskUsageSummary } from '../api/types';

// proto uint64 计数 → protojson 字符串；展示层统一 Number 化
function n(v: string | number | undefined): number {
  return Number(v ?? 0);
}

// 千分位人性化（128.4k / 1.2M）——计量摘要的紧凑口径
function fmtTokens(v: number): string {
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`;
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}k`;
  return String(v);
}

function hasUsage(u: TaskUsageSummary | undefined): boolean {
  if (!u) return false;
  return (u.models?.length ?? 0) > 0 || n(u.totals?.total_tokens) > 0;
}

// 计费输入 = input + cache_read + cache_write（baton Usage 同口径）；缓存读占比
// = cache_read / 计费输入——成本结构里最值得一眼看到的一格。
function cacheReadPct(m: TaskModelUsage): number | null {
  const billed = n(m.input_tokens) + n(m.cache_read_tokens) + n(m.cache_write_tokens);
  if (billed <= 0) return null;
  return Math.round((n(m.cache_read_tokens) / billed) * 100);
}

const NUM_COLS = ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'total_tokens'] as const;

// variant：'card'=独立计量卡（默认）；'inline'=并入任务详情"任务概况"首卡
// ——无卡壳单行，空态从一整张卡压成一行诚实空态
// 文案（"这里应该有计量"的可见性语义不变）。
// active：非终态任务 15s 轮询——
// 原实现只在挂载时拉一次，运行中用量在涨卡片却不动，看起来像"没生效"。
export default function TaskUsageCard({ taskId, variant = 'card', active = false }: { taskId: string; variant?: 'card' | 'inline'; active?: boolean }) {
  const { data, isError } = useQuery({
    queryKey: ['task-usage', taskId],
    queryFn: async () => (await api.get(`/v1/tasks/${taskId}/usage`)).data as TaskUsageSummary,
    // 计量随任务进行累积；终态后不再变化。staleTime 让 WS 帧驱动的缓存失效
    // （findings 同款节流）不必带动本卡——计量无需秒级新鲜度。
    staleTime: 30_000,
    refetchInterval: active ? 15_000 : false,
  });

  const summaryLine = (() => {
    if (!hasUsage(data)) return null;
    const calls = n(data!.totals.calls);
    const parts = [`tokens 合计 ${fmtTokens(n(data!.totals.total_tokens))}`];
    const models = data!.models.slice(0, 2).map((m) => `${m.model} ×${n(m.calls)}`).join('，');
    if (models) parts.push(models);
    if (calls > 0 && data!.models.length > 2) parts.push(`等 ${data!.models.length} 模型`);
    const pct = cacheReadPct(data!.totals);
    if (pct !== null) parts.push(`缓存读 ${pct}%`);
    return parts.join(' · ');
  })();

  // summaryLine 非空 ⇒ hasUsage(data)=真 ⇒ data 已定义——明细折叠面板只在有用量时构造
  // （props 求值随 JSX 创建发生，空用量下 `data!.models` 会炸，故不裸建）
  const detailCollapse = summaryLine ? (
    <Collapse
      ghost
      size="small"
      items={[{
        key: 'models',
        label: <Typography.Text type="secondary" style={{ fontSize: 12 }}>分模型明细</Typography.Text>,
        children: (
          <Table<TaskModelUsage>
            size="small"
            pagination={false}
            rowKey="model"
            dataSource={data!.models}
            columns={[
              { title: '模型', dataIndex: 'model', ellipsis: true },
              { title: '回合', dataIndex: 'calls', width: 64, align: 'right', render: (v) => n(v) },
              ...NUM_COLS.map((col) => ({
                title: {
                  input_tokens: '输入',
                  output_tokens: '输出',
                  cache_read_tokens: '缓存读',
                  cache_write_tokens: '缓存写',
                  total_tokens: '合计',
                }[col],
                dataIndex: col,
                width: 76,
                align: 'right' as const,
                render: (v: string | number) => fmtTokens(n(v)),
              })),
            ]}
          />
        ),
      }]}
    />
  ) : null;

  if (variant === 'inline') {
    return (
      <div style={{ marginTop: 6, display: 'flex', alignItems: 'baseline', flexWrap: 'wrap', columnGap: 8, rowGap: 2 }}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>计量</Typography.Text>
        {isError ? (
          // 接口错误与"真无用量"必须可区分
          <Typography.Text type="secondary" style={{ fontSize: 13 }}>计量暂不可用（usage 接口错误——归集未就绪或网络异常）</Typography.Text>
        ) : summaryLine ? (
          <>
            <Typography.Text style={{ fontSize: 13 }}>{summaryLine}</Typography.Text>
            {detailCollapse}
          </>
        ) : (
          <Typography.Text type="secondary" style={{ fontSize: 13 }}>暂无计量（任务未产生用量事件）</Typography.Text>
        )}
      </div>
    );
  }

  return (
    <Card size="small" title="计量" styles={{ body: { padding: '8px 16px' } }}>
      {isError ? (
        <Typography.Text type="secondary" style={{ fontSize: 13 }}>计量暂不可用（usage 接口错误——归集未就绪或网络异常）</Typography.Text>
      ) : summaryLine ? (
        <>
          <Typography.Text style={{ fontSize: 13 }}>{summaryLine}</Typography.Text>
          {detailCollapse}
        </>
      ) : (
        <Typography.Text type="secondary" style={{ fontSize: 13 }}>暂无计量（任务未产生用量事件）</Typography.Text>
      )}
    </Card>
  );
}
