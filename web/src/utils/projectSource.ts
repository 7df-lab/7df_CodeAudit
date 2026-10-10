// 源码来源展示口径（项目列表源码列/项目详情"源码来源"/任务向导源码行三处共用）。
// 2026-10-08 报障收口：此前三处各写一套，两类失真——①无源码项目在列表被无条件
// 假称"上传压缩包"（列渲染只看 repo_url 空否，不查 config 是否真有上传件）；
// ②无名上传被注"创建于文件名记录启用前，未留存名称"——该说法与实证不符：API/e2e
// 上传路径从不写名（2026-09-20 上传档亦无名，<host-id> sim 库 2026-10-08 抽样 197 有
// 上传件/仅 20 有名），与创建时间无关；且原始文件名经 gateway 上传时仅用于判扩展名
// 即丢弃、storage 只存路径/类型/校验和，服务端无档可恢复——恢复只能换源码重传。
import type { ProjectConfigResponse } from '../api/client';

export type ProjectSourceKind = 'named-archive' | 'unnamed-archive' | 'repo' | 'none';

export interface ProjectSourceShape {
  kind: ProjectSourceKind;
  fileName?: string; // named-archive：web 创建/换源码路径落 config 的原始文件名
  fileId?: string;   // archive：上传件指针（config 键；悬浮溯源用）
  repoUrl?: string;
}

// config 入参 = GET /v1/projects/:id/config 的 config map（E-14；可默认/未加载）
export function projectSourceShape(
  repoUrl: string | null | undefined,
  config: ProjectConfigResponse['config'] | null | undefined,
): ProjectSourceShape {
  if (config?.upload_file_id) {
    return config.upload_file_name
      ? { kind: 'named-archive', fileName: config.upload_file_name, fileId: config.upload_file_id }
      : { kind: 'unnamed-archive', fileId: config.upload_file_id };
  }
  if (repoUrl) return { kind: 'repo', repoUrl };
  return { kind: 'none' };
}

// 无名上传/无源码的全站统一短文案（列表/向导）；详情页前缀"上传"由调用点拼接
export const UNNAMED_ARCHIVE_LABEL = '压缩包（名称未记录）';
export const NO_SOURCE_LABEL = '未配置源码';
