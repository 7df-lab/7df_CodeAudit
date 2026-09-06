// 对比视图（14号 §3.3 ⑥；模式E，ADR-186 前称"模式D"）：SAST 与 AI 并行各自完成后的同维度三分桶对比
// —— 单SAST / 单AI / SAST+AI 三类并排，附互为参照指标；数据=GET /v1/tasks/{id}/comparison-report
// 指标语义披露为固定脚注（ADR-133 口径：互为参照的自洽指标，非 DiverseVul 基准 F1）
import { useQuery } from '@tanstack/react-query';
import { Alert, Card, Col, Row, Statistic, Typography } from 'antd';
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
      <Typography.Title level={4}>对比视图（模式E：SAST 与 AI 并行审计，同维度三分桶）</Typography.Title>
      <Row gutter={16}>
        <Col span={8}>
          <Card>
            <Statistic title="SAST+AI（共同发现）" value={s.both_found} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              双方各自独立检出、指向同一问题；其中结论相左 {s.disagreement} 条
            </Typography.Text>
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="单SAST（仅工具检出）" value={s.sast_only} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              规则命中但 AI 语义审计未认可——工具特异性或噪音候选
            </Typography.Text>
          </Card>
        </Col>
        <Col span={8}>
          <Card>
            <Statistic title="单AI（仅 AI 检出）" value={s.ai_only} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              AI 语义审计独有——逻辑缺陷/SAST 规则盲区
            </Typography.Text>
          </Card>
        </Col>
      </Row>
      <Card style={{ marginTop: 16 }} title="输入规模">
        <Descriptions2 s={s} />
      </Card>
      <Card style={{ marginTop: 16 }} title="指标（ComparisonMetrics，proto L491-L501）">
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
        description="sast_* 以 AI 发现集为参照（TP=共同发现, FP=仅SAST, FN=仅AI），ai_* 反之——模式E 互为参照的自洽指标，非 DiverseVul 全量基准 F1（后者走 TP11-T2 evaluate_f1 链路）。"
      />
    </div>
  );
}

function Descriptions2({ s }: { s: NonNullable<ComparisonReport['summary']> }) {
  return (
    <Row gutter={24}>
      <Col><Statistic title="SAST 总发现" value={s.sast_total} /></Col>
      <Col><Statistic title="AI 总发现" value={s.ai_total} /></Col>
      <Col><Statistic title="唯一发现合计" value={s.metrics?.total_unique ?? s.both_found + s.sast_only + s.ai_only} /></Col>
    </Row>
  );
}
