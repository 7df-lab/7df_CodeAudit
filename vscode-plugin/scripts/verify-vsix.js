// 打包关卡：VSIX 必须包含 node_modules/adm-zip（workspaceZip 顶层 require，缺它激活即崩、全部命令注册不上）。
// 背景：3cf397c 修复过 .vscodeignore 放行；后曾因打包带 --no-dependencies 跳过依赖收集再次缺席，
// 装机后表现为 "command 'codeaudit.login' not found"。此校验让该类回归在打包时即失败。
const { execSync } = require('child_process');
const path = require('path');

const vsix = path.join(__dirname, '..', 'codeaudit-vscode-0.1.0.vsix');
let listing = '';
try {
  listing = execSync(`unzip -l "${vsix}"`, { encoding: 'utf8' });
} catch {
  // Windows 无 unzip 时的兜底：PowerShell 读取 zip 条目
  listing = execSync(
    `powershell -NoProfile -Command "Add-Type -AssemblyName System.IO.Compression.FileSystem; [IO.Compression.ZipFile]::OpenRead('${vsix.replace(/'/g, "''")}').Entries.FullName -join [char]10"`,
    { encoding: 'utf8' },
  );
}
if (/node_modules\/adm-zip\/package\.json/i.test(listing)) {
  console.log('verify-vsix: adm-zip 已打包 ✓');
} else {
  console.error('verify-vsix: VSIX 缺少 node_modules/adm-zip —— 激活会失败！检查是否误用 --no-dependencies 或 .vscodeignore 放行规则');
  process.exit(1);
}
