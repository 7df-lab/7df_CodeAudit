// 项目源码来源配置弹窗。ADR-203 补遗把详情页项目配置收成
// 只读后，"向导就地创建的项目/无来源项目"没有任何补配入口——向导警告指向"项目页补齐"
// 是死路，任务启动必败（task-service 解析链三档全空 → project_path 未配置 FAILED）。
// 本弹窗补上事后配置面（后端 PUT 双通道早已有之，纯前端收口）：
//   上传档（优先）→ uploadArchive file_id 落 config（与 ProjectsPage 创建弹窗同契约）；
//   仓库档 → UpdateProject 整对象回写（服务端无字段合并，必须 GET 全量后整提交）
//            + 清项目级上传指针——解析链上传档优先于仓库（ADR-200/203），不清则换仓库
//            永不被读到（陈旧 upload_file_id 静默压新仓库）。
// 二选一口径与项目列表页创建弹窗一致（传了包仓库栏可留空）；repo_url 过服务端安全
// 白名单，400 详情如实透出。config PUT 为整 map 替换——重写前先 GET 现状保无关键。
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Form, Input, Modal, Typography, message } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { apiErrorMessage, getProject, getProjectConfig, updateProject, updateProjectConfig } from '../api/client';
import { ArchiveUploadDragger, type ArchiveUploadValue } from './ArchiveUpload';

// 清项目级上传指针后的 config 形状（解析链上传档的键域：upload_file_id/name 成对）
type ProjectConfigMap = Record<string, string>;

// 剔除上传两键（仓库档必做：陈旧指针会把新仓库静默压住）。config PUT 是整 map
// 替换——无关键必须原样带回，故此处只删不整map重建。
function withoutUploadKeys(config: ProjectConfigMap | undefined): ProjectConfigMap {
  const rest = { ...(config ?? {}) };
  delete rest.upload_file_id;
  delete rest.upload_file_name;
  return rest;
}

export function ProjectSourceModal({ open, projectId, onClose }: {
  open: boolean;
  projectId: string;
  onClose: () => void;
}) {
  const [form] = Form.useForm();
  const qc = useQueryClient();
  const [upload, setUpload] = useState<ArchiveUploadValue>({ fileId: null, fileName: '' });
  // 与宿主页共用 ['project'/'project-config', id] 缓存键——打开即取现状，保存后双端同刷
  const { data: project } = useQuery({
    queryKey: ['project', projectId],
    queryFn: () => getProject(projectId),
    enabled: open && !!projectId,
  });
  const { data: config } = useQuery({
    queryKey: ['project-config', projectId],
    queryFn: () => getProjectConfig(projectId),
    enabled: open && !!projectId,
  });

  // 预填只走"open 上升沿 × 每项目一次"——依赖里带 project 时后台 refetch（窗口聚焦等）
  // 会用 GET 快照 setFieldsValue 吞掉用户正在输入的仓库地址。
  // 关闭沿 resetFields 回收，重开不残留上次输入。
  const prefilledRef = useRef(''); // '' = 未预填；否则为已预填的 project_id
  useEffect(() => {
    if (!open) {
      prefilledRef.current = '';
      form.resetFields();
      return;
    }
    if (!project || prefilledRef.current === project.project_id) return;
    prefilledRef.current = project.project_id;
    setUpload({ fileId: null, fileName: '' });
    form.setFieldsValue({ repo_url: project.repo_url ?? '' });
  }, [open, project, form]);

  const save = useMutation({
    mutationFn: async (values: { repo_url?: string }) => {
      const repoUrl = values.repo_url?.trim() ?? '';
      if (!upload.fileId && !repoUrl) {
        throw new Error('请上传代码压缩包或填写仓库地址（二选一）');
      }
      if (!project) throw new Error('项目信息尚未加载完成，请稍后重试');
      // 与创建弹窗同口径：传了压缩包则以压缩包为源码（仓库栏忽略）
      if (upload.fileId) {
        const next = withoutUploadKeys(config?.config);
        next.upload_file_id = upload.fileId;
        if (upload.fileName) next.upload_file_name = upload.fileName;
        await updateProjectConfig(projectId, next);
        return;
      }
      await updateProject(projectId, { ...project, repo_url: repoUrl });
      // 第二步失败=仓库已换但陈旧上传件仍优先生效（恰是本弹窗要根除的状态）——
      // 如实报半程状态，不报"成功"不静默（评审发现 c1：非原子两步写）
      try {
        await updateProjectConfig(projectId, withoutUploadKeys(config?.config));
      } catch (e) {
        // 原始错误附后（评审 P3b：半程文案不能吞 HTTP 归因——500 与 409 处置不同）
        throw new Error(`仓库地址已保存，但清理旧上传件失败——旧压缩包仍会优先生效；请重试"保存"，或到项目配置中重新上传/删除压缩包（${apiErrorMessage(e)}）`);
      }
    },
    onSuccess: () => {
      message.success('源码来源已更新');
      qc.invalidateQueries({ queryKey: ['project', projectId] });
      qc.invalidateQueries({ queryKey: ['project-config', projectId] });
      onClose();
    },
    onError: (e) => message.error(`源码配置失败：${apiErrorMessage(e)}`),
  });

  return (
    <Modal
      title="配置源码来源"
      open={open}
      onCancel={() => { form.resetFields(); onClose(); }}
      onOk={() => form.submit()}
      confirmLoading={save.isPending}
      okText="保存"
      width={520}
    >
      <Typography.Text type="secondary" style={{ display: 'block', marginBottom: 12 }}>
        项目「{project?.name ?? projectId}」的源码来源——扫描启动时由后端解析（项目压缩包优先于仓库地址）。
      </Typography.Text>
      <Form form={form} layout="vertical" onFinish={(v) => save.mutate(v as { repo_url?: string })}>
        <Form.Item
          name="repo_url"
          label="仓库地址（与上传压缩包二选一）"
          extra={upload.fileId ? '已上传压缩包——此栏可留空，项目以压缩包为源码' : '不填则必须上传代码压缩包'}
        >
          <Input placeholder="https://git.example.com/team/repo.git" allowClear />
        </Form.Item>
        <Form.Item label="代码压缩包（.zip/.tar.gz，≤100MB）">
          <ArchiveUploadDragger value={upload} onChange={setUpload} />
        </Form.Item>
      </Form>
    </Modal>
  );
}
