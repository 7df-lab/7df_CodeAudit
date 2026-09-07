# 外部接口契约：预期输入与预期输出

> 事实源文档：逐条描述本扩展与外部世界（平台网关、VS Code 宿主、工作区文件系统、命令行工具）的接口面。
> 每条契约标注「锁定测试」——该行为由哪个测试/关卡钉住（详见 [regressions.md](regressions.md) 防回归机制）。
> 改接口的纪律：先改本文档 → 再改/加锁定测试 → 最后改实现（见 regressions.md「防回归纪律」）。
> 与代码冲突时以代码为准，并按纪律回到本文档补一笔。

---

## 1. 平台网关 REST 接口（出站，`src/apiClient.ts`）

### 1.0 通用请求契约（全部端点共用）

| 项 | 预期行为 | 锁定测试 |
|---|---|---|
| baseUrl | 构造时剥离尾部 `/`（`http://x:8080/` → `http://x:8080`） | `apiClient.test.ts › login 成功后保存 access+refresh` |
| 鉴权头 | 非跳过鉴权请求携带 `Authorization: Bearer <accessToken>`；跳过鉴权（login/logout/refresh）不带 | `apiClient.test.ts › 401 触发单飞刷新并重放原请求` |
| Content-Type | body 为 JSON 时 `application/json`；FormData（上传）不设置（交由运行时生成 boundary） | 代码路径 `requestJson` |
| 查询串编码 | `encodeQuery`：跳过 `undefined/null/''`；对象值 JSON 编码（ADR-155 网关 decodeQuery 口径）；标量 `String()` | `apiClient.test.ts › encodeQuery（ADR-155 JSON 风格查询参数）` |
| 401 语义 | 非跳过鉴权请求 401 → 单飞刷新（并发请求共享一次）→ 新 token 重放原请求恰好一次；刷新请求自身走裸 fetch 不经 401 拦截（防递归） | `apiClient.test.ts › 401 触发单飞刷新并重放原请求（并发共享一次）` |
| 刷新失败语义 | 无 refresh token 或 refresh 响应非 2xx → 清空会话（tokens.clear）→ 抛 `ApiError` | `apiClient.test.ts › 刷新失败（无 refresh token）抛 401 且清空会话` |
| 429 语义 | 解析响应体 `retry_after`（缺省/非数按 15s），钳位 5~60s，记录 `rateLimitUntil = now + s*1000`（轮询方读它跳过限流窗口）；请求本身仍按非 2xx 抛 ApiError | `apiClient.test.ts › 429 记录退避窗口（retry_after 钳位 5~60s）` |
| 其他非 2xx | 抛 `ApiError(status, "METHOD /path -> status: <body前300字符>", bodyText)` | 代码路径 `requestJson` |
| 响应体 | 2xx 一律 `resp.json()` 解析为泛型 T | 代码路径 `requestJson` |

### 1.1 `POST /v1/auth/login`

- **输入**：`{ username: string, password: string }`（JSON；跳过鉴权）。
- **预期输出**：`LoginResponse { access_token, refresh_token, expires_in_s }`；副作用：两个 token 写入 TokenStore（access 内存缓存 + refresh 持久化，见 data-flows.md §1）。
- **锁定测试**：`apiClient.test.ts › login 成功后保存 access+refresh`。

### 1.2 `POST /v1/auth/refresh`

- **输入**：`{ refresh_token: string }`（裸 fetch，不经 requestJson）。
- **预期输出**：2xx → `LoginResponse`，新 token 对写回 TokenStore，返回新 access；非 2xx → **清空会话** + 抛 `ApiError(resp.status)`。
- **锁定测试**：`apiClient.test.ts › 刷新失败…`；单飞并发行为见上表。

### 1.3 `POST /v1/auth/logout`

- **输入**：`{ access_token: string }`（跳过鉴权）。
- **预期输出**：无论请求成败（finally），本地会话清空；函数正常返回。
- **锁定测试**：代码路径 `logout`；UI 侧命令注册守卫 `guards.test.ts › 命令注册守卫`。

### 1.4 `GET /v1/projects`

- **输入**：无参数。
- **预期输出**：`{ projects: Project[] }`；`projects` 缺失时返回 `[]`。`Project` 形状见 [types.ts](../src/types.ts)。
- **锁定测试**：`apiClient.test.ts › 401 触发单飞刷新并重放原请求`（借道断言响应形状）。

### 1.5 `GET /v1/tools`

- **输入**：无参数。**预期输出**：`{ tools: ToolInfo[] }`，缺失回 `[]`。

### 1.6 `GET /v1/tasks?project_id=<id>`

- **输入**：`project_id` 标量参数（缺省时无 query）。
- **预期输出**：`{ tasks: TaskSummary[] }`，缺失回 `[]`；平台口径为创建时间倒序，插件取首个 `TASK_STATUS_COMPLETED` 作为「最近完成」（`latestCompletedTask`）。
- **锁定测试**：extension 行为测试 `extension.test.ts › 恢复链路`（见 data-flows.md §4.4）。

### 1.7 `POST /v1/tasks`

- **输入**：`{ project_id, scan_mode, sast_tools: string[], config: Record<string,string> }`。
- **`config` 契约（ADR-200，易错点）**：上传响应含 `file_id` → `config.upload_file_id = file_id`；否则旧平台回退 `config.project_path = dir`；**两者都缺 → doScan 抛错中止，不发 createTask**。把桶内对象路径误当 `project_path` 下发会让沙箱拿到 32B 空 tar.gz（历史事故，见 regressions.md #6）。
- **预期输出**：`ScanTask`（含 `task_id`）。
- **锁定测试**：`extension.test.ts › doScan 走 upload_file_id 契约…`、`…file_id/dir 都缺 → 中止并报错`。

### 1.8 `POST /v1/tasks/{id}/start | /cancel | /pause | /resume`

- **输入**：空 JSON 对象 `{}`；路径携带任务 ID。
- **预期输出**：2xx 即 void；语义：start=启动；cancel=取消（终态 TASK_STATUS_CANCELLED）；pause=暂停（TASK_STATUS_PAUSED 非终态）；resume=恢复运行。
- **锁定测试**：`apiClient.test.ts › cancelTask POST 到 /v1/tasks/{id}/cancel`；pause/resume 走同一代码路径。

### 1.9 `GET /v1/tasks/{id}/snapshot?logs_after=<logId>&ai_cursor=<bytes>`

- **输入**：游标可选（两者都缺省时不带 query）；`logs_after` = 已见最后 log_id（服务端只回严格更大的条目），`ai_cursor` = AI 正文已收字节偏移。
- **预期输出**：`TaskSnapshot` 四路同构聚合：`{ task: ScanTask, progress?: TaskProgressFull|null, logs?: { logs: TaskLogEntry[] }|null, ai?: AiLogChunk|null }`。
- **消费方**：TaskWatcher 轮询回退 + bindTask 历史重建，与 WS 重连续订共用游标口径。
- **锁定测试**：`apiClient.test.ts › taskSnapshot 增量游标：logs_after/ai_cursor 进 query，空游标不发参数`。

### 1.10 `GET /v1/findings?task_id=<id>&pagination=<JSON>`

- **输入**：`pagination` 为 JSON 编码对象 `{ page_size: 100, cursor: '' }`（ADR-155）。
- **预期输出**：`FindingsPage { findings: UnifiedFinding[], pagination: { next_cursor, has_next, total } }`；客户端自动翻页累积（上限 50 页防御），返回拼接后的 `UnifiedFinding[]`。
- **锁定测试**：`apiClient.test.ts › listFindings 翻页累积（has_next/next_cursor）`。

### 1.11 `POST /v1/uploads/archive`（multipart/form-data）

- **输入**：字段名 `file`，值为工作区 zip 的 Blob，文件名 `workspace.zip`。
- **预期输出**：`UploadResult { upload_id, file_id?, file_path?, size_bytes?, dir?, files? }`；新平台（ADR-148/163）回 `file_id`（桶内对象锚点），旧平台回 `dir`（解压目录）。
- **锁定测试**：`extension.test.ts › doScan…`（走全局 fetch 桩全链验证）。

---

## 2. 平台网关 WebSocket 接口（出站连接，`src/taskWatcher.ts` + `extension.ts wsUrl`）

### 2.1 URL 形态

```
ws(s)://<gateway>/v1/tasks/{taskId}/ws?token=<encodeURIComponent(accessToken)>
  [&logs_after=<lastLogId>][&ai_cursor=<aiCursor>]
```

- `serverUrl` 的 `http(s)` 前缀机械替换为 `ws(s)`；
- `logs_after`/`ai_cursor` 仅在非空/大于 0 时附加——**断线重连带游标续订，服务端自此起算不重发**（ADR-189）。

### 2.2 帧契约（服务端 → 插件）

- 每帧为完整 `TaskSnapshot` JSON（与 REST 快照同构，四路 task/progress/logs/ai）。
- **预期输入**：合法 JSON 帧 → 归并进 ProgressState 并 `emit('snapshot')`；终态帧（COMPLETED/CANCELLED/TIMEOUT/DEAD）额外 `emit('terminal', status)` 并关闭 watcher（不再重连、不再轮询）。
- **预期输入**：非 JSON 帧 → 静默忽略（不崩、不重连）。

### 2.3 连接生命周期

| 事件 | 预期行为 | 锁定测试 |
|---|---|---|
| onopen | `setWsLive(true)` + `onWsEvent('open')` | `taskWatcher.test.ts › onWsEvent 透出 open/close/error 原始细节` |
| onclose（reason 含 "not found"） | **任务已被平台删除/归档**：置 closed、`onTaskGone(reason)`、绝不重连 | `taskWatcher.test.ts › WS 关闭原因为 "task not found"…` |
| onclose（其他） | `setWsLive(false)` → 5s 后重连（终态后 close 已阻止） | `taskWatcher.test.ts › WS 帧驱动状态；终态帧触发 terminal…` |
| onerror | `onWsEvent('error', detail)` → 回退轮询 → 5s 重连 | 同上 |
| 无 WebSocket 环境（makeSocket 抛异常） | 纯 10s 快照轮询兜底至终态 | `taskWatcher.test.ts › WS 无环境…纯轮询兜底至终态` |
| 轮询遇 429 限流窗口 | 本轮跳过，10s 后重试 | `taskWatcher.test.ts › 429 限流窗口内跳过本轮轮询` |
| 轮询遇 404 "not found" | `onTaskGone` + 终止轮询 | `taskWatcher.test.ts › 轮询快照 404 not found…` |

- **`onTaskGone` 的上层语义**（extension）：本地 progress 落终态 `TASK_STATUS_DEAD`、`taskRunning/taskPaused` 上下文清位、释放扫描互斥、警告通知「已在平台删除或归档——已停止进度同步」。
- 常量：`WS_BACKOFF_POLL_MS = 10_000`、`WS_RECONNECT_MS = 5_000`。

---

## 3. 平台 DTO 契约（`src/types.ts`，protojson JSON 口径）

与 web 控制台同源（proto/codeaudit_common.proto 的 snake_case JSON）。解析容忍规则：

| protojson 形态 | 预期处理 | 锁定测试 |
|---|---|---|
| int64 字段（`ts_ms`/`next_cursor`/`total_bytes`） | 十进制字符串或数字两种形态都接受，经 `Number()` 归一（非法→0） | `progressModel.test.ts › parseTsMs/fmt…`、`asNumber` 用例 |
| bytes 字段（`ai.chunk`） | base64 串 → utf-8 渲染文本 | `progressModel.test.ts › ai chunk 增量追加…` |
| Timestamp（`started_at` 等） | ISO 串，可能带 9 位小数秒 → 截到毫秒解析；非法/缺失 → null | `progressModel.test.ts › parseTsMs 容忍 9 位小数秒` |
| 枚举 | 一律枚举名字符串（`SEVERITY_HIGH`/`TASK_STATUS_RUNNING`/`STAGE_STATUS_*`），未知值有兜底显示 | `progressModel.test.ts › 标签映射` |
| 任务终态 | `TASK_STATUS_COMPLETED / CANCELLED / TIMEOUT / DEAD` 为终态（`isTerminalTaskStatus`） | `taskWatcher.test.ts › 终态帧触发 terminal` |

`UnifiedFinding` 关键字段语义（`diff_patch` / `ai_fix_suggestion` / `location` / `ai_confidence`）见 internal-interfaces.md §4 与 §7。

---

## 4. VS Code 宿主接口（入站：宿主调用扩展）

### 4.1 命令（20 个，`package.json contributes.commands` ⇄ `extension.ts registerCommand`）

一致性由守卫测试钉住：`guards.test.ts › 命令注册守卫（package.json ⇄ extension.ts 互为全集）`。
触发形态与预期反馈：

| 命令 | 输入形态 | 预期反馈（输出） |
|---|---|---|
| `codeaudit.login` | 无参；三个 InputBox（地址/用户名/密码），取消即中止 | 成功：`loggedIn` 上下文 + 信息通知；失败：错误通知（含代理排查提示）；serverUrl 写全局配置 |
| `codeaudit.logout` | 无参 | 清会话 + `loggedIn=false` + 信息通知 |
| `codeaudit.selectProject` | 无参；QuickPick 单选项目 | `projectId` 写**工作区**配置 + `boundProject` 上下文 + 信息通知；未登录→警告；平台无项目→信息 |
| `codeaudit.scanWorkspace` | 无参 | 见 data-flows.md §4.1 扫描链路；扫描互斥：进行中→警告拒绝；listTools 连通性前置探测失败→「无法连接平台」错误且不打包 |
| `codeaudit.runningMenu` | 无参（状态栏运行中点击） | QuickPick 汇聚暂停/取消/查看 AI 上下文 |
| `codeaudit.cancelScan` | 无参；模态确认 | 确认→POST cancel + 信息；无任务→信息 |
| `codeaudit.pauseScan` / `resumeScan` | 无参 | POST pause/resume + 信息；失败→错误通知 |
| `codeaudit.refreshFindings` | 无参 | 有 lastTaskId：仅重拉 findings；无：兜底绑定平台该项目最近完成任务 |
| `codeaudit.clearFindings` | 无参 | 清本地诊断+树，平台数据不动，信息通知 |
| `codeaudit.showAiContext` | 无参 | 展示/解析底部面板视图 + 增量推送 |
| `codeaudit.openFinding` | `UnifiedFinding` 或树节点包装（`{finding}`） | 跳转编辑器选中行 + 侧栏切漏洞详情；无位置→信息通知 |
| `codeaudit.fixFinding` | `UnifiedFinding` / 树节点包装 / 无参（→QuickPick 选发现） | 修复链路见 data-flows.md §4.2；诚实降级：无建议→警告 |
| `codeaudit.applyLowRiskFixes` | 无参；QuickPick **多选** | 候选筛选→逐条应用（同核心）→汇总通知；单条失败跳过记日志 |
| `codeaudit.rollbackFixes` | 无参 | 优先登记表最近 applied 记录；无登记兜底最近 checkpoint |
| `codeaudit.rollbackFix` | `UnifiedFinding`/树节点包装 / 无参（→QuickPick 列出可回滚项） | 见回滚链路 data-flows.md §4.3；无已应用修复→信息通知 |
| `codeaudit.copyFindingId` / `copyFilePath` | 树节点包装或 finding | 写剪贴板 + 状态栏提示 3s |
| `codeaudit.openConsole` | 无参 | `openExternal(consoleUrl || serverUrl:4173 + /tasks/{lastTaskId})` |
| `codeaudit.selectTask` | 无参；QuickPick 单选 | 列平台该项目任务（时间倒序）→ bindTask 拉历史结果 |

- 树菜单（view/item/context）传的是 `TreeNode` 包装，命令入口经 `asFinding` 解包——回归锁：`extension.test.ts › asFinding 解包`（经由 rollbackFix/fixFinding 命令驱动）。

### 4.2 配置（8 键，`codeaudit.*`）

一致性守卫：`guards.test.ts › 配置键守卫（src 读取 ⇄ package.json 声明互为一致）`。
键与默认值：`serverUrl`（http://localhost:8080）、`consoleUrl`（空）、`projectId`（空，写工作区）、`scanMode`（SCAN_MODE_PARALLEL，五枚举）、`sastTools`（[]）、`excludeGlobs`（7 项默认）、`minPackFiles`（10）、`autoOpenAiContext`（true）。

### 4.3 上下文键（`setContext`，供 when 子句消费）

`codeaudit.loggedIn / boundProject / hasTask / taskRunning / taskPaused / findingDetail`。
守卫：`guards.test.ts › 上下文键守卫（extension 设置的键都被 package.json when 子句消费）`。

### 4.4 视图与容器

- 活动栏容器 `codeaudit`：「扫描结果」树（`codeaudit.findings`）、「任务进度」树（`codeaudit.progress`，when `!codeaudit.findingDetail`）、「漏洞详情」webview（`codeaudit.findingDetail`，when 互斥）。
- 底部面板容器 `codeaudit-panel`：「AI 交互上下文」webview（`codeaudit.aiContext`）。
- 守卫：`guards.test.ts › 视图 ID 守卫`。欢迎页 4 条（viewsWelcome）按上下文键组合显示。

### 4.5 URI 深度链接（`vscode://codeaudit.codeaudit-vscode/<action>`）

- **输入**：`scan` 或 `selectTask`（取 path/query 首段）。
- **预期输出**：分发执行同名命令；未知 action → 仅记日志警告。
- 锁定测试：`extension.test.ts › URI 深度链接分发`。

### 4.6 编辑器灯泡（CodeActionProvider，scheme=file）

- **输入**：文档 + 光标 range 与诊断相交。
- **预期输出**：`CodeAudit: AI 修复此漏洞` QuickFix，命令 `codeaudit.fixFinding`，参数 = 与该文件路径匹配的 `UnifiedFinding`（首个）。无相交诊断或无匹配发现 → 返回 `[]`。

### 4.7 webview postMessage 协议（双向）

**下行（插件 → AI 上下文视图）**，`AiViewUpdate`：
```json
{ "type": "update", "h1Html": "<…>", "percent": 42, "chipsHtml": "<…>",
  "logsHtml": "<…>", "aiHtml": "<…>" }
```
- 页内脚本仅处理 `type==='update'`；**增量更新不整页重载**——整页 HTML 只在 resolve/换任务/不可见恢复失败时重绘。锁定测试：`aiContextViewProvider.test.ts`（enableScripts 回归锁 + postUpdate 行为）、`extension.test.ts › AI 上下文视图增量推送`。

**上行（漏洞详情视图 → 插件）**，`FindingDetailAction`：
```json
{ "type": "action", "action": "openLocation" | "fix" | "rollback" }
```
- 插件仅在当前存在选中发现时受理；未知 action 忽略。锁定测试：`extension.test.ts › 漏洞详情按钮动作回传`。

### 4.8 虚拟文档 scheme `codeaudit-fix`

- **输入**：URI query = base64(JSON `{content: string}`)。
- **预期输出**：provideTextDocumentContent 返回 content；解析失败回 `''`。用于修复前后 diff 审阅（左=before 快照，右=当前文件或空文档）。

---

## 5. 工作区文件系统边界

| 方向 | 场景 | 预期行为 | 锁定测试 |
|---|---|---|---|
| 读 | 打包扫描（findFiles 20000 上限 + excludeGlobs） | 单文件读取失败**不阻塞整包**（zipFiles try/catch 跳过） | `workspaceZip.test.ts › 跳过读取失败的文件` |
| 读 | 补丁目标文件（Update/Delete 段） | 经 `openTextDocument` 取**编辑器缓冲区当前内容**为真相；文件不存在→解析器 Missing File 整体拒绝 | `extension.test.ts › Update 引用不存在的文件…` |
| 读 | minPackFiles 防空包 | 打包清单 < 阈值（默认 10）→ 中止上传并明确报错；`0` 关闭 | `extension.test.ts › minPackFiles 阈值中止` |
| 写 | 补丁落盘 | Update 走 WorkspaceEdit + 显式 save（保留撤销栈）；Add/Move 目标/Delete 源走 fs；目标目录自动创建 | `extension.test.ts › 机器补丁 Update/Add/Delete/Move` |
| 写 | 补丁引用路径 | 工作区禁闭：拒绝绝对路径、`..` 逃逸、空路径；Add/Move 目标已存在 → 拒绝覆盖 | `extension.test.ts › 路径禁闭` 系列 |
| 写 | 回滚写回 | Update 还原内容 + 显式 save；null 条目（修复前不存在）→ 删除文件；applyEdit 被拒→报错且登记不变（checkpoint 未消耗） | `extension.test.ts › 回滚…`、`…applyEdit 拒绝` |

---

## 6. 命令行工具入口（开发/验收用，非运行时）

| 脚本 | 输入 | 预期输出 | 依赖 |
|---|---|---|---|
| `npm test` | 无 | tsc 编译 out-test + mocha 全绿（vscode 模块重定向到 `test/mocks/vscode.js` 桩） | 无外部依赖，离线 |
| `npm run package` | 无 | vsce 打包 vsix + verify-vsix 关卡：VSIX 必含 `node_modules/adm-zip`（缺失 exit 1） | vsce |
| `node test/smoke.js [serverUrl]` | 在线网关 | 登录→上传→建任务→轮询→拉发现全链日志 | 平台网关 |
| `node test/fixflow.e2e.js [serverUrl]` | 在线网关 | 多风险同文件顺序修复→逐字节回滚→再应用 | 平台网关 |
| `node test/mockGateway.js [port]` | 无 | 本地 mock 网关（真实 diff_patch 回放），驱动修复 UI 全流程 | 无 |
