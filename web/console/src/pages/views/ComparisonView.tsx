// 对比视图（14号 §3.3 ⑥，模式C）：四象限 + 指标表，数据=GET /v1/tasks/{id}/comparison-report
// 指标语义披露为固定脚注（ADR-133 口径：互为参照的自洽指标，非 DiverseVul 基准 F1）
import { useQuery } from '@tanstack/react-query';
import { Alert, Card, Descriptions, Statistic, Typography } from 'antd';
import { api } from '../../api/client';
import type { ComparisonReport } from '../../api/types';

export default function ComparisonView({ taskId }: { taskId: string }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ['comparison-report', taskId],
    queryFn: async () =>
      (await api.get(`/v1/tasks/${taskId}/comparison-report`)).data as ComparisonReport,
  });

  if (isLoading) return <Typography.Text type="secondary">加载中…</Typography.Text>;
  if (error || !data?.summary) {
    return <Alert type="warning" showIcon message="对比报告不可用（任务未完成或后端不可达）" />;
  }
  const s = data.summary;
  const m = s.metrics;

  return (
    <div>
      <Typography.Title level={4}>对比视图（模式C 四象限）</Typography.Title>
      <Card style={{ marginBottom: 16 }}>
        <Descriptions column={3} size="small">
          <Descriptions.Item label="共同发现">{s.both_found}</Descriptions.Item>
          <Descriptions.Item label="仅 SAST">{s.sast_only}</Descriptions.Item>
          <Descriptions.Item label="仅 AI">{s.ai_only}</Descriptions.Item>
          <Descriptions.Item label="结论分歧">{s.disagreement}</Descriptions.Item>
          <Descriptions.Item label="SAST 总数">{s.sast_total}</Descriptions.Item>
          <Descriptions.Item label="AI 总数">{s.ai_total}</Descriptions.Item>
        </Descriptions>
      </Card>
      <Card title="指标（ComparisonMetrics，proto L491-L501）">
        {/* 数据直显（P4）；七个指标全部来自后端计算（ADR-133），页面零计算 */}
        <Statistic title="SAST precision" value={m?.sast_precision ?? 0} precision={3} style={{ display: 'inline-block', marginRight: 24 }} />
        <Statistic title="SAST recall" value={m?.sast_recall ?? 0} precision={3} style={{ display: 'inline-block', marginRight: 24 }} />
        <Statistic title="SAST F1" value={m?.sast_f1 ?? 0} precision={3} style={{ display: 'inline-block', marginRight: 24 }} />
        <Statistic title="AI precision" value={m?.ai_precision ?? 0} precision={3} style={{ display: 'inline-block', marginRight: 24 }} />
        <Statistic title="AI recall" value={m?.ai_recall ?? 0} precision={3} style={{ display: 'inline-block', marginRight: 24 }} />
        <Statistic title="AI F1" value={m?.ai_f1 ?? 0} precision={3} style={{ display: 'inline-block' }} />
      </Card>
      <Alert
        style={{ marginTop: 16 }}
        type="info"
        showIcon
        message="指标口径披露（固定脚注）"
        description="sast_* 以 AI 发现集为参照（TP=共同发现, FP=仅SAST, FN=仅AI），ai_* 反之——模式C互为参照的自洽指标，非 DiverseVul 全量基准 F1（后者走 TP11-T2 evaluate_f1 链路）。"
      />
    </div>
  );
}
