// 项目详情（14号 §3.2 P0）：GET /v1/projects/:id + config 读写 + 删除（admin 口径）
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Card, Descriptions, Form, Input, Popconfirm, Typography, message } from 'antd';
import { useNavigate, useParams } from 'react-router-dom';
import { api } from '../api/client';
import type { Project } from '../api/types';

export default function ProjectDetailPage() {
  const { id = '' } = useParams();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [form] = Form.useForm();

  const { data: project, isLoading } = useQuery({
    queryKey: ['project', id],
    queryFn: async () => (await api.get(`/v1/projects/${id}`)).data as Project, // proto L890: 裸 Project
  });
  const { data: config } = useQuery({
    queryKey: ['project-config', id],
    queryFn: async () => (await api.get(`/v1/projects/${id}/config`)).data as {
      project_id: string; config: Record<string, string>;
    }, // proto L894: 裸 ProjectConfig
  });

  const saveConfig = useMutation({
    mutationFn: async (values: { config: Record<string, string> }) =>
      api.put(`/v1/projects/${id}/config`, { config: { project_id: id, config: values.config } }),
    onSuccess: () => {
      message.success('配置已保存');
      qc.invalidateQueries({ queryKey: ['project-config', id] });
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
      <Card title="项目配置（project.config）">
        <Form form={form} layout="inline" initialValues={{ config: config?.config ?? {} }} onFinish={(v) => saveConfig.mutate(v)}>
          <Form.Item name={['config', 'project_path']} label="project_path">
            <Input placeholder="网关宿主机路径（14号 Q4：V1 限制已如实标注）" style={{ width: 360 }} />
          </Form.Item>
          <Button type="primary" htmlType="submit" loading={saveConfig.isPending}>
            保存
          </Button>
        </Form>
      </Card>
    </div>
  );
}
