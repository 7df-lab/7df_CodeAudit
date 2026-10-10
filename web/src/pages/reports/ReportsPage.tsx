// 报告中心（14号 §3.3 ⑧，P1）：GET /v1/reports + GET /v1/reports/{id} + 下载流聚合；
// FAILED 报告如实展示失败原因与“重新生成”（GenerateReport 幂等修复后可重试）。
// 注：报告 Status/ErrorMessage 不在 proto Report 字段内（L1263）——P4 原则下列表只展示
// proto 字段；报告不可下载/内容缺失时引导用“重新生成”。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Space, Table, Tag, Tooltip, Typography, message } from 'antd';
import type { TableProps } from 'antd';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import dayjs from 'dayjs';
import { useEffect, useState } from 'react';
import { api, errStatus, getReportContent, listAllProjects, listAllTasks, listReports, openReportWindow, regenerateReport } from '../../api/client';
import type { ReportRow } from '../../api/types';
import { REPORT_FORMAT, reportFileExt } from '../../dict';
import { MONO_FONT } from '../../dict/tokens';
import PageHeader from '../../components/PageHeader';
import { EmptyState } from '../../components/states';
import { usePageTitle } from '../../hooks/usePageTitle';

const fmtLabel = (f: string) => REPORT_FORMAT[f] ?? '—'; // UNSPECIFIED/历史未记录 → —（不显示"未知"误导）

export default function ReportsPage() {
  usePageTitle('报告中心');
  const qc = useQueryClient();
  // 任务↔报告双向导航（ADR-142 补全）：?task=<task_id> 过滤本任务报告
  const [params, setParams] = useSearchParams();
  const taskFilter = params.get('task') ?? '';
  // ADR-256：契约迁移 lastID 游标 → offset+total（任务/项目同款），
  // 并落地服务端排序/筛选——排序键白名单 report_id|task_id|format|generated_at，
  // 筛选白名单 format EQ；排序/筛选变化即回第一页；total=服务端真实分母。
  const PAGE_SIZE = 20;
  const [page, setPage] = useState(1);
  const [sorter, setSorter] = useState<{ field: string; order: 'SORT_ORDER_ASC' | 'SORT_ORDER_DESC' }>({ field: 'generated_at', order: 'SORT_ORDER_DESC' });
  const [formatFilter, setFormatFilter] = useState<string | null>(null);
  useEffect(() => { setPage(1); }, [taskFilter, sorter, formatFilter]);
  const { data, isLoading } = useQuery({
    queryKey: ['reports', taskFilter, page, sorter, formatFilter],
    queryFn: () => listReports({
      ...(taskFilter ? { task_id: taskFilter } : {}),
      pagination: { page_size: PAGE_SIZE, cursor: String((page - 1) * PAGE_SIZE) },
      ...(formatFilter ? { filter: { field: 'format', operator: 'FILTER_OPERATOR_EQ', value: formatFilter } } : {}),
      sort: sorter,
    }),
  });
  const total = data?.pagination?.total ?? 0;
  const sortOrderOf = (field: string): 'ascend' | 'descend' | undefined =>
    sorter.field === field ? (sorter.order === 'SORT_ORDER_ASC' ? 'ascend' : 'descend') : undefined;
  const onTableChange: TableProps<ReportRow>['onChange'] = (pag, filters, sort) => {
    setPage(pag.current ?? 1);
    const srt = (Array.isArray(sort) ? sort[0] : sort) as { field?: string; order?: string } | undefined;
    const next = srt?.field && srt.order
      ? { field: String(srt.field), order: srt.order === 'ascend' ? ('SORT_ORDER_ASC' as const) : ('SORT_ORDER_DESC' as const) }
      : null;
    // 纯翻页不带排序变化时不得换引用——否则 [sorter] 重置效应把页码打回第 1 页
    setSorter((prev) => (prev?.field === next?.field && prev?.order === next?.order ? prev : (next ?? prev)));
    const fmt = (filters?.format as (string | number | boolean)[] | null | undefined)?.[0];
    setFormatFilter(fmt != null ? String(fmt) : null);
  };

  // 2026-09-09 用户指令"报告中心应增加报告对应的项目名称和任务ID"：任务列本就在位，
  // 项目列经两级一次性索引解析（task_id→project_id→项目名）；索引未命中如实回落。
  // 2026-10-08：索引改 listAllTasks 全量（['tasks-all'] 共享缓存——此前 page_size 200
  // 被服务端钳 100，老报告的任务落索引外、项目列恒 —）
  const { data: tasksIndex } = useQuery({
    queryKey: ['tasks-all'],
    queryFn: listAllTasks,
    staleTime: 60_000,
  });
  const { data: projectsIndex } = useQuery({
    queryKey: ['projects-index'],
    queryFn: async () => ({ projects: await listAllProjects() }), // B5-P2-7: 全量翻页（200 被服务端钳 100）
    staleTime: 60_000,
  });
  const taskProject = (tid: string) => tasksIndex?.find((t) => t.task_id === tid)?.project_id ?? '';
  const projectName = (pid: string) => projectsIndex?.projects.find((p) => p.project_id === pid)?.name ?? pid;

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

  // 在线查看分流：JSON/CSV/UNSPECIFIED 历史档 → 站内阅读视图
  // /reports/:id（审计句+严重度光谱+逐条代码证据）；HTML → 排版成品，维持沙箱窗渲染；
  // PDF → 二进制档无文本可读，直接下载。（UNSPECIFIED 历史档实证同为 JSON 形状——
  // <host-id> sim 库 2026-10-08 抽样，阅读视图按内容嗅探兜底）
  const navigate = useNavigate();
  const view = async (rec: ReportRow) => {
    if (rec.format === 'REPORT_FORMAT_HTML') {
      try {
        // E-29① 文本形态取回（与阅读视图同口），沙箱窗渲染收口不变
        const { content } = await getReportContent(rec.report_id);
        const opened = openReportWindow(content, 'text/html');
        if (!opened) message.warning('弹出窗口被浏览器拦截，请允许弹出窗口后重试');
      } catch {
        message.error('查看失败（报告可能不存在或后端不可达）');
      }
      return;
    }
    if (rec.format === 'REPORT_FORMAT_PDF') {
      download(rec);
      message.info('PDF 报告无站内阅读视图，已开始下载');
      return;
    }
    navigate(`/reports/${rec.report_id}`);
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
      <PageHeader
        title={<>报告中心 {taskFilter && (
          <Tag closable color="blue" onClose={() => setParams({})}>
            任务：{taskFilter}
          </Tag>
        )}</>}
      />
      {/* 设计整改 P2: 列表容器统一为裸 Table（此前 Reports/Users 包 Card、Projects/Tasks 裸放） */}
      <Table
        rowKey="report_id"
        loading={isLoading}
        dataSource={data?.reports ?? []}
        pagination={{
          current: page,
          pageSize: PAGE_SIZE,
          total,
          showSizeChanger: false,
        }}
        onChange={onTableChange}
        locale={{ emptyText: <EmptyState description="暂无报告（任务完成后由编排器生成）" /> }}
          columns={[
            {
              // 2026-09-09 GUI 评审: 报告 ID 普遍 60+ 字符, 全量展示换行两行且无信息量
              title: '报告', dataIndex: 'report_id',
              sorter: true, sortDirections: ['descend', 'ascend'], sortOrder: sortOrderOf('report_id'),
              render: (v: string) => (
                <Tooltip title={v}>
                  <Link to={`/reports/${v}`} style={{ wordBreak: 'break-all', fontFamily: MONO_FONT, fontSize: 13 }}>
                    {v.length > 42 ? `${v.slice(0, 42)}…` : v}
                  </Link>
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
            {
              title: '格式', dataIndex: 'format',
              sorter: true, sortDirections: ['descend', 'ascend'], sortOrder: sortOrderOf('format'),
              filters: Object.entries(REPORT_FORMAT).map(([value, label]) => ({ value, text: String(label) })),
              filteredValue: formatFilter ? [formatFilter] : null,
              render: (f: string) => fmtLabel(f),
            },
            {
              title: '生成时间', dataIndex: 'generated_at',
              sorter: true, sortDirections: ['descend', 'ascend'], sortOrder: sortOrderOf('generated_at'), defaultSortOrder: 'descend' as const,
              render: (v: string | null) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '—'),
            },
            {
              title: '操作',
              render: (_: unknown, rec: ReportRow) => (
                <Space>
                  <Button size="small" onClick={() => view(rec)}>在线查看</Button>
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
    </div>
  );
}
