// 融合视图（14号 §3.3 ⑤，模式B）：数据即 findings 本身——dedup_group/matched_findings/is_unique
// 是 proto UnifiedFinding 融合字段（L84-86）。P2：不展示任何页面计算的"融合分"。
import { useQuery } from '@tanstack/react-query';
import { Card, Row, Tag, Typography } from 'antd';
import { api } from '../../api/client';
import type { UnifiedFinding } from '../../api/types';
import { SEVERITY, zh } from '../../dict';

export default function FusionView({ taskId }: { taskId: string }) {
  const { data, isLoading } = useQuery({
    queryKey: ['fusion-findings', taskId],
    queryFn: async () =>
      (await api.get('/v1/findings', { params: { task_id: taskId, pagination: { page_size: 100 } } })).data as {
        findings: UnifiedFinding[];
      },
  });

  if (isLoading) return <Typography.Text type="secondary">加载中…</Typography.Text>;
  const findings = data?.findings ?? [];

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
      <Typography.Title level={4}>融合视图（合并组 → 去重对齐）</Typography.Title>
      <Typography.Paragraph type="secondary">
        依据 proto L84-86：组内成员共享 dedup_group，primary 为保留的 SAST 主发现；下方“未合并发现”为单侧独有项。
      </Typography.Paragraph>

      {groups.size === 0 && uniques.length === 0 && (
        <Typography.Text type="secondary">暂无发现（任务完成或无命中）</Typography.Text>
      )}

      {[...groups.entries()].map(([gid, members]) => {
        const primary = members.find((m) => m.source_tool !== 'ai_agent') ?? members[0];
        const others = members.filter((m) => m.finding_id !== primary.finding_id);
        return (
          <Card key={gid} size="small" style={{ marginBottom: 12 }} title={`合并组 ${gid}（${members.length} 条）`}>
            <Typography.Text strong>
              primary: <Tag color="blue">{primary.source_tool}</Tag> {primary.title}
            </Typography.Text>
            <div style={{ marginTop: 8 }}>
              <Typography.Text type="secondary">组成员：</Typography.Text>
              {others.map((o) => (
                <Tag key={o.finding_id}>
                  {o.source_tool}: {o.title}
                </Tag>
              ))}
              {others.length === 0 && <Tag>无（其余成员已按去重对齐并入 primary）</Tag>}
            </div>
            <div style={{ marginTop: 8 }}>
              <Tag color="red">{zh(SEVERITY, primary.severity)}</Tag>
              <Tag>{primary.cwe_id || 'CWE—'}</Tag>
            </div>
          </Card>
        );
      })}

      {uniques.length > 0 && (
        <>
          <Typography.Title level={5} style={{ marginTop: 16 }}>
            未合并发现（is_unique，单侧独有）
          </Typography.Title>
          <Row gutter={[8, 8]}>
            {uniques.map((f) => (
              <Card key={f.finding_id} size="small" style={{ marginBottom: 8, width: '100%' }}>
                <Tag color="blue">{f.source_tool}</Tag> {f.title}{' '}
                <Tag color="red">{zh(SEVERITY, f.severity)}</Tag>
              </Card>
            ))}
          </Row>
        </>
      )}
    </div>
  );
}
