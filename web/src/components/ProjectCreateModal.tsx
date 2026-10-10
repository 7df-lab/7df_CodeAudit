// 新建项目弹窗——项目页与 /projects/new 薄壳页共用宿主（流程统一批 2026-10-07）。
// 自 ProjectsPage 抽出：此前项目创建 Modal 只活在项目列表页，/projects/new 深链无法
// 落地（任务侧 /tasks/new 薄壳已有先例）；抽出后两条创建流程入口同构
// （<资源>/new 薄壳页 + 页内按钮直开弹窗）。
// 自动链显式化（P1-4）：此前"创建后自动建任务并启动"是隐式副作用（ADR-162/163，
// 人类指令 2026-09-01"创建项目后任务应自动执行"）；本批改为表单内显式复选框
// （默认勾选=保持该指令的默认行为），与任务向导 autoStart/详情页快捷弹窗同口径。
// 任务 payload 经 buildScanTaskPayload 单一事实源组装（B5-P1-3 三份拷贝收敛）。
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Button, Checkbox, Form, Input, Modal, Select, message } from 'antd';
import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { apiErrorMessage, createTask } from '../api/client';
import { ArchiveUploadDragger, type ArchiveUploadValue } from './ArchiveUpload';
import { autoRunTask } from '../tasks/stateMachine';
import { DEPRECATED_SCAN_MODES, SCAN_MODE } from '../dict';
import { buildScanTaskPayload } from '../tasks/scanModes';
import { createProjectWithSource } from '../utils/projectCreate';

export function ProjectCreateModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [form] = Form.useForm();
  // ADR-203: 弹窗上传态——受控值对象单一状态源（fileId+fileName；chip/上传中锁随
  // ArchiveUploadDragger 内部走），onRemove/重开由组件与收尾双向清零，
  // 杜绝 ADR-202 在任务页修过的 file_id 残留。
  const [upload, setUpload] = useState<ArchiveUploadValue>({ fileId: null, fileName: '' });

  // B4-2（审计修复）：弹窗收尾清空（确定/取消共用）——旧 file_id 残留会随 config
  // 写入新项目（源码指向错包）。取消不触发 invalidate（无服务端变更）。
  const resetModalState = () => {
    form.resetFields();
    setUpload({ fileId: null, fileName: '' });
  };
  const finishModal = () => {
    onClose();
    resetModalState();
    qc.invalidateQueries({ queryKey: ['projects'] });
  };

  const create = useMutation({
    mutationFn: async (values: { name: string; repo_url?: string; default_branch: string; default_scan_mode: string; auto_scan?: boolean }) => {
      // 创建链（二选一拦截+createProject+上传件落 config）经 createProjectWithSource
      // 共享（与向导 Step0 就地创建同源；configFailed=部分成功标记，onSuccess 处理）
      const { project, configFailed } = await createProjectWithSource(values, upload);
      return {
        pid: project.project_id,
        repoUrl: values.repo_url?.trim() ?? '',
        configFailed,
        mode: values.default_scan_mode,
        autoScan: values.auto_scan !== false,
      };
    },
    onSuccess: async ({ pid, repoUrl, configFailed, mode, autoScan }) => {
      if (configFailed) {
        message.warning('项目已创建，但压缩包关联失败——请到项目详情页补配源码');
        finishModal();
        navigate(`/projects/${pid}`);
        return;
      }
      message.success('项目已创建');
      // ADR-162/163: 按默认模式自动创建扫描任务并自动启动（审批流废除，2026-09-01）。
      // P1-4 显式化：auto_scan 复选框默认勾选=默认行为不变，用户可只建项目不扫。
      // config 留空——源码来源由 task-service 启动时解析（项目 upload_file_id /
      // repo_url 兜底链，ADR-203），前端不再拼装任务级源码字段。
      if (pid && autoScan && (upload.fileId || repoUrl)) {
        try {
          const t = await createTask(buildScanTaskPayload({ project_id: pid, scan_mode: mode }));
          message.success(upload.fileId ? '已自动创建扫描任务（启动时从存储拉回代码包），正在自动启动…' : '已自动创建扫描任务（启动时自动拉取仓库），正在自动启动…');
          finishModal();
          // 失效真实存在的任务缓存 key（['tasks-infinite'] 是无限滚动时代遗物，失效是 no-op）；
          // ['tasks-all']=任务域全集（I-99，统计带/索引），创建即失效防旧计数冒充
          qc.invalidateQueries({ queryKey: ['tasks-all'] });
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
    onError: (e) => message.error(`创建失败：${apiErrorMessage(e)}`),
  });

  return (
    <Modal
      title="新建项目"
      width={640}
      open={open}
      onCancel={() => { onClose(); resetModalState(); }}
      onOk={() => form.submit()}
      confirmLoading={create.isPending}
    >
      <Form form={form} layout="vertical" onFinish={(v) => create.mutate(v)}>
        <Form.Item name="name" label="名称" rules={[{ required: true }]}>
          <Input maxLength={64} showCount />
        </Form.Item>
        <Form.Item
          name="repo_url"
          label="仓库地址（与上传压缩包二选一）"
          extra={upload.fileId ? '已上传压缩包——此栏可留空，项目以压缩包为源码' : '不填则必须上传代码压缩包'}
        >
          <Input placeholder="https://git.example.com/team/repo.git" />
        </Form.Item>
        {/* ADR-203: 零落盘直传——multipart 经 gateway 流式转发 storage（MinIO），
            不再有"解包目录/解包文件数"概念（旧 res.dir/res.files 契约已死，勿复活）。
            上传交互抽 ArchiveUploadDragger 共享件（详情页配置源码弹窗同款）。 */}
        <Form.Item label="代码压缩包（.zip/.tar.gz，≤100MB）">
          <ArchiveUploadDragger value={upload} onChange={setUpload} />
        </Form.Item>
        <Form.Item name="default_branch" label="默认分支" initialValue="main" rules={[{ required: true }]}>
          <Input />
        </Form.Item>
        <Form.Item name="default_scan_mode" label="默认扫描模式" initialValue="SCAN_MODE_PARALLEL">
          <Select
            options={Object.entries(SCAN_MODE)
              .filter(([value]) => !DEPRECATED_SCAN_MODES.has(value) && value !== 'SCAN_MODE_UNSPECIFIED') // ADR-182 弃用 + R: UNSPECIFIED 不进新建入口
              .map(([value, label]) => ({ value, label }))}
          />
        </Form.Item>
        <Form.Item name="auto_scan" valuePropName="checked" initialValue={true} style={{ marginBottom: 0 }}>
          <Checkbox>创建后立即按默认模式扫描并启动（不勾则只建项目，稍后经"新建任务"发起）</Checkbox>
        </Form.Item>
      </Form>
    </Modal>
  );
}
