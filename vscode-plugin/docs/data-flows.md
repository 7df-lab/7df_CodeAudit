# 其他数据流转与代码交互

> 外部接口（[external-interfaces.md](external-interfaces.md)）与模块间函数契约
> （[internal-interfaces.md](internal-interfaces.md)）之外的数据流：持久化资产、状态机、
> 事件/定时器拓扑、端到端走查、宿主进程交互与离线工具链。每节标注锁定测试。

---

## 1. 持久化资产清单（重启后仍在的状态）

| 资产 | 位置 | 写入方/时机 | 格式与语义 | 锁定测试 |
|---|---|---|---|---|
| access token | **SecretStorage** `codeaudit.access`；进程内另有 `refreshCache` | login / refresh 成功 | 明文串；SecretStorage 由宿主加密保管。注意：`setTokens` 只在 refresh 非空时持久化 refresh | 手工验收；TokenStore 契约见 apiClient.test.ts |
| refresh token | SecretStorage `codeaudit.refresh` | login / refresh / boot 读取 | `boot()` 在 activate 后异步执行，完成后重算 `loggedIn` 上下文并触发任务恢复 | extension.test.ts › 恢复链路（依赖 boot 时序） |
| 上次任务 ID | **workspaceState** `codeaudit.lastTaskId` | doScan 建任务 / bindTask 绑定 | 字符串；平台删除该任务后被清空（写 `undefined`） | extension.test.ts › 恢复链路 404 分支 |
| 项目绑定 | 工作区 `.vscode/settings.json` `codeaudit.projectId` | selectProject 命令 | 由 VS Code 配置系统托管（Workspace 目标） | guards.test.ts › 配置键守卫 |
| 修复快照 | **globalStorage** `checkpoints/cp-<ts>-<seq>/` | 每次补丁落盘前 | `manifest.json`：`{绝对路径: 内容文件名|null}`；null=修复前不存在（回滚=删除）；内容文件名为路径消毒串；**checkpoint 保留不删**（支持回滚后再次应用） | checkpoint.test.ts 8 例 |
| 修复登记 | globalStorage `fix-registry.json` | recordApplied / markRolledback | `FixRecord[]`（findingId 唯一键，applied⇄rolledback 状态机）；损坏视为无记录 | fixRegistry.test.ts 5 例 |

## 2. 状态机

### 2.1 任务态 × UI 派生（单一真相源 `progress: ProgressState | null`）

```
无任务 ──scan──▶ 阶段文案(phase) ──startTask──▶ 跟踪中 ──终态──▶ 收尾
  ▲                                                    │
  └────────────── clearTaskUi（仅 COMPLETED 成功路径）◄──┘
失败/取消/死亡路径：progress 保留终态供检视，不回欢迎态
```

- `phase`（打包中…/上传中…/创建任务…/拉取结果…/刷新结果…）只在任务建立前/结果拉取期生效，`startTask` 成功后由 percent 接管。
- 状态栏主图标：未登录→「未登录」（点击登录）；**非终态**任务→`{pct}%`（点击开 AI 视图；终态历史任务的 progress 不占用百分比位）；有 phase→phase 文案；否则 `{N} 发现`/「空闲」。
- 状态栏扫描快捷按钮（固定槽位随任务态切换）：空闲→▶扫描；运行中→⏸暂停；暂停中→▶恢复。
  锁定测试：extension.test.ts › 状态栏随任务态切换。
- 上下文键派生：`taskRunning` = watchTask 置位、终态/onTaskGone 清位；`taskPaused` 跟随快照 status；`hasTask` = 有任务记录。
- `cancelRequested`：用户主动取消置位，终态分支据此把非 COMPLETED 统一归因为「已取消」。

### 2.2 扫描互斥

`scanning` 布尔：doScan 入口检查（进行中→警告拒绝）→ 终态（`terminal` 事件）或 doScan 异常时释放。
onTaskGone 也释放。锁定测试：extension.test.ts › 扫描互斥。

### 2.3 修复登记状态机

```
（无记录）--applyMachinePatch--> applied --rollback--> rolledback --再应用--> applied(新checkpoint覆盖)
```

- applied 集合 → 树徽章「✔ 已修复（可回滚）」+ contextValue=findingFixed（内联按钮切换）+ 详情页按钮切换。
- knownFindingIds（applied+rolledback）→ 低风险批量候选排除（已应用不重复处理；回滚过不翻案）。
- 锁定测试：fixRegistry.test.ts、lowRiskApply.test.ts、extension.test.ts › 低风险批量。

### 2.4 AI 视图渲染游标 `lastPaint: {taskId, version} | null`

- null 或 taskId 变化 → 整页重绘（`renderAiContextHtml`）；
- 同任务 → 仅 `progress.version > lastPaint.version` 时 postMessage 增量；
- 整页渲染后由 onRendered 回调同步 lastPaint（防冗余推送）。
- 视图不可见时完全跳过（`view.visible`）。
- 锁定测试：extension.test.ts › AI 上下文视图增量推送。

## 3. 事件与定时器拓扑

```
WS 帧/轮询快照 ──settle──▶ emit('snapshot') ─▶ extension 快照回调
                             │                    ├─ applyFrame(progress, snap)
                             │                    ├─ setCtx(taskPaused)
                             │                    ├─ 沙箱收包校验（一次性，见 §4.5）
                             │                    └─ refreshProgressUi ─┬─ 进度树 setItems
                             │                                          ├─ 状态栏
                             │                                          └─ AI 视图增量/整页（§2.4）
                             └─ 终态 ─▶ emit('terminal') ─▶ extension 终态回调（§4.1）+ watcher.close()
定时器：WS 断线 5s 重连；轮询 10s/轮；429 限流窗口内跳轮 —— 全部经 TaskWatcher.timers 注册，close 一锅清
```

- 定时器/Socket 全部可注入（`setTimeoutFn/clearTimeoutFn/makeSocket/now`）——单测无真实时钟。
- 锁定测试：taskWatcher.test.ts（6 例）。

## 4. 端到端数据流走查

### 4.1 扫描链路（doScan → watchTask → 终态）

```
findFiles(20000上限, excludeGlobs) → minPackFiles 阈值检查 → adm-zip 打包（读盘失败跳过）
→ POST /v1/uploads/archive → config.upload_file_id‖project_path 二选一（都缺→中止）
→ POST /v1/tasks → POST /v1/tasks/{id}/start → watchTask(taskId, zip字节数)
→ WS/轮询双通道 → 终态 COMPLETED → listFindings 翻页累积 → renderFindings（诊断+树）
→ clearTaskUi（回欢迎态）→ 完成通知；其他终态 → 对应警告/错误，progress 保留检视
```
锁定测试：extension.test.ts › doScan 系列（upload 契约/互斥/阈值/终态收尾）。

### 4.2 修复链路（两路径同归 applyMachinePatch 核心）

```
fixFinding(finding)
 ├─ diff_patch 非空（主路径）→ applyMachinePatch：
 │   listUpdatedFiles → 逐路径 resolveWsPath 禁闭 → openTextDocument 预载（缓冲区=真相）
 │   → computePatchChanges（语法/缺文件/失配→DiffError 整体拒绝）
 │   → 变更路径禁闭 + Add/Move 目标存在性 → checkpoint → 应用 → 登记 → diff 审阅 + 通知
 │   （fuzz≥1000 → 升级为警告「补丁未精确锚定」）
 └─ diff_patch 空（兜底）：无 ai_fix_suggestion → 警告「暂无修复建议」；
     有建议无 ```diff 围栏 → 警告「无法自动修复」（诚实降级，不伪造补丁）；
     有围栏 → parseUnifiedDiff → applyPatchToLines（失配整体拒绝）→ checkpoint →
     WorkspaceEdit 全文替换 + 显式 save → 登记 → diff 审阅
```
锁定测试：extension.test.ts › applyMachinePatch 系列 + 兜底/降级系列。

### 4.3 回滚链路

```
rollbackFix(发现) / rollbackFixes(最近 applied 记录‖最近 checkpoint)
→ 纯 Update 修复优先外科回滚：invertFilePatch 逆补丁内容锚定（fuzz>1/文件漂移→降级），
  只撤销本修复变更，同文件更晚修复保留，行号按逆补丁锚点增量迁移
→ 降级/含 Add/Delete/Move 的修复走 checkpoint 路径：
  同文件更晚修复覆盖警告（可选确认）→ checkpoints.restore(id)
  （缺失/损坏→错误+登记不变）→ writeRestored（内容还原+显式保存+磁盘终验；
  null 条目=删除文件；Delete/Move 源已被补丁移除的条目=fs 直写重建）
→ markRolledback（整文件覆盖时被一并覆盖的更晚修复同步解除）→ renderFindings → 详情刷新 → 可重新应用
```
锁定测试：extension.test.ts › 回滚系列（含 applyEdit 拒绝、save 失败、逐字节还原）。

### 4.4 历史恢复链路（窗口重载/重启）

```
tokens.boot() 完成 → restoreLastTask：
  workspaceState.lastTaskId 存在 → bindTask(silent)
  否则 已登录+已绑定项目 → latestCompletedTask（listTasks 首个 COMPLETED）→ bindTask(silent)
bindTask：snapshot→进度重建→非终态则 watchTask(resumeState=true) 续订→findings→renderFindings
  404 not found → 清 lastTaskId（不恢复死任务）
```
锁定测试：extension.test.ts › 恢复链路（COMPLETED 重建 / 非终态续订 / 404 清指针）。

### 4.5 沙箱收包校验支线（防空包白审）

```
watchTask 携带 expectedUploadBytes（zip 字节数）→ 每次快照后（一次性，uploadSizeChecked 置位）
→ sandboxPackCheck(logs, expected)：最近「打包完成 …（N 字节）」行
→ N < max(64B, 上传量/100) → cancelTask + 错误通知（归因：平台拉取/解包链路）
```
锁定测试：progressModel.test.ts › sandboxPackCheck 4 例（纯函数）+ extension.test.ts › 沙箱收包校验接线（取消动作）。

### 4.6 任务消亡支线

WS 1011 "not found" 或快照 404 not found → onTaskGone → 本地落 `TASK_STATUS_DEAD` + 上下文清位 + 互斥释放 + 警告。
锁定测试：taskWatcher.test.ts 2 例 + extension.test.ts › onTaskGone 落终态。

## 5. 宿主进程交互（扩展 → VS Code）

| 交互 | 数据流 | 锁定测试 |
|---|---|---|
| diff 审阅视图 | `vscode.diff(beforeUri, rightUri, title)`；beforeUri=`codeaudit-fix` scheme + base64 query 内容；右侧=当前文件/Move 目标/空文档（Delete） | extension.test.ts › diff 审阅数据 |
| setContext | 6 个上下文键驱动 when 菜单/欢迎页/视图互斥 | guards.test.ts › 上下文键守卫 |
| openExternal | 控制台 URL 推导（consoleUrl‖serverUrl:4173）+ `/tasks/{lastTaskId}` | extension.test.ts › 打开控制台 |
| URI handler | 深链首段 → executeCommand 分发 | extension.test.ts › URI 深度链接分发 |
| CodeAction | 诊断相交 + 文件路径匹配 → fixFinding QuickFix | extension.test.ts › 编辑器灯泡 |
| 输出通道 | 「CodeAudit」日志：登录/打包清单/任务启动/WS 原始事件（open/close/error）/终态统计/低风险跳过原因 | 诊断事实源（WS 静默回退轮询时可区分「没推」与「连不上」） |

## 6. 离线工具链数据流（开发/验收）

| 流 | 链路 | 说明 |
|---|---|---|
| 单测 | `tsc -p tsconfig.test.json` → `out-test/` → `node test/run.js` | run.js 把 `vscode` 模块重定向到 `test/mocks/vscode.js` 内存桩；全离线 |
| 打包 | `vsce package` → `verify-vsix.js` | 关卡：VSIX 必含 node_modules/adm-zip（缺失 exit 1） |
| 冒烟 | `test/smoke.js` | 真实网关全链（登录→上传→建任务→轮询→发现） |
| E2E | `test/fixflow.e2e.js` | 真实平台多风险同文件顺序修复→逐字节回滚→再应用 |
| Mock 网关 | `test/mockGateway.js` | 无平台时驱动修复 UI 全流程（真实 diff_patch 回放，任务 1.5s 自动 COMPLETED） |

## 7. 防回归机制索引

见 [regressions.md](regressions.md)：缺陷档案（bug 类别 → 守卫位置）+ 三条纪律
（修 bug 必附回归锁测试；改契约先改文档再改测试后改实现；门禁 = npm test 全绿 + package 关卡）。
