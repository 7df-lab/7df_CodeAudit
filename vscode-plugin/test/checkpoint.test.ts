import * as assert from 'assert';
import { CheckpointStore, type FileSystemLike } from '../src/checkpoint';

function makeFs(): FileSystemLike & { files: Map<string, string> } {
  const files = new Map<string, string>();
  // 生产代码经 path.join 产生平台分隔符（win32 为 \），归一化后统一按 / 存取。
  // readFileSync 故意复刻真实 Node 语义：不传 encoding 返回 Buffer——
  // 若生产代码忘传 'utf-8'，快照内容会变成 Buffer 对象，回滚 applyEdit 将静默失败
  // （线上事故：fs.readFileSync(path) 返回 Buffer，WorkspaceEdit.replace 拒绝后无任何提示）。
  const norm = (p: string) => p.replace(/\\/g, '/');
  return {
    files,
    existsSync: (p) => {
      const n = norm(p);
      return files.has(n) || [...files.keys()].some((k) => k.startsWith(n + '/'));
    },
    mkdirSync: () => undefined,
    readFileSync: (p, encoding) => {
      const v = files.get(norm(p));
      if (v === undefined) throw new Error(`ENOENT ${p}`);
      return (encoding === 'utf-8' ? v : Buffer.from(v, 'utf-8')) as unknown as string;
    },
    writeFileSync: (p, d) => void files.set(norm(p), d),
    unlinkSync: (p) => void files.delete(norm(p)),
    // 内存桩无目录实体（readdirSync 按前缀推导）：删目录 no-op，删除效果由 unlinkSync 体现
    rmdirSync: () => undefined,
    readdirSync: (p) => {
      const n = norm(p);
      // Set 去重对齐真实 fs 语义：同一目录下多个文件只返回一次目录名
      // （不去重会让 list() 重复计数，每文件上限清理提前误触发）
      return [...new Set([...files.keys()].filter((k) => k.startsWith(n + '/')).map((k) => k.slice(n.length + 1).split('/')[0]))];
    },
  };
}

describe('CheckpointStore', () => {
  it('save 空集合返回 null（无文件无需 checkpoint）', () => {
    const store = new CheckpointStore('/cp', makeFs());
    assert.strictEqual(store.save({}), null);
  });

  it('save → latest → restoreLatest 返回原内容', () => {
    const fs = makeFs();
    const store = new CheckpointStore('/cp', fs);
    const id = store.save({ '/w/src/a.ts': 'old-a', '/w/src/b.ts': 'old-b' });
    assert.ok(id!.startsWith('cp-'));
    assert.strictEqual(store.latest(), id);
    const restored = store.restoreLatest();
    assert.deepStrictEqual(restored, { '/w/src/a.ts': 'old-a', '/w/src/b.ts': 'old-b' });
  });

  it('restoreLatest 的快照必须是 utf-8 字符串而非 Buffer（回归锁）', () => {
    const store = new CheckpointStore('/cp', makeFs());
    store.save({ '/w/a.py': 'import sqlite3\n\nreturn x\n' });
    const restored = store.restoreLatest();
    assert.ok(restored);
    for (const v of Object.values(restored!)) {
      assert.strictEqual(typeof v, 'string', `快照应为 string，实际 ${typeof v}（readFileSync 未传 'utf-8'？）`);
    }
  });

  it('多个 checkpoint 时 latest 取最新；无 checkpoint 时 restore 返回 null', () => {
    const store = new CheckpointStore('/cp', makeFs());
    store.save({ '/a': '1' });
    const id2 = store.save({ '/b': '2' });
    assert.strictEqual(store.latest(), id2);
    assert.deepStrictEqual(store.restoreLatest(), { '/b': '2' });
    const empty = new CheckpointStore('/none', makeFs());
    assert.deepStrictEqual(empty.restoreLatest(), null);
  });

  it('null 快照 = 修复前不存在的文件（Add/Move 目标），restore 原样返回 null', () => {
    const store = new CheckpointStore('/cp', makeFs());
    store.save({ '/w/a.py': 'old', '/w/new_file.py': null });
    assert.deepStrictEqual(store.restoreLatest(), { '/w/a.py': 'old', '/w/new_file.py': null });
  });

  it('restore(id) 按 id 恢复任意 checkpoint（不删除，支持回滚后再次应用/多次回滚）', () => {
    const store = new CheckpointStore('/cp', makeFs());
    const id1 = store.save({ '/w/a.py': 'v1' })!;
    const id2 = store.save({ '/w/a.py': 'v2' })!;
    assert.notStrictEqual(id1, id2);
    assert.deepStrictEqual(store.restore(id1), { '/w/a.py': 'v1' });
    assert.deepStrictEqual(store.restore(id2), { '/w/a.py': 'v2' });
    // 可重复读取（checkpoint 保留）
    assert.deepStrictEqual(store.restore(id1), { '/w/a.py': 'v1' });
  });

  it('restore 不存在的 id / 空 id / 损坏 manifest → null 不抛异常', () => {
    const store = new CheckpointStore('/cp', makeFs());
    store.save({ '/w/a.py': 'v1' });
    assert.strictEqual(store.restore('cp-none'), null);
    assert.strictEqual(store.restore(''), null);
    const f = makeFs();
    const s2 = new CheckpointStore('/cp', f);
    s2.save({ '/w/a.py': 'v1' });
    f.writeFileSync('/cp/' + s2.latest()! + '/manifest.json', '{broken', 'utf-8');
    assert.strictEqual(s2.restore(s2.latest()!), null);
  });

  it('每文件快照上限 100：第 101 份起最旧 checkpoint 中该文件条目被移除，同 cp 其他文件保留（回归锁：无界增长）', () => {
    const store = new CheckpointStore('/cp', makeFs());
    for (let i = 1; i <= 101; i++) {
      store.save({ '/w/a.py': `rev-${i}`, ...(i === 1 ? { '/w/keep.ts': 'keep' } : {}) });
    }
    const ids = store.list(); // (ts,seq) 倒序，新→旧
    assert.strictEqual(ids.length, 101, '最旧 cp 仍含其他文件（keep.ts），目录不删');
    assert.deepStrictEqual(store.restore(ids[ids.length - 1]), { '/w/keep.ts': 'keep' }, '最旧 cp 的 a.py 条目被清理、keep.ts 原样保留');
    // 最新快照完整可恢复；保留窗口的最旧一份（第 2 次保存）仍在
    assert.strictEqual(store.restore(ids[0])!['/w/a.py'], 'rev-101');
    assert.strictEqual(store.restore(ids[99])!['/w/a.py'], 'rev-2');
  });

  it('每文件快照上限 100：单文件 checkpoint 条目清空后整目录删除（回归锁：无界增长）', () => {
    const store = new CheckpointStore('/cp', makeFs());
    for (let i = 1; i <= 101; i++) store.save({ '/w/b.py': `v${i}` });
    const ids = store.list();
    assert.strictEqual(ids.length, 100, '最旧单文件 cp 的 manifest 清空 → 目录整体删除');
    assert.strictEqual(store.restore(ids[ids.length - 1])!['/w/b.py'], 'v2', '保留窗口的最旧一份是第 2 次保存');
  });

  it('仅标点不同的两个文件不碰撞：各自内容独立保存（回归锁：旧散列命名会互相覆盖）', () => {
    const store = new CheckpointStore('/cp', makeFs());
    const id = store.save({
      'C:\\ws\\src\\foo.bar.ts': 'CONTENT-A',
      'C:\\ws\\src\\foo_bar.ts': 'CONTENT-B',
    });
    const restored = store.restore(id!)!;
    assert.strictEqual(restored['C:\\ws\\src\\foo.bar.ts'], 'CONTENT-A', 'foo.bar.ts 的内容被 foo_bar.ts 覆盖 = 散列碰撞回归');
    assert.strictEqual(restored['C:\\ws\\src\\foo_bar.ts'], 'CONTENT-B');
  });
});
