# engine 外部接口契约 — 预期输入 / 预期输出

> 事实源：
> `services/gateway-service/cmd/main.go`（路由链）+ `internal/handler/transcode.go`（REST→gRPC 转码）
> + `internal/handler/{upload,sourcefile,taskwatch}.go`（本地端点/WS）+ `internal/middleware/{jwt,ratelimit,logging}.go`。
> 本文档由守门测试 `tests/test_guardrails.py`（路由快照/鉴权链/错误映射）与
> `internal/handler/transcode_test.go`（行为级路由表）共同看护：接口或本文与代码漂移，门禁即红。
> 历史缺陷与锁定测试的映射见根目录 `REGRESSIONS.md`。

## 0. 服务定位与调用链

```
web 控制台 / vscode 插件 / e2e 脚本 ──HTTP/JSON + WebSocket──> gateway(:8080)
                                                              │ REST→gRPC 转码（protojson）
                              ┌────────┬────────┬────────┬───┴────┬────────┬────────┐
                              ▼        ▼        ▼        ▼        ▼        ▼
                          project   task   runtime  sast   result   storage
                          :50052   :50054   :50057    :50051  :50058   :50055
```

gateway 是**唯一外部入口**（纯 HTTP，无 gRPC 服务端；7 服务中唯一不注册 gRPC 的部署单元）。
除转发外，gateway 自身实现四个本地端点：上传直传管道（upload.go）、任务源码全文读取
（sourcefile.go）、任务观测 WS（taskwatch.go）、健康探针（health.go）。

## 1. 通用约定

- **Base URL**：开发态 `http://127.0.0.1:8080`（`ports.gateway`，env `CODEAUDIT_GATEWAY_PORT` 覆盖）；
  模拟栈经主仓库 sim.sh 钉 18080 段；生产 `192.0.2.122:8090`（8080 与 部署主机 宿主 openshell-gateway 冲突，
  经 `CODEAUDIT_HOST_GATEWAY` 钉 8090——engine/deploy/env.template 与 deploy/README §端口一致）。
- **鉴权三链**（`cmd/main.go:74-83`）：
  | 链 | 路径 | 中间件序 |
  |---|---|---|
  | 公共 | `/health` | 无（免 JWT 免限流，compose healthcheck 依赖） |
  | 免认证链 | `/v1/auth/*` | Logging → RateLimit（**无 JWT**——登录是令牌来源） |
  | 保护链 | 其余 `/v1/*` | Logging → JWT → RateLimit → handler（JWT 外置使限流键可读 sub，⑨；Logging 外置=业务面访问日志+WS `?token=` 脱敏生效，R137 第四轮审计修复 17d584eb 丢包） |
- **JWT**（`middleware/jwt.go`）：`Authorization: Bearer <token>`，HS256，强制 HMAC 族签名方法。
  claims：`{sub:user_id, username, role, exp, iat, type}`。**特例**：`Upgrade: websocket`
  请求允许 `?token=<JWT>` 查询参数（浏览器 WS 无法自定义头，），日志层对该参数脱敏
  REDACTED（⑧）。**网关只接受 `type=access` 令牌**（签发面两类恒携带 type，缺省即
  拒绝 fail-closed；R137 第四轮审计——此前 refresh(7d) 可作 Bearer 打全部业务路由且绕过
  R87/R88 撤销语义，`TestJWTMiddleware_RejectsNonAccessToken` 锁定）。**网关本地撤销集**：
  logout 通道把 access 串写入网关内存撤销名单（R88），业务路由命中即 401 "token has been
  revoked"；深层撤销在 project-service——access 黑名单仅 GetCurrentUser 路径生效，R87 会话
  纪元在 **refresh 准入**判定（Logout/改密/重置/停用推进纪元踢出该用户全部旧会话，PG 写穿+
  启动回放）。**Login 拒绝非 ACTIVE 账号**（state≠ACTIVE 专用错误文案，停用即踢不再
  可被重登绕过）。

  `TestTokenTTL_MatchesContract` 锁定，`expires_in_s` 返回 1800）；yaml 键
  `gateway.jwt.access_ttl_min/refresh_ttl_day` 仍为死配置（无代码消费，见 §7 漂移表）。
- **限流**（`middleware/ratelimit.go`）：令牌桶 100 req/min。
  键 = `user:<JWT sub>`（已认证）/ 客户端 IP（未认证）；XFF 仅 `trust_proxy=true` 时取**最右**
  值（防伪造）。超限 429 `{"error":"rate limit exceeded","retry_after":60}` + `Retry-After: 60`。
- **响应 JSON 规则**（protojson `{EmitUnpopulated:true, UseProtoNames:true}`）：**snake_case 键、
  零值字段也输出、enum 输出名字符串、int64 输出为十进制字符串、bytes 输出 base64、
  Timestamp 输出 RFC3339 字符串**。四个本地组装端点（/health、/v1/uploads/archive、
  /v1/tasks/{id}/source-file、/v1/tools）为手写 JSON，不遵循零值全输出规则。
- **请求体**：protojson 解码 `DiscardUnknown:true`（容忍未知字段），空 body 合法；坏 JSON → 400。
  **幂等键由网关生成**（`gw-<24hex>`，`newRequestID`）——REST 客户端不携带 proto metadata；
  解码后注入（protojson.Unmarshal 会重置消息，TP12-T3 回归；decodeBody 内实现）。
- **调用身份贯通**：网关不向 gRPC 转发 metadata；身份只经显式字段传递——
  `CreateScanTask.created_by`（自 JWT sub 注入，）、`ListNotifications.user_id`
  （强制 JWT 身份，⑩）、`ChangePassword.user_id`（强制 self）、`GetCurrentUser.access_token`。

### 错误契约

统一错误体 `{"error":"<message>"}`；gRPC 透传错误 message 前缀 `<CodeName>: `。
映射表（`grpcToHTTP`，transcode.go:112-137；已有行为级锁定测试 `TestGrpcToHTTP_AuthSemantics`）：

| gRPC code | HTTP | 语义 |
|---|---|---|
| InvalidArgument | 400 | 参数校验失败 |
| NotFound | 404 | 实体不存在 |
| AlreadyExists | 409 | 幂等同键异体 |
| FailedPrecondition / Aborted | 409 | 状态机非法转移 |
| PermissionDenied | 403 | 权限不足（admin 门禁为网关本地 403） |
| Unauthenticated | **401** | 登录失败与过期令牌同码（前端静默刷新语义） |
| Unimplemented | 501 | 下游显式未实现 |
| Unavailable | 503 | 下游不可达/连接未配置 |
| DeadlineExceeded | 504 | 调用超时（`gateway.grpc_call_timeout_s`=30s） |
| 其他（Internal 等） | 500 | 兜底 |

网关自身状态码分布：400（body/multipart 非法、超 100MB、坏 query）· 401（JWT 层）·
403（`admin role required` / `admin role or self required` / `resource owner or admin required` / `project write requires admin`）· 404（未知路由/资源不存在/通知非本人）· 405（方法不符）·
409 · 413（source-file >5MiB）· 415（二进制源文件）· 429 · 500 ·
**501（未映射 `/v1/<域>`，诚实降级）** · 502（storage 上传流失败/通知归属核验失败）·
503（后端连接未配置）· 504。

## 2. 路由总表

下表为机器可读契约（守门测试解析本节表格并与代码抽取的路由面比对；新增/删除路由必须同步本表）。

| 方法+路径 | 鉴权 | 转发目标 / 实现 |
|---|---|---|
| GET /health | 公开 | 本地 health.go |
| POST /v1/auth/login | 限流 | UserService/Login |
| POST /v1/auth/register | 限流 | UserService/RegisterUser |
| GET /v1/auth/registration-policy | 公开（限流链） | UserService/GetRegistrationPolicy（V2.2 仅 mode 三值） |
| POST /v1/auth/refresh | 限流 | UserService/RefreshToken |
| POST /v1/auth/logout | 限流 | UserService/Logout |
| POST /v1/uploads/archive | JWT | 本地管道 → StorageService/UploadFile（客户端流） |
| GET /v1/users/me | JWT | UserService/GetCurrentUser（Bearer 填入 access_token） |
| GET /v1/users | JWT+admin | UserService/ListUsers |
| GET /v1/invites | JWT+admin | UserService/ListInviteCodes（V2.2 ；?include_terminal= 缺省仅有效码；config 注入码只读随行） |
| POST /v1/invites | JWT+admin | UserService/CreateInviteCode（max_uses 未设=1/0=不限；expires_at 未设=30 天；never_expires 互斥；created_by=网关注入 JWT sub，body 无需传） |
| POST /v1/invites/{id}/revoke | JWT+admin | UserService/RevokeInviteCode（状态翻转幂等） |
| DELETE /v1/invites/{id} | JWT+admin | UserService/DeleteInviteCode（仅终态码，Q14 两步走护栏） |
| POST /v1/users | JWT+admin | UserService/CreateUser |
| GET /v1/users/{id} | JWT（admin 或 self） | UserService/GetUser |
| PUT /v1/users/{id} | JWT（admin 或 self；非 admin body 钉死路径身份且仅 email 可变） | UserService/UpdateUser |
| GET /v1/users/{id}/permissions | JWT（admin 或 self） | UserService/GetUserPermissions |
| POST /v1/users/{id}/password | JWT（user_id 强制 self） | UserService/ChangePassword |
| POST /v1/users/{id}/password:reset | JWT+admin | UserService/ResetPassword |
| POST /v1/projects | JWT（创建者自动登记成员，） | ProjectService/CreateProject |
| GET /v1/projects | JWT+admin（裸列表，） | ProjectService/ListProjects（?sort={"field":"name\|created_at","order":...}+?filter={"conditions":[{name CONTAINS\|default_scan_mode EQ}]} 服务端排序/筛选，AND 语义白名单外 400） |
| GET /v1/projects/{id} | JWT（成员或 admin） | ProjectService/GetProject |
| PUT /v1/projects/{id} | JWT+admin（写面=管理面） | ProjectService/UpdateProject |
| DELETE /v1/projects/{id} | JWT+admin（写面=管理面） | ProjectService/DeleteProject |
| GET /v1/projects/{id}/config | JWT（成员或 admin） | ProjectService/GetProjectConfig |
| PUT /v1/projects/{id}/config | JWT+admin（写面=管理面） | ProjectService/UpdateProjectConfig |
| POST /v1/tasks | JWT（created_by 注入） | TaskService/CreateScanTask |
| GET /v1/tasks | JWT（裸列表暂无归属过滤，二期） | TaskService/ListScanTasks |
| GET /v1/tasks/{id} | JWT（owner 或 admin） | TaskService/GetScanTask |
| GET /v1/tasks/{id}/progress | JWT（owner 或 admin） | TaskService/GetTaskProgress |
| GET /v1/tasks/{id}/logs | JWT（owner 或 admin） | TaskService/GetTaskLogs |
| GET /v1/tasks/{id}/snapshot | JWT（owner 或 admin） | 本地聚合 4 路（task+progress+logs+ai-log） |
| GET /v1/tasks/{id}/ws | JWT（?token= 特例；owner 或 admin） | 本地 WS（StreamTaskSnapshot/StreamAIInteractionLog 优先，回退轮询） |
| GET /v1/tasks/{id}/ai-log | JWT（owner 或 admin） | RuntimeService/GetAIInteractionLog |
| GET /v1/tasks/{id}/source-file | JWT（owner 或 admin） | 本地源树读取（GetScanTask 校验 + GetProjectConfig 回退③） |
| GET /v1/tasks/{id}/context | JWT（owner 或 admin） | TaskService/GetTaskContext |
| GET /v1/tasks/{id}/metrics | JWT（owner 或 admin） | SASTFusionService/CalculateMetrics |
| GET /v1/tasks/{id}/comparison-report | JWT（owner 或 admin） | SASTFusionService/GenerateComparisonReport |
| POST /v1/tasks/{id}/report | JWT（owner 或 admin；body task_id 钉死路径） | ReportService/GenerateReport（format 缺省 JSON） |
| POST /v1/tasks/{id}/start | JWT（owner 或 admin） | TaskService/StartTask |
| POST /v1/tasks/{id}/cancel | JWT（owner 或 admin） | TaskService/CancelScanTask |
| POST /v1/tasks/{id}/retry | JWT（owner 或 admin） | TaskService/RetryScanTask |
| POST /v1/tasks/{id}/pause | JWT（owner 或 admin） | TaskService/PauseTask（?agent=<id> 可选=暂停同时换笔，TP20-T2 切换 Agent；RUNNING 态切换入口） |
| POST /v1/tasks/{id}/agent | JWT（owner 或 admin，与 pause 同面） | TaskService/SetTaskAgent（body {"agent"}；PAUSED/QUEUED 态切换，TP20-T2） |
| GET /v1/agents | JWT（登录可读，非 admin 专属） | RuntimeService/ListAgents（Agent 清单：id/dialect/is_default+能力五布尔；创建页与切换 Modal 同源，TP20-T2） |
| POST /v1/tasks/{id}/resume | JWT（owner 或 admin） | TaskService/ResumeTask |
| POST /v1/tasks/{id}/complete | JWT（owner 或 admin） | TaskService/CompleteTask |
| GET /v1/findings | JWT（带 task_id=owner 或 admin；裸列表 admin） | ResultService/ListFindings |
| GET /v1/findings/{id} | JWT（finding→task 归属） | ResultService/GetFinding |
| PUT /v1/findings/{id}/verdict | JWT（finding→task 归属） | ResultService/UpdateVerdict |
| POST /v1/findings/verdict:batch | JWT（逐 finding→task 归属，去重查证） | ResultService/BatchUpdateVerdict |
| GET /v1/reports | JWT（带 task_id=owner 或 admin；裸列表 admin） | ReportService/ListReports（契约迁 offset 游标+total；?sort={"field":"report_id\|task_id\|format\|generated_at","order":...}+?filter={"conditions":[{format EQ}]}；旧 lastID 游标 400） |
| GET /v1/reports/{id} | JWT（report→task 归属） | ReportService/GetReport |
| GET /v1/reports/{id}/download | JWT（report→task 归属） | 本地聚合 ReportService/DownloadReport 服务端流 |
| GET /v1/tools | JWT | 本地组装（ListAvailableTools + 逐工具 ValidateToolConfig） |
| GET /v1/notifications | JWT（user_id 强制 JWT） | NotificationService/ListNotifications |
| POST /v1/notifications/{id}/read | JWT（归属核验前置） | NotificationService/MarkNotificationRead |
| POST /v1/notifications/read-all | JWT（List 未读+逐条标记组合，） | NotificationService/ListNotifications + MarkNotificationRead |
| GET /v1/tasks/{id}/usage | JWT（owner 或 admin） | TaskService/GetTaskUsage（计量聚合） |
| GET /v1/tasks/{id}/graph | JWT（owner 或 admin） | TaskService/GetTaskGraph（切片5+6 暴露批 P1a：graph.log 回放折叠投影） |
| POST /v1/tasks/{id}/graph/nodes/{nid}/message | JWT（owner 或 admin） | RuntimeService/InjectGraphNodeMessage（v0.5 人机共编辑：追加 user 消息，回合边界生效；graph.human_message 审计） |
| POST /v1/tasks/{id}/graph/nodes/{nid}/reopen | JWT（owner 或 admin） | RuntimeService/ReopenGraphNode（v0.5 终态节点重开；活跃图拒绝） |
| POST /v1/tasks/{id}/graph/nodes/{nid}/claim | JWT（owner 或 admin） | RuntimeService/ClaimGraphNode（v0.5 认领层：human 占住节点，TTL 租约） |
| DELETE /v1/tasks/{id}/graph/nodes/{nid}/claim | JWT（owner 或 admin） | RuntimeService/ReleaseGraphNode（v0.5：释放 human 认领，调度恢复） |
| GET /v1/orchestrations | JWT | TaskService/ListOrchestrations（负载目录；?include_deprecated/?include_history） |
| POST /v1/orchestrations | admin | TaskService/CreateOrchestration |
| PUT /v1/orchestrations/{name} | admin | TaskService/UpdateOrchestration（append 新修订；路径段=编排名，proto `UpdateOrchestrationRequest.name`） |
| DELETE /v1/orchestrations/{name} | admin | TaskService/DeprecateOrchestration（?unde_precate=true 恢复；路径段=编排名） |
| GET /v1/workspaces | JWT+admin | RuntimeService/ListWorkspaces（?limit=&offset=；切片3b team 上车，Provider 页工作区选择器数据源） |
| POST /v1/workspaces | JWT+admin | RuntimeService/CreateWorkspace（{name,labels?}；重名/资源守卫 409） |
| DELETE /v1/workspaces/{name} | JWT+admin | RuntimeService/DeleteWorkspace（空区才可删；含资源 409 details 原文；路径段=区名） |
| GET /v1/inference/providers | JWT+admin | RuntimeService/ListInferenceProviders（?workspace=，） |
| POST /v1/inference/providers | JWT+admin | RuntimeService/UpsertInferenceProvider（body workspace，） |
| GET /v1/inference/providers/{name} | JWT+admin | RuntimeService/GetInferenceProvider（?workspace=，） |
| PUT /v1/inference/providers/{name} | JWT+admin | RuntimeService/UpsertInferenceProvider（路径名权威；?workspace= 覆盖 body） |
| DELETE /v1/inference/providers/{name} | JWT+admin | RuntimeService/DeleteInferenceProvider（?workspace=，） |
| GET /v1/inference/route | JWT+admin | RuntimeService/GetInferenceRoute（?workspace=，） |
| PUT /v1/inference/route | JWT+admin | RuntimeService/SetInferenceRoute（?workspace= 覆盖 body，） |


`TestTP12T0_ApprovalRoutesRemoved`）。其余任意 `/v1/<域>` → 501（锁定测试
`TestTranscoder_UnknownRoute501`）。

## 3. 端点明细

### 3.1 GET /health

- 输入：无。聚合探针：六后端（project/task/result/runtime/storage/sast-adapter）
  gRPC health **并行**探测（第四轮审计修复锁域收窄——此前互斥锁横跨 RPC 被串行化）。
  全 SERVING → 200 `{"status":"ok","service":"gateway-service","deps":{...}}`；任一非
  SERVING/不可达 → **503** `{"status":"degraded",...,"deps":{"<name>":"ok|not-serving:…|unreachable:…|not-configured"}}`。非 GET → 405。

### 3.2 /v1/auth/*（仅 POST；非 POST 或路径段数≠1 → 405）

- **login**：body `{"username":str 必填, "password":str 必填}`。输出 200：
  `{"access_token":str, "refresh_token":str, "expires_in_s":"1800"}`（int64→字符串；
  账号 state≠ACTIVE（停用/锁定）→ 401 `"user account is not active"`（密码已验证，
  专用文案不算用户名枚举）；
  30min 契约值）。
  错误：用户不存在/密码错 → 401 `Unauthenticated: login failed: ...`。
- **register**：body `{"username","email","password":str, "invite_code":str}`。
  注册策略由 `auth.registration_mode` 配置（invitation 默认/open/disabled）。
  输出 200 = LoginResponse（注册即登录，自注册固定 ROLE_DEVELOPER）。错误：409 用户名冲突；
  邀请码错 → InvalidArgument/PermissionDenied → 400/403。
- **refresh**：body `{"refresh_token":str 必填}`（空 → 400）。输出新令牌对；无效/过期/
  type≠refresh → 401；R87 会话纪元准入——token iat 早于该用户纪元（Logout/改密/重置/停用
  推进）→ 401（旧会话全踢出）。
- **logout**：body `{"access_token":str 必填}`（空 → 400）。输出 `{}`；access 串同步写入
  网关本地撤销集（业务路由即时 401）。

### 3.3 POST /v1/uploads/archive

- 输入：`multipart/form-data`，字段名 `file`；扩展名白名单 `.zip/.tar.gz/.tgz`；上限 100MB
  （MaxBytesReader + 计数双保险）。
- 行为：multipart 流式解析（gateway 零落盘）→ 64KiB 分块直传 storage `UploadFile` 客户端流，
  首块带 `FilePath=uploads/up-<hex><ext>` 与 ContentType；上传 ctx 120s。
- 输出 200：`{"upload_id":str, "file_id":str, "file_path":str, "size_bytes":int}`。
  `file_id` 供任务创建 `config.upload_file_id` 引用。
- 错误：405 非 POST；400 缺 file 字段/白名单外/超限/multipart 坏；502 storage 流失败。

### 3.4 /v1/users

- **GET me**：无参数；输出 User `{"user_id","username","email","state","role","must_change_password","created_at"}`。
- **GET（列表，admin）**：query `pagination`(JSON)、`state`(标量)、`username_contains`(标量)；
  输出 `{"users":[User],"pagination":{"next_cursor","has_next","total"}}`。非 admin → 403。
- **POST（建号，admin）**：body `{"username","email","password","role"}`；输出 User；
  幂等键网关生成，同键异体 409。
- **GET/PUT {id}**：PUT body `{"user":{...}}`（路径 id 回填空 user_id）；输出 User；404。
  **鉴权（R73/R85）**：GET/PUT {id} 与 GET {id}/permissions 一律 admin 或 self；
  PUT 非 admin（自助）通道 body 钉死到路径身份，且仅 email 可变（role/state/username
  由网关强制剥离，服务端对 UNSPECIFIED 的 role/state 与空 username 保全存量）——
  UpdateUser 保全 role/state/must_change_password（proto3 bool 无 presence）。
- **GET {id}/permissions**：输出 `{"user_id","permissions":[str]}`；admin 或 self。
- **POST {id}/password**：body `{"old_password","new_password"}`；**user_id 强制取自 JWT**，
  不收 body 指定（路径 {id} 被忽略）；输出 `{}`。
- **POST {id}/password:reset（admin）**：body 空；输出 `{"temporary_password":str,"must_change_password":true}`
  （一次性临时密码仅此一次返回）。

### 3.5 /v1/projects

- **POST**：body `{"project":{"name","repo_url","default_branch","default_scan_mode",...}}`；
  输出 Project。错误：400/409（同键异体）。
- **GET 列表**：query `pagination`/`filter`（标量自动加引号，JSON 原样透传）；
  输出 `{"projects":[Project],"pagination":{...}}`。
- **GET/PUT/DELETE {id}**：标准 CRUD；DELETE 输出 `{}`。
- **GET/PUT {id}/config**：输出/输入 `{"project_id","config":{str:str}}`（config 键如
  `project_path`/`upload_file_id`/`repo_url`，见 data-flows.md §2）。
- 未知子路由 → 404 `unknown projects route`。

### 3.6 /v1/tasks

- **POST**：body `{"project_id":str 必填, "scan_mode":enum, "sast_tools":[str], "priority":enum,
  "config":{str:str}}`（scan_mode 枚举名如 `SCAN_MODE_PARALLEL`/`SCAN_MODE_SAST_ONLY`/`SCAN_MODE_AI_ONLY`/
  `SCAN_MODE_AI_ENHANCED_SAST`/`SCAN_MODE_COMPARE`）。`created_by` 网关自 JWT 注入。
  **增量扫描（可选）**：`"incremental":bool`（增量意图）+ `"baseline_task_id":str`
  （显式基线：存在/同项目/COMPLETED 三者齐备，否则创建即 400，不静默替换；缺省=服务端按
  "同项目最近 COMPLETED 且源码可达"自动选定）+ `"git_anchor":{"commit","branch","dirty","remote"}`
  （插件采集的版本锚点快照，非 git 工作区可省）+ `"diff_hint":str`（客户端 git diff
  --name-status 原文，仅审计提示不作 diff 依据）。无可用基线/diff 失败时**自动降级全量**，
  任务 `config.incremental_degraded_reason` 如实记因（诚实降级，无静默）。
  **负载 pack（可选）**：config 约定键 `"pack":str`（pack 名，平台方随部署提供
  于 configs/packs/）——创建即钉死修订版：响应 config 带 `pack_revision_id`（"名@修订号"）
  与 `pack_spec_hash`（该修订内容 sha256），任务运行期 pack 更新不影响已创建任务；未知
  pack 创建即 400（不静默回落缺省负载）。不带 pack 键=缺省负载面，行为与回填前一致。
  **授权目标（切片4后半，任务【资源】字段）**：`"authorized_targets":[
  {"host":str,"port":int,"allowed_ips":[str,...]}, ...]`——targeted 档（pack
  spec.network.profile）沙箱的 egress 授权面；创建时强契约校验（host=RFC1123
  主机名、port 1-65535、allowed_ips 每项真实 IP/CIDR，拒载给字段路径）。host 按主机名
  访问（policy DNS 模型，直连真实 IP 恒拒=上游 SSRF 防线）；protocol 恒 tcp（引擎
  注入透明中继激活配方，不接受调用方指定）。快照进 ScanTask，runtime 与
  targeted.yaml 模板组合为 spec.policy 整档注入（单向收紧：endpoints 只来自本字段）。
  输出 ScanTask：`{"task_id","project_id","scan_mode","sast_tools","status","priority","stages":[...],
  "created_at","updated_at","created_by","error_message","retry_count","config",
  "baseline_task_id","changed_files":[str],"deleted_files":[str],"git_anchor":{...},"diff_source"}`
  ——增量快照在 StartTask 的 Prepare 阶段回写（`diff_source="content"`；空 baseline=全量或已降级），
  重试不重算；changed/deleted 为相对剥壳后项目根的规范化路径。
  **task_id = 网关幂等键**（同键重放返回原任务；增量四字段入同键异体指纹）。创建后网关写任务→上传目录链接文件
  `.codeaudit-task-<task_id>`（失败仅日志）。
- **GET 列表**：query `pagination`/`project_id`/`filter`（filter 仅支持 scan_mode/status 字段
  与 EQ/NEQ 算子，其余 400）。输出 `{"tasks":[ScanTask],"pagination":{...}}`；稳定序
  created_at **降序**（newest-first）+ task_id 升序决胜（最新优先；R68 改排序键
  未改方向；锁定测试 `TestListScanTasks_NewestFirst`）。
- **GET {id}**：输出 ScanTask；404。
- **GET {id}/progress**：输出 `{"task_id","status","overall_percent","stages":[...]}`（overall=完成阶段/总阶段）。
- **GET {id}/logs?after_log_id=&limit=**：游标增量；输出
  `{"logs":[{"log_id","task_id","ts_ms","level","source","message"}]}`（ts_ms int64→字符串）。
  after_log_id 必须可解析 int64 否则 400。
- **GET {id}/snapshot?logs_after=&ai_cursor=**：单口聚合（task 必达；progress/logs(Limit=500)/
  ai-log 尽力而为），输出 `{"task":{...},"progress":{...},"logs":{"logs":[...]},"ai":{...}}`。
  注意：当前代码固定 Limit=500，`log_limit` 参数未实现（§7 漂移表）。
- **GET {id}/ai-log?cursor=&max_bytes=**：字节游标增量（cursor≥0）；
  输出 `{"chunk":base64(utf-8 文本),"next_cursor":"<int64>","complete":bool,"total_bytes":"<int64>"}`。
  条目不存在 → `complete:false` 诚实等待（非错误）。
- **GET {id}/source-file?path=**：`path` 必填（项目相对路径或裸文件名）；
  输出 `{"path","content","total_lines","bytes","root_via","resolved_via"}`。
  根解析四流：①`repos_dir/<task_id>`（repo 流）①b `repos_dir/uploads-<task_id>/unpacked`+剥壳
  ②上传链接文件 ③project config project_path ④唯一内容回退（mtime 最新）——`root_via` 如实披露。
  错误：400 缺 path；404 任务不存在/根不可解析/文件未找到；413 >5MiB；415 前 8KB 含 NUL。
  安全：safeJoin 穿越拒绝 + EvalSymlinks 根内校验 + 软链拒绝 + `.codeaudit-task-` 标记文件拒读。
- **GET {id}/context**：输出 `{"task_id","project_config_json","cpg_storage_path","sast_finding_ids"}`；
  任务未完成/未知 → 404。
- **GET {id}/metrics**：输出 ComparisonMetrics（对比视图；precision/recall/F1/duration 等）。
- **GET {id}/comparison-report**：输出 ComparisonReport（四象限 summary + venn_data_url）。
- **POST {id}/report**：body `{"task_id"(可省,路径回填),"template_id","format"}`；
  format 缺省 `REPORT_FORMAT_JSON`（可显式 HTML）；输出 `{"result":{"report_id","report_url","format","file_size_bytes","summary"}}`。
- **POST {id}/{action}**：action ∈ start/cancel/retry/pause/resume/complete/agent；cancel 可带
  `{"reason"}`，retry 可带 `{"scope","stage_ids"}`；输出均为 ScanTask。非法状态转移 → 409
  `FailedPrecondition: invalid state transition: X → Y`；未知 action → 404。
- **切换 Agent（TP20-T2）**：RUNNING → `POST {id}/pause?agent=<id>`（回合边界暂停+换笔：
  task_logs 专行「切换 Agent：from → to（任务已暂停，等待接续）」+AI 时间线 marker
  「── 切换 Agent from → to ──」）；PAUSED/QUEUED → `POST {id}/agent` body `{"agent"}`
  （专行同款，QUEUED 态文案「（排队中，启动时生效）」；不写 AI marker——AI 流静止期）。
  清单=GET /v1/agents（首条 is_default）；同 agent 重设幂等；非法 id → 400。

### 3.7 /v1/findings

- **GET 列表**：query `task_id`/`pagination`/`filter`/`sort`；`filter`/`sort` 为
  **保留参数（当前被 result-service 静默忽略，实现为 backlog——带条件调用得未过滤未排序
  结果，勿依赖；R137 第四轮审计文档校准）**；输出
  `{"findings":[UnifiedFinding],"pagination":{...}}`。UnifiedFinding 核心字段：
  `finding_id/task_id/project_id/source_tool/source_rule_id/source_raw(base64)/location{...}/
  cwe_id/title/description/severity/confidence/evidence{...}/ai_verdict/ai_confidence/ai_reasoning/
  ai_fix_suggestion/diff_patch/matched_findings/is_unique/dedup_group/status/created_at/updated_at/
  inherited_from_task_id`（空=本任务实扫产出；非空=从该基线任务复制的继承项——
  连带 verdict/AI 建议终态，双视图筛选的来源标记）。
  分页默认 20、上限 100（`result.page_size_default/max`）；cursor 为 base64(JSON)，坏 → 400。
- **GET {id}**：输出 `{"finding":UnifiedFinding}`；404。
- **PUT {id}/verdict**：body `{"verdict":enum,"confidence":float,"reasoning":str}`（body
  携带 `finding_id` 被忽略——路径段权威，R137 越权旁路修复）；
  输出 AuditFinding。verdict 枚举名：`AI_VERDICT_TRUE_POSITIVE/FALSE_POSITIVE/LIKELY_TRUE/
  LIKELY_FALSE/UNCERTAIN/NEEDS_MANUAL`。
- **POST verdict:batch**：body `{"finding_ids":[str],"verdict":enum,"confidence":float}`；
  输出 `{"updated_count":int32}`。不存在条目跳过不计入。

### 3.8 /v1/reports

- **GET 列表**：query `task_id`/`pagination`；输出 `{"reports":[{"report_id","task_id","format","url","generated_at"}],"pagination":{...}}`。
- **GET {id}**：输出 Report；404。
- **GET {id}/download**：**二进制流**，`Content-Type: application/octet-stream`、
  `Content-Disposition: attachment; filename="<id>.bin"`（聚合 DownloadReport 64KiB 分块流）。
  流中途断：已发头，静默截断并日志留痕（诚实降级：半途失败不伪装完整）。

### 3.9 GET /v1/tools

- 无参数（非 GET 或带子路径 → 404）。输出本地组装 JSON：
  `{"tools":[{"tool_id","name","supported_languages":[str],"output_format","valid":bool,"errors":[str]}]}`。
  仅列**有执行映射**的工具（当前 bandit/opengrep；解析器≠执行器，诚实口径）。

### 3.10 /v1/notifications

- **GET ?unread_only=true**：**user_id 强制取自 JWT**（query 里的 user_id 被忽略，⑩ IDOR
  修复）；输出 `{"notifications":[{"notification_id","user_id","type","event","title","body","payload",
  "created_at","read"}],"pagination":{...}}`。
- **POST {id}/read**：先 List 本人通知做归属核验（⑩），非本人 → 404
  `notification not found`；核验调用失败 → 502。输出 Notification。
- **POST read-all**：一键全部已读。组合式实现（零 proto 改动，同款）：
  List 本人未读（user_id 强制 JWT，归属天然安全）→ 逐条 MarkNotificationRead；
  单条失败不整批失败，计数如实返回。输出 `{"marked":int}`。

### 3.11 /v1/inference/*（推理 provider/路由管理面，）

全部 **JWT+admin**（requireAdmin；非 admin → 403）。透传链：gateway →
RuntimeService → openshell-manager `/api/v1/inference/*` → OpenShell 网关
（权威存储 gateway.db）。workspace 请求级可选（切片3后半）：per-team 命名空间
配置入口——GET/DELETE 经 `?workspace=` query、PUT/POST 经 body `workspace` 字段（query
非空时覆盖 body，与路径名权威同型）；缺省不传=runtime 用全局配置
`runtime.sandbox.workspace` 注入（任务面经 TaskOrigin.workspace 按 Project.team
贯穿，恒等映射 team 名即 workspace 名）。**credentials 只进不出**：任何响应不回显凭据
（因此前端无法显示"已设置"，以保存成功回执为准；**空 credentials={} 更新=保留已存凭据**

manager api-external 3.19 同批钉契；删键须删除重建 provider）。

**键名约定**：
OpenShell 网关按类型认约定键——类型仅六种 `openai/anthropic/nvidia/deepinfra/
google-vertex-ai/aws-bedrock`（zhipu/deepseek 是厂商不是类型，走 openai/anthropic
兼容协议）：非 anthropic 型共用 config `OPENAI_BASE_URL` + credentials `OPENAI_API_KEY`；
anthropic 型：config `ANTHROPIC_BASE_URL` + credentials `API_KEY`——旧约定键 `BASE_URL`
已被新镜像**无视**（静默回落官方端点、区域 403），与 `DEEPSEEK_*`（deepseek 型除名）
同为死键。别名/死键（`base_url`/`baseurl`/`BASE_URL`/`api_key`/`apikey`/`deepseek_*`/
约定键全小写形态等）会被网关**静默存储但不被识别**，切路由连通性验证时才失败——
runtime-service 入口拒绝并按类型指路约定键名（自定义无关键不受影响）。

**类型×沙箱适配器能力**：
**anthropic 型 provider 可用于 AI 审计**。链路：沙箱 DSH 经 llm-pi-ai 的
anthropic-messages 适配器（随 sdk profile 发布，bridge 按 `DSH_PROVIDER=
anthropic-relay` 在 $DSH_HOME/settings.yaml 激活）打网关 L7 inference.local 的
`/v1/messages`——anthropic 协议**原生直通**（sim 实测 200 流式/非流式），凭据由
网关按路由携带（沙箱 env 只见 openshell-injected 占位符）；上游 ANTHROPIC_BASE_URL 由用户
自填（任意 anthropic 兼容端点，如智谱 `https://open.bigmodel.cn/api/anthropic`，
切路由验证实测 `validated_endpoints=[.../api/anthropic/v1/messages]`）。openai 兼容
族仍走 deepseek 适配器 + L7 `/v1/chat/completions`（网关改写 model 字段）。已知
边界两条：①L7 无兼容路由时 `/v1/messages` 报 400 "no compatible inference route
available"（此前"anthropic 400 拒"的实为该形态）；②ANTHROPIC_BASE_URL 明文 http:// 时网关
验证层回落官方 api.anthropic.com（区域 403）——**anthropic 型 ANTHROPIC_BASE_URL 须 https**。

- **GET /providers**：输出 `{"providers":[{"name","type","config":{str:str}}]}`（无凭据）。
- **POST /providers**：body `{"name":str 必填,"type":str 必填,"credentials":{str:str},
  "config":{str:str}}`；幂等键网关生成。输出 `{"name","created"}`（upsert 语义：
  已存在走 Update，created=false）。
- **GET /providers/{name}**：输出单个 `{"name","type","config"}`；不存在 → 404。
- **PUT /providers/{name}**：同 POST body（**路径名权威**，覆盖 body 同名字段）。
- **DELETE /providers/{name}**：幂等删除；输出 `{"deleted":bool}`（不存在 → false）。
- **GET /route**：输出 `{"provider","model","version"}`（uint64→字符串；未设置路由时
  provider/model 为空串）。
- **PUT /route**：body `{"provider":str 必填,"model":str 必填,"no_verify":bool 默认false}`。
  输出 `{"provider","model","version","validation_performed":bool,
  "validated_endpoints":[{"url","protocol"}]}`——no_verify=false 时网关做连通性验证，
  验证失败按 manager 错误原样透出（400/503 等）。
- **错误映射**：manager 400→400 InvalidArgument、404→404 NotFound、401/403→403、
  不可达/其余 → 503 Unavailable（诚实降级）。

### 3.12 GET /v1/tasks/{id}/usage（任务计量聚合，切片3后半）

JWT owner 或 admin（与 logs/snapshot 同款 canAccessTask 归属核验）。输出
`{"task_id","tenant","user","models":[{"model","calls","input_tokens",
"output_tokens","cache_read_tokens","cache_write_tokens","reasoning_tokens",
"total_tokens"}],"totals":{同 models 元素，model 空}}`。

数据链：baton `usage.recorded`（.tasklog/events.jsonl，TaskOrigin 贯通打的
tenant/user 标）→ 任务终态（COMPLETED/FAILED/TIMEOUT/CANCELLED/DEAD）task-service
异步归集 `task_usage` 表（事务全量重算，幂等）→ 本端点聚合读。**计费口径**：
`total_tokens = input+cache_read+cache_write+output`（reasoning 通常已含于 output，
不重复计入；baton 口径 input=未缓存输入）。内存档（无 PG）任务存在时返回空聚合
（诚实降级）；tasklog 未配置/BATON_BIN 禁用的部署同空聚合。

### 3.13 GET /v1/tasks/{id}/graph（图执行视图投影，切片5+6 暴露批 P1a）

- 权威源=共享卷 `<interaction_dir>/<taskID>.graph.log` 回放折叠（与 resume 同源；
  R116 撕裂容忍同纪律：坏行丢弃+服务端日志）。
- 响应：`{task_id, nodes[]{id,type,intent,declared_by,from,status,attempts,started_at,ended_at,result_ref},
  plan_triggers[]{trigger,detail,at,seq}, paused, pause_reason, converged,
  converge_summary, open_remaining, updated_at}`；节点=声明序（seq 因果序）。
- 非图任务/无事件：`200` + 空 `nodes`（诚实空态，不 404——前端整块不渲染）。
- 消费面：web 任务详情"图执行视图"卡（泳道+计划批次带；设计=主仓库
  docs/designs/platform-graph-web-exposure.md §2.1）。

## 4. WebSocket：GET /v1/tasks/{id}/ws

- **升级**：gorilla/websocket；`CheckOrigin` 恒 true（鉴权已由 JWT 保证）。鉴权 `?token=<JWT>`
  特例（仅 `Upgrade: websocket` 请求头存在时）。
- **请求参数**：`token`、`logs_after`(str 日志游标)、`ai_cursor`(int64≥0 字节游标)——带游标则
  首帧即增量、不重发已见内容。
- **帧协议**：单向服务端推送 JSON 文本帧，与 snapshot 同构：
  ```json
  {"type":"snapshot","task":{...},"progress":{...},"logs":{"logs":[...]},
   "ai":{"chunk":base64,"next_cursor":"<int64>","complete":bool,"total_bytes":"<int64>"}}
  ```
  客户端入站数据帧一律丢弃但续期读限。首帧必推（携带全量）；其后有变化才推帧，**静默期
  每 30s 补发一帧当前快照**（`wsIdleFrameEvery`，A3/R134——客户端 45s 看门狗不误杀；保活
  帧 ai 面零值化、旗面摘除，R136/F2）。
- **双路径**：优先流式订阅（task-service `StreamTaskSnapshot` + runtime
  `StreamAIInteractionLog`，50ms 合并窗口）；流不可用/中途断流未收束 → 回退 250ms 轮询聚合
  （游标不重不漏，回退前冲刷待推增量——②）。
- **保活**：服务端无条件每 20s ping；静默期每 30s 补发快照数据帧（见上）；90s 无任何
  入站帧（含 pong）拆线；单帧写超时 5s。AI 流收束而任务未终态（纪元边界：pause/resume/
  换笔/auto-retry）→ 2s 后重臂订阅续写同任务日志（`aiRearmDelay`，R132）。
- **连接寿命**：6h 硬上限（`wsMaxLifetime`，gw-f6a3523：32.5min 审计撞 30min 旧值与 30min
  access TTL 竞态后上调；仅作泄漏兜底，活性由 ping/pong+读限承担）。到期 close 1000。
- **收束语义**：任务终态（COMPLETED/CANCELLED/TIMEOUT/DEAD）**且** AI 日志收束
  （complete=true，或"永不会有 AI 日志"推断：纯 SAST 模式/终态后 3s 宽限仍零字节）→
  推最终帧后 close 1000 `"task settled"`。确定性错误（NotFound/InvalidArgument/PermissionDenied）
  → close 1011；瞬时错误容忍连续 8 拍（≈2s）后 1011 `"watch unstable"`（③）。
  前端断线自动回退 snapshot 轮询（3s）。

## 5. 配置面（gateway 消费的全部键）

| 配置键 | env 覆盖 | 现值 | 用途 |
|---|---|---|---|
| `ports.gateway` | `CODEAUDIT_GATEWAY_PORT` | 8080 | 监听端口 |
| `gateway.jwt_secret_env` | — | `CODEAUDIT_JWT_SECRET` | 密钥环境变量名（值只入 env，；空=拒启） |
| `addresses.project` | `CODEAUDIT_PROJECT_SERVICE_ADDR` | localhost:50052 | |
| `addresses.task` | `CODEAUDIT_TASK_SERVICE_ADDR` | localhost:50054 | |
| `addresses.result` | `CODEAUDIT_RESULT_SERVICE_ADDR` | localhost:50058 | |
| `addresses.storage` | `CODEAUDIT_STORAGE_SERVICE_ADDR` | localhost:50055 | |
| `addresses.sast_adapter` | `CODEAUDIT_SAST_ADAPTER_ADDR` | localhost:50051 | |
| `addresses.runtime` | `CODEAUDIT_RUNTIME_ADDR` | localhost:50057 | |
| `gateway.trust_proxy` | `CODEAUDIT_TRUST_PROXY` | false | 信任 XFF（仅可信代理后开启） |
| `gateway.rate_limit_per_min` | — | 100 | 限流 |
| `gateway.grpc_call_timeout_s` | — | 30 | 南向调用上界 |
| `gateway.shutdown_grace_s` | — | 15 | 优雅停机排空窗口 |
| `gateway.uploads_dir` | — | data/uploads | source-file 遗留读路径 |
| `gateway.repos_dir` | — | data/repos | source-file repo 流根（与 task.repos_dir 同值） |
| `gateway.jwt.access_ttl_min` / `refresh_ttl_day` | — | 30 / 7 | **死配置**（无代码消费，§7） |

容器内 compose 用服务名覆盖 `addresses.*`；`agent_repos` 共享卷挂 `/data/repos`（与
task-service 同卷——source-file 读的就是 task 解包/clone 的树）。

## 6. 消费者与所用端点

| 消费者 | 所用 |
|---|---|
| web 控制台（codeaudit/web 仓） | /v1/* 大面但**不含** context/metrics/progress/logs/complete/ai-log |
| vscode 插件（codeaudit/vscode-plugin 仓） | login/refresh/logout、projects、tools、tasks（创建含增量四字段/列表/动作 start·cancel·pause·resume/snapshot）+WS 游标续订、findings、uploads/archive（plugin docs/external-interfaces.md §1-2 全集；均为 web 消费集子集，无独占端点） |
| e2e（tests/e2e/，真实栈） | login→projects→uploads/archive→tasks→…全链 |
| compose healthcheck | GET /health |
| dsh-pentest-sse 沙箱内 bridge（间接） | 经 gateway_dial_addr Host 头路由（见 data-flows.md §3） |

**无消费者端点诚实标注**：`GET {id}/progress`、
`GET {id}/context`、`POST {id}/complete`、`GET {id}/logs` 四端点（§3.6）**主仓库内无消费者**，
属对外 API 预留/运维调试面。保留依据=实现真实：task-service `progressOf`/`logsAfterLocked`
与 StreamTaskSnapshot 快照流共享同一内核，非死代码桩；快照/WS 已覆盖产品消费。
ai-log 亦无产品前端消费（人工运维 curl，manual-test-guide 在案；web 经 snapshot/ws 内嵌

图执行视图卡），不再属无消费者清单。

## 7. 已知文档/配置与代码漂移（以代码为准；修复须人类裁决）

| # | 漂移 | 事实（代码） | 影响 |
|---|---|---|---|
| D1 | `configs/codeaudit.yaml:48-50` `gateway.jwt.access_ttl_min(30)/refresh_ttl_day(7)

| D3 | `03_接口规范.md` §1.1 宣称 gateway "gRPC 直通"与 ValidatePermission 转发 | 网关无 gRPC 服务端、无 ValidatePermission 调用；实际暴露面远超该表 | 设计文档滞后，本文为准 |
| D4 | transcode.go:550 注释宣称 snapshot 支持 `log_limit` | 代码固定 `Limit:500` 未读该参数 | 注释失真；参数未实现 |

## 8. 本文档的看护机制

- `tests/test_guardrails.py`：路由表 ↔ transcode.go 字面量双向比对；鉴权链口径；§7 漂移表存在性。
- `services/gateway-service/internal/handler/transcode_test.go`：行为级路由表（真 HTTP 请求 →
  非 404 即路由可达）、501/405/审批废除锁定。
- 历史 bug 的行为锁定测试见 `REGRESSIONS.md`（变异自检证明其有效）。
