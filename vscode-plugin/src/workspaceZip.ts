// 工作区打包：vscode.workspace.findFiles（尊重默认 excludes + 用户配置的排除 glob）→ AdmZip Blob。
// 上传通道复用平台现状 /v1/uploads/archive 整包直传（方案 A，设计批准的一期口径）。
import AdmZip = require('adm-zip');

export interface FileInput {
  relPath: string; // 仓库相对路径，zip 内保持该结构
  absPath: string;
}

export async function zipFiles(
  files: FileInput[],
  readFile: (absPath: string) => Promise<Buffer>,
  onSkip?: (relPath: string, err: unknown) => void,
): Promise<Blob> {
  const zip = new AdmZip();
  for (const f of files) {
    try {
      zip.addFile(f.relPath, await readFile(f.absPath));
    } catch (e) {
      // 单文件读取失败不阻塞整包（权限/符号链接断裂），但必须留痕：
      // 被跳过的文件不会进入本次审计，属覆盖缺口，调用方应告知用户
      onSkip?.(f.relPath, e);
    }
  }
  return new Blob([zip.toBuffer()]);
}
