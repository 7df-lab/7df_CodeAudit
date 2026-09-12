// gitAnchor.ts — 增量扫描的 git 集成（ADR-225 D5：B 混合路线）。
//
// 职责（伞仓 docs/designs/incremental-scan.md §5.1，验收 F16-F18）：
//   F16 锚点采集：vscode.git 内置扩展 API（git 随 VS Code 捆绑，不引新依赖）；
//        非 git/未激活工作区 → null（不报错不阻塞，扫描退化为纯内容 diff 入口）；
//        多根工作区取第一个根的仓库（README 钉死口径）；untracked 计入 dirty。
//   F17 无变更预检：commit 相同且双方 dirty=false 才提示（dirty 内容可能不同）。
//   F18 变更预告：基线锚点之后的 commit 数 + 未提交文件数。
//   diff_hint：git 变更清单（name-status 风格）——服务端仅交叉核对提示，不作 diff 依据（D5）。
//
// 设计纪律：所有 git 访问集中在本文件；纯函数（buildDiffHint/sameAnchor/previewText/
// countNewCommits）与 vscode API 访问分离，纯函数单测直跑不桩 vscode。

import * as path from 'path';
import * as vscode from 'vscode';
import type { GitAnchor } from './types';

/** vscode.git 扩展 API 的最小结构面（只取本插件消费的字段；官方 API v1） */
interface GitRepoLike {
  state: {
    HEAD: { commit?: string; name?: string } | undefined;
    /** 工作区变更（含 untracked——A16.4：untracked 计入 dirty 口径与 hint 一致） */
    workingTreeChanges: { uri: vscode.Uri; status?: number }[];
    indexChanges: { uri: vscode.Uri; status?: number }[];
    refs?: { name: string; remote?: string; type: number }[];
    remotes?: { name: string; fetchUrl?: string; pushUrl?: string }[];
  };
  log(options?: { maxEntries?: number }): Promise<{ hash: string }[]>;
}

/** 注入口：测试桩替换；生产=真 vscode.git 扩展 */
let repoProvider: () => Promise<GitRepoLike | null> = async () => {
  const ext = vscode.extensions.getExtension('vscode.git');
  if (!ext) return null;
  const api = await (ext.activate() as Promise<{ getAPI?(version: number): unknown }>);
  const git = api?.getAPI?.(1) as { repositories?: { rootUri: vscode.Uri; state: unknown; log?: unknown }[] } | undefined;
  if (!git?.repositories?.length) return null;
  const root = vscode.workspace.workspaceFolders?.[0];
  const repo = root
    ? git.repositories.find((r) => r.rootUri.toString() === root.uri.toString()) ?? git.repositories[0]
    : git.repositories[0];
  return (repo as unknown) as GitRepoLike;
};

/** 测试注入点（test/gitAnchor.test.ts 桩 vscode.git） */
export function setRepoProviderForTest(p: () => Promise<GitRepoLike | null>): void {
  repoProvider = p;
}

/** F16：采集版本锚点；任何失败（无 git/未初始化/API 异常）→ null，绝不阻塞扫描 */
export async function collectGitAnchor(): Promise<GitAnchor | null> {
  try {
    const repo = await repoProvider();
    if (!repo) return null;
    const commit = repo.state.HEAD?.commit ?? '';
    if (!commit) return null; // 仓库未初始化/无提交：等价非 git 工作区
    const dirty = repo.state.workingTreeChanges.length > 0 || repo.state.indexChanges.length > 0;
    const remote = repo.state.remotes?.find((r) => r.name === 'origin')?.fetchUrl
      ?? repo.state.remotes?.[0]?.fetchUrl ?? '';
    return { commit, branch: repo.state.HEAD?.name ?? '', dirty, remote };
  } catch {
    return null;
  }
}

/** 变更条目（工作区+暂存去重；路径相对工作区根、正斜杠） */
export interface GitChangeEntry {
  relPath: string;
  /** git name-status 语义字母：A/M/D/R（未识别状态按 M 兜底——hint 仅提示，误判无害） */
  status: 'A' | 'M' | 'D' | 'R';
}

/** F16.4+A11 附带：采集变更清单（diff_hint 素材）；与 dirty 同源枚举保证口径一致 */
export async function collectGitChanges(rootFsPath: string): Promise<GitChangeEntry[]> {
  try {
    const repo = await repoProvider();
    if (!repo) return [];
    const seen = new Map<string, GitChangeEntry>();
    const mapStatus = (s: number | undefined): GitChangeEntry['status'] => {
      // vscode.git Status 枚举：INDEX_ADDED=1/INDEX_MODIFIED=3/DELETED=6/MODIFIED=7/...
      // 未识别一律 M（hint 是提示不是依据；R 的精确认定需要 HEAD 对比，成本不值）
      if (s === 1) return 'A';
      if (s === 6) return 'D';
      return 'M';
    };
    const push = (c: { uri: vscode.Uri; status?: number }) => {
      const rel = relativePath(rootFsPath, c.uri.fsPath);
      if (!rel) return;
      if (!seen.has(rel)) seen.set(rel, { relPath: rel, status: mapStatus(c.status) });
    };
    repo.state.workingTreeChanges.forEach(push);
    repo.state.indexChanges.forEach(push);
    return [...seen.values()];
  } catch {
    return [];
  }
}

/** diff_hint 原文（git diff --name-status 同款格式，服务端交叉核对用；空清单→空串） */
export function buildDiffHint(changes: GitChangeEntry[]): string {
  return changes.map((c) => `${c.status}\t${c.relPath}`).join('\n');
}

/** F17：无变更判定——commit 相同且双方均无未提交改动（dirty=true 内容可能不同，不得提示无变更） */
export function sameAnchorNoChange(current: GitAnchor | null, baseline: GitAnchor | null | undefined): boolean {
  if (!current || !baseline) return false;
  return current.commit === baseline.commit && !current.dirty && !baseline.dirty;
}

/** F18.1：基线锚点之后的 commit 数（log 倒序数到基线 commit 为止；未命中=全量计数，仅展示用） */
export async function countNewCommits(baselineCommit: string): Promise<number> {
  try {
    const repo = await repoProvider();
    if (!repo || !baselineCommit || !repo.log) return -1;
    const entries = await repo.log({ maxEntries: 500 });
    const idx = entries.findIndex((e) => e.hash === baselineCommit);
    return idx < 0 ? entries.length : idx;
  } catch {
    return -1;
  }
}

/** F18.1：变更预告文案（非 git/不可得 → 空串，QuickPick 无此行不塌陷） */
export function previewText(newCommits: number, changes: GitChangeEntry[]): string {
  if (newCommits < 0) return changes.length > 0 ? `未提交文件 ${changes.length} 个` : '';
  return `基线锚点后 ${newCommits} 个提交 + 未提交文件 ${changes.length} 个`;
}

function relativePath(root: string, abs: string): string {
  const rel = path.relative(root, abs).replace(/\\/g, '/');
  return rel && !rel.startsWith('..') ? rel : '';
}
