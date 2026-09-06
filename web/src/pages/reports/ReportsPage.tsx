// 报告中心（14号 §3.3 ⑧，P1）：GET /v1/reports + GET /v1/reports/{id} + 下载流聚合；
// FAILED 报告如实展示失败原因与“重新生成”（GenerateReport 幂等修复后可重试）。
// 注：报告 Status/ErrorMessage 不在 proto Report 字段内（L1263）——P4 原则下列表只展示
// proto 字段；报告不可下载/内容缺失时引导用“重新生成”。
import { useMutation, useQuery } from '@tanstack/react-query';
import { Button, Card, Space, Table, Tag, Typography, message } from 'antd';
import { useSearchParams } from 'react-router-dom';
import dayjs from 'dayjs';
import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api } from '../../api/client';
import type { ReportRow } from '../../api/types';
import { REPORT_FORMAT, reportFileExt } from '../../dict';

const fmtLabel = (f: number) => REPORT_FORMAT[f] ?? '—'; // 0/历史未记录 → —（不显示"未知"误导）

export default function ReportsPage() {
  // 任务↔报告双向导航（ADR-142 补全）：?task=<task_id> 过滤本任务报告
  const [params, setParams] = useSearchParams();
  const taskFilter = params.get('task') ?? '';
  // ADR-164: 服务端游标翻页——报告游标为 lastID（不透明），仅可顺序前进：
  // cursors[i]=第 i+1 页请求游标（首页空串），翻页时收集 next_cursor 供下一页/回退使用。
  const PAGE_SIZE = 20;
  const [page, setPage] = useState(1);
  const [cursors, setCursors] = useState<string[]>(['']);
  useEffect(() => { setPage(1); setCursors(['']); }, [taskFilter]);
  const cursor = cursors[page - 1] ?? '';
  const { data, isLoading } = useQuery({
    queryKey: ['reports', taskFilter, page, cursor],
    queryFn: async () => (await api.get('/v1/reports', { params: {
      pagination: { page_size: PAGE_SIZE, cursor },
      ...(taskFilter ? { task_id: taskFilter } : {}),
    } })).data as {
      reports: ReportRow[];
      pagination?: { next_cursor?: string; has_next?: boolean };
    },
  });
  const hasNext = data?.pagination?.has_next ?? false;
  // total 不可知（lastID 游标契约无 total）——用 hasNext 推导"是否还有下一页"，
  // antd simple 模式仅前后翻页，不做跳页（跳不到未访问过的游标）。
  const total = page * PAGE_SIZE + (hasNext ? 1 : 0);
  const goPage = (p: number) => {
    if (p < 1 || p > page + 1) return;
    if (p === page + 1) {
      if (!hasNext || !data?.pagination?.next_cursor) return;
      setCursors((cs) => [...cs, data.pagination!.next_cursor!]);
    }
    setPage(p);
  };

  const regenerate = useMutation({
    mutationFn: async (taskId: string) => api.post(`/v1/tasks/${taskId}/report`, {}),
    onSuccess: () => message.success('报告生成请求已提交'),
  });

  // 在线查看：取回内容，HTML 新窗口渲染，JSON 新窗口 pretty 展示（ADR-142 报告真实化）
  const view = async (reportId: string) => {
    try {
      const resp = await api.get(`/v1/reports/${reportId}/download`, { responseType: 'blob' });
      const blob = resp.data as Blob;
      const url = URL.createObjectURL(blob);
      const head = await blob.slice(0, 1).text();
      if (head === '<') {
        window.open(url, '_blank');
      } else {
        const text = await blob.text();
        const w = window.open('', '_blank');
        if (w) {
          w.document.write('<pre style="font-size:13px;white-space:pre-wrap">' +
            JSON.stringify(JSON.parse(text), null, 2).replace(/[<>&]/g, (c) => ({'<':'&lt;','>':'&gt;','&':'&amp;'}[c] || c)) + '</pre>');
        }
      }
    } catch {
      message.error('查看失败（报告可能不存在或后端不可达）');
    }
  };

  const download = async (rec: ReportRow) => {
    try {
      const resp = await api.get(`/v1/reports/${rec.report_id}/download`, { responseType: 'blob' });
      const url = URL.createObjectURL(resp.data as Blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `${rec.report_id}.${reportFileExt(rec.format)}`; // 按格式给扩展名（此前恒 .bin）
      a.click();
      URL.revokeObjectURL(url);
    } catch {
      message.error('下载失败（报告可能不存在或后端不可达）');
    }
  };

  return (
    <div>
      <Space style={{ marginBottom: 8 }}>
        <Typography.Title level={3} style={{ margin: 0 }}>报告中心</Typography.Title>
        {taskFilter && (
          <Tag closable color="blue" onClose={() => setParams({})}>
            任务：{taskFilter}
          </Tag>
        )}
      </Space>
      <Card>
        <Table
          rowKey="report_id"
          loading={isLoading}
          dataSource={data?.reports ?? []}
          // ADR-164: 服务端游标翻页（lastID 不透明游标 → simple 模式前后翻页）
          pagination={{
            simple: true,
            current: page,
            pageSize: PAGE_SIZE,
            total,
            onChange: goPage,
            showSizeChanger: false,
          }}
          locale={{ emptyText: '暂无报告（任务完成后由编排器生成）' }}
          columns={[
            { title: '报告', dataIndex: 'report_id' },
            {
              title: '任务', dataIndex: 'task_id',
              render: (v: string) => <Link to={`/tasks/${v}`}>{v}</Link>,
            },
            { title: '格式', dataIndex: 'format', render: (f: number) => fmtLabel(f) },
            { title: '生成时间', dataIndex: 'generated_at', render: (v: string | null) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '—') },
            {
              title: '操作',
              render: (_: unknown, rec: ReportRow) => (
                <Space>
                  <Button size="small" onClick={() => view(rec.report_id)}>在线查看</Button>
                  <Button size="small" onClick={() => download(rec)}>下载</Button>
                </Space>
              ),
            },
          ]}
        />
      </Card>
      <Typography.Paragraph type="secondary" style={{ marginTop: 12 }}>
        报告由任务编排生成（04 §2 S9/S10）；下载经网关聚合 ReportChunk 服务端流。
        内存存储模式下，早期任务的详情可能已随重启清除（任务列点击提示"任务不存在"属预期）——报告文件本身仍可查看/下载。
      </Typography.Paragraph>
    </div>
  );
}
