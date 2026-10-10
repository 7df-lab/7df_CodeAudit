// 项目创建链共享纯函数：二选一拦截 + createProject + 上传件
// 落 config 的组合，此前在 ProjectCreateModal 与向导 Step0 就地创建两处逐字重复
// （Standards 轴 Duplicated Code 发现）。调用方差异只在成功后的 UI 动作，由各自
// onSuccess 处理；configFailed=部分成功标记（项目已建、源码关联失败——如实呈现，
// 交互审计 Top4-③ 口径，不误报整体失败诱发重复建项目）。
import { createProject, updateProjectConfig } from '../api/client';
import type { Project } from '../api/types';
import type { ArchiveUploadValue } from '../components/ArchiveUpload';

export interface CreateProjectWithSourceValues {
  name: string;
  repo_url?: string;
  default_branch: string;
  default_scan_mode: string;
}

export async function createProjectWithSource(
  values: CreateProjectWithSourceValues,
  upload: Pick<ArchiveUploadValue, 'fileId' | 'fileName'>,
): Promise<{ project: Project; configFailed: boolean }> {
  // 上传与仓库二选一：都默认拦截（repo_url 不设 antd 必填，避免"传了包还被逼填地址"）
  if (!upload.fileId && !values.repo_url?.trim()) {
    throw new Error('请上传代码压缩包或填写仓库地址（二选一）');
  }
  const project = await createProject({
    name: values.name,
    ...(values.repo_url?.trim() ? { repo_url: values.repo_url.trim() } : {}),
    default_branch: values.default_branch,
    default_scan_mode: values.default_scan_mode,
  });
  // ADR-203: 上传件 file_id 落项目 config（ADR-200 端点返回 file_id）；原始文件名
  // 一并落 config.upload_file_name（项目页/详情展示，storage 对象键不含原名）
  let configFailed = false;
  if (project.project_id && upload.fileId) {
    try {
      await updateProjectConfig(project.project_id, {
        upload_file_id: upload.fileId,
        ...(upload.fileName ? { upload_file_name: upload.fileName } : {}),
      });
    } catch {
      configFailed = true;
    }
  }
  return { project, configFailed };
}
