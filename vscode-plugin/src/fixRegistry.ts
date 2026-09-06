// 修复登记（纯逻辑，可单测）：每个 AI 修复一条记录——发现 → checkpoint 映射 +
// 状态机（applied ⇄ rolledback）。"补丁随时可应用/可回滚"的语义靠它支撑：
// 应用后发现保留在面板（标记"已修复"），回滚后可再次应用（重新应用会生成新
// checkpoint 并覆盖同发现旧记录——以最近一次为准）。
// 持久化到 globalStorage JSON：窗口重载后"已修复"徽章与按发现回滚入口仍在。
// fs 可注入以便单测。
import * as fs from 'fs';
import * as path from 'path';
import type { FilePatch } from './diffParse';

export interface FixRecord {
  findingId: string;
  label: string;
  checkpointId: string;
  /** 本次修复触及的绝对路径（Update/Delete/Move 源 + Add/Move 目标） */
  files: string[];
  appliedAt: number;
  state: 'applied' | 'rolledback';
  /**
   * 逐文件逆补丁数据（仅纯 Update 修复且差异可计算时存在）：回滚时交换 old/new
   * 精确锚定撤销本修复引入的变更——同文件更晚修复的内容保留，任意序回滚成立。
   * 缺失（旧记录/含 Add/Delete/Move/差异过大）→ 回滚降级 checkpoint 整文件覆盖。
   */
  patches?: Record<string, FilePatch>;
  /**
   * 应用时各文件上所有发现的 1-based 行号快照（findingId → 行号）：
   * 整文件覆盖回滚（跳回该修复前状态）时据此恢复行号；外科回滚不用（增量偏移）。
   */
  linesBefore?: Record<string, Record<string, number>>;
}

export interface FsLite {
  existsSync(p: string): boolean;
  readFileSync(p: string, encoding: 'utf-8'): string;
  writeFileSync(p: string, data: string, encoding: 'utf-8'): void;
  mkdirSync(p: string, opts: { recursive: boolean }): void;
}

export class FixRegistry {
  private records = new Map<string, FixRecord>(); // findingId → record

  constructor(private filePath: string, private fsOps: FsLite = fs as unknown as FsLite) {
    if (!this.fsOps.existsSync(filePath)) return;
    try {
      const arr = JSON.parse(this.fsOps.readFileSync(filePath, 'utf-8')) as FixRecord[];
      for (const r of arr) this.records.set(r.findingId, r);
    } catch {
      // 坏文件视为无记录：checkpoint 内容仍在磁盘，面板可重新应用修复
    }
  }

  private persist(): void {
    this.fsOps.mkdirSync(path.dirname(this.filePath), { recursive: true });
    this.fsOps.writeFileSync(this.filePath, JSON.stringify([...this.records.values()], null, 2), 'utf-8');
  }

  recordApplied(rec: FixRecord): void {
    this.records.set(rec.findingId, rec);
    this.persist();
  }

  /** 返回被回滚的记录；无已应用记录（未应用/已回滚）返回 null */
  markRolledback(findingId: string): FixRecord | null {
    const r = this.records.get(findingId);
    if (!r || r.state !== 'applied') return null;
    r.state = 'rolledback';
    this.persist();
    return r;
  }

  byFinding(findingId: string): FixRecord | undefined {
    return this.records.get(findingId);
  }

  /** 当前"已修复"的发现集合（树徽章用） */
  appliedFindingIds(): Set<string> {
    const s = new Set<string>();
    for (const r of this.records.values()) if (r.state === 'applied') s.add(r.findingId);
    return s;
  }

  appliedRecords(): FixRecord[] {
    return [...this.records.values()].filter((r) => r.state === 'applied');
  }

  /** 出现过登记的发现集合（applied + rolledback）：低风险自动应用排除用——用户回滚过的发现不被自动翻案 */
  knownFindingIds(): Set<string> {
    return new Set(this.records.keys());
  }
}
