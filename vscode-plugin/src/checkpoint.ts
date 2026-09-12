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
  unlinkSync(p: string): void;
  rmdirSync(p: string): void;
}

/** 同一绝对路径在全部 checkpoint 中的条目上限（含 null 标记条目）：
 *  每次保存后按文件增量清理——超出上限的最旧快照被移除（manifest 去键 + 删内容文件，
 *  manifest 清空则整目录删除）。被清理的旧登记回滚走既有「checkpoint 缺失/损坏」诚实降级。 */
export const CHECKPOINT_PER_FILE_KEEP = 100;

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
    for (const abs of keys) this.pruneFile(abs);
    return id;
  }

  /**
   * 按文件增量清理（save 后调用）：该文件在全部 checkpoint 中的条目超过
   * CHECKPOINT_PER_FILE_KEEP 时，从最旧 checkpoint 起移除其条目（删内容文件 +
   * manifest 去键；manifest 清空 → 删 manifest + 整目录）。清理不感知修复登记表——
   * 100 份上限足够深，被清理的登记按发现回滚时走既有「缺失/损坏（文件可能被清理）」降级。
   */
  private pruneFile(abs: string): void {
    let seen = 0;
    for (const id of this.list()) {
      const dir = path.join(this.rootDir, id);
      const manifestPath = path.join(dir, 'manifest.json');
      let manifest: Record<string, string | null>;
      try {
        manifest = JSON.parse(this.fsOps.readFileSync(manifestPath, 'utf-8')) as Record<string, string | null>;
      } catch {
        continue; // 坏 manifest 的孤儿目录：restore 已容错，不在此处理
      }
      if (!(abs in manifest)) continue;
      seen++;
      if (seen <= CHECKPOINT_PER_FILE_KEEP) continue;
      const stored = manifest[abs];
      if (stored !== null) {
        try {
          this.fsOps.unlinkSync(path.join(dir, stored));
        } catch {
          /* 尽力而为：manifest 去键后 restore 不会再引用该文件 */
        }
      }
      delete manifest[abs];
      if (Object.keys(manifest).length === 0) {
        try {
          this.fsOps.unlinkSync(manifestPath);
          this.fsOps.rmdirSync(dir);
        } catch {
          /* 尽力而为：残留空目录无引用，不影响 list/restore 语义 */
        }
      } else {
        this.fsOps.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2), 'utf-8');
      }
    }
  }

  list(): string[] {
    if (!this.fsOps.existsSync(this.rootDir)) return [];
    // 按 (时间戳, 序号) 数值序倒序：id 含 `cp-<ts>-<seq>`，seq 跨位数（9→10）时字典序会
    // 把 cp-…-10 排在 cp-…-9 之前，latest() 取错最近快照（低风险批量连修高危）
    const parsed = this.fsOps.readdirSync(this.rootDir)
      .filter((d) => d.startsWith('cp-'))
      .map((d) => {
        const m = /^cp-(\d+)-(\d+)$/.exec(d);
        return m ? { id: d, ts: Number(m[1]), seq: Number(m[2]) } : { id: d, ts: -1, seq: -1 };
      });
    parsed.sort((a, b) => (b.ts - a.ts) || (b.seq - a.seq) || (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
    return parsed.map((p) => p.id);
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
