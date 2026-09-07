// workspaceZip 契约测试（纯注入，无 fs 依赖）：docs/internal-interfaces.md §8。
import * as assert from 'assert';
import AdmZip = require('adm-zip');
import { zipFiles } from '../src/workspaceZip';

async function blobBuffer(blob: Blob): Promise<Buffer> {
  return Buffer.from(await blob.arrayBuffer());
}

describe('workspaceZip', () => {
  it('打包文件保持 relPath 结构，zip 内容可回读', async () => {
    const blob = await zipFiles(
      [
        { relPath: 'src/a.py', absPath: '/ws/src/a.py' },
        { relPath: 'README.md', absPath: '/ws/README.md' },
      ],
      async (abs) => Buffer.from(`content of ${abs}`),
    );
    const zip = new AdmZip(await blobBuffer(blob));
    const names = zip.getEntries().map((e) => e.entryName).sort();
    assert.deepStrictEqual(names, ['README.md', 'src/a.py']);
    assert.strictEqual(zip.readAsText('src/a.py'), 'content of /ws/src/a.py');
  });

  it('单文件读取失败被跳过，不阻塞整包（契约：打包不允许被个别坏文件打断）', async () => {
    const blob = await zipFiles(
      [
        { relPath: 'bad.txt', absPath: '/ws/bad.txt' },
        { relPath: 'ok.txt', absPath: '/ws/ok.txt' },
      ],
      async (abs) => {
        if (abs.endsWith('bad.txt')) throw new Error('EACCES: permission denied');
        return Buffer.from('ok');
      },
    );
    const zip = new AdmZip(await blobBuffer(blob));
    const names = zip.getEntries().map((e) => e.entryName);
    assert.deepStrictEqual(names, ['ok.txt']);
  });

  it('全部文件读取失败仍产出合法空 zip Blob（防空包由 minPackFiles 层负责，打包层不抛）', async () => {
    const blob = await zipFiles(
      [{ relPath: 'x.txt', absPath: '/ws/x.txt' }],
      async () => { throw new Error('boom'); },
    );
    const zip = new AdmZip(await blobBuffer(blob));
    assert.strictEqual(zip.getEntries().length, 0);
  });
});
