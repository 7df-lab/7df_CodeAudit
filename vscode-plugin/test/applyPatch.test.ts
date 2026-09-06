import * as assert from 'assert';
import * as os from 'os';
import {
  computePatchChanges,
  DiffError,
  listUpdatedFiles,
  PatchActionType,
} from '../src/applyPatch';

// —— ADR-183 验收工作区（9 行 SQL 注入，与平台 vuln_fix_demo.py / 沙箱证据同构）——
const DEMO = [
  'import sqlite3', '', '',
  'def get_user(conn, user_id):',
  '    cursor = conn.cursor()',
  `    query = "SELECT * FROM users WHERE id = '" + user_id + "'"`,
  '    cursor.execute(query)',
  '    # 执行查询并返回结果',
  '    return cursor.fetchone()',
];

// 真实沙箱运行产出（平台 evidence/adr183_diff_patch/06_findings_r2.json 的 diff_patch 原文）
const REAL_PATCH =
  '*** Begin Patch\n' +
  '*** Update File: vuln_fix_demo.py\n' +
  '@@     cursor = conn.cursor()\n' +
  `-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"\n` +
  '-    cursor.execute(query)\n' +
  '+    query = "SELECT * FROM users WHERE id = ?"\n' +
  '+    cursor.execute(query, (user_id,))\n' +
  '*** End Patch';

const F = { 'f.txt': 'a\nb\nc' };

describe('applyPatch：ADR-183 平台契约场景（真实 diff_patch 全链）', () => {
  it('A. 真实沙箱补丁 fuzz=0 应用：参数化替换就位、return 行保留', () => {
    const { changes, fuzz } = computePatchChanges(REAL_PATCH, { 'vuln_fix_demo.py': DEMO.join('\n') });
    assert.strictEqual(fuzz, 0, '文件未改动时，服务端逐字校验过的补丁应 fuzz=0');
    const after = changes['vuln_fix_demo.py'].newContent!.split('\n');
    assert.ok(after.includes('    query = "SELECT * FROM users WHERE id = ?"'), '参数化查询已替换');
    assert.ok(after.includes('    cursor.execute(query, (user_id,))'), '参数元组绑定执行');
    assert.strictEqual(after[8], '    return cursor.fetchone()', 'return 行保留');
    assert.ok(!after.some((l) => l.includes("'\" + user_id + \"'")), '拼接构造行已移除');
  });

  it('B. 验收口径 3删3增：行数守恒 9→9', () => {
    const patch =
      '*** Begin Patch\n' +
      '*** Update File: vuln_fix_demo.py\n' +
      '@@     cursor = conn.cursor()\n' +
      `-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"\n` +
      '-    cursor.execute(query)\n' +
      '-    # 执行查询并返回结果\n' +
      '+    query = "SELECT * FROM users WHERE id = ?"\n' +
      '+    cursor.execute(query, (user_id,))\n' +
      '+    # 参数化查询：用户输入不进入 SQL 文本\n' +
      '*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'vuln_fix_demo.py': DEMO.join('\n') });
    assert.strictEqual(fuzz, 0);
    const after = changes['vuln_fix_demo.py'].newContent!.split('\n');
    assert.ok(after.includes('    # 参数化查询：用户输入不进入 SQL 文本'));
    assert.strictEqual(after.length, DEMO.length, `行数守恒（实际 ${after.length}）`);
  });

  it('C. 行号漂移（头部插入 3 行）不影响内容锚定，结果与非漂移一致', () => {
    const drifted = ['# header 1', '# header 2', '# header 3', ...DEMO];
    const { changes, fuzz } = computePatchChanges(REAL_PATCH, { 'vuln_fix_demo.py': drifted.join('\n') });
    assert.strictEqual(fuzz, 0);
    const after = changes['vuln_fix_demo.py'].newContent!.split('\n');
    const base = computePatchChanges(REAL_PATCH, { 'vuln_fix_demo.py': DEMO.join('\n') }).changes['vuln_fix_demo.py'].newContent!.split('\n');
    assert.deepStrictEqual(after.slice(3), base);
  });

  it('D. 多文件：一个补丁两个 Update File 段，各自独立锚定', () => {
    const appPy = ['import os', '', 'TOKEN = "hardcoded-secret"', ''];
    const settingsPy = ['import os', 'DEBUG = True', ''];
    const patch =
      '*** Begin Patch\n' +
      '*** Update File: app.py\n' +
      '@@ import os\n' +
      ' \n' +
      '-TOKEN = "hardcoded-secret"\n' +
      '+TOKEN = CodeAudit.secrets.APP_TOKEN\n' +
      '*** Update File: settings.py\n' +
      '@@ import os\n' +
      '-DEBUG = True\n' +
      '+DEBUG = False\n' +
      '*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, {
      'app.py': appPy.join('\n'),
      'settings.py': settingsPy.join('\n'),
    });
    assert.strictEqual(fuzz, 0);
    assert.deepStrictEqual(changes['app.py'].newContent!.split('\n'), ['import os', '', 'TOKEN = CodeAudit.secrets.APP_TOKEN', '']);
    assert.deepStrictEqual(changes['settings.py'].newContent!.split('\n'), ['import os', 'DEBUG = False', '']);
  });

  it('E. 不可锚定上下文 → DiffError 整体拒绝（绝不部分应用）', () => {
    const bad = REAL_PATCH
      .replace(`-    query = "SELECT * FROM users WHERE id = '" + user_id + "'"`, '-    this line exists nowhere in the workspace xyzzy plugh 42')
      .replace('-    cursor.execute(query)', '-    neither does this one zorkmid frobnicate');
    assert.throws(
      () => computePatchChanges(bad, { 'vuln_fix_demo.py': DEMO.join('\n') }),
      (e: Error) => e instanceof DiffError && /vuln_fix_demo\.py: hunk 1/.test(e.message) && /上下文未命中/.test(e.message),
    );
  });
});

describe('applyPatch：Update 段锚定语义（Cline PatchParser 逐字对标）', () => {

  it('@@ 定义行（defStr）：游标推进到该行之后，变更紧随锚定', () => {
    const patch = '*** Begin Patch\n*** Update File: f.py\n@@ def foo():\n-    return 1\n+    return 2\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'f.py': 'def foo():\n    return 1' });
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['f.py'].newContent, 'def foo():\n    return 2');
  });

  it('defStr 仅 trim 命中（缩进漂移）→ fuzz+1，文件自身缩进保留', () => {
    const patch = '*** Begin Patch\n*** Update File: f.py\n@@ def foo():\n-    return 1\n+    return 2\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'f.py': '  def foo():\n    return 1' });
    assert.strictEqual(fuzz, 1);
    assert.strictEqual(changes['f.py'].newContent, '  def foo():\n    return 2');
  });

  it('裸 @@ 行仅作分段，不参与锚定', () => {
    const patch = '*** Begin Patch\n*** Update File: f.txt\n@@\n a\n-b\n+B\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, F);
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['f.txt'].newContent, 'a\nB\nc');
  });

  it('*** End of File：上下文在末尾 → 末尾锚定 fuzz=0', () => {
    const patch = '*** Begin Patch\n*** Update File: f.txt\n@@\n c\n+X\n*** End of File\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, F);
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['f.txt'].newContent, 'a\nb\nc\nX');
  });

  it('*** End of File 未在末尾命中 → 回退全文扫描，fuzz+10000（透明标注）', () => {
    const patch = '*** Begin Patch\n*** Update File: f.txt\n@@\n b\n+c2\n*** End of File\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'f.txt': 'a\nb\nc\nd' });
    assert.strictEqual(fuzz, 10000);
    assert.strictEqual(changes['f.txt'].newContent, 'a\nb\nc2\nc\nd');
  });

  it('顺序游标：两个 @@ 段在重复块上依序各锚定一处（首个→第二处）', () => {
    const file = ['h', 'blk', 't', 'blk', 'end'];
    const patch =
      '*** Begin Patch\n*** Update File: f.txt\n@@\n blk\n-t\n+T1\n@@\n blk\n-end\n+T2\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'f.txt': file.join('\n') });
    assert.strictEqual(fuzz, 0);
    assert.deepStrictEqual(changes['f.txt'].newContent!.split('\n'), ['h', 'blk', 'T1', 'blk', 'T2']);
  });

  it('无前缀行按上下文行容错（peek 补空格）', () => {
    const patch = '*** Begin Patch\n*** Update File: f.txt\n@@\n a\nomega\n+tail\n c\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'f.txt': 'a\nomega\nc' });
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['f.txt'].newContent, 'a\nomega\ntail\nc');
  });

  it('上下文块 trim 级命中（缩进漂移）fuzz=100；相似度级命中 fuzz=1000', () => {
    const file = { 'f.txt': 'alpha\nbeta\nc' };
    const trimPatch = '*** Begin Patch\n*** Update File: f.txt\n@@\n   alpha\n-beta\n+BETA\n*** End Patch';
    const r1 = computePatchChanges(trimPatch, file);
    assert.strictEqual(r1.fuzz, 100, '两端 trim 后全等');
    assert.strictEqual(r1.changes['f.txt'].newContent, 'alpha\nBETA\nc');
    const simPatch = '*** Begin Patch\n*** Update File: f.txt\n@@\n alphX\n-beta\n+BETA\n*** End Patch';
    const r2 = computePatchChanges(simPatch, file);
    assert.strictEqual(r2.fuzz, 1000, '相似度≥0.66 首个命中');
    assert.strictEqual(r2.changes['f.txt'].newContent, 'alpha\nBETA\nc');
  });

  it("canonicalize 转义还原：文件中的 \\' 与补丁中的裸引号精确命中 fuzz=0", () => {
    const patch = '*** Begin Patch\n*** Update File: q.txt\n@@\n it\\\'s fine\n x = 1\n+new line\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, { 'q.txt': "it's fine\nx = 1" });
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['q.txt'].newContent, "it's fine\nx = 1\nnew line");
  });
});

describe('applyPatch：Add / Delete / Move', () => {
  it('Add File：newFile 逐行拼接（平台 EOL）', () => {
    const patch = '*** Begin Patch\n*** Add File: note.txt\n+one\n+two\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, {});
    assert.strictEqual(changes['note.txt'].type, PatchActionType.ADD);
    assert.strictEqual(changes['note.txt'].newContent, ['one', 'two'].join(os.EOL));
    assert.strictEqual(fuzz, 0);
  });

  it('Delete File：oldContent 快照', () => {
    const patch = '*** Begin Patch\n*** Delete File: f.txt\n*** End Patch';
    const { changes } = computePatchChanges(patch, F);
    assert.strictEqual(changes['f.txt'].type, PatchActionType.DELETE);
    assert.strictEqual(changes['f.txt'].oldContent, 'a\nb\nc');
  });

  it('Move to：movePath 携带 + 内容按补丁变更', () => {
    const patch = '*** Begin Patch\n*** Update File: old.txt\n*** Move to: new.txt\n@@\n one\n-two\n+TWO\n*** End Patch';
    const { changes } = computePatchChanges(patch, { 'old.txt': 'one\ntwo' });
    assert.strictEqual(changes['old.txt'].movePath, 'new.txt');
    assert.strictEqual(changes['old.txt'].newContent, 'one\nTWO');
  });

  it('Move to 的 CRLF 文件：新路径内容保留 CRLF', () => {
    const patch = '*** Begin Patch\n*** Update File: old.txt\n*** Move to: new.txt\n@@\n one\n-two\n+TWO\n*** End Patch';
    const { changes } = computePatchChanges(patch, { 'old.txt': 'one\r\ntwo' });
    assert.strictEqual(changes['old.txt'].newContent, 'one\r\nTWO');
  });

  it('Add 目标已在 currentFiles → DiffError File already exists', () => {
    const patch = '*** Begin Patch\n*** Add File: f.txt\n+x\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, F), /Add File Error: File already exists: f\.txt/);
  });

  it('Add 段行缺 + 前缀 → DiffError', () => {
    const patch = '*** Begin Patch\n*** Add File: n.txt\nhello\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, {}), /Invalid Add File line \(missing '\+'\)/);
  });
});

describe('applyPatch：段级校验与语法（对标 Cline）', () => {
  it('Update 引用不存在的文件 → DiffError Missing File（整体拒绝）', () => {
    const patch = '*** Begin Patch\n*** Update File: nope.txt\n@@\n a\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, F), /Update File Error: Missing File: nope\.txt/);
  });

  it('同文件重复段 → DiffError Duplicate', () => {
    const patch = '*** Begin Patch\n*** Update File: f.txt\n@@\n a\n*** Update File: f.txt\n@@\n b\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, F), /Duplicate update for file: f\.txt/);
  });

  it('未知指令行 → DiffError Unknown line', () => {
    const patch = '*** Begin Patch\n*** Rename File: f.txt\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, F), /Unknown line while parsing/);
  });

  it('哨兵不完整（Begin 无 End）→ DiffError incomplete sentinels', () => {
    assert.throws(
      () => computePatchChanges('*** Begin Patch\n*** Add File: n.txt\n+hello', {}),
      /Invalid patch text - incomplete sentinels/,
    );
  });

  it('自由格式（无 sentinel）自动包裹 Begin/End 后应用', () => {
    const { changes, fuzz } = computePatchChanges('*** Update File: f.txt\n@@\n a\n-b\n+B\n', F);
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['f.txt'].newContent, 'a\nB\nc');
  });

  it('遗留 bash 包装（%%bash / apply_patch <<EOF / EOF）被剥离', () => {
    const patch = '%%bash\napply_patch <<"EOF"\n*** Begin Patch\n*** Add File: n.txt\n+hello\n*** End Patch\nEOF';
    const { changes } = computePatchChanges(patch, {});
    assert.strictEqual(changes['n.txt'].newContent, 'hello');
  });

  it('End sentinel 带尾部空白仍被接受', () => {
    const patch = '*** Begin Patch\n*** Add File: n.txt\n+hello\n*** End Patch ';
    const { changes } = computePatchChanges(patch, {});
    assert.strictEqual(changes['n.txt'].newContent, 'hello');
  });

  it('上下文失配 message 含路径/hunk 序号/上下文预览（≤200+省略号）', () => {
    const patch = '*** Begin Patch\n*** Update File: f.txt\n@@\n unrelated heading\n missing middle\n+replacement\n absent footer\n*** End Patch';
    assert.throws(
      () => computePatchChanges(patch, F),
      (e: Error) => {
        assert.ok(e instanceof DiffError);
        assert.match(e.message, /f\.txt: hunk 1: 上下文未命中（相似度 0\.\d\d）/);
        assert.match(e.message, /上下文预览:/);
        assert.match(e.message, /absent footer/);
        return true;
      },
    );
  });
});

describe('applyPatch：跳跃 hunk（平台 fixpatch 逐行顺序锚定）', () => {
  // 真实案例（mica-mqtt CWE-208）：@@ 定义行锚 import 区，delete 行在几十行外的方法体，
  // 中间无上下文连接。整段连续锚定必然失败；旧逻辑相似度兜底会错位（import 插进方法体）。
  const JUMP_PATCH = [
    '*** Begin Patch',
    '*** Update File: Auth.java',
    '@@ import java.nio.charset.StandardCharsets;',
    '+import java.security.MessageDigest;',
    ' import java.util.Base64;',
    '-\t\tboolean equals = token.equals(authorization.substring(length));',
    '+\t\t// 常量时间比较',
    '+\t\tboolean equals = MessageDigest.isEqual(a, b);',
    '*** End Patch',
  ].join('\n');

  const FILE = [
    'package x;',
    'import java.nio.charset.StandardCharsets;',
    'import java.util.Base64;',
    'import java.util.Objects;',
    '',
    'public class Auth {',
    '	public HttpResponse doFilter() {',
    '		int length = 6;',
    '		if (length >= authorization.length()) {',
    '			return response(request);',
    '		}',
    '		boolean equals = token.equals(authorization.substring(length));',
    '		if (equals) {',
    '			return chain.doFilter(request);',
    '		}',
    '		return response(request);',
    '	}',
    '}',
  ].join('\n');

  it('delete 行与上下文之间允许间隙：逐行顺序锚定，import 插入顶部 import 区、替换发生在方法体', () => {
    const { changes, fuzz } = computePatchChanges(JUMP_PATCH, { 'Auth.java': FILE });
    const out = (changes['Auth.java']?.newContent ?? '').split('\n');
    assert.strictEqual(fuzz, 0, '逐字顺序命中不记 fuzz（平台口径）');
    assert.ok(out.some((l: string) => l === 'import java.security.MessageDigest;'), '+import 行随 chunk 落在 delete 行位置（apply_patch 语义，平台 Go 端一致）');
    assert.strictEqual(out[3] ?? '', 'import java.util.Base64;');
    assert.ok(!out.some((l: string) => l.includes('token.equals(authorization)')), '旧比较行应被替换');
    const replaced = out.findIndex((l: string) => l.includes('MessageDigest.isEqual(a, b)'));
    assert.ok(replaced > 10, '替换应发生在方法体内（原 delete 行位置）');
    // 整体结构守恒：删 1 行增 3 行（insLines=import+注释+比较，全部落在 delete 行位置）
    assert.strictEqual(out.length, FILE.split('\n').length + 2);
  });

  it('逐行锚定任一行未命中 → 整体拒绝（不落到相似度错位）', () => {
    const broken = FILE.replace('import java.util.Base64;', 'import java.util.Base64X;');
    assert.throws(() => computePatchChanges(JUMP_PATCH, { 'Auth.java': broken }),
      (e: unknown) => e instanceof Error);
  });
});

describe('applyPatch：行尾（块运算 LF 空间，输出还原文件自身 EOL）', () => {
  it('CRLF 文件 + LF 补丁 → 输出保留 CRLF（不被整体改写）', () => {
    const patch = '*** Begin Patch\n*** Update File: note.txt\n@@\n alpha\n-beta\n+BETA\n+inserted\n gamma\n*** End Patch';
    const { changes } = computePatchChanges(patch, { 'note.txt': 'alpha\r\nbeta\r\ngamma' });
    assert.strictEqual(changes['note.txt'].newContent, 'alpha\r\nBETA\r\ninserted\r\ngamma');
  });

  it('纯 LF 文件不被引入 CRLF', () => {
    const patch = '*** Begin Patch\n*** Update File: lf.txt\n@@\n one\n-two\n+TWO\n three\n*** End Patch';
    const { changes } = computePatchChanges(patch, { 'lf.txt': 'one\ntwo\nthree' });
    assert.strictEqual(changes['lf.txt'].newContent, 'one\nTWO\nthree');
  });

  it('CRLF 补丁文本（每行尾带回车符）被归一后正常解析', () => {
    const patch = '*** Begin Patch\r\n*** Update File: f.txt\r\n@@\r\n a\r\n-b\r\n+B\r\n*** End Patch';
    const { changes, fuzz } = computePatchChanges(patch, F);
    assert.strictEqual(fuzz, 0);
    assert.strictEqual(changes['f.txt'].newContent, 'a\nB\nc');
  });
});

describe('applyPatch：语法健壮性（回归锁：拒绝而非挂死/错位）', () => {
  it('Update 段头部后紧跟空行 → DiffError 拒绝（曾经是同步死循环，冻结扩展宿主）', () => {
    // 空行使 peek 原地 break、空上下文 findContext 返回 start、endPatchIndex 原位——
    // 三个游标全不前进，旧实现 while 循环挂死
    const patch = '*** Begin Patch\n*** Update File: a.ts\n\n@@ c\n x\n-y\n+Y\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, { 'a.ts': 'c\nx\ny\n' }), /no progress/);
  });

  it('两个 Update 段之间夹杂空行 → DiffError 拒绝（不部分应用）', () => {
    const patch = '*** Begin Patch\n*** Update File: a.ts\n@@ c\n x\n-y\n+Y\n\n*** End Patch';
    assert.throws(() => computePatchChanges(patch, { 'a.ts': 'c\nx\ny\n' }), (e: unknown) => e instanceof Error);
  });

  it('跳跃 hunk 多段：第二段 @@ 从第一段消费区之后继续扫描（游标单调推进）', () => {
    // 第一段消费 [X, ctx]（文件 0-1 行）后，第二段的 defStr/上下文必须从第 2 行起
    // 扫描——两段顺序各锚定一处，内容锚定单调向前，绝不回吃已消费区域
    const file = ['X', 'ctx', 'X', 'ctx', 'end'];
    const patch = [
      '*** Begin Patch',
      '*** Update File: f.txt',
      '@@ X',
      '-ctx',
      '+CTX1',
      '@@ X',
      '-end',
      '+END2',
      '*** End Patch',
    ].join('\n');
    const { changes, fuzz } = computePatchChanges(patch, { 'f.txt': file.join('\n') });
    assert.strictEqual(fuzz, 0);
    assert.deepStrictEqual(changes['f.txt'].newContent!.split('\n'), ['X', 'CTX1', 'X', 'ctx', 'END2']);
  });
});

describe('applyPatch：listUpdatedFiles（调用方预载文件内容用）', () => {
  it('列出 Update/Delete 段路径，不含 Add 段', () => {
    const patch =
      '*** Begin Patch\n*** Update File: a.py\n@@\n x\n*** Delete File: b.py\n*** Add File: c.py\n+d\n*** End Patch';
    assert.deepStrictEqual(listUpdatedFiles(patch), ['a.py', 'b.py']);
  });
});

describe('applyPatch：同文件乱序多段（用户实测回归：曾因顺序游标不回溯整体拒绝）', () => {
  const F = ['h', 'def a():', '  x = 1', '  old1()', '', 'def b():', '  y = 2', '  old2()', 'tail'].join('\n');
  // 两个 @@ 段乱序：远端段（def b）写在前、近邻段（def a）写在后
  const REVERSED = [
    '*** Begin Patch',
    '*** Update File: f.py',
    '@@ def b():',
    '-  old2()',
    '+  NEW2()',
    '@@ def a():',
    '-  old1()',
    '+  NEW1()',
    '*** End Patch',
  ].join('\n');

  it('乱序双段：两段各自锚定成功并按位置排序应用', () => {
    const { changes, fuzz } = computePatchChanges(REVERSED, { 'f.py': F });
    assert.strictEqual(fuzz, 0);
    const out = changes['f.py'].newContent!.split('\n');
    assert.ok(out.some((l) => l.includes('NEW1()')), '近邻段已应用');
    assert.ok(out.some((l) => l.includes('NEW2()')), '远端段已应用');
    assert.deepStrictEqual(out, ['h', 'def a():', '  x = 1', '  NEW1()', '', 'def b():', '  y = 2', '  NEW2()', 'tail']);
  });

  it('正序双段行为不变（顺序优先语义回归锁）', () => {
    const ORDERED = [
      '*** Begin Patch',
      '*** Update File: f.py',
      '@@ def a():',
      '-  old1()',
      '+  NEW1()',
      '@@ def b():',
      '-  old2()',
      '+  NEW2()',
      '*** End Patch',
    ].join('\n');
    const { changes, fuzz } = computePatchChanges(ORDERED, { 'f.py': F });
    assert.strictEqual(fuzz, 0);
    assert.ok(changes['f.py'].newContent!.includes('NEW1()'));
    assert.ok(changes['f.py'].newContent!.includes('NEW2()'));
  });

  it('回溯不救重叠段：同区域两段仍整体拒绝', () => {
    const OVERLAP = [
      '*** Begin Patch',
      '*** Update File: f.py',
      '@@ def a():',
      '-  x = 1',
      '+  x = 2',
      '@@ def a():',
      '-  x = 1',
      '+  x = 3',
      '*** End Patch',
    ].join('\n');
    assert.throws(() => computePatchChanges(OVERLAP, { 'f.py': F }), /currentIndex|重叠|origIndex/);
  });

  it('乱序双段带上下文行（apply_patch 标记空格口径）：逐字锚定不因标记剥离失配', () => {
    // apply_patch 语法：裸上下文行首字符是标记空格，内容 = 去 1 个前导空格。
    // 回归 GUI fx-7 实测：上下文内容按文件原样缩进书写时被剥掉 1 格，乱序场景
    // 下逐字锚定（拒绝模糊）找不到目标 → 整补丁被拒
    const FC = [
      'import os',
      '',
      '',
      'def load(conn, user_id):',
      '    # comment',
      '    q = old(user_id)',
      '    return q',
      '',
      '',
      'def drop(conn, name):',
      '    # comment2',
      '    run("DROP " + name)',
      '    return True',
    ].join('\n');
    const PATCH_CTX = [
      '*** Begin Patch',
      '*** Update File: fc.py',
      '@@ def drop(conn, name):',
      '     # comment2',
      '-    run("DROP " + name)',
      '+    run("DROP ?", (name,))',
      '@@ def load(conn, user_id):',
      '     # comment',
      '-    q = old(user_id)',
      '+    q = new(user_id)',
      '*** End Patch',
    ].join('\n');
    const { changes, fuzz } = computePatchChanges(PATCH_CTX, { 'fc.py': FC });
    assert.strictEqual(fuzz, 0);
    const out = changes['fc.py'].newContent!.split('\n');
    assert.ok(out.some((l) => l.includes('new(user_id)')), '近端段已应用');
    assert.ok(out.some((l) => l.includes('run("DROP ?"')), '远端段已应用');
  });
});
