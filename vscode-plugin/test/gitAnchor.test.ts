// ADR-225 增量扫描 S2 锁定测试（验收 F16-F18 + F20 树徽章 + createTask 请求体）。
// 纯函数直跑；vscode.git 访问经 setRepoProviderForTest 注入桩（不依赖真 git 扩展）。
import * as assert from 'assert';
import * as path from 'path';
import * as vscode from 'vscode';
import {
  buildDiffHint, collectGitAnchor, collectGitChanges, countNewCommits,
  previewText, sameAnchorNoChange, setRepoProviderForTest, type GitChangeEntry,
} from '../src/gitAnchor';
import { findingDescription } from '../src/treeModel';
import type { GitAnchor, UnifiedFinding } from '../src/types';

function fakeRepo(over: Partial<{
  commit: string; branch: string; work: [string, number?][]; index: [string, number?][]; remotes: { name: string; fetchUrl?: string }[]; log: string[];
}> = {}) {
  const uriOf = (p: string) => vscode.Uri.file(path.resolve('/ws', p));
  return {
    state: {
      HEAD: { commit: over.commit ?? 'c1234567890abcdef', name: over.branch ?? 'main' },
      workingTreeChanges: (over.work ?? []).map(([p, s]) => ({ uri: uriOf(p), status: s })),
      indexChanges: (over.index ?? []).map(([p, s]) => ({ uri: uriOf(p), status: s })),
      remotes: over.remotes ?? [{ name: 'origin', fetchUrl: 'https://git.example/x.git' }],
    },
    log: async () => (over.log ?? ['c1234567890abcdef']).map((hash) => ({ hash })),
  };
}

const ANCHOR = (over: Partial<GitAnchor> = {}): GitAnchor =>
  ({ commit: 'abc', branch: 'main', dirty: false, remote: '', ...over });

describe('gitAnchor（ADR-225 D5）', () => {
  afterEach(() => setRepoProviderForTest(async () => null));

  it('F16.1 锚点采集：commit/branch/dirty/origin', async () => {
    setRepoProviderForTest(async () => fakeRepo({ work: [['src/a.py', 7]] }));
    const a = await collectGitAnchor();
    assert.strictEqual(a?.commit, 'c1234567890abcdef');
    assert.strictEqual(a?.branch, 'main');
    assert.strictEqual(a?.dirty, true);
    assert.strictEqual(a?.remote, 'https://git.example/x.git');
  });

  it('F16.2 非 git / 仓库未初始化 → null 不报错', async () => {
    setRepoProviderForTest(async () => null);
    assert.strictEqual(await collectGitAnchor(), null);
    setRepoProviderForTest(async () => fakeRepo({ commit: '' }));
    assert.strictEqual(await collectGitAnchor(), null);
  });

  it('F16.4 变更清单：工作区+暂存去重、相对路径正斜杠、untracked 计入 dirty 口径一致', async () => {
    setRepoProviderForTest(async () => fakeRepo({
      work: [['src/a.py', 7], ['new.py', 1], ['out/b.py', 1]],
      index: [['src/a.py', 3], ['gone.py', 6]],
    }));
    const changes = await collectGitChanges(path.resolve('/ws'));
    const byPath = Object.fromEntries(changes.map((c: GitChangeEntry) => [c.relPath, c.status]));
    assert.deepStrictEqual(byPath, {
      'src/a.py': 'M', // 工作区先见（7），暂存同路径不覆盖
      'new.py': 'A',
      'out/b.py': 'A',
      'gone.py': 'D',
    });
    const anchor = await collectGitAnchor();
    assert.strictEqual(anchor?.dirty, true); // 与 hint 同源枚举
  });

  it('diff_hint 原文=name-status 风格；空清单→空串', () => {
    assert.strictEqual(
      buildDiffHint([{ relPath: 'a.py', status: 'M' }, { relPath: 'b.py', status: 'A' }]),
      'M\ta.py\nA\tb.py',
    );
    assert.strictEqual(buildDiffHint([]), '');
  });

  it('F17 无变更判定矩阵：commit 同且双方干净才真', () => {
    assert.strictEqual(sameAnchorNoChange(ANCHOR(), ANCHOR()), true);
    assert.strictEqual(sameAnchorNoChange(ANCHOR({ dirty: true }), ANCHOR()), false); // 当前脏
    assert.strictEqual(sameAnchorNoChange(ANCHOR(), ANCHOR({ dirty: true })), false); // 基线脏
    assert.strictEqual(sameAnchorNoChange(ANCHOR({ commit: 'zzz' }), ANCHOR()), false);
    assert.strictEqual(sameAnchorNoChange(null, ANCHOR()), false); // 非 git 不提示
    assert.strictEqual(sameAnchorNoChange(ANCHOR(), null), false); // 基线无锚点（旧任务）
  });

  it('F18 变更预告：commit 计数到基线为止；不可得→未提交文件口径', async () => {
    setRepoProviderForTest(async () => fakeRepo({
      log: ['h9', 'h8', 'c1234567890abcdef', 'h7'],
    }));
    assert.strictEqual(await countNewCommits('c1234567890abcdef'), 2);
    assert.strictEqual(await countNewCommits('missing'), 4); // 未命中=全量计数（仅展示）
    assert.strictEqual(previewText(3, [{ relPath: 'a', status: 'M' }]), '基线锚点后 3 个提交 + 未提交文件 1 个');
    assert.strictEqual(previewText(-1, [{ relPath: 'a', status: 'M' }]), '未提交文件 1 个');
    assert.strictEqual(previewText(-1, []), ''); // 非 git 无此行不塌陷
  });

  it('F20.2 树视图继承角标', () => {
    const f = (inherited?: string): UnifiedFinding => ({
      finding_id: 'x', task_id: 't', project_id: 'p', source_tool: 'bandit', source_rule_id: 'r',
      cwe_id: '', title: '', description: '', severity: 'SEVERITY_HIGH', confidence: 1,
      ai_verdict: '', ai_confidence: 0, ai_reasoning: '', ai_fix_suggestion: '', diff_patch: '',
      location: null, dedup_group: '', is_unique: true, inherited_from_task_id: inherited,
    });
    assert.ok(findingDescription(f('base-1')).endsWith('· 继承'));
    assert.ok(!findingDescription(f()).includes('继承'));
    assert.ok(!findingDescription(f('')).includes('继承'));
  });
});
