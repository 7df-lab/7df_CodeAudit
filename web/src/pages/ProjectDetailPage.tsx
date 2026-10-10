// 项目详情（14号 §3.2 P0）：GET /v1/projects/:id + 源码来源展示与补配 + 删除（admin 口径）
// ADR-181（人类反馈 2026-09-02）：详情页必须能看到项目关联的任务，否则查看详情无意义——
// 消费 ADR-160 已生效的 ListScanTasks project_id 服务端过滤（契约 L1108-1112）。
// ADR-203 补遗（审核意见②退役口径）：手填 project_path 配置表单移除——该档零存量、
// ADR-203 起零写入方；项目源码来源=上传件（config.upload_file_id）或仓库地址。
// 2026-10-03 用户报障收口：补遗把配置面收成纯只读后，"向导就地建的无来源项目"再无
// 补配入口（向导"在项目页补齐"的指引是死路）——项目配置卡恢复编辑面（ProjectSourceModal：
// 仓库档=UpdateProject 整对象回写+清陈旧上传指针；上传档=uploadArchive 落 config），
// 建任务快捷弹窗对无来源项目亮警告并直达补配，不再放行必败任务。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Descriptions, Popconfirm, Table, Tag, Tooltip, Typography, message } from 'antd';
import dayjs from 'dayjs';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useState } from 'react';
import { api, apiErrorMessage, errStatus, getProject, getProjectConfig } from '../api/client';
import { useCanWrite } from '../auth/session';
import type { ScanTask } from '../api/types';
import { TaskNewModal } from './tasks/TaskNewPage';
import { ProjectSourceModal } from '../components/ProjectSourceModal';
import { SCAN_MODE, TASK_STATUS, zh } from '../dict';
import { MONO_FONT, STATUS_COLOR } from '../dict/tokens';
import PageHeader from '../components/PageHeader';
import { PageLoading } from '../components/states';
import { usePageTitle } from '../hooks/usePageTitle';
import { UNNAMED_ARCHIVE_LABEL } from '../utils/projectSource';

export default function ProjectDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const { data: project, isLoading } = useQuery({
    queryKey: ['project', id],
    queryFn: () => getProject(id), // proto L890: 裸 Project
  });
  const {
    data: config,
    isSuccess: configOk,
    isError: configErr,
    error: configError,
  } = useQuery({
    queryKey: ['project-config', id],
    queryFn: () => getProjectConfig(id), // proto L894: 裸 ProjectConfig（ADR-203: upload_file_id 展示）
  });
  // 源码来源是否已配置（解析链口径：项目上传件或仓库地址任一在位即可启动）。
  // config 查询未决（加载中）时不亮警告/不改按钮标签——"已决"=成功或 404（从未配置过
  // config 是合法态）；其余错误（5xx 等）不算"未配置"，不得据此亮警告误导（评审修正：
  // 原实现把一切 error 当 404，服务故障时页面谎报"未配置"）
  const configSettled = configOk || (configErr && errStatus(configError) === 404);
  const hasSource = !!(config?.config?.upload_file_id || project?.repo_url);
  const [sourceOpen, setSourceOpen] = useState(false);
  // 关联任务（ADR-160 project_id 过滤；列表口径与任务页一致）
  const { data: tasks, isLoading: tasksLoading } = useQuery({
    queryKey: ['project-tasks', id],
    queryFn: async () =>
      (await api.get('/v1/tasks', { params: { project_id: id, pagination: { page_size: 50 } } })).data as {
        tasks: ScanTask[];
      },
  });

  const remove = useMutation({
    mutationFn: async () => api.delete(`/v1/projects/${id}`),
    onSuccess: () => {
      message.success('项目已删除');
      qc.invalidateQueries({ queryKey: ['projects'] });
      navigate('/projects');
    },
    // B3-3（审计修复）：此前删除失败静默（Popconfirm 消失后无任何反馈）——补状态码
    onError: (e) => {
      const status = errStatus(e);
      message.error(`项目删除失败${status ? `（HTTP ${status}）` : ''}：${apiErrorMessage(e)}`);
    },
  });

  // 创建任务入口统一向导（用户裁决 2026-10-07"内容不统一"）：本页独立快捷弹窗退役。
  // 三次反馈收口（同日）：跳转 /tasks/new 会断回程——薄壳关闭落任务列表而非来源详情页，
  // 故本页作为向导第三宿主**原地悬浮**（TaskNewModal+presetProjectId 预选本项目），
  // 关闭即回详情页；向导是能力超集（编排/工具/Agent 可选、模式默认跟随项目
  // default_scan_mode、autoStart 默认勾选、无来源警告在参数步拦截+直达补配）。
  const [taskOpen, setTaskOpen] = useState(false);

  usePageTitle(project?.name ?? '项目详情');
  if (isLoading) return <PageLoading />;

  return (
    <div>
      <PageHeader title={project?.name ?? id} />
      <Card style={{ marginBottom: 16 }}>
        <Descriptions column={2} size="small">
          <Descriptions.Item label="项目 ID"><span style={{ fontFamily: MONO_FONT }}>{project?.project_id}</span></Descriptions.Item>
          <Descriptions.Item label="仓库">{project?.repo_url || '—'}</Descriptions.Item>
          <Descriptions.Item label="默认分支">{project?.default_branch}</Descriptions.Item>
          <Descriptions.Item label="创建时间">{project?.created_at ? dayjs(project.created_at).format('YYYY-MM-DD HH:mm:ss') : '—'}</Descriptions.Item>
        </Descriptions>
        {/* 悬浮向导（原地弹出，关闭回本页——不跳转不断回程）；ADR-257: 操作面按钮 VIEWER 隐藏，删除=admin（后端防线） */}
        {useCanWrite() && (
          <Button type="primary" style={{ marginTop: 12, marginRight: 8 }} onClick={() => setTaskOpen(true)}>
            创建扫描任务
          </Button>
        )}
        {useCanWrite() && (
          <Popconfirm title="确认删除该项目？" onConfirm={() => remove.mutate()}>
            <Button danger style={{ marginTop: 12 }}>删除项目</Button>
          </Popconfirm>
        )}
      </Card>

      <Card title={`关联任务（${tasks?.tasks?.length ?? 0}）`} style={{ marginBottom: 16 }}>
        <Table<ScanTask>
          rowKey="task_id"
          size="small"
          loading={tasksLoading}
          dataSource={tasks?.tasks ?? []}
          pagination={false}
          // 建任务引导（2026-09-11 用户报障）：空态直链任务向导并深链预选本项目
          locale={{
            emptyText: (
              <span>
                该项目暂无任务——<Link to={`/tasks/new?project_id=${id}`}>前往任务向导创建</Link>
              </span>
            ),
          }}
          columns={[
            {
              title: '任务',
              dataIndex: 'task_id',
              render: (v: string) => <Link to={`/tasks/${v}`}>{v}</Link>,
            },
            { title: '模式', dataIndex: 'scan_mode', render: (v: string) => zh(SCAN_MODE, v) },
            {
              title: '状态',
              dataIndex: 'status',
              render: (v: string) => <Tag color={STATUS_COLOR[v]}>{zh(TASK_STATUS, v)}</Tag>,
            },
            { title: '创建时间', dataIndex: 'created_at', render: (v: string | null) => (v ? dayjs(v).format('YYYY-MM-DD HH:mm:ss') : '—') },
            { title: '重试', dataIndex: 'retry_count' },
          ]}
        />
      </Card>

      {/* 2026-10-03 用户报障收口：源码来源展示 + 补配入口（此前纯只读——就地创建的
          无来源项目死路，任务启动必败）。编辑面=ProjectSourceModal：
          仓库档整对象回写+清陈旧上传指针 / 上传档落 config，与创建弹窗同契约 */}
      <Card
        title="项目配置"
        style={{ marginBottom: 16 }}
        extra={(
          <Button size="small" onClick={() => setSourceOpen(true)}>
            {/* hasSource 定标签；未知态（加载中/5xx）回落"配置源码"——弹窗自带 GET，
                任何态打开都安全，不误称"更换"（评审 P3a） */}
            {hasSource ? '更换源码' : '配置源码'}
          </Button>
        )}
      >
        <Descriptions column={1} size="small">
          <Descriptions.Item label="源码来源">
            {config?.config?.upload_file_id
              ? (
                // 上传件名只认 config.upload_file_name（web 创建/换源码路径写入；
                // API/e2e 上传路径不写名且原名经网关即弃、服务端无档——2026-10-08
                // 报障：无名不再注"创建于文件名记录启用前"（与实证不符），统一短
                // 文案；悬停 file_id 溯源，换源码重传即可补录名称）
                <Tooltip title={config.config.upload_file_id}>
                  {config.config.upload_file_name
                    ? `上传压缩包（${config.config.upload_file_name}）`
                    : `上传${UNNAMED_ARCHIVE_LABEL}`}
                </Tooltip>
              )
              : project?.repo_url
                ? `仓库（${project.repo_url}）`
                : configSettled
                  ? '未配置（上传压缩包或填写仓库地址）'
                  : configErr
                    ? '源码配置加载失败（状态未知）' // 非 404 错误不算"未配置"也不永挂"加载中"（评审 P3a：5xx 永显加载中=失真）
                    : '加载中…'}
          </Descriptions.Item>
        </Descriptions>
      </Card>

      <ProjectSourceModal
        open={sourceOpen}
        projectId={id}
        onClose={() => setSourceOpen(false)}
      />

      {/* 创建任务=向导悬浮窗（第三宿主，preset 预选本项目；关闭回原地不断回程） */}
      <TaskNewModal open={taskOpen} onClose={() => setTaskOpen(false)} presetProjectId={id} />
    </div>
  );
}
