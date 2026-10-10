// 项目列表（14号 §3.2 P0）：GET /v1/projects
// 职责边界：项目=源码归属（上传包/仓库地址二选一），任务=扫描执行（config 留空，
// 源码来源由项目解析；任务级 config.upload_file_id 保留为单次覆盖档，见 ADR-200/202）。
// 创建弹窗自本页抽出为 ProjectCreateModal 共享件（流程统一批 2026-10-07）：项目页按钮
// 直开 + /projects/new 薄壳页共用，与任务侧 TaskNewModal 同构；?new=1 旧深链改为
// 兼容重定向至 /projects/new（此前就地开弹窗+剥参，与任务侧两套范式）。
import type { TableProps } from 'antd';
import { useQuery } from '@tanstack/react-query';
import { Button, Card, Table, Typography } from 'antd';
import { useEffect, useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { getProjectConfig, getProjects } from '../api/client';
import { useCanWrite } from '../auth/session';
import { SCAN_MODE, zh } from '../dict';
import PageHeader from '../components/PageHeader';
import { ProjectCreateModal } from '../components/ProjectCreateModal';
import { EmptyState, ListSkeleton } from '../components/states';
import type { Project } from '../api/types';
import { usePageTitle } from '../hooks/usePageTitle';
import { NO_SOURCE_LABEL, projectSourceShape, UNNAMED_ARCHIVE_LABEL } from '../utils/projectSource';

// 仓库列的上传件名单元格（2026-09-09 用户指令"项目页应显示上传压缩包的名称"）：
// 原始文件名不进 storage 对象键（uploads/<id><ext>，网关只拿它判扩展名），落项目
// config.upload_file_name（web 创建/换源码路径写入）。
// 2026-10-08 报障修复（口径收口 utils/projectSource.ts）：①无源码项目此前被无条件
// 假称"上传压缩包"——config 已决且无上传件 → 如实"未配置源码"；②无名上传（API/e2e
// 路径不写名，服务端无档可恢复）→ 统一短文案，不再冒充也没有冗长注解。
function UploadNameCell({ projectId }: { projectId: string }) {
  const { data, isSuccess, isError } = useQuery({
    queryKey: ['project-config', projectId],
    queryFn: () => getProjectConfig(projectId),
    staleTime: 60_000,
  });
  const shape = projectSourceShape(null, data?.config);
  if (isSuccess && shape.kind === 'none') {
    return <Typography.Text type="secondary">{NO_SOURCE_LABEL}</Typography.Text>;
  }
  if (isError) {
    return <Typography.Text type="secondary">源码状态未知</Typography.Text>;
  }
  return (
    <Typography.Text title={shape.fileId}>
      {shape.kind === 'named-archive' ? shape.fileName
        : shape.kind === 'unnamed-archive' ? UNNAMED_ARCHIVE_LABEL
        : '…'}
    </Typography.Text>
  );
}

export default function ProjectsPage() {
  usePageTitle('项目');
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  // ?new=1 旧深链（总览"新建项目"入口曾用）→ 兼容重定向 /projects/new（保书签/外链；
  // 流程统一批 2026-10-07 前为就地开弹窗+剥参）。副作用在 useEffect（render 期
  // navigate/setState 是路由期写副作用——StrictMode 双调告警，2026-09-21 教训）。
  const [searchParams] = useSearchParams();
  const newParam = searchParams.get('new');
  useEffect(() => {
    if (newParam === '1') navigate('/projects/new', { replace: true });
  }, [newParam, navigate]);
  // ADR-164: 服务端游标翻页（offset 游标+total, 契约 L1188-1189；DESC 排序保证新项目首页顶部）
  // ADR-256: 服务端排序/筛选（sorter/filter 变化即回第一页；排序键白名单 name|created_at，
  // 筛选白名单 default_scan_mode EQ——与任务页同款服务端语义，量级增长不受前端约束）
  const [page, setPage] = useState(1);
  const PAGE_SIZE = 10;
  const [sorter, setSorter] = useState<{ field: string; order: 'SORT_ORDER_ASC' | 'SORT_ORDER_DESC' } | null>(null);
  const [modeFilter, setModeFilter] = useState<string | null>(null);
  useEffect(() => { setPage(1); }, [sorter, modeFilter]);

  const { data, isLoading, error } = useQuery({
    queryKey: ['projects', page, sorter, modeFilter],
    queryFn: () => getProjects({
      pagination: { page_size: PAGE_SIZE, cursor: String((page - 1) * PAGE_SIZE) },
      ...(modeFilter ? { filter: { field: 'default_scan_mode', operator: 'FILTER_OPERATOR_EQ', value: modeFilter } } : {}),
      ...(sorter ?? {}),
    }),
  });
  const total = data?.pagination?.total ?? 0;
  const rows = data?.projects ?? [];

  // 列头排序/筛选 → 服务端参数（受控：sortOrder/filteredValue 由 state 派生）
  const onTableChange = (_pag: unknown, filters: Record<string, unknown>, sort: unknown) => {
    const srt = (Array.isArray(sort) ? sort[0] : sort) as { field?: string; order?: string } | undefined;
    const next = srt?.order
      ? { field: String(srt.field), order: srt.order === 'ascend' ? ('SORT_ORDER_ASC' as const) : ('SORT_ORDER_DESC' as const) }
      : null;
    // 值等不换引用（防 [sorter] 重置效应在纯翻页时把页码打回首页）
    setSorter((prev) => (prev?.field === next?.field && prev?.order === next?.order ? prev : next));
    const mode = (filters?.mode as (string | number | boolean)[] | null | undefined)?.[0];
    setModeFilter(mode != null ? String(mode) : null);
  };

  const columns: TableProps<Project>['columns'] = [
    {
      title: '项目', dataIndex: 'name',
      sorter: true,
      sortOrder: sorter?.field === 'name' ? (sorter.order === 'SORT_ORDER_ASC' ? 'ascend' : 'descend') : undefined,
      render: (_: unknown, rec: Project) => <Link to={`/projects/${rec.project_id}`}>{rec.name}</Link>,
    },
    { title: 'ID', dataIndex: 'project_id' },
    // 2026-09-09 用户指令: 上传型项目显示压缩包原始文件名（不再千篇一律"（上传压缩包）"）
    { title: '源码', dataIndex: 'repo_url', render: (v: string, rec: Project) => v || (rec.project_id ? <UploadNameCell projectId={rec.project_id} /> : '—') },
    { title: '默认分支', dataIndex: 'default_branch' },
    {
      title: '默认模式', dataIndex: 'default_scan_mode',
      sorter: true,
      sortOrder: sorter?.field === 'default_scan_mode' ? (sorter.order === 'SORT_ORDER_ASC' ? 'ascend' : 'descend') : undefined,
      filters: Object.entries(SCAN_MODE).map(([value, label]) => ({ value, text: String(label) })),
      filteredValue: modeFilter ? [modeFilter] : null,
      render: (m: string) => zh(SCAN_MODE, m),
    },
  ];

  return (
    <div>
      {/* ADR-164: 工具栏布局与任务页一致（标题左/新建右）；设计整改 P1: PageHeader 统一 */}
      <PageHeader title="项目" extra={useCanWrite()
        ? <Button type="primary" onClick={() => setOpen(true)}>新建项目</Button> : undefined} />
      {isLoading && rows.length === 0 ? (
        <Card><ListSkeleton /></Card>
      ) : (
        <Table
          rowKey="project_id"
          loading={isLoading}
          dataSource={rows}
          columns={columns}
          // ADR-164: 服务端翻页（此前 ADR-161 曾关闭隐式分页——现改为显式游标翻页,
          // DESC 排序保证新项目始终在第一页顶部）
          pagination={{
            current: page,
            pageSize: PAGE_SIZE,
            total,
            onChange: (p) => setPage(p),
            showSizeChanger: false,
          }}
          onChange={onTableChange}
          locale={{ emptyText: <EmptyState description={error
            ? ((error as { response?: { status?: number } })?.response?.status === 403
                ? '无权限查看项目列表（当前角色受限）' // ADR-257: 403≠服务不可用，不再误导排障方向
                : '项目列表加载失败（服务不可用）')
            : '暂无项目，点击右上角"新建项目"创建'} /> }}
        />
      )}
      <ProjectCreateModal open={open} onClose={() => setOpen(false)} />
    </div>
  );
}
