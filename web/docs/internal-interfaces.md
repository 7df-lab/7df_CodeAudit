# CodeAudit Console — 内部接口契约（模块间预期输入 / 预期输出）

> 范围：本仓库内部模块之间的全部调用面。分层：
> `pages/ → (client.ts 端点层 | stateMachine | chainParser | dict) → axios → [外部]`，
> 横切：`session.tsx 上下文`、`apiEvents 事件总线`、`components 展示组件`、`testsupport/fakeGateway 测试台`。
>
> 纪律（ADR-203）：**页面禁止 `.data as {手写形状}`**——REST 响应形状单点锚定在 client.ts 类型化端点；
> 形状漂移由 `tsc -b` 在消费点报红，而非等 GUI 事故。守卫脚本 `npm run guard` 部分强制。
>
> **锚点编号 [I-nn]**：对应 `src/__tests__/` 用例（本文档 ↔ 测试双向追溯）。

---

## 1. api/client.ts —— 端点层（唯一直接持有 axios 实例的模块）

导出面（其余模块一律经此访问 HTTP；只有拦截器内部与 bootRefresh 使用裸 fetch）：

| ID | 导出 | 输入 → 输出 | 契约要点 | 锚点 |
|----|------|-------------|----------|------|
| I-01 | `api`（axios 实例） | baseURL `'/'`；paramsSerializer 见 E-00a | 请求拦截器注入 Bearer；响应拦截器实现 §E-40..46 | client.test.ts / clientInterceptors.test.ts |
| I-02 | `setAccessToken / getAccessToken` | string ↔ string（模块内存变量） | access 永不落 localStorage | session.test.tsx（login 后内存态） |
| I-03 | `saveRefreshToken / readRefreshToken` | string ↔ string；键 `codeaudit.refresh_token`（`TOKEN_KEY` 导出） | 经 `globalThis.localStorage?.` 可选访问（node 测试环境可注入桩） | client.test.ts |
| I-04 | `clearSession` | — → void | 同时清内存 access 与 localStorage refresh | client.test.ts「refresh 失败」 |
| I-05 | `noteRateLimit(retryAfterS?) / pollIntervalMs(base)` | number → void / number | clamp [5,60]s；退避期内返回剩余毫秒（ceil），否则原样 base | clientInterceptors.test.ts「429」 |
| I-06 | `uploadArchive(file: File)` | File → `UploadArchiveResponse{upload_id, file_id, file_path, size_bytes}` | FormData 字段名 `file`；`Content-Type: multipart/form-data` 显式；timeout 120_000 | clientContract.test.ts |
| I-07 | `getProjects(pagination?)` | `{page_size, cursor?}?` → `ListProjectsResponse` | cursor 缺省补 `''`（首屏语义，E-00a JSON 内空游标保留） | clientParams.test.ts |
| I-08 | `getProject(id)` / `getProjectConfig(id)` | string → `Project` / `ProjectConfigResponse` | 响应**裸形**（E-14/E-15） | clientContract.test.ts |
| I-09 | `updateProjectConfig(id, config)` | `(string, Record<string,string>)` → `ProjectConfigResponse` | 请求体**双层包装** `{config:{project_id, config}}`（E-16——扁平体被 protojson 丢字段） | clientContract.test.ts |
| I-10 | `createProject(payload)` | `CreateProjectPayload` → `Project` | 请求体包装 `{project: payload}`（E-13） | clientContract.test.ts |
| I-11 | `getTools()` | — → `{tools: ToolInfo[]}` | gateway 手写形状在此锚定（E-32） | TaskNewPage.test.tsx |
| I-12 | `createTask(payload)` | `CreateTaskPayload{project_id, scan_mode, sast_tools, config}` → `{task_id}` | config 键域见 E-19 | TaskNewPage.test.tsx |
| I-13 | `bootRefresh()` | — → `Promise<string \| null>` | 有 refresh→裸 fetch 续签返回新 access；失败/无 token → clearSession + null（**不抛错**，F5 恢复永不阻塞进站） | session.test.tsx boot 分支 |
| I-14 | `getReportContent(reportId)` | string → `{format: 'html'\|'json', content: string}` | `responseType:'text'` + `transformResponse:[(d)=>d]`（禁 axios 自动 parse）；`<` 前缀嗅探分型 | clientContract.test.ts |
| I-15 | `getSourceFile(taskId, path)` | `(string, string)` → `SourceFileResp` | 失败时把服务端 `{error}` 详情提为 `Error.message`（降级横幅可读） | clientContract.test.ts |

## 2. api/apiEvents.ts —— 事件总线常量（client ↔ errors 组件解耦）

| ID | 导出 | 值 | 生产者 → 消费者 | 锚点 |
|----|------|----|----------------|------|
| I-20 | `API_ERROR_EVENT` | `'codeaudit:api-error'` | client 拦截器（403/501/503，auth 豁免）→ `ApiErrorOverlay`（403/501 整页；503 横幅） | errors.test.tsx；clientInterceptors.test.ts |
| I-21 | `API_OK_EVENT` | `'codeaudit:api-ok'` | client 拦截器（任意 2xx）→ ApiErrorOverlay 撤横幅 | 同上 |
| I-22 | `ApiErrorCode` | `403\|404\|501\|503` | — | — |

## 3. auth/session.tsx —— 会话上下文

| ID | 接口 | 输入 → 输出 | 契约要点 | 锚点 |
|----|------|-------------|----------|------|
| I-30 | `SessionProvider` | 包裹整树 | 挂载即 boot：有 refresh_token → `bootRefresh()` → `refreshUser()`；booting 期间消费方必须渲染「会话恢复中」（Shell/LoginPage 双处防闪烁） | session.test.tsx；AppRouting.test.tsx |
| I-31 | `useSession(): SessionCtx` | — → `{user, booting, login, register, logout, refreshUser}` | 无 Provider 时**必须抛错**（防静默 undefined） | session.test.tsx「无 Provider 抛错」 |
| I-32 | `login(u,p)` | 凭证 → void（user 状态更新） | E-01 → setAccessToken + saveRefreshToken → refreshUser；失败上抛由页面兜文案 | session.test.tsx |
| I-33 | `register(u,e,p,invite)` | 四参 → void | `invite ''→undefined` 剔除键（E-02）；注册即登录 | session.test.tsx |
| I-34 | `logout()` | — → void | E-04 携带 access_token；**finally 清会话**（服务端错也登出） | session.test.tsx |
| I-35 | `CurrentUser` | — | `role?` 驱动菜单显隐 + RequireAdmin；`must_change_password?` 驱动 Shell 强改密锁 | AppRouting.test.tsx |

## 4. tasks/stateMachine.ts —— 状态机展示镜像（纯逻辑 + 动作分发）

| ID | 导出 | 输入 → 输出 | 契约要点 | 锚点 |
|----|------|-------------|----------|------|
| I-40 | `ALLOWED_ACTIONS / allowedActions(status?)` | 状态枚举串 → `TaskAction[]` | **客户端展示镜像**：只决定按钮可见性；服务端才是转换权威。未知状态 → `[]`（不自造动作）。2026-09-01 审批流废除后 CREATED=[start]、RUNNING=[pause,cancel]、PAUSED=[resume,cancel]、DEAD=[retry,cancel] | stateMachine.test.ts（全状态分支） |
| I-41 | `isTerminal(status?)` | 状态 → boolean | 终态四值 COMPLETED/CANCELLED/TIMEOUT/DEAD（与服务端 IsTerminal 同源口径） | stateMachine.test.ts |
| I-42 | `progressRefetchInterval(status?)` | 状态 → `3000 \| false` | 非终态恒 3s（ADR-156 QUEUED 也轮询）；终态 false 自停 | stateMachine.test.ts |
| I-43 | `actionLabel(a)` | TaskAction → 中文 | 启动/取消/人工重试/暂停任务/恢复任务 | — |
| I-44 | `dispatchAction(task, a)` | `(ScanTask, TaskAction)` → task_id | 动作→端点映射：start/cancel/retry/pause/resume → `POST /v1/tasks/{id}/<动作>`（E-21）；非法提交交后端拒绝 | TaskDetailSnapshot.test.tsx |
| I-45 | `autoRunTask(taskId)` | string → void | `POST /v1/tasks/{id}/start`（创建后自动启动；失败上抛由调用方 warning） | ProjectsPage.test.tsx（行为级） |

## 5. findings/chainParser.ts —— AI 结论链路解析（纯函数）

| ID | 导出 | 输入 → 输出 | 契约要点 | 锚点 |
|----|------|-------------|----------|------|
| I-50 | `parseChain(text?: string \| null)` | 自由文本 → `{hops: ChainHop[], files: string[]}` | 六类引用形态（file:line / 行区间 / 中文行引用 / L 前缀 / lines 英文 / 全角括号数字区间）；反引号段置零抑制噪声；hops 按原文顺序去重；行引用挂接最近文件；role 仅关键词命中（sink 优先），**不推测**；行引用先于文件→丢弃 | chainParser.test.ts（人类指令实例逐字 + 普适形态） |
| I-51 | `baseName(p)` | 路径 → 末段 | 文件选择器基名对齐 | chainParser.test.ts |
| I-52 | `ChainHop` | `{path, line?, endLine?, snippet, role?}` | path 为**原文写法**（裸文件名/截断路径），消费方交服务端回退解析（E-24） | codeContext.test.tsx |

## 6. dict/index.ts —— 枚举中文字典（展示翻译，值域=proto 枚举，不自造）

| ID | 导出 | 契约要点 | 锚点 |
|----|------|----------|------|
| I-60 | `AI_VERDICT`（7 键）/ `SCAN_MODE`（5 新 + 2 弃用，键序=展示序）/ `TASK_STATUS`（含 PENDING 保留值）/ `SEVERITY` / `STAGE_STATUS` / `STAGE_TYPE` / `REVIEW_DEPTH` / `ROLE` / `USER_STATE` / `REPORT_FORMAT`（数值键） | 完整性由 dict.test.ts 锁定（新增 proto 枚举值必须同步，否则 fallback 显示原键——P4 不隐藏数据） | dict.test.ts |
| I-61 | `zh(map, key)` | 未知键回显原键；空键 → UNSPECIFIED 中文 | dict.test.ts |
| I-62 | `DEPRECATED_SCAN_MODES` | 新建入口过滤弃用模式；历史展示不过滤 | dict.test.ts；TaskNewPage.test.tsx |
| I-63 | `reportFileExt(format?)` | 1=pdf/2=html/4=csv/其余(0,3,undefined)=json（编排器缺省产 JSON） | dict.test.ts |

## 7. components —— 受控展示组件（props 契约，数据由页面下发，不自拉）

| ID | 组件 | Props 输入 | 行为契约 | 锚点 |
|----|------|------------|----------|------|
| I-70 | `TaskLogPanel` | `{logs: TaskLogEntry[], terminal, onRefresh, refreshing, live?}` | 级别过滤（all/warn/error）；warn/err 徽标；`hhmmss(Number(ts_ms))`（int64 字符串防御，E-00c）；**滚底跟随**：新数据滚底、用户上翻 40px 阈值停跟随；空态文案 | TaskLogPanel.test.tsx |
| I-71 | `AIInteractionLogPanel` | `{text, totalBytes, complete, onRefresh, refreshing, live?}` | `parseTimeline` 标记行解析（💭/✍/📋/🤖/──/══/■/⚠/▶ 六类条目）；思考流式展开（未收束）/折叠（归档）；任务下发/子任务折叠块（字节数+首行预览）；渐进回看窗口 400 条；下载完整日志；整页 Modal 辅入口；**内联与 Modal 各持 ref**（共享 ref 被 Modal 抢占致滚底永久失效——2026-09-06 修复，变异 M8 锁定） | AIInteractionLogPanel.test.tsx（7 例） |
| I-72 | `ErrorPage({code: 403\|404\|501})` | code | 403/404 Result 页 + 501 灰卡（诚实降级话术，非故障话术） | errors.test.tsx |
| I-73 | `ApiErrorOverlay()` | 无 props（事件驱动） | 订阅 I-20/I-21；503 横幅可关闭；403/501 整页可关闭返回项目页 | errors.test.tsx |

## 8. App.tsx —— 路由与全局壳

| ID | 接口 | 契约要点 | 锚点 |
|----|------|----------|------|
| I-80 | 路由表 | 见 README；`*` → 404 不静默重定向；`/` → `/projects` | AppRouting.test.tsx |
| I-81 | Shell 守卫链 | 顺序：`booting` → 恢复中；`!user` → `/login`（replace）；`must_change_password && path!==/change-password` → 锁改密页（防回环放行自身） | AppRouting.test.tsx |
| I-82 | `RequireAdmin` | `user.role !== 'ROLE_ADMIN'` → ErrorPage 403（后端 requireAdmin 为最终防线，前端仅体验层） | AppRouting.test.tsx；UsersPage.test.tsx（页面级双保险） |
| I-83 | `useUnreadCount()` | — → number | queryKey `['notify-unread', user_id]`，enabled !!user，60s 兜底轮询；菜单项渲染 `（N 未读）`，点击失效缓存并跳通知页 | AppRouting.test.tsx |
| I-84 | 菜单高亮 | 按路径前缀选中（projects/tasks/reports/notifications/admin/users） | — |

## 9. 跨模块状态契约：react-query 缓存键与失效图（曾多发静默失效 bug，此处单点化）

| ID | queryKey | 写入方 | 失效方（必须成对） | 锚点 |
|----|----------|--------|--------------------|------|
| I-90 | `['task-snapshot', taskId]` | 快照轮询 + **WS 帧 setQueryData**（同构吸收） | 动作成功后 invalidate；`['tasks']` 前缀同刷 | TaskDetailSnapshot.test.tsx |
| I-91 | `['task-reports', taskId]` / `['report-content', id]` | 任务详情报告卡 | regenerate 成功必须失效 `['task-reports', taskId]`（**只失效 `['reports']` 前缀不覆盖此键**——2026-09-06 修复） | TaskDetailPage.test.tsx |
| I-92 | `['reports', taskFilter, page, cursor]` / `['reports-index']` | 报告中心 / 任务列表索引 | regenerate 失效 `['reports']` 前缀 | ReportsTasksPaging.test.tsx |
| I-93 | `['findings', taskId, cursor]` / `['finding', id]` | 列表 / 详情 | triage 成功失效**两者**（只刷详情则列表行停留旧标签，ADR-152） | FindingDetailPage.test.tsx；FindingsPage.test.tsx |
| I-94 | `['notify-unread', uid]` / `['notifications', uid]` | App 角标 / 通知页 | markRead 失效**两者**（ADR-156 角标即时消失） | pages2.test.tsx |
| I-95 | `['projects', page]` / `['project', id]` / `['project-config', id]` / `['project-tasks', id]` | 项目页群 | 建项目/删项目后 invalidate `['projects']` | ProjectsPage.test.tsx |
| I-96 | `['tasks-page', project, mode, page]` | 任务列表 | 自动建任务后 invalidate（**禁用死键** `['tasks-infinite']`——无限滚动时代遗物，invalidate 是 no-op，2026-09-06 修复；guard.sh 禁复活） | ProjectsPage.test.tsx |
| I-97 | `['source-file', taskId, path]` | 发现详情全文 | staleTime 5min；失败降级片段不缓存错误 | codeContext.test.tsx |
| I-98 | `['inference-providers']` / `['inference-route']` | Provider 管理页两卡（ADR-217） | 保存/删除失效 `['inference-providers']`；**切路由失效两者**（列表"当前使用"标记读 route，漏刷则标记滞留旧 provider） | ProvidersPage.test.tsx |

**QueryClient 全局缺省**（main.tsx）：`retry: 1, refetchOnWindowFocus: false`。快照/源码/用户列表按需覆写 `retry: false`（404 终态语义，E-47）。

## 10. 页面复用契约（组件既是路由页又是内嵌体）

| ID | 组件 | 复用关系 | 输入差异 |
|----|------|----------|----------|
| I-A0 | `FindingDetailBody({findingId})` | 路由薄壳 FindingDetailPage（深链 `/findings/:fid`）+ FindingsPage 行展开 + ReviewView 行展开 | 同一 props；内嵌态随外层缓存失效联动（I-93） |
| I-A1 | `FindingsPage({taskId})` | 任务详情终态 Tabs 内嵌 | 内嵌时不带路由（taskId 直传） |
| I-A2 | `FusionView / ComparisonView / ReviewView({taskId})` | Tabs 内嵌 + 对比路由页 | ComparisonView 仅模式E入口（E-21 完成态按钮） |
| I-A3 | `TaskDetailPage({taskId})` | 路由经 `TaskDetailWithParams` 从 useParams 取 id | 空串 id → 快照 404 专页（内存存储重启语义） |

## 11. 测试台接口（testsupport/fakeGateway.ts —— 测试代码的公共内部契约）

| ID | 导出 | 契约要点 |
|----|------|----------|
| I-B0 | `useFakeGateway(routes)` | axios **adapter 层**造假：`api/client` 真实代码全量执行；路由键 `'GET /v1/tasks/:id'`（`:seg` 路径参数、`* /path` 任意方法）；值=handler`(ctx)=>payload` 或直接载荷；**未建模路由抛错**（响亮失败，禁静默空成功）；`requests` 日志按用例隔离（beforeEach 清空） |
| I-B1 | `httpError(status, body)` | handler 内抛出 → 转 axios 形状 → **经真实拦截器链回放**（401 刷新/429 退避/503 重试均可测） |
| I-B2 | `HandlerCtx` | `{params, query: URLSearchParams, body(已 JSON 解析), raw(FormData 原样), config}` |

**纪律（guard.sh 强制）**：测试代码禁止 `vi.mock('../api/client')` 整模块替换——那会绕过全部真实客户端行为，mock 与契约无任何关联（ADR-203 前的假绿根源；pages2.test.tsx 已迁移，此模式禁再入）。
