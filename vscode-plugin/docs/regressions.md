# 缺陷档案与防回归机制（regressions）

> 本仓库防回归机制 = **三份接口契约文档 + 锁定测试 + 守卫测试 + 本档案**。
> 目标：每类已发生过的 bug 都有机器守卫；同类错误不允许第二次溜进门禁。

## 一、机制构成

1. **契约文档**（docs/external-interfaces.md / internal-interfaces.md / data-flows.md）：
   每条接口契约标注「锁定测试」——契约的机器可验证形态。
2. **锁定测试**（test/*.test.ts）：每个历史 bug 修复时同 commit 附带的最小复现测试，
   命名含「回归锁」或描述缺陷语义。修 bug 不带测试 = 交付无效。
3. **守卫测试**（test/guards.test.ts）：结构性约束（命令注册/配置键/视图 ID/上下文键/
   测试模块覆盖），不依赖具体行为，防止"无声漂移"类缺陷。
4. **行为测试**（test/extension.test.ts + test/mocks/vscode.js 内存桩）：胶水层
   extension.ts 的端到端行为（修复/回滚/扫描/恢复/安全禁闭），历史上这里无任何覆盖。
5. **门禁**：`npm test` 全绿（单测+守卫+行为）→ `npm run package`（VSIX 依赖关卡）。
   两关全过才算通过。

## 二、防回归纪律（改代码前读）

1. **修任何 bug**：同一 commit 内 ①最小复现测试（并入对应 test 文件，用例名写清缺陷
   语义）②在本档案表追加一行（类别/根因/守卫位置）③受影响契约文档同步修订。
2. **改任何接口**（REST 端点、DTO 字段、命令/配置/上下文键、模块函数签名/语义）：
   先改契约文档 → 再改锁定测试 → 最后改实现。文档与实现冲突以代码为准，但必须立即回写。
3. **新增 src 模块**：guards.test.ts 的模块覆盖守卫会强制它至少被一个测试文件引用——
   新模块必须带测试，否则 `npm test` 红。
4. **不要绕过守卫**：守卫失败说明结构性约束被破坏（如命令改名没同步 package.json），
   修根因，不是改守卫让它闭嘴。

## 三、缺陷档案

| # | 缺陷类别 | 根因 | 当年症状 | 守卫位置（机器守卫） | 备注/修复 commit |
|---|---|---|---|---|---|
| 1 | VSIX 缺运行时依赖 | `.vscodeignore`/打包参数导致 adm-zip 未入包 | 装机后激活即崩、`command 'codeaudit.login' not found`（历史上发生过两次） | `npm run package` 的 `scripts/verify-vsix.js` 关卡（exit 1） | 3cf397c、c53723e |
| 2 | 命令声明/注册漂移 | package.json 声明的命令在 extension.ts 漏注册或改名不同步 | command not found 弹窗 | `guards.test.ts › 命令注册守卫`（双向全集相等） | 本档案新增 |
| 3 | 配置键死键 | src 读取的配置键在 package.json 无声明（静默失效） | 配置项不生效、无任何报错 | `guards.test.ts › 配置键守卫` | 本档案新增 |
| 4 | 测试体系外的无声模块 | 新增 src 模块没进测试编译/没人写测试 | 模块零覆盖，回归无人发现 | `guards.test.ts › 测试模块覆盖守卫`（src/*.ts ⇄ 测试 import 双向；tsconfig.test.json exclude 只允许 node_modules/out） | 本档案新增 |
| 5 | webview 冻结在首帧 | postMessage 增量架构迁移后漏开 `enableScripts`，页内脚本不执行、增量消息全静默丢弃 | 面板永远停在"轮询回退/暂无日志"，但状态栏/进度树正常 | `aiContextViewProvider.test.ts › resolveWebviewView 必须开 enableScripts（回归锁）` | ca788a3 |
| 6 | 空包上传 / 沙箱空项目白审 | ①findFiles 冷启动瞬态返回不全；②桶内对象路径被误当 project_path 下发，沙箱对不存在路径打包得 32B 空 tar.gz | 上传成功但扫描产出误导性「0 发现」 | `progressModel.test.ts › sandboxPackCheck` 4 例（近空包下限判废）+ `extension.test.ts › minPackFiles 阈值` + `› doScan upload_file_id 契约` + `› 沙箱收包校验接线` | 1780e6e、85dd553 |
| 7 | 补丁相似度级错位 | 跳跃 hunk（@@ 定义行与 delete 行相隔未列入补丁的代码）整段锚定失败后落到相似度匹配，把 import 插进方法体 | 补丁"应用成功"但内容错位 | `applyPatch.test.ts › 跳跃 hunk…`、`› 逐行锚定任一行未命中 → 整体拒绝`（tryLineByLineAnchor） | 709d823 |
| 8 | 回滚内容变 Buffer | readFileSync 不传 encoding 返回 Buffer，WorkspaceEdit.replace 静默失败 | 回滚后文件内容异常/编辑无声失败 | `checkpoint.test.ts › restoreLatest 的快照必须是 utf-8 字符串而非 Buffer（回归锁）` + FileSystemLike 类型强制 encoding 参数 | 更早事故 |
| 9 | 日志增量丢重 | log_id 十进制串跨位数后字典序失效（"9">"10"），WS 重连重发被误判增量 | 任务日志丢条目/重复 | `progressModel.test.ts › logs 按 log_id 数值序增量去重（回归锁）` | ADR-167 期 |
| 10 | 平台任务删除后死循环 | WS 1011/快照 404 后继续 5s 重连、10s 轮询 | 网络请求风暴、UI 永远"运行中" | `taskWatcher.test.ts › WS 关闭原因为 "task not found"…`、`› 轮询快照 404 not found…` + `extension.test.ts › onTaskGone 落终态` | c53723e、af56e9e |
| 11 | 401 刷新风暴/递归 | 并发 401 各自触发刷新；刷新请求自身再被 401 拦截递归 | token 刷新放大、会话被清 | `apiClient.test.ts › 401 触发单飞刷新并重放原请求（并发共享一次）` + refresh 走裸 fetch 路径 | ADR-155 期 |
| 12 | 恢复任务后进度冻结 | bindTask 只拉一次快照不续订，运行中/暂停中任务重启后永不更新 | 重启后 UI 停在旧状态、恢复按钮失效 | `extension.test.ts › 恢复链路 › 非终态任务续订快照流` | f1e54ad |
| 13 | 修复未落盘即丢 | applyEdit 只改内存缓冲区，不显式 save，关窗即失（且与 checkpoint 语义矛盾） | 显示修复成功但磁盘未变 | `extension.test.ts › save 失败路径`（登记不落、磁盘不变、显式报错） | 更早事故 |
| 14 | 部分应用/静默错切 | 任一 hunk 失配仍应用其余，或相似度兜底错位 | 工作区被改出半套补丁 | `applyPatch.test.ts › E. 不可锚定上下文 → DiffError 整体拒绝`、`diffParse.test.ts › 任一 hunk 未命中 → 整体拒绝` + `extension.test.ts › 机器补丁被拒绝…磁盘不变` | 修复引擎设计基线 |
| 15 | 补丁路径逃逸/覆盖 | 补丁引用 `..`/绝对路径写出工作区，或 Add/Move 覆盖既有文件 | 工作区外文件被改写 | `extension.test.ts › 路径禁闭`系列 + `› Add 目标已存在拒绝覆盖`（服务端 NormalizeDiffPatch 之外的插件侧兜底） | 本档案新增 |
| 16 | 换行符整文件改写 | LF 补丁应用后 CRLF 文件被整体改写为 LF（diff 噪声爆炸） | 一行修改变成整文件 diff | `applyPatch.test.ts › CRLF 文件 + LF 补丁 → 输出保留 CRLF` 等 3 例 | 引擎期 |
| 17 | 暂停态语义误导 | 平台 pause 语义是"排空推理缓冲后静止"，暂停瞬间 WS 仍推流，UI 却显示"流式接收中" | 用户误以为暂停失效 | `progressModel.test.ts › AI 入口…暂停态文案`（如实标注 + live 徽标熄灭） | fede15a |
| 18 | Delete/Move 修复永远无法回滚 | `writeRestored` 对已删除文件走 `openTextDocument`（缺失文件必抛错）→ 回滚整体失败 | Delete File / Move to 补丁应用后，按发现回滚报"文件读取异常"，登记停在 applied | `extension.test.ts › Delete+Add 多段补丁…回滚`、`› Move to…回滚`（内存桩行为测试） | 2026-09-07 测试体系补齐时发现并修复：缺失文件改 fs 直写重建 |
| 19 | checkpoint latest() 取错快照 | `cp-<ts>-<seq>` 按字典序排序，seq 跨位数（9→10）时 `cp-…-10` 排在 `cp-…-9` 之前 | 同毫秒连存 ≥10 个 checkpoint 时（低风险批量连修场景）`回滚最近一次`还原到错误版本 | `checkpoint.test.ts › 多个 checkpoint 时 latest 取最新`（测试负载加大后自然暴露） | 2026-09-07 修复：按 (ts, seq) 数值序排序 |
| 20 | 终态历史任务状态栏滞留 | bindTask 绑定已完成历史任务后 `progress` 残留非 null，状态栏走百分比分支显示 `0%` 而非「N 发现」 | 切换/恢复历史任务后状态栏永远显示 0%，点击无响应感 | `extension.test.ts › 恢复链路：重启后绑定上次任务…`（statusBars 断言） | 并行会话 56a75af 先修复（状态栏百分比只对非终态任务展示）；本仓行为测试独立收敛到同一断言 |

## 四、如何新增一条档案

```markdown
| <下一个编号> | <缺陷类别一句话> | <根因一句话> | <用户可见症状> |
| <守卫位置：测试文件 › 用例名 / 打包关卡 / 守卫测试> | <修复 commit 或设计依据> |
```

判据：只有当"这类错误再次发生时，门禁必然变红"才有资格写进本表——
即守卫位置必须是一条会真实执行的断言（单测/守卫/打包关卡），不能是文档或评审约定。
