# SAST Samples

These sample files are used for adapter testing with real SAST tools.

- `python/app.py` - SQL injection vulnerability (bandit target)
- `javascript/app.js` - eval with user input (eslint target)
- `java/App.java` - SQL string concatenation (spotbugs target)

Run bandit: `bandit -f json tests/samples/python/app.py`
Run eslint: `eslint tests/samples/javascript/app.js`
Run spotbugs: `spotbugs tests/samples/java/App.java`

## 大型语料（不入库，ADR-178）

`package/`（解压树，132MB）与 `package.zip`（40MB）是一套 OpenWrt/MTK 固件 SDK 真实源码树
（alexa avs-device-sdk、ser2net、luci、googletest 等），2026-08-27 由 TP11-T1「模式D大型语料
贯通」一次性入库，仅用于该次 E2E 验证（bandit 扫描整树）。**无任何已提交测试/脚本/CI 依赖
它们**，日常单测/契约/冒烟/e2e 只用上方小样本（python / python_flask / javascript / java）。

按 ADR-178（2026-09-01 人类决策）该语料移出 git 跟踪、仅本机留存；git 历史未重写，需要复现
模式D大型语料验证时从历史恢复：

    git restore --source=4821dea --worktree -- tests/samples/package tests/samples/package.zip

