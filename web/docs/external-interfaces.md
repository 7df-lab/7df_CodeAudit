# CodeAudit Console — 外部接口契约（预期输入 / 预期输出）

> 范围：本仓库（codeaudit-web）与**外部世界**的全部交互面。
> 本应用是纯前端 SPA，唯一外部对端是**同源网关**（浏览器零直连微服务，14号 P1）：
> 开发/预览期经 Vite 代理（`vite.config.ts` server.proxy，`CODEAUDIT_GATEWAY_URL`，含 WS），
> 生产期经 nginx 反代（`nginx/default.conf.template`，`CODEAUDIT_GATEWAY_UPSTREAM`）。
>
> **契约锚点编号 [E-nn]**：每条契约都由 `src/__tests__/` 下的用例锚定（测试文件:用例名见末列）。
> 改动任何一行契约 = 必须同步改测试 + 本文档（同 commit）；`npm run guard` 静态守卫部分锚点。

---

## 0. 跨端点全局约定（所有 /v1 调用共享）

| # | 约定 | 预期行为 | 违反后果（历史实证） | 锚点 |
|---|------|----------|---------------------|------|
| E-00a | 查询参数序列化 | 标量照常（`task_id=t-1`）；对象/数组值 JSON 编码（`pagination={"page_size":20,"cursor":"5"}`）；undefined/null 剔除。`api.defaults.paramsSerializer` 统一覆写（client.ts:21） | axios 默认 bracket 风格 `pagination[page_size]=20`，网关 decodeQuery 解不出 cursor → 所有列表"加载更多/翻页"恒回第一页（GUI 实测 20 行重复追加） | clientParams.test.ts 全部 4 例 |
| E-00b | 认证头 | access_token 仅存内存（模块变量）；请求拦截器对非空 token 加 `Authorization: Bearer <access>`（client.ts:50） | — | clientInterceptors.test.ts「成功请求携带…」 |
| E-00c | protojson 形状 | 响应字段 = proto message 的 snake_case 直出；`int64 → 字符串`、`bytes → base64 字符串`、`Timestamp → RFC3339 字符串`、`enum → 枚举名字符串`、`map<string,string> → 对象` | int64 当 number 用 → `NaN` 渲染（hhmmss 回归） | TaskLogPanel hhmmss（Number(tsMs)）；TaskDetailSnapshot.test.tsx 游标断言 |
| E-00d | 错误 UX 契约 | 见 §2 拦截器行为表。401=静默刷新/跳登录；403/501=全局事件→整页错误组件（auth 端点豁免）；503=重试 3 次退避→降级横幅事件；429=记录退避截止时刻拉长轮询 | 403 被当 401 处理→无权限用户被登出；限流雪崩（4 轮询器时代 429 冻结页面） | clientInterceptors.test.ts、errors.test.tsx |
| E-00e | 未认证初始态 | access 缺失时首个请求 401 → 触发刷新链（§2）；无 refresh_token → 清会话跳 `/login` | — | session.test.tsx boot 分支 |

---

## 1. 端点清单（方法 / 输入 / 输出 / 错误）

### 1.1 认证与会话（/v1/auth/*，/v1/users/me）

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-01 | `POST /v1/auth/login` | JSON `{username, password}` | `{access_token, refresh_token, expires_in_s}`（LoginResponse） | 401 凭证错 → LoginPage 文案「登录失败：用户名或密码错误，或服务不可用」；**不触发刷新链**（url 含 /v1/auth/ 豁免） | session.test.tsx；clientInterceptors.test.ts「auth 端点 401 不刷新」 |
| E-02 | `POST /v1/auth/register` | JSON `{username, email, password, invite_code?}`；**invite_code 空串必须从 body 剔除**（`'' \|\| undefined`，JSON.stringify 丢 undefined 键）；V2.2 起 invitation 档邀请码必填（前端 required+动态文案，见 E-57） | 同 LoginResponse（注册即登录，） | 409+「invite code」语义=邀请码无效/未开放；409 其他=用户名/邮箱冲突；400=字段格式；均由 RegisterPage 分档如实展示 | session.test.tsx「register…剔除」；RegisterPage.test.tsx |
| E-03 | `POST /v1/auth/refresh` | JSON `{refresh_token}`（localStorage `codeaudit.refresh_token`）。**走裸 fetch，不经 axios 拦截器**（防递归刷新） | 同 LoginResponse；滚动续签：响应 refresh_token 非空则覆盖存储 | 非 2xx → refresh 失败：清会话 + `window.location.assign('/login')`（api 请求路径）/ 返回 null（bootRefresh 路径） | client.test.ts 两例；session.test.tsx boot 分支 |
| E-04 | `POST /v1/auth/logout` | JSON **必须携带 `{access_token}`**（proto L1203；空 body 恒 400——历史 bug：前端空 body 且清会话掩盖错误） | `{}` | 任意响应/错误都走 finally 清会话（logout 语义不受服务端状态影响） | session.test.tsx「logout 携带 access_token」 |
| E-05 | `GET /v1/users/me` | 无参数（网关从 JWT 注入身份） | `CurrentUser{user_id, username, email, role?, must_change_password?, state?, created_at?}`（role/must_change_password 为 V2.1 增量，旧令牌可缺省） | 401 → 刷新链 | session.test.tsx；AppRouting.test.tsx |

### 1.2 用户管理（admin；前端双重门禁 + 后端 requireAdmin 最终防线）

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-06 | `GET /v1/users` | query：`pagination={cursor}`（加载更多）、`username_contains`、`state`（可省） | `{users: User[], pagination?: {next_cursor?, has_next?, total?}}` | 403（非 admin）→ UsersPage「加载失败」如实展示（retry:false 不重试） | UsersPage.test.tsx（含 403 例） |
| E-07 | `POST /v1/users` | JSON `{username, email, password, role?}`（role 缺省时**剔除**，后端默认 DEVELOPER） | `{user_id, ...}`；首登须改密 | 40x → message.error 原文 | UsersPage.test.tsx「新建用户」 |
| E-08 | `PUT /v1/users/:id` | JSON `{user: {user_id, username, email, state}}`（**全量 user，Q2a 既有契约**；state ∈ USER_STATE_ACTIVE/INACTIVE） | `{}` | 自停用被前端禁用（不能停自己） | UsersPage.test.tsx「停用即 PUT 全量」 |
| E-09 | `POST /v1/users/:id/password:reset` | 空 JSON `{}` | `{temporary_password, must_change_password}`——临时密码**仅此一次**返回 | 失败 → message.error | UsersPage.test.tsx「重置密码」 |
| E-10 | `POST /v1/users/me/password` | JSON `{old_password, new_password}`；**body 禁止 user_id**（self 语义，网关 JWT 注入） | `{}`；成功后 must_change_password 清除 → refreshUser 放行 | 40x → 「修改失败：旧密码不正确或新密码不满足要求…」 | ChangePasswordPage.test.tsx 两例 |

### 1.3 上传（multipart 特例）

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-11 | `POST /v1/uploads/archive` | **multipart/form-data**，字段名 `file`（File 对象）；timeout 300s（B4-3：120s→300s，慢速上行大包与 nginx 反代窗对齐）；≤100MB（zip/tar.gz；前端 file.size 预检，超限本地拒绝不发请求）；nginx `client_max_body_size 100m` | `{upload_id, file_id, file_path, size_bytes}`——`file_id` 是唯一下游消费字段 | 4xx/5xx → 上传失败文案（「上传失败（仅支持 zip/tar.gz，≤100MB）」/「上传失败：<详情>」）；**响应形状变更必须显式修 file_id 消费链**（改形状无一测试报红的历史教训 → 本契约行即锚） | clientContract.test.ts「uploadArchive…」；ProjectsPage.test.tsx |

### 1.4 项目

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-12 | `GET /v1/projects` | query `pagination={page_size, cursor}`（offset 字符串；首页 `""`，**空游标显式保留**——client 归一）；增 `sort={field: name\|created_at, order}`（受控列头，UNSPECIFIED=DESC）与 `filter={conditions:[{name CONTAINS\|default_scan_mode EQ}]}`（AND-only，白名单外 400） | `ListProjectsResponse{projects: Project[], pagination?}`（**total=过滤后真实总数**；缺省排序 created_at DESC） | 白名单外 400 如实透出；加载失败 → 表格空态 | ProjectsPage.test.tsx（服务端排序/筛选）；clientContract E-12 空游标 |
| E-13 | `POST /v1/projects` | JSON **双层包装 `{project: {name, repo_url?, default_branch, default_scan_mode}}`**（proto L844 包装消息） | 裸 `Project`（protojson 直出，无包装） | repo_url 与上传件二选一由前端拦截（都缺省不发请求） | clientContract.test.ts「createProject…」；ProjectsPage.test.tsx |
| E-14 | `GET /v1/projects/:id` | 路径参数 | 裸 `Project`（proto L845；**无包装**——页面直接消费） | 404 → 详情页 project 为空渲染 id | ProjectDetailPage.test.tsx |
| E-15 | `GET /v1/projects/:id/config` | 路径参数 | `{project_id, config: Record<string,string>}`（proto L849） | — | clientContract.test.ts |
| E-16 | `PUT /v1/projects/:id/config` | JSON **双层包装 `{config: {project_id, config: Record<string,string>}}`**（UpdateProjectConfigRequest；**扁平体会被 protojson 丢字段 → "project not found"，E2E 实证后修正**） | `{project_id, config}` | — | clientContract.test.ts「updateProjectConfig 双层包装」（ProjectsPage.test.tsx 行为级） |
| E-17 | `DELETE /v1/projects/:id` | 路径参数 | `{}`；成功后失效列表缓存并跳 /projects | — | ProjectDetailPage.test.tsx「删除项目」 |
| E-58 | `PUT /v1/projects/:id` | JSON **双层包装 `{project: Project 整对象}`**（UpdateProjectRequest）。**服务端整体替换无字段合并——调用方必须 GET（E-14）全量读回后改字段整提交，漏带字段被清零**；网关钉死 `body.project.project_id`=URL 路径段（R73/R91 错位防线，client 同口径兜底）；repo_url 过服务端安全白名单（危险 scheme 400 详情透出） | 裸 `Project`（整体替换后回执） | 两步写半程失败（仓库已存/清指针失败）必须如实报半程状态不静默；配套清上传指针走 E-16（解析链上传档优先，不清则换仓库永不被读到） | clientContract.test.ts「updateProject 双层包装…（E-58）」；ProjectDetailPage.test.tsx「源码补配…」 |

### 1.5 任务

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-18 | `GET /v1/tasks` | query：`pagination={page_size:20, cursor:"(page-1)*20"}`（**服务端钳 page_size 上限 100**——proto"默认20，最大100"；全量聚合一律走 client `listAllTasks` 翻页，勿裸发 >100）；`project_id`（服务端过滤）；`filter={"conditions":[{field:"scan_mode",operator:"FILTER_OPERATOR_EQ",value:<mode>}]}`（契约 L1108-1112 形状；**服务端未实现的过滤字段会诚实报 400**） | `{tasks: ScanTask[], pagination: {next_cursor, has_next, total}}`（**pagination 仅在有下页时填**——末页无该键，B5-P1-2） | — | ReportsTasksPaging.test.tsx（TasksPage 游标前进+project_id/filter.conditions 形状锚定）；TasksPage.test.tsx（B5-P1-2 末页分页行为）；ProjectDetailPage.test.tsx（project_id 过滤守卫）；clientContract.test.ts |

| E-20 | `GET /v1/tasks/:id/snapshot` | query：`logs_after=<log_id>`（增量游标）、`ai_cursor=<int>`（字节游标；均首省）、`node_cursor=<id>:<int>`（**可重复/逗号多组**——graph-ui-channels 批2：图任务逐节点通道游标，节点缺省=0；非图任务零影响） | `TaskSnapshot{task: ScanTask, progress?, logs?: {logs: TaskLogEntry[]}, ai?: {chunk(b64), next_cursor, complete, total_bytes, granularity?, channels?: AILogChannel[]}}`——`AILogChannel{node_id, chunk(b64), next_cursor, complete, total_bytes, granularity?}（b4：granularity=protojson 枚举名 AI_LOG_GRANULARITY_TOKEN|TURN|UNSPECIFIED——到达粒度声明，真源=方言；UNSPECIFIED/缺失=盘回落/旧后端，客户端回落 agent 判别）`（graph-ui-channels 批2：图任务逐节点通道增量，非图任务恒空数组）（聚合单口；**响应须被幂等吸收：客户端按 log_id 去重、AI 游标单调**） | 404=任务不存在/已被清除（内存存储重启语义，专页）；其他=「加载失败（<status>）」+重试 | TaskDetailSnapshot.test.tsx（增量吸收/404/500 三分支） |
| E-21 | `POST /v1/tasks/:id/{start,cancel,retry,pause,resume}` | 空 body `{}`；**pause 可带 query `?agent=<id>`**（TP20-T2 切换 Agent：RUNNING 态回合边界暂停+换手） | `{}`；服务端状态机为转换权威，非法转换 FailedPrecondition → 「操作被拒绝：<msg>」 | 前端按钮可见性=展示镜像（stateMachine.ts ALLOWED_ACTIONS），不预校验放行；切换入口在页面层判态（E-57） | TaskDetailSnapshot.test.tsx「RUNNING 动作…」；stateMachine.test.ts |
| E-56b | `GET /v1/agents`（TP20-T2 切换 Agent 收口） | 无 query（登录可读，非 admin 专属——创建页与切换 Modal 同源） | `{agents: AgentProviderInfo[]}`——`{id, dialect, is_default, tool_events, structured_submit, interaction, session_resume, conclude_semantics}`（capabilities=注册表如实声明，未声明=false） | **失败=调用方降级**：切换入口禁用（Modal 整体禁用+错误说明）/创建页锁定缺省项（仍可提交）——**不回落硬编码清单**（计划 §0 裁决） | AgentSwitchModal.test.tsx；TaskNewPage.test.tsx「TP20-T2」 |
| E-57 | `POST /v1/tasks/:id/agent`（TP20-T2：PAUSED/QUEUED 态切换） | JSON `{agent: <id>}`（路径 task_id 权威，body task_id 不采信） | `ScanTask`（换笔后快照，回显 agent）；同 agent 重设=幂等零动作；RUNNING → 409（走 E-21 pause?agent=）；终态 → 409 | 成功后失效任务快照（专行进 task_logs、AI 时间线 marker 由引擎写） | AgentSwitchModal.test.tsx「PAUSED 态确认…」；TaskDetailPage.test.tsx「切换 Agent 入口」 |
| E-22 | `POST /v1/tasks/:id/report` | 空 JSON `{}` | `{report_id}`；成功后必须失效 `['task-reports', taskId]` | — | TaskDetailPage.test.tsx「重新生成报告…」 |
| E-23 | `GET /v1/tasks/:id/ws` **(WebSocket)** | 升级请求，query：`token=<access>`、`logs_after?`、`ai_cursor?`、`node_cursor=<id>:<int>?`（可重复/逗号多组，批2 重连续订位）；协议随页面 http(s) 切 ws(s) | 帧 = JSON `TaskSnapshot & {type: 'snapshot'}`（`ai.channels` 同构下发含 b4 granularity 元数据；收束=主面 complete 且全通道排干）；在线时服务端 **250ms 聚合推帧**；`type!=='snapshot'` 或缺 task 的帧必须被忽略；终态+AI 收束帧后**客户端主动 close 且不再重连**；**非收束断线立即补拉一次快照回填**（服务端游标已越过 WS pend 内容，116af13；401 断线经单飞刷新自愈 token 竞态），随后 5s 重连循环（终态不重连）；卸载必须 `ws.close()`（泄漏回归） | 连接失败/无 WS 环境 → 回退快照轮询（10s） | TaskDetailPage.test.tsx「卸载关闭」「断线立即补拉」「AI 帧逐帧到达」；TaskDetailSnapshot.test.tsx「WS 帧…」 |
| E-24 | `GET /v1/tasks/:id/source-file` | query `path=<项目相对路径或裸文件名>`（服务端回退解析）；`resolve_only=1`（可选，存在性探测专用：只做根内解析不读内容，三态 200=`exists` / 404+`{error}` 含 "file not found in project"=`missing` / 其余=`unknown`；源根不可解析与网关故障一律 `unknown` 不误判；老网关忽略未知参数照常回全文→200 仍判 `exists`，平滑兼容） | `SourceFileResp{path, content, total_lines, bytes, root_via, resolved_via}` | 4xx/5xx → **客户端把服务端 `{error}` 详情提为 Error.message**（供降级横幅可读；比 axios 通用语更好） | clientContract.test.ts「getSourceFile…」「E-24b probeSourceFile 三态」；codeContext.test.tsx |
| E-48 | `GET /v1/tasks/:id/usage` | 路径参数 | `TaskUsageSummary{task_id, tenant, user, models: TaskModelUsage[], totals}`——models 元素 `{model, calls, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens}`（int64 全字符串，E-00c）；baton usage.recorded 归集（非终态/未配置 tasklog=空聚合诚实降级） | 空聚合渲染"暂无计量数据"非报错 | TaskUsageCard.test.tsx（5 例） |
| E-49 | `GET /v1/tasks/:id/graph` | 路径参数（图执行视图卡）；5s 轮询（graph 状态 web 暴露 §2.1） | `TaskGraph{task_id, nodes[](声明序), plan_triggers[], paused, pause_reason, converged, converge_summary, open_remaining, updated_at}`——节点 `{id,type,intent,declared_by,from,status,attempts,started_at,ended_at,result_ref}`；**非图任务=200 空 nodes**（整卡不渲染，不报错） | 同 E-20 加载失败语义 | TaskGraphCanvas.test.tsx |

### 1.6 发现

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-25 | `GET /v1/findings` | query：`task_id`、`pagination={page_size:100, cursor}` | `{findings: UnifiedFinding[], pagination}`。**禁止发 filter 参数**：服务端 ListFindings 未接线 filter 且网关 DiscardUnknown 静默丢弃 → 发了等于没发 | — | FindingsPage.test.tsx「请求不再携带死 filter 参数」 |
| E-26 | `GET /v1/findings/:id` | 路径参数 | `{finding: UnifiedFinding}`（**有包装**，与 E-14 裸 Project 相区别） | — | FindingDetailPage.test.tsx |
| E-27 | `PUT /v1/findings/:id/verdict` | JSON `{direction: <VERDICT_DIRECTION 枚举>, reasoning: string}` | `{}`；成功后失效 `['findings']` 与 `['finding', id]`（否则列表行停留「未判定」） | 回写失败 → message.error 原文 | FindingsPage.test.tsx；FindingDetailPage.test.tsx |

### 1.7 报告

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-28 | `GET /v1/reports` | query：`task_id?`、`pagination={page_size, cursor}`（迁 **offset 游标**，旧 lastID 非数字=400）、`sort={field: report_id\|task_id\|format\|generated_at, order}`（缺省 generated_at DESC 由受控列头显式携带）、`filter={conditions:[{format EQ}]}`（AND-only） | `{reports: ReportRow[], pagination?: {next_cursor?, has_next?, **total**}（真实分母）}`（ReportRow.format 为 protojson **枚举名字符串** `REPORT_FORMAT_*`，同 E-00c；`REPORT_FORMAT_UNSPECIFIED`=历史未记录。B5-P1-1 实证：旧"数值枚举"记载与网关实现矛盾——transcode protojson 未开 UseEnumNumbers，实发枚举名，数字键控字典恒 miss → 格式列恒"—"/下载恒 .json） | — | ReportsTasksPaging.test.tsx（offset+排序参数+task 过滤）；pages2.test.tsx（格式映射） |
| E-29 | `GET /v1/reports/:id/download` | 路径参数；两种消费形态：①`responseType:'text'` + 关闭 JSON transform②`responseType:'blob'`（下载）。getReportContent 对 Blob 载荷兜底 `.text()`（mock 台/网关变体） | 报告正文：JSON 文本或 HTML 文本（`<` 前缀嗅探分型）；下载文件名 **`<report_id>.<reportFileExt(format)>`** | 下载失败 → message.error；不可下载时引导「重新生成」；HTML 档在线查看维持 openReportWindow 沙箱窗 | clientContract.test.ts「getReportContent…」；dict.test.ts（扩展名映射）；ReportDetailPage.test.tsx（阅读视图分型/降级） |

### 1.8 通知 / 工具 / 对比

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-30 | `GET /v1/notifications` | query `user_id=<当前用户>`（App 角标 60s 轮询 + 通知页同参数；**服务端强制取 JWT 身份、query user_id 被忽略**——engine ⑩ IDOR 修复后参数仅兼容残留） | `{notifications: [{notification_id, user_id, title, body, read, created_at}]}` | — | pages2.test.tsx；AppRouting.test.tsx（角标） |
| E-31 | `POST /v1/notifications/:id/read` | 空 body | `{}`；成功后失效 `['notifications', uid]` **与** `['notify-unread']`（角标即时消失，） | — | pages2.test.tsx |
| E-31a | `POST /v1/notifications/read-all` | 空 body | `{marked: <int>}`（网关组合 List 未读+逐条标记，engine ；user_id 强制 JWT）；成功后失效同 E-31 两者；页面按钮未读=0 时禁用 | — | pages2.test.tsx（read-all） |
| E-32 | `GET /v1/tools` | 无 | `{tools: ToolInfo[]}`——**gateway 手写 JSON**（非 transcode）：`{tool_id, name, supported_languages, output_format, valid, errors}`；`valid=false` 的工具只展示不可选 | — | TaskNewPage.test.tsx（tools 模型） |
| E-33 | `GET /v1/tasks/:id/comparison-report` | 路径参数 | `ComparisonReport{report_id, summary: ComparisonSummary{...七桶计数+metrics 七指标}, venn_data_url(诚实留空 )}` | 加载失败/无 summary → 「对比报告不可用」警告 | views.test.tsx |

### 1.9 推理 Provider / 路由管理

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-34 | `GET /v1/inference/providers` | 无 | `{providers: InferenceProvider[]}`——`{name, type, config:{str:str}}`，**响应永远无 credentials**（服务端按省略脱敏，凭据只在网关加密存储） | 503=推理链不可用（列表空态文案区分错误/空） | InferenceConfigPanel.test.tsx |
| E-35 | `POST /v1/inference/providers` | `{name, type, credentials:{str:str}, config:{str:str}}` | `{name, created}`——upsert 语义（created 由 manager 判存在性） | 400=缺字段；403=非 admin | InferenceConfigPanel.test.tsx「新建」 |
| E-36 | `PUT /v1/inference/providers/:name` | 路径 name **权威**（服务端覆盖 body 同名字段）；body 同 E-35（无 name） | 同 E-35（created:false） | 同 E-35；404=不存在 | InferenceConfigPanel.test.tsx「编辑」 |
| E-37 | `DELETE /v1/inference/providers/:name` | 路径参数 | `{deleted:bool}`（幂等：不存在→false） | 403=非 admin | InferenceConfigPanel.test.tsx「删除保护」 |
| E-38 | `GET /v1/inference/route` | 无 | `{provider, model, version}`——version 为 uint64 的 **protojson 字符串**；未设置路由时 provider/model 空串 | 503=推理链不可用 | InferenceConfigPanel.test.tsx |
| E-39 | `PUT /v1/inference/route` | `{provider, model, no_verify?}`——页面验证开关 ON=`no_verify:false`（**开关语义取反于字段名**，onFinish 显式翻转） | `{provider, model, version, validation_performed, validated_endpoints:[{url,protocol}]}`——网关连通性验证回执，成功 message 直出端点列表 | 验证失败：服务端 `{error}` 详情直出（路由不变更，诚实失败） | InferenceConfigPanel.test.tsx「切路由」 |

### 1.10 编排目录与管理 / 工作区管理

| ID | 端点 | 预期输入 | 预期输出（200） | 错误语义 | 锚点 |
|----|------|----------|----------------|----------|------|
| E-50 | `GET /v1/orchestrations` | query `include_deprecated=true`?（编排管理页含停用）、`include_history=true`?（修订史） | `{orchestrations: OrchestrationInfo[]}`——`{name, latest_revision, spec_yaml(规范化全量), spec_hash, execution, description, deprecated, readonly, updated_at, history?[]}`；路径寻址全用**编排 name**（encodeURIComponent） | 加载失败如实展示 | clientContract.test.ts「编排 CRUD 契约锚定」 |
| E-51 | `POST /v1/orchestrations` | JSON `{name(DNS-1123，与 spec_yaml.name 一致), spec_yaml(≤64KB 全量)}` | OrchestrationInfo（创建即 revision 1） | 400=spec 校验失败（错误带字段路径）；409=同名已存在（变更走 PUT）/内置只读名拒建 | 同上 |
| E-52 | `PUT /v1/orchestrations/:name` | 路径 name 权威；body `{name, spec_yaml}`（append 新修订，不覆盖历史） | OrchestrationInfo（latest_revision+1） | 404=不存在（新建走 POST）；400 同 E-51 | 同上 |
| E-53 | `DELETE /v1/orchestrations/:name` | 路径 name；query `unde_precate=true`=恢复 | OrchestrationInfo（deprecated 翻转） | 404；409=内置只读编排不可停用 | 同上 |
| E-54 | `GET /v1/workspaces` | 无（?limit=&offset= 透传可选） | `{workspaces: WorkspaceInfo[]}`——`{name, phase, labels}` | 403=非 admin | clientContract.test.ts「getWorkspaces…」 |
| E-55 | `POST /v1/workspaces` | JSON `{name, labels?}` | WorkspaceInfo | 409 重名/资源守卫：**`{error}` details 原文透出**（比 axios 文案可行动） | clientContract.test.ts「createWorkspace…」 |
| E-56 | `DELETE /v1/workspaces/:name` | 路径 name（encodeURIComponent） | `{name, deleted}` | 409 含资源守卫原文透出（删前须清空沙箱/服务） | clientContract.test.ts「deleteWorkspace 409 资源守卫原文透出」 |
| E-62 | `GET /v1/auth/registration-policy` | 无（**/v1/auth 链免 JWT**；限流照旧） | `{mode: 'invitation'\|'open'\|'disabled'}`——仅枚举值，无码表无计数（Q10） | 非 2xx=调用方回落 invitation 形态（后端 FailedPrecondition 兜底） | clientContract.test.ts「V2.2 邀请码端点契约」；RegisterPage/LoginPage 档位用例 |
| E-63 | `GET /v1/invites` | `?include_terminal=`（缺省仅有效码）；`?pagination={"cursor":N}`（JSON 风格，） | `{codes: InviteCode[], config_codes?: string[], pagination}`——config 注入码只读随行（Q4/Q13） | 403=非 admin（管理面全路由 admin 门禁） | clientContract.test.ts「listInviteCodes 透传」 |
| E-59 | `POST /v1/invites` | JSON `{max_uses?, expires_at?, never_expires?, note?}`（max_uses 未设=1；expires_at 未设=30 天；never_expires 与 expires_at 互斥 400） | InviteCode（created_by=网关注入 JWT sub，body 不传） | 400=参数互斥/过期时间在过去/负值 | clientContract.test.ts「形状直通」 |
| E-60 | `POST /v1/invites/:id/revoke` | 路径 id | InviteCode（state=revoked；重复吊销幂等回放原码） | 404=不存在；403=非 admin | clientContract.test.ts「形状直通」 |
| E-61 | `DELETE /v1/invites/:id` | 路径 id | `{}` | **412 FailedPrecondition=活跃码须先吊销（Q14 两步走）**；404=不存在 | clientContract.test.ts「形状直通」 |

---

## 2. HTTP 错误 → 前端行为映射（拦截器契约，client.ts）

| ID | 触发 | 预期行为 | 明确禁止 | 锚点 |
|----|------|----------|----------|------|
| E-40 | 401 且 url 含 `/v1/auth/` | 直接 reject（**不刷新、不重放**） | 刷新递归 | clientInterceptors.test.ts「auth 端点 401」 |
| E-41 | 401 其他端点（未重试过） | **单飞刷新**：并发 401 共享一个 refresh Promise → 重放原请求（新 Bearer）；响应 refresh_token 非空则滚动存储 | 并发刷新风暴（每请求各刷一次） | client.test.ts「单飞 refresh」 |
| E-42 | 刷新失败 | **分类处置**：400/401/403/无令牌=凭据失效 `clearSession()` + `location.assign('/login')`；5xx/网络层=保留凭据（不清会话，等恢复） | 死 token 循环 401 / 误清健康凭据 | client.test.ts「refresh 失败」族 |
| E-43 | 429 | 读 `response.data.retry_after`（缺失→15s；**clamp 到 [5,60]s**）记入退避截止时刻；`pollIntervalMs(base)` 在截止前返回剩余毫秒（轮询拉长） | 错误态按固定频率撞击限流器 | clientInterceptors.test.ts「429…」 |
| E-44 | 503 | 自动重试至多 3 次，退避 1s/2s/4s（`_retry503` 计数在原 config 上携带）；**仅幂等 GET**（B5-P2-5 收窄，POST 不重试）；期间任一成功即恢复；**耗尽后** reject 且派发 `API_ERROR_EVENT(503)`（auth 端点豁免） | 首次 503 即报错/无限重试/非幂等重试 | clientInterceptors.test.ts「503…」两例 |
| E-45 | 403 / 501（auth 端点豁免） | 派发 `API_ERROR_EVENT(detail=status)` → ApiErrorOverlay 整页错误组件（可关闭返回） | 吞掉或当 404 处理 | clientInterceptors.test.ts；errors.test.tsx |
| E-46 | 任意成功响应 | 派发 `API_OK_EVENT` → 撤降级横幅（服务恢复即时反馈） | 横幅滞留 | clientInterceptors.test.ts「API_OK…」 |
| E-47 | 查询类 404（快照/任务详情） | **不重试**（react-query retry:false）——NotFound 如实终态 | 404 被重试放大 | TaskDetailSnapshot.test.tsx |

## 3. WebSocket 帧契约（E-23 详述）

```
连接：ws(s)://<同源>/v1/tasks/{task_id}/ws?token=<access>[&logs_after=<log_id>][&ai_cursor=<int>]
帧入：{"type":"snapshot","task":{...},"progress":{...}|null,"logs":{"logs":[...]},"ai":{"chunk":"<b64>","next_cursor":"<int64字符串>","complete":bool,"total_bytes":"<int64字符串>"}}
```

客户端处理规则（TaskDetailPage `absorbSnapshot`——轮询响应与 WS 帧**共用一条吸收路径**）：
1. 日志按 `log_id` 集合去重，新日志追加并推进 `logs_after` 游标（首帧/重连交叠兜底，杜绝重复行）；
2. AI 增量：`next_cursor`（Number 化）**严格大于**当前游标且 chunk 非空才追加（base64 → UTF-8 解码）；
3. `ai.complete/total_bytes` 每帧覆盖；
4. 轮询与 WS 的暂停关系：WS open → 轮询停（refetchInterval false）；非收束 close → **立即补拉一次快照回填**（116af13：断流窗=观测空白）→ 5s 后重连、轮询恢复；
5. 终态收束判定：`isTerminal(task.status) && ai.complete && next_cursor >= total_bytes` → close 且不再重连。
6. 终态复活重续臂：任务状态从终态翻回非终态（FAILED 重试/
   resume）→ `wsEpoch` 递增并重挂 WS 推流——旧"终态收束后不再重连"对新纪元任务失效。
7. 增量吸收幂等：快照在途时 WS 帧已推进游标的交叠块按字节裁掉已交付
   前缀（主面 ai 与通道 node_cursor 同款）——仅比块尾会整块重复 append。

锚点：TaskDetailSnapshot.test.tsx（WS 帧 + 增量吸收共用用例组）。

## 4. 外部环境变量（部署面契约）

| 变量 | 生效位置 | 缺省 | 消费者 |
|------|----------|------|--------|
| `CODEAUDIT_GATEWAY_URL` | vite dev/preview 代理目标 | `http://localhost:8080` | vite.config.ts server.proxy（`/v1`，ws:true） |
| `CODEAUDIT_GATEWAY_UPSTREAM` | 容器 nginx 反代上游 | `host.docker.internal:8090` | nginx/default.conf.template（envsubst 注入） |
| `CODEAUDIT_WEB_PORT` | docker compose 宿主端口 | `8088` | docker-compose.yml |

两个名字**不得混用**（dev 用 URL、容器用 UPSTREAM——README 显式警告）。静态面契约由 guard.sh 锚定字符串。
