import * as assert from 'assert';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { FixRegistry, type FixRecord } from '../src/fixRegistry';

const rec = (findingId: string, appliedAt: number, files = ['C:/w/a.py'], state: FixRecord['state'] = 'applied'): FixRecord => ({
  findingId, label: `修复 ${findingId}`, checkpointId: `cp-${findingId}`, files, appliedAt, state,
});

function tmpPath(): string {
  return path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'fixreg-')), 'fix-registry.json');
}

describe('FixRegistry', () => {
  it('record → appliedFindingIds/byFinding；markRolledback 翻状态且徽章集合排除', () => {
    const r = new FixRegistry(tmpPath());
    r.recordApplied(rec('f1', 100));
    r.recordApplied(rec('f2', 200, ['C:/w/b.py']));
    assert.deepStrictEqual(r.appliedFindingIds(), new Set(['f1', 'f2']));
    assert.strictEqual(r.byFinding('f1')?.checkpointId, 'cp-f1');
    assert.strictEqual(r.markRolledback('f1')?.state, 'rolledback');
    assert.deepStrictEqual(r.appliedFindingIds(), new Set(['f2']));
    assert.strictEqual(r.markRolledback('f1'), null, '已回滚再回滚 = null');
    assert.strictEqual(r.markRolledback('nope'), null, '未应用过 = null');
  });

  it('持久化往返：新实例从文件恢复记录；重新应用覆盖同发现旧记录', () => {
    const p = tmpPath();
    const a = new FixRegistry(p);
    a.recordApplied(rec('f1', 100));
    a.recordApplied(rec('f1', 300, ['C:/w/a2.py'])); // 重新应用（回滚后再修）：覆盖旧记录
    const b = new FixRegistry(p);
    assert.strictEqual(b.byFinding('f1')?.appliedAt, 300);
    assert.deepStrictEqual([...b.appliedFindingIds()], ['f1']);
    assert.deepStrictEqual(b.byFinding('f1')?.files, ['C:/w/a2.py']);
  });

  it('损坏的登记文件视为无记录（不抛异常，checkpoint 仍在可重新应用）', () => {
    const p = tmpPath();
    fs.writeFileSync(p, '{broken json', 'utf-8');
    const r = new FixRegistry(p);
    assert.deepStrictEqual(r.appliedRecords(), []);
    assert.deepStrictEqual(r.appliedFindingIds(), new Set());
  });

  it('knownFindingIds 返回全部登记过的发现（applied+rolledback），供自动应用排除', () => {
    const r = new FixRegistry(tmpPath());
    r.recordApplied(rec('f1', 100));
    r.recordApplied(rec('f2', 200));
    r.markRolledback('f2');
    assert.deepStrictEqual(r.knownFindingIds(), new Set(['f1', 'f2']));
    assert.deepStrictEqual(new FixRegistry(tmpPath()).knownFindingIds(), new Set());
  });

  it('appliedRecords 只含 applied 状态；files 记录用于同文件更晚修复的覆盖告警', () => {
    const r = new FixRegistry(tmpPath());
    r.recordApplied(rec('f1', 100, ['C:/w/a.py']));
    r.recordApplied(rec('f2', 200, ['C:/w/a.py']));
    r.recordApplied(rec('f3', 300, ['C:/w/other.py']));
    r.markRolledback('f3');
    const applied = r.appliedRecords();
    assert.deepStrictEqual(applied.map((x) => x.findingId).sort(), ['f1', 'f2']);
    // f2 比 f1 晚且同文件：回滚 f1 时应被列为受影响
    const f1 = r.byFinding('f1')!;
    const later = applied.filter((x) => x.appliedAt > f1.appliedAt && x.files.some((y) => f1.files.includes(y)));
    assert.deepStrictEqual(later.map((x) => x.findingId), ['f2']);
  });
});

describe('FixRegistry：patches/linesBefore 可选字段（外科回滚数据，向后兼容）', () => {
  it('recordApplied 携带 patches/linesBefore → 重载读回一致', () => {
    const p = tmpPath();
    const r = new FixRegistry(p);
    r.recordApplied({
      ...rec('f1', 100),
      patches: { 'C:/w/a.py': { oldPath: 'a.py', newPath: 'a.py', hunks: [{ oldStart: 1, oldLines: ['x'], newLines: ['y'] }] } },
      linesBefore: { 'C:/w/a.py': { f1: 7, other: 12 } },
    });
    const r2 = new FixRegistry(p);
    const got = r2.byFinding('f1');
    assert.deepStrictEqual(got?.patches?.['C:/w/a.py'].hunks, [{ oldStart: 1, oldLines: ['x'], newLines: ['y'] }]);
    assert.deepStrictEqual(got?.linesBefore?.['C:/w/a.py'], { f1: 7, other: 12 });
  });

  it('旧格式记录（无 patches/linesBefore）加载不报错且字段为 undefined', () => {
    const p = tmpPath();
    fs.writeFileSync(p, JSON.stringify([rec('old', 1)]), 'utf-8');
    const r = new FixRegistry(p);
    const got = r.byFinding('old');
    assert.ok(got);
    assert.strictEqual(got.patches, undefined);
    assert.strictEqual(got.linesBefore, undefined);
  });
});

describe('FixRegistry：persist 原子写（tmp+rename，回归锁：写一半崩溃丢全部登记）', () => {
  it('tmp 写入失败 → 旧登记文件保持完整（不经 tmp 直写目标）；成功后无 .tmp 残留', () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'fixreg-atomic-'));
    const file = path.join(dir, 'fix-registry.json');
    try {
      // 先落一份完整登记
      const a = new FixRegistry(file);
      a.recordApplied(rec('f1', 100));
      const before = fs.readFileSync(file, 'utf-8');
      // 第二次持久化：tmp 写入失败（模拟写一半崩溃/磁盘满）→ 旧文件必须原样
      const failingWrite = {
        existsSync: (p: string) => fs.existsSync(p),
        readFileSync: (p: string, e: 'utf-8') => fs.readFileSync(p, e),
        writeFileSync: (p: string, _d: string, _e: 'utf-8') => { if (String(p).endsWith('.tmp')) throw new Error('EIO: no space'); },
        mkdirSync: (p: string, o: { recursive: boolean }) => fs.mkdirSync(p, o),
        renameSync: (from: string, to: string) => fs.renameSync(from, to),
      };
      const b = new FixRegistry(file, failingWrite);
      assert.throws(() => b.recordApplied(rec('f2', 200)), /EIO/);
      assert.strictEqual(fs.readFileSync(file, 'utf-8'), before, '旧登记完整——持久化不得绕开 tmp 直写目标文件');
      // 正常路径：成功后目标文件更新、无 .tmp 残留
      const c = new FixRegistry(file);
      c.recordApplied(rec('f3', 300));
      const arr = JSON.parse(fs.readFileSync(file, 'utf-8')) as FixRecord[];
      assert.deepStrictEqual(arr.map((r) => r.findingId).sort(), ['f1', 'f3']);
      assert.strictEqual(fs.readdirSync(dir).filter((x) => x.endsWith('.tmp')).length, 0, '成功路径无 tmp 残留');
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
