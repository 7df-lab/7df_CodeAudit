# CodeAudit VS Code 插件（codeaudit-vscode）

CodeAudit 代码审计平台的 VS Code 原生扩展（伞仓子仓 `codeaudit/vscode-plugin`）：在编辑器内一键扫描工作区、实时跟踪任务进度 / 任务日志 / AI 交互上下文、以 Problems + 侧栏树展示漏洞结果，并对带 AI 修复建议的漏洞一键应用补丁——落盘前自动 checkpoint，随时可按发现回滚。

- 版本 `0.1.0`，私有项目（`private` + `UNLICENSED`），发布者 `codeaudit`
- 运行环境：VS Code `^1.94.0`
- 唯一运行时依赖：`adm-zip`（工作区打包用；VSIX 内必须携带，见「开发 → 打包」）
- 平台接入：engine 网关 REST + WebSocket（接口面见下文）

## 功能总览（按代码现状）

| 能力 | 说明 |
|---|---|
| 登录 | 用户名/密码换 JWT；access/refresh 存 VS Code SecretStorage；401 单飞刷新（并发请求共享一次刷新）；429 按 `retry_after` 退避（5~60s 钳位） |
| 项目绑定 | QuickPick 选择平台项目，`project_id` 写入工作区 `.vscode/settings.json` |
| 一键扫描 | `findFiles` 收集（默认 2 万文件上限）→ 防空包阈值检查 → adm-zip 打包 → 上传 → 建任务 → 启动 → 实时跟踪；同一时刻只允许一个扫描（防重复消耗平台沙箱） |
| 任务跟踪 | WS 实时推送 + 断线 5s 重连（带游标续订）+ 10s 快照轮询兜底；支持暂停/恢复/取消 |
| 实时视图 | 底部面板「AI 交互上下文」webview：任务态头 + 任务日志滚动窗 + AI 正文流式区；postMessage 增量更新，不整页重载，贴底才跟随 |
| 结果展示 | 行内波浪线 + Problems 面板（source=CodeAudit）；侧栏树按文件分组、组内严重级降序；点击漏洞跳转代码位置并切换出「漏洞详情」视图 |
| AI 修复 | 主路径消费平台 `diff_patch`（apply_patch 语法，Cline PatchParser 移植，支持多文件/Add/Delete/Move to）；兜底路径从 `ai_fix_suggestion` 提取 ```diff 围栏；一键应用（无确认门）+ 应用后 diff 审阅视图；低风险修复可多选批量应用（手动触发的按钮/命令，绝不自动改盘） |
| 回滚 | 落盘前 checkpoint 到插件全局存储；支持按发现回滚、回滚最近一次批量修复；回滚后可重新应用 |
| 历史恢复 | 窗口重载/重启后自动绑定上次任务，从平台拉历史结果（绝不重扫）；「切换任务」可绑定项目下任意历史任务 |
| 其他入口 | 状态栏（登录态/百分比/发现数 + 扫描/暂停/恢复快捷按钮）、编辑器灯泡 QuickFix、`vscode://codeaudit.codeaudit-vscode/scan|selectTask` 深度链接、「在控制台打开」跳 Web 控制台 |

## 使用流程

1. **登录**：`CodeAudit: 登录平台` — 输入网关地址（默认 `http://localhost:8080`）、用户名、密码（JWT，与 Web 控制台同一套账号）。
2. **绑定项目**：`CodeAudit: 选择平台项目并绑定工作区` — 项目 ID 写入工作区 `.vscode/settings.json` 的 `codeaudit.projectId`。
3. **扫描**：`CodeAudit: 扫描工作区`（侧栏标题栏 ▶ / 状态栏「▶ 扫描」/ 命令面板 / URI 深链均可触发）。打包上传 → 平台建任务并启动 → 状态栏显示百分比，底部面板自动展开 AI 交互上下文（`codeaudit.autoOpenAiContext` 可关）。
4. **跟踪**：「任务进度」树实时展示任务头（状态/百分比/WS 或轮询）→ 各阶段（图标/耗时/错误 tooltip）→ AI 交互上下文入口（字节量/流式状态）→ 失败摘要。运行中可暂停/恢复/取消（取消有模态确认）。
5. **看结果**：完成后自动拉取 findings — Problems 面板 + 侧栏「扫描结果」树；点击条目跳转位置，侧栏「任务进度」切换为「漏洞详情」（严重级徽章/元信息/描述/AI 分析/修复建议/机器补丁 + 操作按钮）。新扫描开始时自动切回「任务进度」。
6. **修复**：对带修复建议的发现点灯泡 / 树上内联 💡 / 详情页「AI 修复」/ 命令面板（无参数时 QuickPick 选目标）。补丁应用后展示修复前后 diff 视图，发现标记「✔ 已修复（可回滚）」。
7. **回滚**：树上已修复条目的内联 ↩ 按钮（按发现回滚），或 `CodeAudit: 回滚最近一次批量修复`。同文件上更晚应用的修复会被波及时有明确警告。

## 界面与命令

**视图布局**：

- 活动栏容器 `codeaudit`：「扫描结果」树、「任务进度」树、「漏洞详情」webview（后两者按 `codeaudit.findingDetail` 上下文互斥切换）
- 底部面板容器 `codeaudit-panel`：「AI 交互上下文」webview（不占编辑器空间）

**命令清单**（19 个，与 `package.json` contributes 一致）：

| 命令 | 标题 | 备注 |
|---|---|---|
| `codeaudit.login` | 登录平台 | |
| `codeaudit.logout` | 退出登录 | |
| `codeaudit.selectProject` | 选择平台项目并绑定工作区 | |
| `codeaudit.scanWorkspace` | 扫描工作区 | 扫描互斥：进行中会被拒绝 |
| `codeaudit.cancelScan` | 取消当前任务 | 模态确认 |
| `codeaudit.pauseScan` | 暂停扫描 | `when: taskRunning` |
| `codeaudit.resumeScan` | 恢复扫描 | `when: taskPaused` |
| `codeaudit.refreshFindings` | 刷新扫描结果 | 本地无任务记录时兜底绑定平台该项目最近完成的任务 |
| `codeaudit.clearFindings` | 清空本地结果 | 只清本地诊断与面板，平台数据不动 |
| `codeaudit.showAiContext` | 查看 AI 交互上下文 | |
| `codeaudit.openFinding` | 打开漏洞位置 | |
| `codeaudit.fixFinding` | AI 修复此漏洞 | |
| `codeaudit.applyLowRiskFixes` | 批量应用低风险修复 | 扫描结果面板标题栏按钮；QuickPick 多选逐条裁决后批量应用 |
| `codeaudit.copyFindingId` | 复制 Finding ID | |
| `codeaudit.copyFilePath` | 复制文件路径 | 树上文件节点与发现条目均可用 |
| `codeaudit.rollbackFixes` | 回滚最近一次批量修复 | 优先走修复登记表，无登记时兜底最近 checkpoint |
| `codeaudit.rollbackFix` | 回滚此漏洞修复 | 已修复条目的内联按钮/右键 |
| `codeaudit.openConsole` | 在控制台打开 | 跳 Web 控制台任务页（非网关 API 地址） |
| `codeaudit.selectTask` | 切换任务 | 列出平台该项目任务（时间倒序），绑定即拉历史结果 |

**状态栏**：左侧盾牌图标 + 短态（`未登录` / 打包上传阶段文案 / 任务百分比 / `N 发现` / `空闲`），点击打开 AI 交互上下文；旁边固定槽位的快捷按钮随任务态切换（空闲 ▶扫描 / 运行中 ⏸暂停 / 暂停中 ▶恢复）。

## 配置项

| 配置 | 默认值 | 说明 |
|---|---|---|
| `codeaudit.serverUrl` | `http://localhost:8080` | 平台网关地址（gateway-service HTTP/WS 入口） |
| `codeaudit.consoleUrl` | 空 | Web 控制台地址；空 = 由 serverUrl 推导同主机 `:4173`（控制台用浏览器 cookie 登录态，无需 token） |
| `codeaudit.projectId` | 空 | 工作区绑定的平台项目（「选择平台项目」命令自动写入） |
| `codeaudit.scanMode` | `SCAN_MODE_PARALLEL` | 五种扫描模式之一：`SCAN_MODE_SAST_ONLY`（A 纯SAST）/ `SCAN_MODE_AI_ONLY`（B 纯AI）/ `SCAN_MODE_PARALLEL`（C SAST+AI 融合，默认）/ `SCAN_MODE_AI_ENHANCED_SAST`（D AI增强SAST）/ `SCAN_MODE_COMPARE`（E SAST+AI 对比） |
| `codeaudit.sastTools` | `[]` | SAST 工具列表；空 = 平台按项目语言自动选择 |
| `codeaudit.excludeGlobs` | `node_modules` `.git` `dist` `build` `.venv` `vendor` `.vscode` | 打包上传时排除的 glob |
| `codeaudit.minPackFiles` | `10` | 打包文件数低于该值时中止上传（防 VS Code 文件枚举瞬态异常产出空包）；`0` 关闭 |
| `codeaudit.autoOpenAiContext` | `true` | 扫描启动时自动展开 AI 交互上下文面板 |

## 扫描链路与平台接口

扫描链路：`findFiles`（尊重 excludeGlobs，上限 20000 文件）→ `minPackFiles` 阈值检查 → adm-zip 打 zip（单文件读取失败不阻塞整包，但逐个记日志并告警——被跳过文件不会进入本次审计）→ `POST /v1/uploads/archive` → 用上传件引用建任务（新平台 `config.upload_file_id`，旧平台回退 `config.project_path = dir`；两者都缺则报错）→ `POST /v1/tasks/{id}/start` → TaskWatcher 跟踪。

REST 客户端（`src/apiClient.ts`）实际消费的接口面：

| 接口 | 用途 |
|---|---|
| `POST /v1/auth/login` / `POST /v1/auth/refresh` / `POST /v1/auth/logout` | JWT 登录/刷新/登出 |
| `GET /v1/projects` | 项目列表（绑定用） |
| `GET /v1/tools` | SAST 工具列表 |
| `GET /v1/tasks?project_id=` | 任务列表（历史绑定/切换） |
| `POST /v1/tasks` | 建任务（`project_id` + `scan_mode` + `sast_tools` + `config`） |
| `POST /v1/tasks/{id}/start` `/cancel` `/pause` `/resume` | 任务生命周期 |
| `GET /v1/tasks/{id}/snapshot?logs_after&ai_cursor` | 快照聚合口（task/progress/logs/ai 四路一响应；轮询与 WS 重连续订共用游标口径） |
| `GET /v1/findings?task_id&pagination` | 发现列表；JSON 风格分页参数（对象值 JSON 编码），page_size=100 自动翻页累积 |
| `WS /v1/tasks/{id}/ws?token&logs_after&ai_cursor` | 实时推送（token 走 query；重连带游标续订） |

DTO 与平台 proto 对齐（`UnifiedFinding`/`ScanTask`/`TaskProgress` 等 snake_case JSON 口径，含 protojson int64-as-string、base64 chunk、ISO Timestamp 的容忍解析）。

## 修复与回滚机制

**双路径补丁**（`doFixFinding`）：

1. **主路径**：`finding.diff_patch`（apply_patch 语法）。`src/applyPatch.ts` 是 Cline PatchParser 的完整移植，支持 `*** Update/Add/Delete File:` 与 `*** Move to:`、多文件段；Update 段 `@@` 定义行 + 上下文块锚定，`*** End of File` 追加语义。
2. **兜底路径**：`diff_patch` 为空（服务端校验拒绝或旧任务）时，从 `ai_fix_suggestion` 提取 ```` ```diff ```` 围栏按 unified diff 解析（`src/diffParse.ts`）。
3. **诚实降级**：两条路径都拿不到机器补丁（建议为自然语言）时明确告知「暂无法自动修复」，绝不伪造补丁。

**锚定与失败语义**（两路径同源 Cline）：内容锚定不信任补丁声明行号，顺序游标 + 四级容错（0=精确 / 1=行尾空白 / 100=两端空白 / 1000=相似度≥0.66）；fuzz>0 在通知中透明标注，fuzz≥1000（相似度级锚定，可能错位）升级为警告。**任一 hunk 锚定失败 → 整体拒绝，绝不部分应用、绝不静默错切**。

**路径安全**（插件侧兜底，服务端 NormalizeDiffPatch 已挡一道）：补丁引用的相对路径经工作区禁闭校验（拒绝绝对路径与 `..` 逃逸）；Add/Move 目标必须不存在（拒绝覆盖既有文件）。

**checkpoint + 修复登记**：

- 应用前把受影响文件内容快照到 `<globalStorage>/checkpoints/cp-<时间戳>-<序号>/`；值为 null 的条目 = 修复前不存在的文件（Add/Move 目标），回滚时删除而非还原。
- `<globalStorage>/fix-registry.json` 记录每条修复（发现 → checkpoint 映射 + applied/rolledback 状态机），窗口重载后「✔ 已修复」徽章与按发现回滚入口仍在。
- 应用语义：Update 走 `WorkspaceEdit` + 显式保存（保留撤销栈）；Add/Delete/Move 走 fs。应用后逐文件打开「修复前虚拟文档 ↔ 当前文件」diff 审阅视图——审阅是事后动作而非门禁，不满意随时回滚。
- 发现应用补丁后**保留**在树和 Problems（应用补丁 ≠ 风险记录消失），仅刷新「✔ 已修复（可回滚）」徽章，内联按钮切换为回滚；回滚后可重新应用（新 checkpoint 覆盖旧记录）。

**低风险批量应用**（`CodeAudit: 批量应用低风险修复`，扫描结果面板标题栏按钮）：插件绝不自动改工作区——由人点按钮触发，QuickPick 多选逐条勾选要应用的候选。候选条件（筛选逻辑在 `src/lowRiskApply.ts` 纯函数，有单测）：LOW/INFO、AI 置信度≥0.9、带机器补丁、登记表未见过（已应用不重复处理，显式回滚过的不翻案）。选中项走与手动修复同一套 `applyMachinePatch` 核心——路径禁闭、整体拒绝、checkpoint、登记全部一致；区别仅在不打开 diff 审阅视图、以一条汇总通知收尾，单条补丁校验失败跳过并记入 CodeAudit 输出，不中断其余。

## 任务跟踪细节

- **双通道**：WS 优先（`ws://…/v1/tasks/{id}/ws?token=…`），断线 5s 重连且带 `logs_after`/`ai_cursor` 游标续订（服务端自此起算不重发）；WS 不可用（如被 VS Code 代理解析器劫持）自动回退 10s 快照轮询，增量游标口径相同。两种通道在进度树/状态栏/webview 徽标中如实标注「WS 实时 / 轮询」。
- **429 退避**：限流窗口内跳过轮询轮次。
- **任务已删除/归档**：WS 1011 "not found" 或快照 404 时终止重连与轮询（防死循环），本地进度落终态「异常终止」并提示。
- **进度归并**（`src/progressModel.ts` 纯函数）：logs 按 log_id 去重追加（容量 500，与服务端环形缓存同容量）；AI 正文按字节游标连续性追加（超 512KB 保尾）；总进度优先取 `progress.overall_percent`，缺失时按阶段完成度估算。内容有演进才 version++ 触发重渲染。
- **沙箱收包校验**（防空包白审）：沙箱日志「项目打包完成 …（N 字节）」低于近空包下限（max(64B, 上传量/100)；空 tar.gz 实测 32B 签名）时，判定空项目进沙箱（平台拉取/解包链路故障特征），立即取消任务并明确归因——绝不放行一轮注定产出误导性「0 发现」的白审。重打包 tar.gz 与上传 zip 字节数不等属正常，故按下限判废而非相等比对。

## 开发

```bash
npm install
npm run compile   # tsc 编译到 out/
npm run watch     # 增量编译
npm test          # tsc 编译测试 + mocha 跑全部单测（当前 139 个全过）
npm run package   # vsce 打包 vsix + verify-vsix 关卡
```

- 测试在纯 Node 进程运行：`test/run.js` 把 `vscode` 模块重定向到 `test/mocks/vscode.js` 桩实现；业务逻辑全部抽成不 import vscode 的纯模块（见下表），因此无需启动扩展宿主即可覆盖。
- 真实链路脚本（需平台网关在线，默认 `http://localhost:8080`）：`node test/smoke.js`（登录→上传→建任务→轮询→拉发现）、`node test/fixflow.e2e.js`（多风险同文件顺序修复→逐字节回滚→再应用）；`node test/mockGateway.js` 可在无平台时驱动修复 UI 全流程。
- **打包关卡**：`scripts/verify-vsix.js` 校验 VSIX 内必须含 `node_modules/adm-zip`（workspaceZip 顶层 require，缺它扩展激活即崩、全部命令注册不上——历史上真实发生过两次）。`.vscodeignore` 已放行 adm-zip，勿用 `--no-dependencies` 打包。
- Node ≥ 20（实测 22）：REST 客户端与测试依赖全局 `fetch`/`FormData`/`Blob`。

**模块结构**（`extension.ts` 为薄胶水层，业务逻辑全部在可单测的纯模块中）：

| 模块 | 职责 |
|---|---|
| `extension.ts` | 激活入口：命令/视图/状态栏/URI handler 注册，各模块装配 |
| `apiClient.ts` | REST 客户端：JWT 单飞刷新、429 退避、JSON 风格分页、findings 翻页累积 |
| `taskWatcher.ts` | WS + 轮询双通道任务跟踪（定时器/Socket 可注入，可单测） |
| `progressModel.ts` | 四路帧归并为单一 ProgressState + 进度树节点数据 |
| `aiContextView.ts` / `aiContextViewProvider.ts` | AI 上下文渲染纯函数 / WebviewView 胶水（postMessage 增量） |
| `findingDetailView.ts` | 漏洞详情 webview 渲染纯函数 |
| `treeModel.ts` / `diagnosticsMapper.ts` | findings → 文件分组树 / → VS Code Diagnostic |
| `workspaceZip.ts` | adm-zip 工作区打包 |
| `applyPatch.ts` | apply_patch 语法 PatchParser（Cline 移植，主路径） |
| `lowRiskApply.ts` | 批量应用低风险修复的候选筛选纯函数（LOW/INFO · AI 置信度≥0.9 · 带机器补丁） |
| `diffParse.ts` | unified diff 解析（```diff 围栏兜底路径）+ 共享锚定判定 |
| `checkpoint.ts` / `fixRegistry.ts` | 回滚快照存储 / 修复登记状态机（均持久化、fs 可注入） |
| `types.ts` | 平台 proto 对齐 DTO |

## 已知边界（按现状）

- **单工作区根**：所有路径解析、诊断映射取 `workspaceFolders[0]`，多根工作区只认第一个。
- **AI 正文为纯文本流**：webview 中以 `<pre>` 渲染（escapeHtml 后进 DOM，无 markdown 渲染）；日志展示最近 200 条。
- **无精确位置的发现**：不进 Problems（无行列可标），只进侧栏树「(无位置)」分组。
- **扫描结果为快照语义**：本地诊断反映扫描时刻的代码；文件后续改动不会自动重校验，「✔ 已修复」徽章仅表示补丁已应用，不代表风险已被平台复验消除。
- **E2E/冒烟脚本依赖真实平台**：单元测试（139 个）无外部依赖，可离线跑。
- **详情 webview 首次交互**：窗口重载/详情视图首次解析后，第一次点击操作按钮可能被 webview 加载时序吞掉，重试一次即可（第二次起稳定）。
- **mockGateway 生命周期**：`node test/mockGateway.js [port] [autoCompleteMs]`——任务 start 后 autoCompleteMs（默认 1500）自动 COMPLETED；第 3 参数可拉长 RUNNING 窗口（如 15000）供 GUI 人工测试暂停/恢复/取消；对不存在任务按真实网关口径返回 404。
