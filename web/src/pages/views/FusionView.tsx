// 融合视图（14号 §3.3 ⑤，模式B）：数据即 findings 本身——dedup_group/matched_findings/is_unique
// 是 proto UnifiedFinding 融合字段（L84-86）。P2：不展示任何页面计算的"融合分"。
import { useQuery } from '@tanstack/react-query';
import { Card, Row, Tag, Typography } from 'antd';
import { api } from '../../api/client';
import type { UnifiedFinding } from '../../api/types';
import { SEVERITY, zh } from '../../dict';
import { SEVERITY_COLOR } from '../../dict/tokens';
import { PageLoading, QueryError } from '../../components/states';

export default function FusionView({ taskId }: { taskId: string }) {
  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: ['fusion-findings', taskId],
    queryFn: async () =>
      (await api.get('/v1/findings', { params: { task_id: taskId, pagination: { page_size: 100 } } })).data as {
        findings: UnifiedFinding[];
        pagination?: { has_next?: boolean };
      },
  });

  //  补错误分支——此前查询失败永远停在加载文案（与 ReviewView 同病）
  if (isLoading) return <PageLoading />;
  if (isError) return <QueryError error={error} onRetry={() => refetch()} />;
  const findings = data?.findings ?? [];
  // （审计修复）：视图单页拉 100 条截断——has_next=true 时如实提示，不再静默丢剩余
  const truncated = data?.pagination?.has_next === true;

  // dedup_group 非空 → 组视图；空 → 未合并分区
  const groups = new Map<string, UnifiedFinding[]>();
  const uniques: UnifiedFinding[] = [];
  for (const f of findings) {
    if (f.dedup_group) {
      const arr = groups.get(f.dedup_group) ?? [];
      arr.push(f);
      groups.set(f.dedup_group, arr);
    } else {
      uniques.push(f);
    }
  }

  return (
    <div>
      <Typography.Title level={4}>融合结果</Typography.Title>
      {truncated && (
        <Typography.Paragraph type="warning" style={{ marginBottom: 8 }}>
          仅显示前 100 条，请用发现页完整审阅
        </Typography.Paragraph>
      )}
      <Typography.Paragraph type="secondary">
        同一位置的多个来源发现已合并为一条：扫描工具发现作为主条目，AI 结论并入其中；
        下方“独立发现”为仅单一来源报告的问题。
      </Typography.Paragraph>

      {groups.size === 0 && uniques.length === 0 && (
        <Typography.Text type="secondary">暂无发现（任务完成或无命中）</Typography.Text>
      )}

      {[...groups.entries()].map(([gid, members]) => {
        const primary = members.find((m) => m.source_tool !== 'ai_agent') ?? members[0];
        const others = members.filter((m) => m.finding_id !== primary.finding_id);
        return (
          <Card key={gid} size="small" style={{ marginBottom: 12 }} title={`合并组 ${gid.replace(/^group_/, '')}（${members.length} 条）`}>
            <Typography.Text strong>
              主条目：<Tag color="blue">{primary.source_tool}</Tag> {primary.title}
            </Typography.Text>
            <div style={{ marginTop: 8 }}>
              <Typography.Text type="secondary">并入的发现：</Typography.Text>
              {others.map((o) => (
                <Tag key={o.finding_id}>
                  {o.source_tool}: {o.title}
                </Tag>
              ))}
              {others.length === 0 && <Tag>无（其余成员与主条目相同，已合并）</Tag>}
            </div>
            <div style={{ marginTop: 8 }}>
              <Tag color={SEVERITY_COLOR[primary.severity]}>{zh(SEVERITY, primary.severity)}</Tag>
              <Tag>{primary.cwe_id || 'CWE—'}</Tag>
            </div>
          </Card>
        );
      })}

      {uniques.length > 0 && (
        <>
          <Typography.Title level={5} style={{ marginTop: 16 }}>
            独立发现（仅单一来源报告）
          </Typography.Title>
          <Row gutter={[8, 8]}>
            {uniques.map((f) => (
              <Card key={f.finding_id} size="small" style={{ marginBottom: 8, width: '100%' }}>
                <Tag color="blue">{f.source_tool}</Tag> {f.title}{' '}
                <Tag color={SEVERITY_COLOR[f.severity]}>{zh(SEVERITY, f.severity)}</Tag>
              </Card>
            ))}
          </Row>
        </>
      )}
    </div>
  );
}
