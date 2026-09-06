// 审核视图（14号 §3.3 ⑦，旧模式D，已弃用 ADR-182）。
// 诚实声明（ADR-139 设计缺口）：ReviewSASTResults 的 AuditReviewReport（OverallAssessment+
// 逐条 opinion）随 RPC 返回但无持久化查询通道（proto 无对应读取 RPC）——本视图展示**已持久化**
// 的逐条结论（result-service 的 ai_verdict/ai_reasoning），不伪造审核报告。零 LLM 参与的批次，
// 后端写入 NEEDS_MANUAL+原因，原样可见。
import { useQuery } from '@tanstack/react-query';
import { Alert, Button, Card, Table, Tag, Typography } from 'antd';
import { api } from '../../api/client';
import FindingDetailBody from '../findings/FindingDetailBody';
import type { UnifiedFinding } from '../../api/types';
import { AI_VERDICT, zh } from '../../dict';

export default function ReviewView({ taskId }: { taskId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['review-findings', taskId],
    queryFn: async () =>
      (await api.get('/v1/findings', { params: { task_id: taskId, pagination: { page_size: 100 } } })).data as {
        findings: UnifiedFinding[];
      },
  });

  if (isLoading) return <Typography.Text type="secondary">加载中…</Typography.Text>;
  const findings = data?.findings ?? [];

  return (
    <div>
      <Typography.Title level={4}>审核视图（旧模式D）</Typography.Title>
      <Alert
        style={{ marginBottom: 16 }}
        type="warning"
        showIcon
        message="审核报告（整体评估+逐条 opinion）当前未持久化"
        description="ReviewSASTResults 的 AuditReviewReport 随 RPC 返回、无读取通道（proto 缺口，ADR-139）。本页展示已落盘的逐条结论；零 LLM 参与批次为 NEEDS_MANUAL——请人工复核，不冒充已审核。"
      />
      <Card>
        <Table
          rowKey="finding_id"
          size="small"
          dataSource={findings}
          expandable={{
            expandedRowRender: (rec: UnifiedFinding) => <FindingDetailBody findingId={rec.finding_id} />,
            rowExpandable: () => true,
            expandIcon: (props: import('rc-table/es/interface').RenderExpandIconProps<UnifiedFinding>) => (
              <Button
                size="small"
                type={props.expanded ? 'default' : 'primary'}
                onClick={(e) => props.onExpand(props.record, e)}
              >
                {props.expanded ? '收起' : '风险详情'}
              </Button>
            ),
          }}
          pagination={false}
          columns={[
            { title: '发现', dataIndex: 'title' },
            { title: '来源', dataIndex: 'source_tool', width: 110 },
            {
              title: '已落盘结论', dataIndex: 'ai_verdict', width: 140,
              render: (v: string) => <Tag>{zh(AI_VERDICT, v)}</Tag>,
            },
            {
              title: '结论理由（原文）', dataIndex: 'ai_reasoning',
              render: (v: string) => <Typography.Text style={{ whiteSpace: 'pre-wrap' }}>{v || '—'}</Typography.Text>,
            },
          ]}
          locale={{ emptyText: '暂无发现' }}
        />
      </Card>
    </div>
  );
}
