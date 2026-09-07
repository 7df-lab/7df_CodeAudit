// 结构守卫：不测行为，测"结构性约束没有被无声破坏"（命令漂移/死配置键/测试盲区）。
// 依据：docs/regressions.md 缺陷档案 #2/#3/#4；此类缺陷的症状是 runtime 才炸或静默失效，
// 行为测试难以枚举，靠静态一致性检查钉死。
import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';

// out-test/vscode-plugin/test/*.test.js → 仓库根 = ../../..
const ROOT = path.resolve(__dirname, '..', '..', '..');
const pkg = JSON.parse(fs.readFileSync(path.join(ROOT, 'package.json'), 'utf8'));
const extSrc = fs.readFileSync(path.join(ROOT, 'src', 'extension.ts'), 'utf8');

describe('guards（结构守卫：无声漂移类缺陷的门禁）', () => {
  it('命令注册守卫：package.json 声明 ⇄ extension.ts 注册互为全集（回归锁：command not found 类缺陷）', () => {
    const declared: string[] = pkg.contributes.commands.map((c: { command: string }) => c.command).sort();
    const registered = [...extSrc.matchAll(/registerCommand\(\s*'([^']+)'/g)].map((m) => m[1]).sort();
    assert.deepStrictEqual(registered, declared, 'contributes.commands 与 registerCommand 必须一一对应');
  });

  it('配置键守卫：src 读取的配置键 ⇄ package.json configuration 声明互为一致（回归锁：死配置键）', () => {
    const declared = Object.keys(pkg.contributes.configuration.properties)
      .map((k) => k.replace('codeaudit.', '')).sort();
    const used = [...new Set([...extSrc.matchAll(/cfg\(\)\.get(?:<[^>]*>)?\(\s*'([^']+)'/g)].map((m) => m[1]))].sort();
    assert.deepStrictEqual(used, declared, 'src 读取的键都必须有声明、声明的键都必须被读取');
  });

  it('视图 ID 守卫：package.json views ⇄ extension 注册互为全集（回归锁：视图注册漂移）', () => {
    const declared: string[] = [];
    for (const container of Object.values<{ id: string }[]>(pkg.contributes.views)) {
      for (const v of container) declared.push(v.id);
    }
    const registered = [...extSrc.matchAll(/register(?:TreeDataProvider|WebviewViewProvider)\(\s*'([^']+)'/g)].map((m) => m[1]);
    assert.deepStrictEqual([...new Set(registered)].sort(), [...new Set(declared)].sort());
  });

  it('上下文键守卫：extension 设置的上下文键都被 package.json when 子句消费（回归锁：死上下文键）', () => {
    const setKeys = [...new Set([...extSrc.matchAll(/setCtx\(\s*'([^']+)'/g)].map((m) => m[1]))];
    const whens: string[] = [];
    const walk = (o: unknown): void => {
      if (!o || typeof o !== 'object') return;
      if (Array.isArray(o)) { o.forEach(walk); return; }
      const obj = o as Record<string, unknown>;
      if (typeof obj.when === 'string') whens.push(obj.when);
      Object.values(obj).forEach(walk);
    };
    walk(pkg.contributes);
    const whenText = whens.join(' ');
    for (const k of setKeys) {
      assert.ok(whenText.includes(k), `上下文键 ${k} 没有任何 when 子句消费（死键或拼写漂移）`);
    }
  });

  it('测试模块覆盖守卫：src/*.ts 每个模块都被至少一个测试引用；tsconfig.test.json 不新增排除（回归锁：测试盲区）', () => {
    const tsconfig = JSON.parse(fs.readFileSync(path.join(ROOT, 'tsconfig.test.json'), 'utf8'));
    assert.deepStrictEqual([...tsconfig.exclude].sort(), ['node_modules', 'out'],
      'tsconfig.test.json 的 exclude 不得加入 src 文件——那是把代码移出测试体系（regressions.md #4）');
    const srcModules = fs.readdirSync(path.join(ROOT, 'src'))
      .filter((f) => f.endsWith('.ts')).map((f) => f.replace(/\.ts$/, '')).sort();
    const testDir = path.join(ROOT, 'test');
    let testSources = '';
    for (const f of fs.readdirSync(testDir)) {
      if (f.endsWith('.test.ts')) testSources += fs.readFileSync(path.join(testDir, f), 'utf8');
    }
    const imported = [...new Set([...testSources.matchAll(/'\.\.\/src\/([a-zA-Z]+)'/g)].map((m) => m[1]))].sort();
    assert.deepStrictEqual(imported, srcModules, 'src 模块 ⇄ 测试引用必须互为全集（新模块必须带测试，见 docs/regressions.md 纪律 3）');
  });
});
