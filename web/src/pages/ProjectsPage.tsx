// 项目列表（14号 §3.2 P0）：GET /v1/projects；创建→POST（网关生成幂等键）
// ADR-203（人类 2026-09-05 裁决：保留弹窗上传入口并改造，推翻方案b移除）：
// 上传压缩包经 gateway 零落盘直传 storage（ADR-200 通道，返回 file_id），file_id 存
// 项目 config.upload_file_id；扫描任务启动时 task-service 从项目配置兜底解析拉包解包。
// 职责边界：项目=源码归属（上传包/仓库地址二选一），任务=扫描执行（config 留空，
// 源码来源由项目解析；任务级 config.upload_file_id 保留为单次覆盖档，见 ADR-200/202）。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Upload, type UploadFile } from 'antd';
import { Button, Form, Input, Modal, Select, Space, Table, Typography, message } from 'antd';
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { InboxOutlined } from '@ant-design/icons';
import {
  createProject,
  createTask,
  getProjects,
  updateProjectConfig,
  uploadArchive,
} from '../api/client';
import { autoRunTask } from '../tasks/stateMachine';
import { DEPRECATED_SCAN_MODES, SCAN_MODE, zh } from '../dict';
import type { Project } from '../api/types';

// ADR-182: 需要 SAST 工具的扫描模式（模式B 纯AI 无工具；含弃用模式自动任务兼容）
const NEEDS_TOOLS = new Set([
  'SCAN_MODE_SAST_ONLY',
  'SCAN_MODE_PARALLEL',
  'SCAN_MODE_COMPARE',
  'SCAN_MODE_TRADITIONAL_FIRST',
  'SCAN_MODE_SAST_REVIEW',
]);

export default function ProjectsPage() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [form] = Form.useForm();
  // ADR-203: 弹窗上传态——受控 fileList（下一步/上一步往返不适用此页，但弹窗重开需清空）
  // 与 uploadFileId 单一状态源；onRemove 同步清零，杜绝 ADR-202 在任务页修过的 file_id 残留。
  const [fileList, setFileList] = useState<UploadFile[]>([]);
  const [uploadFileId, setUploadFileId] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  // ADR-164: 服务端游标翻页（offset 游标+total, 契约 L1188-1189；DESC 排序保证新项目首页顶部）
  const [page, setPage] = useState(1);
  const PAGE_SIZE = 10;

  const { data, isLoading, error } = useQuery({
    queryKey: ['projects', page],
    queryFn: () => getProjects({ page_size: PAGE_SIZE, cursor: String((page - 1) * PAGE_SIZE) }),
  });
  const total = data?.pagination?.total ?? 0;

  const finishModal = () => {
    setOpen(false);
    form.resetFields();
    setFileList([]);
    setUploadFileId(null);
    qc.invalidateQueries({ queryKey: ['projects'] });
  };

  const create = useMutation({
    mutationFn: async (values: { name: string; repo_url?: string; default_branch: string; default_scan_mode: string }) => {
      // 上传与仓库二选一：都缺省在此拦截（repo_url 不设 antd 必填，避免"传了包还被逼填地址"）
      if (!uploadFileId && !values.repo_url?.trim()) {
        throw new Error('请上传代码压缩包或填写仓库地址（二选一）');
      }
      const resp = await createProject({
        name: values.name,
        ...(values.repo_url?.trim() ? { repo_url: values.repo_url.trim() } : {}),
        default_branch: values.default_branch,
        default_scan_mode: values.default_scan_mode,
      });
      const pid = resp.project_id;
      // ADR-203: 上传件 file_id 落项目 config（ADR-200 端点返回 file_id，不再有解包目录）
      if (pid && uploadFileId) {
        await updateProjectConfig(pid, { upload_file_id: uploadFileId });
      }
      return { pid, repoUrl: values.repo_url?.trim() ?? '' };
    },
    onSuccess: async ({ pid, repoUrl }) => {
      message.success('项目已创建');
      // ADR-162/163: 创建后按默认模式自动创建扫描任务并自动启动（审批流废除，2026-09-01）。
      // config 留空——源码来源由 task-service 启动时解析（项目 upload_file_id / project_path /
      // repo_url 三档兜底链，ADR-203），前端不再拼装任务级源码字段。
      if (pid && (uploadFileId || repoUrl)) {
        const mode = form.getFieldValue('default_scan_mode') as string;
        try {
          const t = await createTask({
            project_id: pid,
            scan_mode: mode,
            sast_tools: NEEDS_TOOLS.has(mode) ? ['opengrep'] : [],
            config: {},
          });
          message.success(uploadFileId ? '已自动创建扫描任务（启动时从存储拉回代码包），正在自动启动…' : '已自动创建扫描任务（启动时自动拉取仓库），正在自动启动…');
          finishModal();
          // 失效真实存在的任务缓存 key（['tasks-infinite'] 是无限滚动时代遗物，失效是 no-op）
          qc.invalidateQueries({ queryKey: ['tasks-page'] });
          qc.invalidateQueries({ queryKey: ['project-tasks'] });
          navigate(`/tasks/${t.task_id}`);
          // 人类指令 2026-09-01"创建项目后任务应自动执行"：创建→启动直达
          autoRunTask(t.task_id).then(() => {
            message.success('扫描任务已自动启动');
            qc.invalidateQueries({ queryKey: ['tasks-page'] });
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
    onError: (e) => message.error((e as Error).message || '创建失败（详见响应）'),
  });

  const columns = [
    { title: '项目', dataIndex: 'name', render: (_: unknown, rec: Project) => <Link to={`/projects/${rec.project_id}`}>{rec.name}</Link> },
    { title: 'ID', dataIndex: 'project_id' },
    { title: '仓库', dataIndex: 'repo_url', render: (v: string, rec: Project) => v || (rec.project_id ? '（上传压缩包）' : '—') },
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
            label="仓库地址（与上传压缩包二选一）"
            extra={uploadFileId ? '已上传压缩包——此栏可留空，项目以压缩包为源码' : '不填则必须上传代码压缩包'}
          >
            <Input placeholder="https://git.example.com/team/repo.git" />
          </Form.Item>
          {/* ADR-203: 零落盘直传——multipart 经 gateway 流式转发 storage（MinIO），
              不再有"解包目录/解包文件数"概念（旧 res.dir/res.files 契约已死，勿复活） */}
          <Form.Item label="代码压缩包（.zip/.tar.gz，≤25MB）">
            <Upload.Dragger
              accept=".zip,.tgz,.tar.gz"
              maxCount={1}
              fileList={fileList}
              disabled={uploading}
              beforeUpload={async (file) => {
                setUploading(true);
                try {
                  const res = await uploadArchive(file);
                  setUploadFileId(res.file_id);
                  setFileList([{ uid: res.file_id, name: file.name, status: 'done' }]);
                  message.success('上传成功——项目将以该压缩包为源码');
                } catch (e) {
                  message.error(`上传失败：${(e as Error).message}`);
                } finally {
                  setUploading(false);
                }
                return Upload.LIST_IGNORE; // 受控 fileList，禁 antd 自行追加
              }}
              onRemove={() => {
                setFileList([]);
                setUploadFileId(null);
              }}
            >
              <p className="ant-upload-drag-icon"><InboxOutlined /></p>
              <p className="ant-upload-text">点击或拖拽上传压缩包</p>
              <p className="ant-upload-hint">扫描任务启动时从存储拉回解包，解包目录即扫描目标（ADR-200）</p>
            </Upload.Dragger>
          </Form.Item>
          <Form.Item name="default_branch" label="默认分支" initialValue="main" rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="default_scan_mode" label="默认扫描模式" initialValue="SCAN_MODE_PARALLEL">
            <Select
              options={Object.entries(SCAN_MODE)
                .filter(([value]) => !DEPRECATED_SCAN_MODES.has(value)) // ADR-182: 弃用模式不进新建入口
                .map(([value, label]) => ({ value, label }))}
            />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
