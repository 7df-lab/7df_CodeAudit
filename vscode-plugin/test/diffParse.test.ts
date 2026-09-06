import * as assert from 'assert';
import {
  applyPatchToLines,
  applyResolvedChunks,
  buildFilePatch,
  extractDiffBlock,
  findContext,
  formatPatchFailures,
  invertFilePatch,
  parseUnifiedDiff,
  shiftLine,
  type HunkEdit,
} from '../src/diffParse';

const SUGGESTION = `修复建议：使用参数化查询。

\`\`\`diff
--- a/src/db/query.ts
+++ b/src/db/query.ts
@@ -3,4 +3,4 @@
 const id = req.params.id;
-const sql = \`SELECT * FROM t WHERE id=\${id}\`;
+const sql = 'SELECT * FROM t WHERE id=?';
 db.query(sql, [id]);
\`\`\`

同时建议开启 ORM。`;

// 原始 SQL 注入示例（与 mock 网关 / 测试工作区同构）
const VULN_FILE = [
  'import sqlite3',
  '',
  'def get_user(user_id):',
  '    conn = sqlite3.connect("app.db")',
  '    cursor = conn.cursor()',
  '    # SQL injection: string concatenation',
  '    query = "SELECT * FROM users WHERE id = " + user_id',
  '    cursor.execute(query)',
  '    return cursor.fetchone()',
];

const BLOCK = ['    # SQL injection: string concatenation', '    query = "SELECT * FROM users WHERE id = " + user_id', '    cursor.execute(query)'];
const NEW_BLOCK = ['    # 参数化查询', '    query = "SELECT * FROM users WHERE id = ?"', '    cursor.execute(query, (user_id,))'];

const HUNK6: HunkEdit = { oldStart: 6, oldLines: BLOCK, newLines: NEW_BLOCK };

const patch = (hunks: HunkEdit[]): { oldPath: string; newPath: string; hunks: HunkEdit[] } => ({
  oldPath: 'vuln_fix_demo.py',
  newPath: 'vuln_fix_demo.py',
  hunks,
});

describe('diffParse：解析', () => {
  it('提取 ```diff 围栏块；无围栏返回 null', () => {
    assert.ok(extractDiffBlock(SUGGESTION)!.includes('@@ -3,4 +3,4 @@'));
    assert.strictEqual(extractDiffBlock('纯文本建议，无补丁'), null);
    assert.strictEqual(extractDiffBlock(''), null);
  });

  it('解析 hunk：oldStart/oldLines/newLines（围栏尾部换行不计入）', () => {
    const patches = parseUnifiedDiff(extractDiffBlock(SUGGESTION)!);
    assert.strictEqual(patches.length, 1);
    const p = patches[0];
    assert.strictEqual(p.oldPath, 'src/db/query.ts');
    assert.strictEqual(p.newPath, 'src/db/query.ts');
    assert.strictEqual(p.hunks.length, 1);
    assert.strictEqual(p.hunks[0].oldStart, 3);
    assert.strictEqual(p.hunks[0].oldLines.length, 3); // 2 上下文 + 1 删除（围栏尾部换行不再计入）
    assert.strictEqual(p.hunks[0].newLines.length, 3);
    assert.ok(p.hunks[0].newLines[1].includes('id=?'));
  });

  it('diff 文本末尾换行的 split 空串不当作上下文行（不吞 hunk 后的文件行）', () => {
    const file = ['l1', 'l2', 'l3', 'l4', 'l5', '    target();', '    keep();', '    return x;'];
    const diff = '--- a/f.py\n+++ b/f.py\n@@ -6,1 +6,1 @@\n-    target();\n+    TARGET();\n';
    const p = parseUnifiedDiff(diff)[0];
    assert.deepStrictEqual(p.hunks[0].oldLines, ['    target();']);
    const out = applyPatchToLines(file, p);
    assert.ok(out.lines);
    assert.deepStrictEqual(out.lines, ['l1', 'l2', 'l3', 'l4', 'l5', '    TARGET();', '    keep();', '    return x;']);
  });

  it('hunk 内真正的空上下文行（裸空串）仍被保留', () => {
    const diff = '--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n\n-b\n+B\n'; // 中间空行为上下文
    const p = parseUnifiedDiff(diff)[0];
    assert.deepStrictEqual(p.hunks[0].oldLines, ['a', '', 'b']);
    assert.deepStrictEqual(p.hunks[0].newLines, ['a', '', 'B']);
  });

  it('"\\ No newline at end of file" 标记被忽略，不影响内容比对', () => {
    const diff = '--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+A\n';
    const p = parseUnifiedDiff(diff)[0];
    assert.deepStrictEqual(p.hunks[0].oldLines, ['a']);
    assert.deepStrictEqual(p.hunks[0].newLines, ['A']);
  });

  it('a/ b/ 前缀与 Windows 反斜杠归一', () => {
    const patches = parseUnifiedDiff('--- a/src\\x.ts\n+++ b/src\\x.ts\n@@ -1 +1 @@\n-a\n+b\n');
    assert.strictEqual(patches[0].oldPath, 'src/x.ts');
  });
});

describe('diffParse：findContext（Cline 逐字同款判定：顺序扫描首个命中）', () => {
  it('级 1 精确：命中即返回 fuzz=0，不校验声明行号', () => {
    const r = findContext(VULN_FILE, BLOCK, 0);
    assert.ok(r.index !== -1);
    assert.strictEqual((r as { index: number }).index, 5);
    assert.strictEqual((r as { fuzz: number }).fuzz, 0);
  });

  it('行号漂移（声明错误）不影响精确命中——fuzz 仍为 0（Cline 语义：行号不参与锚定）', () => {
    const r = findContext(VULN_FILE, BLOCK, 0); // 从 0 扫描，内容在第 6 行
    assert.strictEqual((r as { fuzz: number }).fuzz, 0);
    const out = applyPatchToLines(VULN_FILE, patch([{ ...HUNK6, oldStart: 100 }]));
    assert.strictEqual(out.fuzz, 0);
    assert.strictEqual(out.lines![8], '    return cursor.fetchone()');
  });

  it('级 2 trimEnd：仅行尾空白差异命中 fuzz=1', () => {
    const ctx = BLOCK.map((l, i) => (i === 0 ? `${l}   ` : l)); // 行尾多空格
    const r = findContext(VULN_FILE, ctx, 0);
    assert.ok(r.index !== -1);
    assert.strictEqual((r as { fuzz: number }).fuzz, 1);
  });

  it('级 3 trim：缩进漂移 + 智能引号命中 fuzz=100', () => {
    const ctx = [
      '  # SQL injection: string concatenation',
      '  query = “SELECT * FROM users WHERE id = ” + user_id',
      '  cursor.execute(query)',
    ];
    const r = findContext(VULN_FILE, ctx, 0);
    assert.ok(r.index !== -1, `应 trim 命中，实际 ${JSON.stringify(r)}`);
    assert.strictEqual((r as { fuzz: number }).fuzz, 100);
  });

  it('级 4 相似度：内容轻微改写首个命中 fuzz=1000 且带相似度', () => {
    const ctx = ['    # SQL 注入：字符串拼接', '    query = "SELECT * FROM users WHERE id = " + uid', '    cursor.execute(query)'];
    const r = findContext(VULN_FILE, ctx, 0);
    assert.ok(r.index !== -1, `应相似度命中，实际 ${JSON.stringify(r)}`);
    assert.strictEqual((r as { fuzz: number }).fuzz, 1000);
    assert.ok((r as { similarity: number }).similarity >= 0.66);
  });

  it('重复块不判歧义：顺序扫描取首个命中（Cline 消歧方式）', () => {
    const dup = [...VULN_FILE, ...BLOCK];
    const r = findContext(dup, BLOCK, 0);
    assert.strictEqual((r as { index: number }).index, 5); // 首个，不是第二个（8+1=9 起）
    assert.strictEqual((r as { fuzz: number }).fuzz, 0);
  });

  it('从指定起点续扫：跳过游标之前的命中', () => {
    const dup = [...VULN_FILE, ...BLOCK]; // 块在 5 与 9
    const r = findContext(dup, BLOCK, 8); // 游标从 8 起（Cline 顺序游标语义）
    assert.strictEqual((r as { index: number }).index, 9);
  });

  it('未命中返回 -1 与最高相似度', () => {
    const ctx = ['    logger.info("nothing like this")', '    os.exit(1)', '    foo = barbaz(1, 2, 3)'];
    const r = findContext(VULN_FILE, ctx, 0);
    assert.strictEqual(r.index, -1);
    assert.ok((r as { bestSimilarity: number }).bestSimilarity < 0.66);
  });

  it('空 context（纯插入 hunk）锚定在扫描起点', () => {
    const r = findContext(VULN_FILE, [], 4);
    assert.strictEqual((r as { index: number }).index, 4);
    assert.strictEqual((r as { fuzz: number }).fuzz, 0);
  });
});

describe('diffParse：应用（computePatchChanges 整体拒绝 + applyChunks 递增校验）', () => {
  it('精确锚定应用，hunk 后的文件行保留，fuzz=0', () => {
    const out = applyPatchToLines(VULN_FILE, patch([HUNK6]));
    assert.strictEqual(out.fuzz, 0);
    assert.deepStrictEqual(out.lines!.slice(5, 8), NEW_BLOCK);
    assert.strictEqual(out.lines![8], '    return cursor.fetchone()');
    assert.deepStrictEqual(out.applied, [{ hunkIndex: 0, start: 5, fuzz: 0 }]);
  });

  it('顺序游标：重复代码块上两个 hunk 依序各锚定一处（首个→第二处）', () => {
    const dup = [...VULN_FILE, ...BLOCK]; // 块在 5 与 9
    const h2: HunkEdit = { oldStart: 10, oldLines: BLOCK, newLines: ['    FIXED2'] };
    const out = applyPatchToLines(dup, patch([HUNK6, h2]));
    assert.ok(out.lines);
    assert.deepStrictEqual(out.applied, [
      { hunkIndex: 0, start: 5, fuzz: 0 },
      { hunkIndex: 1, start: 9, fuzz: 0 },
    ]);
    assert.deepStrictEqual(out.lines!.slice(5, 8), NEW_BLOCK);
    assert.strictEqual(out.lines![9], '    FIXED2'); // 第二处重复块被第二 hunk 替换
  });

  it('多 hunk fuzz 汇总', () => {
    const h1: HunkEdit = { oldStart: 1, oldLines: [VULN_FILE[0]], newLines: ['import sqlite3 # safe'] };
    const h2: HunkEdit = {
      oldStart: 6,
      oldLines: ['  # SQL injection: string concatenation', '  query = “SELECT * FROM users WHERE id = ” + user_id', '  cursor.execute(query)'], // trim 级
      newLines: NEW_BLOCK,
    };
    const out = applyPatchToLines(VULN_FILE, patch([h1, h2]));
    assert.ok(out.lines);
    assert.strictEqual(out.lines![0], 'import sqlite3 # safe');
    assert.deepStrictEqual(out.lines!.slice(5, 8), NEW_BLOCK);
    assert.strictEqual(out.lines![8], '    return cursor.fetchone()');
    assert.strictEqual(out.fuzz, 100);
  });

  it('纯插入 hunk（空 oldLines）在游标处插入', () => {
    const ins: HunkEdit = { oldStart: 3, oldLines: [], newLines: ['# inserted'] };
    const out = applyPatchToLines(VULN_FILE, patch([ins]));
    assert.ok(out.lines);
    assert.strictEqual(out.lines![3], '# inserted');
    assert.strictEqual(out.lines!.length, VULN_FILE.length + 1);
  });

  it('任一 hunk 未命中 → 整体拒绝（all-or-nothing），文件不变', () => {
    const bad: HunkEdit = { oldStart: 1, oldLines: ['completely absent line'], newLines: ['x'] };
    const out = applyPatchToLines(VULN_FILE, patch([HUNK6, bad]));
    assert.strictEqual(out.lines, null);
    assert.strictEqual(out.failures.length, 1);
    assert.strictEqual(out.failures[0].hunkIndex, 1);
    assert.match(out.failures[0].reason, /最高相似度 0\.\d\d/);
    assert.ok(out.failures[0].contextPreview.length <= 203);
  });

  it('formatPatchFailures 输出逐 hunk 原因（用户可读）', () => {
    const text = formatPatchFailures('a.py', [
      { hunkIndex: 2, oldStart: 12, reason: '未在文件中找到 hunk 上下文（自第 1 行起扫描，最高相似度 0.41）', bestSimilarity: 0.41, contextPreview: 'ctx' },
    ]);
    assert.match(text, /a\.py/);
    assert.match(text, /hunk #3/);
    assert.match(text, /第 12 行/);
  });
});

describe('diffParse：applyResolvedChunks 递增校验（Cline applyChunks 同款）', () => {
  const file = ['a', 'b', 'c', 'd', 'e'];

  it('升序 chunks 正确拼接', () => {
    const r = applyResolvedChunks(file, [
      { start: 1, insLines: ['B1'], delCount: 1 }, // 替换 b
      { start: 3, insLines: ['D1', 'D2'], delCount: 1 }, // 替换 d
    ]);
    assert.ok('lines' in r);
    assert.deepStrictEqual(r.lines, ['a', 'B1', 'c', 'D1', 'D2', 'e']);
  });

  it('currentIndex > chunk.start → 重叠报错（防御层）', () => {
    const r = applyResolvedChunks(file, [
      { start: 1, insLines: ['X'], delCount: 3 }, // 占 1-3
      { start: 2, insLines: ['Y'], delCount: 1 }, // 与上面重叠
    ]);
    assert.ok('error' in r);
    assert.match(r.error, /重叠/);
  });

  it('chunk 起点越界 → 报错', () => {
    const r = applyResolvedChunks(file, [{ start: 99, insLines: ['X'], delCount: 0 }]);
    assert.ok('error' in r);
    assert.match(r.error, /越界/);
  });

  it('空 chunks 返回原文件', () => {
    const r = applyResolvedChunks(file, []);
    assert.ok('lines' in r);
    assert.deepStrictEqual(r.lines, file);
  });
});

describe('diffParse：unified diff 多 hunk 乱序（用户实测回归）', () => {
  it('hunk 逆序（远端在前）：回溯锚定 + 按位置排序应用', () => {
    const F = ['l1', 'old1', 'l3', 'l4', 'l5', 'old2', 'l7'].join('\n');
    const diff = [
      '--- a/f.txt',
      '+++ b/f.txt',
      '@@ -6,1 +6,1 @@',
      '-old2',
      '+NEW2',
      '@@ -2,1 +2,1 @@',
      '-old1',
      '+NEW1',
      '',
    ].join('\n');
    const patches = parseUnifiedDiff(diff);
    const r = applyPatchToLines(F.split('\n'), patches[0]);
    assert.ok(r.lines, `应应用成功: ${r.failures[0]?.reason ?? ''}`);
    assert.deepStrictEqual(r.lines, ['l1', 'NEW1', 'l3', 'l4', 'l5', 'NEW2', 'l7']);
  });
});

describe('diffParse：buildFilePatch / invertFilePatch / shiftLine（外科回滚数据源）', () => {
  const patchOf = (hunks: HunkEdit[]) => ({ oldPath: 'f.txt', newPath: 'f.txt', hunks });

  it('替换：应用结果等于 after；shifts 记录替换区起点/净差', () => {
    const B = ['a', 'b', 'c', 'd', 'e', 'f', 'g'];
    const A = ['a', 'b', 'c', 'X', 'Y', 'f', 'g'];
    const bp = buildFilePatch(B.join('\n'), A.join('\n'))!;
    assert.strictEqual(bp.hunks.length, 1);
    const r = applyPatchToLines(B, patchOf(bp.hunks));
    assert.deepStrictEqual(r.lines, A);
    assert.deepStrictEqual(bp.shifts, [{ start: 3, delCount: 2, delta: 0 }]);
  });

  it('纯插入：delStart==delEnd 成块，delta=插入行数', () => {
    const bp = buildFilePatch(['a', 'd'].join('\n'), ['a', 'b', 'c', 'd'].join('\n'))!;
    assert.deepStrictEqual(bp.shifts, [{ start: 1, delCount: 0, delta: 2 }]);
    const r = applyPatchToLines(['a', 'd'], patchOf(bp.hunks));
    assert.deepStrictEqual(r.lines, ['a', 'b', 'c', 'd']);
  });

  it('纯删除：insStart==insEnd 成块，delta 为负', () => {
    const bp = buildFilePatch(['a', 'b', 'c', 'd'].join('\n'), ['a', 'd'].join('\n'))!;
    assert.deepStrictEqual(bp.shifts, [{ start: 1, delCount: 2, delta: -2 }]);
    const r = applyPatchToLines(['a', 'b', 'c', 'd'], patchOf(bp.hunks));
    assert.deepStrictEqual(r.lines, ['a', 'd']);
  });

  it('CRLF 内容按 \r\n 或 \n 切行，行内容不带 \r；无差异返回空 hunks', () => {
    const bp = buildFilePatch('a\r\nb\r\nc', 'a\r\nB\r\nc');
    assert.ok(bp);
    assert.deepStrictEqual(bp.hunks[0].oldLines, ['a', 'b', 'c']);
    assert.deepStrictEqual(bp.hunks[0].newLines, ['a', 'B', 'c']);
    const same = buildFilePatch('a\nb\n', 'a\nb\n')!;
    assert.strictEqual(same.hunks.length, 0);
    assert.strictEqual(same.shifts.length, 0);
  });

  it('往返：before → 补丁 → after，逆补丁精确回到 before（fuzz=0）', () => {
    const B = ['import os', '', 'def f():', '    return 1', '', 'def g():', '    return 2'];
    const A = ['import os', '', 'def f():', '    return 42', '', 'def g():', '    return 2'];
    const bp = buildFilePatch(B.join('\n'), A.join('\n'))!;
    const fwd = applyPatchToLines(B, patchOf(bp.hunks));
    assert.deepStrictEqual(fwd.lines, A);
    const back = applyPatchToLines(A, invertFilePatch(patchOf(bp.hunks)));
    assert.ok(back.lines);
    assert.strictEqual(back.fuzz, 0);
    assert.deepStrictEqual(back.lines, B);
  });

  it('同文件双修复乱序回滚（用户 BUG 回归锁）：先回滚 f1 时 f2 内容保留', () => {
    // 两处修复相距超过 hunk 上下文窗口（同文件不同函数的典型场景）
    const B = ['l1', 'l2', 'l3', 'l4', 'l5', 'l6', 'l7', 'l8', 'l9', 'l10', 'l11', 'l12'];
    const F1 = B.map((l) => (l === 'l2' ? 'l2-fixed' : l));
    const F2 = F1.map((l) => (l === 'l10' ? 'l10-fixed' : l));
    const p1 = buildFilePatch(B.join('\n'), F1.join('\n'))!;
    const p2 = buildFilePatch(F1.join('\n'), F2.join('\n'))!;
    // 乱序回滚 f1：F2 上撤销 f1 的修改（fuzz=0 精确），f2 的修改原样保留
    const r1 = applyPatchToLines(F2, invertFilePatch(patchOf(p1.hunks)));
    assert.ok(r1.lines, `外科回滚应成功: ${r1.failures[0]?.reason ?? ''}`);
    assert.strictEqual(r1.fuzz, 0);
    assert.deepStrictEqual(
      r1.lines,
      ['l1', 'l2', 'l3', 'l4', 'l5', 'l6', 'l7', 'l8', 'l9', 'l10-fixed', 'l11', 'l12'],
    );
    // 顺序回滚 f2：F2 上撤销 f2 的修改，回到 F1
    const r2 = applyPatchToLines(F2, invertFilePatch(patchOf(p2.hunks)));
    assert.deepStrictEqual(r2.lines, F1);
  });

  it('f2 落在 f1 的 hunk 上下文窗口内 → 高 fuzz 命中，执行器（fuzz>1 拒绝）能识别不可外科', () => {
    const B = ['l1', 'l2', 'l3', 'l4', 'l5', 'l6'];
    const F1 = B.map((l) => (l === 'l2' ? 'l2-fixed' : l));
    // f2 修改 l4，落在 f1 hunk（l2±3 上下文）窗口内
    const F2 = F1.map((l) => (l === 'l4' ? 'l4-fixed' : l));
    const p1 = buildFilePatch(B.join('\n'), F1.join('\n'))!;
    const r = applyPatchToLines(F2, invertFilePatch(patchOf(p1.hunks)));
    // 锚定侧（f1 修复后内容+上下文）在 F2 中无精确命中 → 只能相似度级命中（fuzz=1000）
    // 或整体失败：extension 层对两者都拒绝外科、降级 checkpoint 整文件覆盖，绝不错切
    assert.ok(r.lines === null || r.fuzz > 1, `应不可精确外科: fuzz=${r.fuzz}`);
  });

  it('内容漂移时逆补丁整体拒绝（lines=null，调用方降级整文件覆盖，绝不错切）', () => {
    const B = ['first-line', 'second-line', 'third-line'];
    const p1 = buildFilePatch(B.join('\n'), ['first-line', 'second-line', 'TOTALLY-CHANGED'].join('\n'))!;
    const drifted = ['ZZZZ', 'YYYY', 'XXXX'];
    const r = applyPatchToLines(drifted, invertFilePatch(patchOf(p1.hunks)));
    assert.strictEqual(r.lines, null);
  });

  it('shiftLine：替换区后的行加净差、区间内贴新起点、纯插入整体后移', () => {
    const replace = [{ start: 4, delCount: 2, delta: 1 }]; // 0-based 4-5 两行换三行
    assert.strictEqual(shiftLine(1, replace), 1);
    assert.strictEqual(shiftLine(4, replace), 4);
    assert.strictEqual(shiftLine(5, replace), 5);
    assert.strictEqual(shiftLine(6, replace), 5);
    assert.strictEqual(shiftLine(7, replace), 8);
    const ins = [{ start: 3, delCount: 0, delta: 2 }];
    assert.strictEqual(shiftLine(3, ins), 3);
    assert.strictEqual(shiftLine(4, ins), 6);
    const multi = [{ start: 2, delCount: 1, delta: -1 }, { start: 8, delCount: 0, delta: 3 }];
    assert.strictEqual(shiftLine(5, multi), 4); // orig 4 过第一块 -1 → 3 → 1-based 4；第二块条件用原始行号不触发
    assert.strictEqual(shiftLine(9, multi), 11); // orig 8：过两块 -1 +3 → 10 → 1-based 11
  });
});
