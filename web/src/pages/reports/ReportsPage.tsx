// 报告中心（14号 §3.3 ⑧，P1）：GET /v1/reports + GET /v1/reports/{id} + 下载流聚合；
// FAILED 报告如实展示失败原因与“重新生成”（GenerateReport 幂等修复后可重试）。
// 注：报告 Status/ErrorMessage 不在 proto Report 字段内（L1263）——P4 原则下列表只展示
// proto 字段；报告不可下载/内容缺失时引导用“重新生成”。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Space, Table, Tag, Tooltip, Typography, message } from 'antd';
import { Link, useSearchParams } from 'react-router-dom';
import dayjs from 'dayjs';
import { useEffect, useState } from 'react';
import { api, errStatus, getProjects, openReportWindow, regenerateReport } from '../../api/client';
import type { ReportRow } from '../../api/types';
import { REPORT_FORMAT, reportFileExt } from '../../dict';

const fmtLabel = (f: number) => REPORT_FORMAT[f] ?? '—'; // 0/历史未记录 → —（不显示"未知"误导）

export default function ReportsPage() {
  const qc = useQueryClient();
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

  // 2026-09-09 用户指令"报告中心应增加报告对应的项目名称和任务ID"：任务列本就在位，
  // 项目列经两级一次性索引解析（task_id→project_id→项目名）；索引未命中如实回落。
  const { data: tasksIndex } = useQuery({
    queryKey: ['tasks-index'],
    queryFn: async () =>
      (await api.get('/v1/tasks', { params: { pagination: { page_size: 200, cursor: '0' } } })).data as {
        tasks: { task_id: string; project_id: string }[];
      },
    staleTime: 60_000,
  });
  const { data: projectsIndex } = useQuery({
    queryKey: ['projects-index'],
    queryFn: () => getProjects({ page_size: 200 }),
    staleTime: 60_000,
  });
  const taskProject = (tid: string) => tasksIndex?.tasks.find((t) => t.task_id === tid)?.project_id ?? '';
  const projectName = (pid: string) => projectsIndex?.projects.find((p) => p.project_id === pid)?.name ?? pid;
  const goPage = (p: number) => {
    if (p < 1 || p > page + 1) return;
    if (p === page + 1) {
      if (!hasNext || !data?.pagination?.next_cursor) return;
      setCursors((cs) => [...cs, data.pagination!.next_cursor!]);
    }
    setPage(p);
  };

  // B3-5（审计修复/D4 裁定接线）：重新生成此前 mutation 挂空无按钮——
  // 报告不可下载/内容缺失或需要刷新时从此处重发 GenerateReport（幂等），成功即失效列表
  const regenerate = useMutation({
    mutationFn: regenerateReport,
    onSuccess: () => {
      message.success('报告生成请求已提交');
      qc.invalidateQueries({ queryKey: ['reports'] });
    },
    onError: (e) => {
      const status = errStatus(e);
      message.error(`重新生成失败${status ? `（HTTP ${status}）` : ''}：${(e as Error).message}`);
    },
  });

  // 在线查看：取回内容，统一经 sandboxed iframe 新窗口渲染（HTML 原样 / JSON 转义 <pre>）
  const view = async (reportId: string) => {
    try {
      const resp = await api.get(`/v1/reports/${reportId}/download`, { responseType: 'blob' });
      const blob = resp.data as Blob;
      const head = await blob.slice(0, 1).text();
      const text = await blob.text();
      // 审计 B3-2 纵深防御（fix-plan-0911 §14 升级）：HTML 不再直开 blob: URL（无法前置
      // 注入 CSP）、也不再 document.write 直写——统一经 openReportWindow 的 sandboxed
      // iframe 渲染（脚本全灭，CSP meta 前置双保险，注入收口在 client.ts）
      if (head === '<') {
        openReportWindow(text, 'text/html');
      } else {
        openReportWindow('<pre style="font-size:13px;white-space:pre-wrap">' +
          JSON.stringify(JSON.parse(text), null, 2).replace(/[<>&]/g, (c) => ({'<':'&lt;','>':'&gt;','&':'&amp;'}[c] || c)) + '</pre>', 'text/html');
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
            {
              // 2026-09-09 GUI 评审: 报告 ID 普遍 60+ 字符, 全量展示换行两行且无信息量
              title: '报告', dataIndex: 'report_id',
              render: (v: string) => (
                <Tooltip title={v}>
                  <Typography.Text style={{ wordBreak: 'break-all' }}>
                    {v.length > 42 ? `${v.slice(0, 42)}…` : v}
                  </Typography.Text>
                </Tooltip>
              ),
            },
            {
              // 2026-09-09 用户指令: 报告对应项目名称（未命中索引如实显示 ID/—）
              title: '项目', dataIndex: 'task_id',
              render: (v: string) => {
                const pid = taskProject(v);
                return pid
                  ? <Link to={`/projects/${pid}`} title={pid}>{projectName(pid)}</Link>
                  : <Typography.Text type="secondary">—</Typography.Text>;
              },
            },
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
                  {/* B3-5 接线：重新生成=重发 GenerateReport（幂等）；仅本条转圈 */}
                  <Button
                    size="small"
                    disabled={!rec.task_id}
                    title={rec.task_id ? undefined : '历史报告缺任务 ID，无法重发生成'}
                    loading={regenerate.isPending && regenerate.variables === rec.task_id}
                    onClick={() => regenerate.mutate(rec.task_id)}
                  >
                    重新生成
                  </Button>
                </Space>
              ),
            },
          ]}
        />
      </Card>
      <Typography.Paragraph type="secondary" style={{ marginTop: 12 }}>
        报告由扫描任务完成后自动生成；旧报告对应的历史任务可能已被清理，点击任务列提示“不存在”属预期——报告文件本身仍可在线查看与下载。
      </Typography.Paragraph>
    </div>
  );
}
