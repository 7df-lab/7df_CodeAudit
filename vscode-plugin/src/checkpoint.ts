// Checkpoint：批量修复前把受影响文件快照到插件全局存储目录，供一键回滚。
// 仿 Cline shadow-checkpoint 思路（设计已批准：任何批量修复前落 checkpoint）。
// fs/path 可注入以便单测。
import * as fs from 'fs';
import * as path from 'path';

export interface FileSystemLike {
  existsSync(p: string): boolean;
  mkdirSync(p: string, opts: { recursive: boolean }): void;
  // 必须显式传 'utf-8'：真实 fs.readFileSync(path) 不传 encoding 返回 Buffer，
  // 会让回滚内容变成 Buffer 对象、WorkspaceEdit.replace 静默失败（已出过事故）。
  readFileSync(p: string, encoding: 'utf-8'): string;
  writeFileSync(p: string, data: string, encoding: 'utf-8'): void;
  readdirSync(p: string): string[];
}

let checkpointSeq = 0;

export class CheckpointStore {
  constructor(private rootDir: string, private fsOps: FileSystemLike = fs as unknown as FileSystemLike) {}

  // 写入一个 checkpoint，返回其 id（时间戳 + 序号：同毫秒多次保存不碰撞）。
  // files: 绝对路径 → 内容；值为 null 表示该文件修复前不存在（Add File / Move to 目标），
  // 回滚时应删除而非还原。
  save(files: Record<string, string | null>): string | null {
    const keys = Object.keys(files);
    if (keys.length === 0) return null;
    const id = `cp-${Date.now()}-${checkpointSeq++}`;
    const dir = path.join(this.rootDir, id);
    this.fsOps.mkdirSync(dir, { recursive: true });
    const manifest: Record<string, string | null> = {};
    // 存储名用目录内序号（0000/0001/…）：按路径字符替换（如非字母数字→_）会让仅
    // 标点不同的两个文件（foo.bar.ts / foo_bar.ts）碰撞同名、互相覆盖，回滚即数据损坏。
    // 旧 checkpoint 的 manifest 存的是当时的实际文件名，restore 按 manifest 读取，天然兼容。
    keys.forEach((abs, i) => {
      const content = files[abs];
      if (content === null) {
        manifest[abs] = null;
        return;
      }
      const stored = String(i).padStart(4, '0');
      this.fsOps.writeFileSync(path.join(dir, stored), content, 'utf-8');
      manifest[abs] = stored;
    });
    this.fsOps.writeFileSync(path.join(dir, 'manifest.json'), JSON.stringify(manifest, null, 2), 'utf-8');
    return id;
  }

  list(): string[] {
    if (!this.fsOps.existsSync(this.rootDir)) return [];
    return this.fsOps.readdirSync(this.rootDir).filter((d) => d.startsWith('cp-')).sort().reverse();
  }

  latest(): string | null {
    return this.list()[0] ?? null;
  }

  // 读取最近一个 checkpoint 的文件快照。
  restoreLatest(): Record<string, string | null> | null {
    return this.restore(this.latest() ?? '');
  }

  // 按 id 读取 checkpoint 的文件快照（按发现回滚用；checkpoint 保留不删——
  // 支持回滚后再次应用、多次回滚审查）。id 不存在/损坏返回 null。
  // 值为 null 的条目 = 修复前不存在的文件，调用方回滚时应删除该文件。
  restore(id: string): Record<string, string | null> | null {
    if (!id) return null;
    const dir = path.join(this.rootDir, id);
    const manifestPath = path.join(dir, 'manifest.json');
    if (!this.fsOps.existsSync(manifestPath)) return null;
    try {
      const manifest = JSON.parse(this.fsOps.readFileSync(manifestPath, 'utf-8')) as Record<string, string | null>;
      const restored: Record<string, string | null> = {};
      for (const [abs, stored] of Object.entries(manifest)) {
        restored[abs] = stored === null ? null : this.fsOps.readFileSync(path.join(dir, stored), 'utf-8');
      }
      return restored;
    } catch {
      return null;
    }
  }
}
