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
| 21 | 扫描互斥竞态双上传 | `doScan` 的 `scanning=true` 在 `listTools()` 网络往返之后才置位，并发第二次调用从 await 窗口穿过守卫 | 连击/深链+手点并发触发扫描 → 双 zip 上传、平台双沙箱消耗；互斥标志形同虚设 | `extension.test.ts › 扫描互斥竞态：第一次卡在连通性探测…仅一次上传（B2-1）` + `› 扫描早退复位互斥…（B2-1）`（立即置位+全部早退路径 finally 复位） | B2 审计批次 |
| 22 | 旧任务终态收尾覆盖新任务 UI | terminal 收尾仅在 await 前查一次 lastTaskId；`listFindings` 挂起窗口内切绑任务后，旧收尾恢复执行照样 renderFindings/clearTaskUi | 扫描完成瞬间切换任务 → 新任务发现/UI 被旧任务收尾清掉、互斥被误释 | `extension.test.ts › 旧任务终态收尾 TOCTOU…（B2-2）`（每次 await 后复查 `taskId===lastTaskId && progress?.taskId===taskId`） | B2 审计批次 |
| 23 | refresh 瞬态失败清凭据 | `doRefresh` 对任何非 2xx 一律 `tokens.clear()`，502/429 也把会话判死 | 网关抖动/限流一次 → 插件被登出，状态栏"未登录"，离线态缓存 token 全部失效 | `apiClient.test.ts › refresh 失败仅 401 清凭据（B2-3）` 3 例（502/429 凭据保留、401 才清） | B2 审计批次 |

## 四、如何新增一条档案

```markdown
| <下一个编号> | <缺陷类别一句话> | <根因一句话> | <用户可见症状> |
| <守卫位置：测试文件 › 用例名 / 打包关卡 / 守卫测试> | <修复 commit 或设计依据> |
```

判据：只有当"这类错误再次发生时，门禁必然变红"才有资格写进本表——
即守卫位置必须是一条会真实执行的断言（单测/守卫/打包关卡），不能是文档或评审约定。

## #24 R58（2026-09-11）：增量完成口径通知不可达（B2-2 次生缺陷）

- **症状**：增量扫描 COMPLETED 且元数据快照拉取成功时，「CodeAudit 增量扫描完成：…（变更 N·删除 D·继承 M·新发现 K）」通知永不显示（A20.1 完成口径死代码）；仅快照请求抛错走 catch 才会带零值展示。
- **根因**：B2-2 收尾 TOCTOU 修复引入——`clearTaskUi()` 收尾本任务时置 `progress = null`，紧随其后的二次复查 `progress?.taskId !== taskId` 变恒真早退；复查把"本任务收尾动作的副作用"误判为"用户已切走"。
- **修复**：二次复查只查 `taskId !== lastTaskId`（切走=绑定变更，由 lastTaskId 反映；本任务 progress 置空是收尾预期，不再参与归属判定）。catch 分支守卫不经 clearTaskUi，保持原样。
- **锁定**：`test/doScan-branches.test.ts` 增量终态通知用例（showInformationMessage 含「变更 N · 删除 D · 继承 M · 新发现 K」）。

## #25 B5-1（2026-09-11）：旧 watcher 在途 404/迟到 WS 1011 污染新任务（R58/B2-2 同族漏网）

- **症状**：任务 A 跟踪中（轮询/WS 在途）切走——`watchTask` 换新任务或 `bindTask` 切绑都会 `close()` 旧 watcher——但 A 的在途快照请求随后以 404 "not found" 返回（旧任务被平台删除），或服务端已发出的 1011 "task not found" close 帧在 close() 后仍送达：新任务 progress 被标 `TASK_STATUS_DEAD`、`scanning` 互斥被误释（运行中的新扫描解锁 → 连击双沙箱消耗）、`codeaudit.taskRunning` 上下文被误清，并误弹「已在平台删除或归档」警告。
- **根因**：B2-2 只给 terminal 收尾补了归属复查，`onTaskGone` 回调无守卫；且 `taskWatcher.pollOnce` 在 `await taskSnapshot` 返回后不复查 `this.closed`——已关闭的旧 watcher 仍会走 404→onTaskGone 路径；WS onclose 的 "not found" 分支同样不查 closed（close() 不撤销已在途的服务端关闭帧）。
- **修复**：双层守卫——① extension.ts `onTaskGone` 回调头部 `if (taskId !== lastTaskId || (progress && progress.taskId !== taskId)) return;`；② taskWatcher.ts `pollOnce` 在 await 返回后复查 `this.closed`（快照不进 settle、404 不触发 onTaskGone）。
- **锁定**：`test/extension.test.ts › 旧 watcher 在途 404/迟到的 WS 1011：新任务 progress 不被标 DEAD、互斥不被误释（回归锁 B5-1）`（FetchScript/wsInstances 桩：A 挂起轮询→doScan 换 C→404 释放+手动 1011 close 帧→断言 taskRunning/10% 状态栏/互斥拦截/无"已删除"警告）+ `test/taskWatcher.test.ts › 轮询在途 404 返回前 watcher 已被 close…（回归锁 B5-1）`（单测粒度：closed 后的 404 拒绝不触发 onTaskGone）。

## #26（2026-09-12）行号校准未贯通反查/跳转：修复行漂移后灯泡消失、树上点击跳错行

- **症状**：应用修复使同文件其他发现的行号漂移后（applyTrackedShifts 已迁移诊断行号），①树上/详情页「打开位置」仍跳扫描原始行（错行）；②编辑器灯泡反查（pickFindingAtLine）用原始行号与校准后的诊断行号错位——精确与区间匹配双双失配，QuickFix 静默消失。多风险同文件顺序修复（README 宣示的核心工作流）高频可感知。
- **根因**：trackedLines 校准表只进诊断渲染（renderFindings→mapFinding）与回滚 QuickPick（rollbackPickItems），未贯通 doOpenFinding 跳转行与 pickFindingAtLine 反查——三处行号坐标系不一致。
- **修复**：doOpenFinding 跳转行取 `trackedLines.get(finding_id) ?? start_line`；pickFindingAtLine 增可选 trackedLines 参数（起点优先校准值、区间跨度保持原始相对跨度平移，镜像 mapFinding 的 end 计算）；CodeActionProvider 传 trackedLinesSnapshot()。
- **锁定**：`treeModel.test.ts › 校准行号（trackedLines）：行漂移后按校准行命中，原始行失配不误配` + `extension.test.ts › 行号校准贯通：修复行漂移后灯泡按校准行命中、打开位置跳校准行`。

## #27（2026-09-12）bindTask 404 无条件清 lastTaskId：旧任务终态收尾被吞、scanning 互斥死锁

- **症状**：切换绑定到刚被平台删除的任务（listTasks 与 taskSnapshot 之间的删除窗口，snapshot 404）后，原任务即使仍健康：其 watcher 终态事件的归属守卫（taskId!==lastTaskId）因 lastTaskId 被清空而恒拦——完成通知/结果拉取永不发生、scanning 永不释放（重启窗口才能再扫描）；持久化指针被清还会让重载丢掉旧任务绑定。
- **根因**：404 分支只考虑了恢复场景（恢复的就是死任务，清指针防重载再 404），没区分「切换目标 404」——此刻旧 watcher 尚未 close（close 在快照成功之后），其收尾依赖 lastTaskId 匹配。
- **修复**：404 两径分治——taskId===prev.lastTaskId（恢复场景，无旧任务在跟）→ 清指针（原行为）；否则保留旧绑定态原样（内存与持久化指针都不动）+ 警告「未切换绑定」。
- **锁定**：`extension.test.ts › 切换绑定到已删除任务（snapshot 404）：旧任务绑定保持，旧任务终态收尾不被吞`（404→警告+指针保持→A 的 WS COMPLETED 帧正常收尾）。

## #28（2026-09-12）写盘互斥不完整（B2-6 同族遗留）：回滚与围栏兜底修复绕过 fixing

- **症状**：B2-6 给 applyMachinePatch 加了 fixing 互斥（理由：多步 await 改盘并发会互相踩 checkpoint/登记），但 ①rollbackRecord（外科/整文件覆盖回滚）②doFixFinding 围栏 diff 兜底路径改盘段 ③doRollback 无登记 restoreLatest 兜底——三处同类改盘完全绕过互斥，可与进行中的修复交叠改盘。
- **修复**：fixing 语义扩展为工作区写盘互斥，统一覆盖三路（外壳模式：进入检查+置位、finally 复位、进行中拒绝且不改盘不建 checkpoint）。
- **锁定**：`extension.test.ts › 写盘互斥扩展：修复卡在 fs 落盘时，回滚（rollbackFix/rollbackFixes）与围栏兜底修复均被拒、不改盘` + `› 写盘互斥扩展：无登记兜底回滚（rollbackFixes→restoreLatest）同样被拒`。

## #29（2026-09-12）taskWatcher 双通道收束竞态与 settle 无守卫（B5-1 同族）：terminal/onTaskGone 可双触发

- **症状**：①close() 后仍在途/已缓冲的 WS 终态帧仍会走 settle→二次 emit terminal（上层收尾重跑=完成通知弹两次、findings 重复拉取）；②任务删除时轮询 404 与迟到 WS 1011 close 帧竞态——404 路径只置 closed 不关 socket，1011 分支不复查 closed，onTaskGone 双触发（两次「已删除/归档」警告）。
- **根因**：B5-1 只修跨任务方向（归属守卫），未防同任务双通道/迟到帧；收束路径（404/1011）不走 close() 统一清理。
- **修复**：settle 入口复查 closed；轮询 404 与 WS 1011 收束统一先 `close()`（关 socket+清定时器）再回调 onTaskGone，1011 分支先复查 closed（先到者收束、后到者拦截）。
- **锁定**：`taskWatcher.test.ts › WS 在途终态帧在 close 后到达：settle 复查 closed，不二次 emit terminal/snapshot` + `› 轮询 404 与迟到 WS 1011 双通道竞态：onTaskGone 恰一次，404 收束时 socket 被 close`。

## #30（2026-09-12）429 响应体双读丢失：json() 消费后 text() 必失败被吞成空串

- **症状**：429 错误通知只有状态码无响应体（限流原因/服务端提示丢失），排查受限。
- **根因**：requestJson 的 429 分支先 `resp.json()` 读 body，随后 `!resp.ok` 分支再 `resp.text()`——body 已消费必抛 TypeError，被 catch 吞成 ''。
- **修复**：429 分支 text 单次读取后就地解析 retry_after 并直接 throw ApiError（body 全文随错误透出）。
- **锁定**：`apiClient.test.ts › 429 响应体单次读取：ApiError 携带完整 body，retry_after 正常解析`。

## #31（2026-09-12）fixRegistry.persist 非原子写：写一半崩溃丢全部登记

- **症状**：登记文件写一半崩溃（磁盘满/进程被杀）→ 坏 JSON → 构造时静默视为无记录——全部「已修复」徽章与按发现回滚入口一次丢失。
- **根因**：persist 直写目标文件，无 tmp+rename 原子替换。
- **修复**：tmp 写入后 renameSync 原子替换；tmp 失败旧文件原样。
- **锁定**：`fixRegistry.test.ts › persist 原子写：tmp 写入失败 → 旧登记文件保持完整；成功后无 .tmp 残留`。

## #32（2026-09-12）增量完成通知元数据失败仍展示「变更 0 · 删除 0」：误导为确认无变更

- **症状**：增量任务 COMPLETED 但收尾的元数据快照拉取失败（500/网络抖动）时，完成通知照常展示「变更 0 · 删除 0」——用户会读成"平台确认无变更"。
- **根因**：catch 分支只 log.warn，changedN/deletedN 保持 0 值继续进入原通知文案，未区分"真 0"与"不可得"。
- **修复**：metaUnavailable 标志——失败时通知只展示可本地推算的继承/新发现数，并如实标注「增量统计（变更/删除数）拉取失败，未展示」。
- **锁定**：`doScan-branches.test.ts › 增量元数据拉取失败：完成通知如实标注统计不可得，不展示误导性「变更 0」`。

## #33（2026-09-12）checkpoint 无清理策略：globalStorage 无界增长

- **症状**：每次修复留一份文件快照且口径为「保留不删」，长期使用磁盘占用无上限。
- **决策**（人类指令 2026-09-12）：同一绝对路径在全部 checkpoint 中的条目上限 100。
- **修复**：save 后按文件增量清理（pruneFile）——超限从最旧 cp 移除该文件条目（删内容文件+manifest 去键；manifest 清空则整目录删除）。清理不感知修复登记表：被清理的旧登记按发现回滚走既有「checkpoint 缺失或损坏（文件可能被清理）」诚实降级（100 份窗口足够深）。
- **锁定**：`checkpoint.test.ts › 每文件快照上限 100：第 101 份起最旧 checkpoint 中该文件条目被移除，同 cp 其他文件保留` + `› 单文件 checkpoint 条目清空后整目录删除`（mutation：去清理调用/上限改 1000 均变红）。

## #34（2026-09-12）doLogin 空网关地址未拦截：baseUrl 置空后报难懂的相对路径错误

- **症状**：登录时网关地址输入空串/纯空白确认后，空值写入全局配置、baseUrl=''，后续请求走相对路径 fetch 报与平台无关的难懂错误。
- **修复**：空/纯空白地址在发起任何请求前拒绝——警告提示且不写配置。
- **锁定**：`extension.test.ts › 登录空网关地址被拒：警告、不写配置、零登录请求`（mutation：删守卫变红）。

## #35（2026-09-12）绑定项目后不自动同步平台已有风险：空面板误导"项目无风险"

- **症状**：绑定平台项目后扫描结果面板为空，直到用户手动扫描或执行「刷新扫描结果」——平台上已有已完成任务的发现（团队协作/此前会话扫过）不可见，空面板会被读成"无风险"。
- **决策**（人类指令 2026-09-12）：加载/绑定平台项目时自动同步已发现的风险。
- **修复**：doSelectProject 绑定成功且空闲（无 scanning、无活跃非终态任务——避免撞 bindTask 切换确认门 B2-5）时，自动 latestCompletedTask(projectId)→bindTask（带轻通知「已绑定任务…N 条发现」）；项目无完成任务/拉取失败静默仅日志（不误导"无风险"、不打扰绑定流程）。启动路径既有口径不变（restoreLastTask：lastTaskId 优先，否则登录+绑定项目兜底最近完成任务）。
- **锁定**：`extension.test.ts › 绑定项目后自动同步：拉取该项目最近完成任务并渲染发现` + `› 项目无完成任务 → 静默零打扰`（mutation：删同步块两用例同红）。

## #36（2026-09-12）测试桩 joinPath 硬编码 win32：POSIX 宿主 16+1 用例全挂（双环境缺陷）

- **症状**：f6b67ba 修复桩 Uri.joinPath（file scheme 以 fsPath 为基准拼接）时硬编码 `path.win32.join`——Windows 宿主（fsPath 为 `C:\…` 形态）正确，但 POSIX 宿主上 `path.win32.join('/tmp/x','a.py')` 产出 `\tmp\x\a.py`，fs.readFileSync 恒 ENOENT → applyMachinePatch 的 openTextDocument 预读全部 Missing File 拒绝，机器补丁/批量修复/回滚/互斥/灯泡/webview 16 用例 until 超时挂。
- **根因**：桩读的是**测试进程所在宿主的真实 fs**，fsPath 必须跟随宿主平台形态（真实 VS Code 语义本就是每平台产出该平台 fsPath）——拼接应委托宿主 `path.join`（Windows 上即 win32、POSIX 上即 posix），不可硬编码任一半。
- **修复**：`path.win32.join` → `path.join`（parts 前导 `[/\\]` 双分隔符剥离）；双平台回归锁断言「产物 fsPath === 宿主 path.join 产物、可被本机 fs 直接读取、POSIX 宿主无反斜杠 / Windows 宿主必有反斜杠」。
- **锁定**：`extension.test.ts › 桩 Uri.joinPath file 产物跟随宿主平台形态且本机 fs 可读（双环境回归锁）`（mutation：退回 win32.join 在 Linux 实测 17 failing 含本用例；还原 263 passing）。
