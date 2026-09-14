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
import { VERDICT_COLOR } from '../../dict/tokens';
import { PageLoading, QueryError } from '../../components/states';

export default function ReviewView({ taskId }: { taskId: string }) {
  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: ['review-findings', taskId],
    queryFn: async () =>
      (await api.get('/v1/findings', { params: { task_id: taskId, pagination: { page_size: 100 } } })).data as {
        findings: UnifiedFinding[];
        pagination?: { has_next?: boolean };
      },
  });

  //  补错误分支——此前查询失败永远停在加载文案（与 FusionView 同病）
  if (isLoading) return <PageLoading />;
  if (isError) return <QueryError error={error} onRetry={() => refetch()} />;
  const findings = data?.findings ?? [];
  // （审计修复）：视图单页拉 100 条截断——has_next=true 时如实提示，不再静默丢剩余
  const truncated = data?.pagination?.has_next === true;

  return (
    <div>
      <Typography.Title level={4}>审核视图（旧模式D）</Typography.Title>
      {truncated && (
        <Typography.Paragraph type="warning" style={{ marginBottom: 8 }}>
          仅显示前 100 条，请用发现页完整审阅
        </Typography.Paragraph>
      )}
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
          // 2026-09-13 间距整改（与 FindingsPage 同批）：fixed 布局 + 显式列宽——内嵌行展开
          // 的表格在 auto 布局下长结论/长路径会把列挤失衡，fixed 下宽度只由容器决定
          tableLayout="fixed"
          scroll={{ x: 640 }}
          expandable={{
            expandedRowRender: (rec: UnifiedFinding) => <FindingDetailBody findingId={rec.finding_id} />,
            rowExpandable: () => true,
            columnWidth: 84,
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
            { title: '发现', dataIndex: 'title', ellipsis: true },
            { title: '来源', dataIndex: 'source_tool', width: 96 },
            {
              title: '已落盘结论', dataIndex: 'ai_verdict', width: 128,
              // 空值显式归一"未判定"（zh 已无枚举特判，B3-5；展示行为不变）
              render: (v: string) => <Tag color={VERDICT_COLOR[v]}>{zh(AI_VERDICT, v || 'AI_VERDICT_UNSPECIFIED')}</Tag>,
            },
            {
              title: '结论理由（原文）', dataIndex: 'ai_reasoning',
              render: (v: string) => <Typography.Text style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{v || '—'}</Typography.Text>,
            },
          ]}
          locale={{ emptyText: '暂无发现' }}
        />
      </Card>
    </div>
  );
}
