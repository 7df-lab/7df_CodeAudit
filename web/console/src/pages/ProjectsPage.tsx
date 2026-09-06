// 项目列表（14号 §3.2 P0）：GET /v1/projects；创建→POST（网关生成幂等键）
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Form, Input, Modal, Select, Space, Table, Typography, Upload, message } from 'antd';
import { UploadOutlined } from '@ant-design/icons';
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { api, uploadArchive } from '../api/client';
import { autoRunTask } from '../tasks/stateMachine';
import { SCAN_MODE, zh } from '../dict';

interface Project {
  project_id: string;
  name: string;
  repo_url: string;
  default_branch: string;
  default_scan_mode: string;
  created_at: string | null;
}

// ADR-162: 需要 SAST 工具的扫描模式（模式A 纯AI 无工具；其余按向导口径需选工具）
const NEEDS_TOOLS = new Set([
  'SCAN_MODE_TRADITIONAL_FIRST',
  'SCAN_MODE_PARALLEL',
  'SCAN_MODE_SAST_REVIEW',
]);

export default function ProjectsPage() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [uploadedDir, setUploadedDir] = useState<string | null>(null);
  const [form] = Form.useForm();
  // ADR-164: 服务端游标翻页（offset 游标+total, 契约 L1188-1189；DESC 排序保证新项目首页顶部）
  const [page, setPage] = useState(1);
  const PAGE_SIZE = 10;

  const { data, isLoading, error } = useQuery({
    queryKey: ['projects', page],
    queryFn: async () => (await api.get('/v1/projects', {
      params: { pagination: { page_size: PAGE_SIZE, cursor: String((page - 1) * PAGE_SIZE) } },
    })).data as {
      projects: Project[];
      pagination?: { total?: number; has_next?: boolean };
    },
  });
  const total = data?.pagination?.total ?? 0;

  const finishModal = () => {
    setUploadedDir(null);
    setOpen(false);
    form.resetFields();
    qc.invalidateQueries({ queryKey: ['projects'] });
  };

  const create = useMutation({
    mutationFn: async (values: { name: string; repo_url?: string; default_branch: string; default_scan_mode: string }) =>
      (await api.post('/v1/projects', { project: values })).data,
    onSuccess: async (resp: { project?: { project_id: string }; project_id?: string }, variables: { name: string; repo_url?: string; default_branch: string; default_scan_mode: string }) => {
      message.success('项目已创建');
      // proto L889: CreateProject 返回裸 Project（非 wrapped）；兼容两种形态
      const pid = resp.project?.project_id ?? resp.project_id;
      let codeSource: string | null = null;
      // ADR-145: 若随弹窗上传了压缩包，把解包目录写入项目 config（供任务创建默认使用）
      if (uploadedDir && pid) {
        try {
          await api.put(`/v1/projects/${pid}/config`, {
            // proto L1121 ProjectConfig: config 为 map<string,string>，project_path 是 map 条目
            config: { project_id: pid, config: { project_path: uploadedDir } },
          });
          message.success('已关联上传的代码目录');
          codeSource = uploadedDir;
        } catch {
          message.warning('项目已创建，但代码目录关联失败（可在项目详情重试）');
        }
      }
      // ADR-162/163（人类需求"仓库与上传双通道均支持四类模式"）：上传代码或配置仓库的
      // 项目，创建后按默认模式自动创建扫描任务并自动启动（审批流废除，2026-09-01）。
      // 上传→config 路径直扫；仓库→config 留空，启动时后端 git clone（失败任务诚实 FAILED）。
      if (pid && (codeSource || variables.repo_url)) {
        const mode = variables.default_scan_mode;
        try {
          const t = await api.post('/v1/tasks', {
            project_id: pid,
            scan_mode: mode,
            sast_tools: NEEDS_TOOLS.has(mode) ? ['opengrep'] : [],
            config: codeSource ? { project_path: codeSource } : {},
          });
          const taskId = (t.data as { task_id: string }).task_id;
          message.success(codeSource
            ? '已按项目默认模式自动创建扫描任务，正在自动启动…'
            : '已自动创建扫描任务（启动时自动拉取仓库），正在自动启动…');
          finishModal();
          qc.invalidateQueries({ queryKey: ['tasks-infinite'] });
          navigate(`/tasks/${taskId}`);
          // 人类指令 2026-09-01"创建项目后任务应自动执行"：链式 提交→批准→启动
          autoRunTask(taskId).then(() => {
            message.success('扫描任务已自动启动');
            qc.invalidateQueries({ queryKey: ['tasks-infinite'] });
          }).catch((e) => {
            message.warning(`任务已创建但自动启动失败（${(e as Error).message}），可在任务页手动续走`);
          });
          return;
        } catch {
          message.warning('任务自动创建失败——请到"新建任务"手动创建（项目已就绪）');
        }
      }
      finishModal();
    },
    onError: () => message.error('创建失败（详见响应）'),
  });

  const columns = [
    { title: '项目', dataIndex: 'name', render: (_: unknown, rec: Project) => <Link to={`/projects/${rec.project_id}`}>{rec.name}</Link> },
    { title: 'ID', dataIndex: 'project_id' },
    { title: '仓库', dataIndex: 'repo_url' },
    { title: '默认分支', dataIndex: 'default_branch' },
    { title: '默认模式', dataIndex: 'default_scan_mode', render: (m: string) => zh(SCAN_MODE, m) },
  ];

  return (
    <div>
      {/* ADR-164: 工具栏布局与任务页一致（标题左/新建右） */}
      <Space style={{ marginBottom: 16, justifyContent: 'space-between', width: '100%' }}>
        <Typography.Title level={3} style={{ margin: 0 }}>项目</Typography.Title>
        <Button type="primary" onClick={() => setOpen(true)}>新建项目</Button>
      </Space>
      <Table
        rowKey="project_id"
        loading={isLoading}
        dataSource={data?.projects ?? []}
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
        locale={{ emptyText: error ? '加载失败（服务不可用）' : '暂无项目，点击右上角创建' }}
      />
      <Modal
        title="新建项目"
        open={open}
        onCancel={() => setOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={create.isPending}
      >
        <Form form={form} layout="vertical" onFinish={(v) => create.mutate(v)}>
          <Form.Item name="name" label="名称" rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item
            name="repo_url"
            label="仓库地址（可选：上传压缩包时无需填写）"
            rules={[{
              // ADR-150: 上传压缩包流程没有 Git 仓库——有上传目录时不再强制 repo_url
              validator: async (_, value) => {
                if (!uploadedDir && !value) throw new Error('填写仓库地址或上传代码压缩包');
              },
            }]}
          >
            <Input
              placeholder="https://git.example.com/team/repo.git"
              disabled={!!uploadedDir}
            />
          </Form.Item>
          <Form.Item name="default_branch" label="默认分支" initialValue="main" rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="default_scan_mode" label="默认扫描模式" initialValue="SCAN_MODE_TRADITIONAL_FIRST">
            <Select
              options={Object.entries(SCAN_MODE).map(([value, label]) => ({ value, label }))}
            />
          </Form.Item>
          <Form.Item label="上传代码压缩包（可选，zip/tar.gz ≤25MB）">
            <Upload
              maxCount={1}
              accept=".zip,.tgz,.tar.gz"
              beforeUpload={async (file) => {
                try {
                  const res = await uploadArchive(file);
                  setUploadedDir(res.dir);
                  message.success(`已上传并解包 ${res.files} 个文件`);
                } catch {
                  message.error('上传失败（仅支持 zip/tar.gz，≤25MB）');
                }
                return false;
              }}
              onRemove={() => setUploadedDir(null)}
            >
              <Button icon={<UploadOutlined />}>选择文件</Button>
            </Upload>
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
