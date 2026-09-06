// 项目详情（14号 §3.2 P0）：GET /v1/projects/:id + 源码来源只读展示 + 删除（admin 口径）
// ADR-181（人类反馈 2026-09-02）：详情页必须能看到项目关联的任务，否则查看详情无意义——
// 消费 ADR-160 已生效的 ListScanTasks project_id 服务端过滤（契约 L1108-1112）。
// ADR-203 补遗（审核意见②退役口径）：手填 project_path 配置表单移除——该档零存量、
// ADR-203 起零写入方；项目源码来源=上传件（config.upload_file_id，只读展示）或仓库地址。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Descriptions, Popconfirm, Table, Tag, Typography, message } from 'antd';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { api } from '../api/client';
import type { Project, ScanTask } from '../api/types';
import { SCAN_MODE, TASK_STATUS, zh } from '../dict';

export default function ProjectDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const { data: project, isLoading } = useQuery({
    queryKey: ['project', id],
    queryFn: async () => (await api.get(`/v1/projects/${id}`)).data as Project, // proto L890: 裸 Project
  });
  const { data: config } = useQuery({
    queryKey: ['project-config', id],
    queryFn: async () => (await api.get(`/v1/projects/${id}/config`)).data as {
      project_id: string; config: Record<string, string>;
    }, // proto L894: 裸 ProjectConfig（ADR-203: upload_file_id 只读展示）
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
  });

  if (isLoading) return <Typography.Text type="secondary">加载中…</Typography.Text>;

  return (
    <div>
      <Typography.Title level={3}>{project?.name ?? id}</Typography.Title>
      <Card style={{ marginBottom: 16 }}>
        <Descriptions column={2} size="small">
          <Descriptions.Item label="项目 ID">{project?.project_id}</Descriptions.Item>
          <Descriptions.Item label="仓库">{project?.repo_url}</Descriptions.Item>
          <Descriptions.Item label="默认分支">{project?.default_branch}</Descriptions.Item>
          <Descriptions.Item label="创建时间">{project?.created_at ?? '—'}</Descriptions.Item>
        </Descriptions>
        <Popconfirm title="确认删除该项目？" onConfirm={() => remove.mutate()}>
          <Button danger style={{ marginTop: 12 }}>删除项目</Button>
        </Popconfirm>
      </Card>

      <Card title={`关联任务（${tasks?.tasks?.length ?? 0}）`} style={{ marginBottom: 16 }}>
        <Table<ScanTask>
          rowKey="task_id"
          size="small"
          loading={tasksLoading}
          dataSource={tasks?.tasks ?? []}
          pagination={false}
          locale={{ emptyText: '该项目暂无任务——可在任务向导中选择本项目创建' }}
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
              render: (v: string) => <Tag color={v === 'TASK_STATUS_COMPLETED' ? 'success' : v === 'TASK_STATUS_RUNNING' ? 'processing' : v === 'TASK_STATUS_FAILED' || v === 'TASK_STATUS_DEAD' ? 'error' : 'default'}>{zh(TASK_STATUS, v)}</Tag>,
            },
            { title: '创建时间', dataIndex: 'created_at', render: (v: string | null) => v ?? '—' },
            { title: '重试', dataIndex: 'retry_count' },
          ]}
        />
      </Card>

      {/* ADR-203 补遗: 源码来源只读（项目持"当前"指针）——上传件 file_id 或仓库地址，
          手填 project_path 编辑表单已随 ADR-148 遗留档退役（零存量零写入方） */}
      <Card title="项目配置（project.config）" style={{ marginBottom: 16 }}>
        <Descriptions column={1} size="small">
          <Descriptions.Item label="源码来源">
            {config?.config?.upload_file_id
              ? `上传压缩包（${config.config.upload_file_id}）`
              : project?.repo_url
                ? `仓库（${project.repo_url}）`
                : '未配置（上传压缩包或填写仓库地址）'}
          </Descriptions.Item>
        </Descriptions>
      </Card>
    </div>
  );
}
