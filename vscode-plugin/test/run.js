// mocha 启动器：把 'vscode' 模块解析重定向到桩实现（测试进程内没有 VS Code）
const Module = require('module');
const path = require('path');
const origResolve = Module._resolveFilename;
Module._resolveFilename = function (request, ...args) {
  if (request === 'vscode') {
    return path.join(__dirname, 'mocks', 'vscode.js');
  }
  return origResolve.call(this, request, ...args);
};

const Mocha = require('mocha');
const { globSync } = require('glob');
const mocha = new Mocha({ timeout: 10000 });
for (const f of globSync('out-test/**/test/*.test.js', { cwd: __dirname + '/..' })) {
  mocha.addFile(path.join(__dirname, '..', f));
}
mocha.run((failures) => process.exit(failures > 0 ? 1 : 0));
