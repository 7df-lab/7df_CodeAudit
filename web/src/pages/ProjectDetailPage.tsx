// 项目详情（14号 §3.2 P0）：GET /v1/projects/:id + 源码来源只读展示 + 删除（admin 口径）
// ADR-181（人类反馈 2026-09-02）：详情页必须能看到项目关联的任务，否则查看详情无意义——
// 消费 ADR-160 已生效的 ListScanTasks project_id 服务端过滤（契约 L1108-1112）。
// ADR-203 补遗（审核意见②退役口径）：手填 project_path 配置表单移除——该档零存量、
// ADR-203 起零写入方；项目源码来源=上传件（config.upload_file_id，只读展示）或仓库地址。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Checkbox, Descriptions, Form, Modal, Popconfirm, Select, Table, Tag, Tooltip, Typography, message } from 'antd';
import dayjs from 'dayjs';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useState } from 'react';
import { api, createTask, errStatus, getProject, getProjectConfig } from '../api/client';
import type { ScanTask } from '../api/types';
import { autoRunTask } from '../tasks/stateMachine';
import { MODE_SPECS } from './tasks/TaskNewPage';
import { SCAN_MODE, TASK_STATUS, zh } from '../dict';
import { MONO_FONT, STATUS_COLOR } from '../dict/tokens';
import PageHeader from '../components/PageHeader';
import { PageLoading } from '../components/states';
import { usePageTitle } from '../hooks/usePageTitle';

export default function ProjectDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const { data: project, isLoading } = useQuery({
    queryKey: ['project', id],
    queryFn: () => getProject(id), // proto L890: 裸 Project
  });
  const { data: config } = useQuery({
    queryKey: ['project-config', id],
    queryFn: () => getProjectConfig(id), // proto L894: 裸 ProjectConfig（ADR-203: upload_file_id 只读展示）
  });
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
    // （审计修复）：此前删除失败静默（Popconfirm 消失后无任何反馈）——补状态码
    onError: (e) => {
      const status = errStatus(e);
      message.error(`项目删除失败${status ? `（HTTP ${status}）` : ''}：${(e as Error).message}`);
    },
  });

  // 2026-09-09 用户指令: 详情页直达"创建扫描任务"——源码由项目层级解析（config 留空，
  // 启动时 task-service 走 ADR-203 兜底链），工具取项目默认口径 opengrep（与项目页自动任务一致）
  const [taskOpen, setTaskOpen] = useState(false);
  const [taskForm] = Form.useForm();
  const createTaskMut = useMutation({
    mutationFn: async (values: { scan_mode: string; auto_start: boolean }) => {
      const resp = await createTask({
        project_id: id,
        scan_mode: values.scan_mode,
        sast_tools: MODE_SPECS[values.scan_mode]?.needsSastTools ? ['opengrep'] : [],
        config: {},
      });
      return { taskId: resp.task_id, autoStart: values.auto_start };
    },
    onSuccess: ({ taskId, autoStart }) => {
      message.success('任务已创建');
      qc.invalidateQueries({ queryKey: ['project-tasks', id] });
      qc.invalidateQueries({ queryKey: ['tasks-page'] });
      setTaskOpen(false);
      navigate(`/tasks/${taskId}`);
      if (autoStart) {
        autoRunTask(taskId)
          .then(() => message.success('扫描任务已自动启动'))
          .catch((e) => message.warning(`自动启动失败（${(e as Error).message}），可在任务页手动续走`));
      }
    },
    onError: (e) => message.error(`任务创建失败：${(e as Error).message}`),
  });

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
        <Button type="primary" style={{ marginTop: 12, marginRight: 8 }} onClick={() => setTaskOpen(true)}>
          创建扫描任务
        </Button>
        <Popconfirm title="确认删除该项目？" onConfirm={() => remove.mutate()}>
          <Button danger style={{ marginTop: 12 }}>删除项目</Button>
        </Popconfirm>
      </Card>

      <Modal
        title="创建扫描任务"
        open={taskOpen}
        onCancel={() => setTaskOpen(false)}
        confirmLoading={createTaskMut.isPending}
        onOk={() => taskForm.submit()}
      >
        {/* 源码来源=项目层级（config 留空）；此处只选模式与启动方式 */}
        <Form form={taskForm} layout="vertical" initialValues={{ scan_mode: 'SCAN_MODE_PARALLEL', auto_start: true }}
          onFinish={(v) => createTaskMut.mutate(v as { scan_mode: string; auto_start: boolean })}>
          <Form.Item name="scan_mode" label="扫描模式" rules={[{ required: true }]}>
            <Select
              options={Object.entries(SCAN_MODE)
                .filter(([value]) => !MODE_SPECS[value]?.deprecated && value !== 'SCAN_MODE_UNSPECIFIED') // R: UNSPECIFIED 不进新建入口
                .map(([value, label]) => ({ value, label }))}
            />
          </Form.Item>
          <Form.Item name="auto_start" valuePropName="checked" style={{ marginBottom: 0 }}>
            <Checkbox>创建后立即启动（不勾则停在待启动）</Checkbox>
          </Form.Item>
        </Form>
      </Modal>

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

      {/* ADR-203 补遗: 源码来源只读（项目持"当前"指针）——上传件 file_id 或仓库地址，
          手填 project_path 编辑表单已随 ADR-148 遗留档退役（零存量零写入方） */}
      <Card title="项目配置" style={{ marginBottom: 16 }}>
        <Descriptions column={1} size="small">
          <Descriptions.Item label="源码来源">
            {config?.config?.upload_file_id
              ? (
                // 2026-09-09 GUI 评审: 存量项目无 upload_file_name（该键 2026-09-09 起才落），
                // 裸内部 file_id 对用户无信息量——回退为说明文案, file_id 悬停可见
                <Tooltip title={config.config.upload_file_id}>
                  {config.config.upload_file_name
                    ? `上传压缩包（${config.config.upload_file_name}）`
                    : '上传压缩包（创建于文件名记录启用前，未留存名称）'}
                </Tooltip>
              )
              : project?.repo_url
                ? `仓库（${project.repo_url}）`
                : '未配置（上传压缩包或填写仓库地址）'}
          </Descriptions.Item>
        </Descriptions>
      </Card>
    </div>
  );
}
