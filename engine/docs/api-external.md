# engine 外部接口契约 — 预期输入 / 预期输出

> 事实源（以代码为准，逐项核对于 2026-09-07，基线 commit `5bc736fd`）：
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
                          project   task   dsh-runtime  sast   result   storage
                          :50052   :50054   :50057    :50051  :50058   :50055
```

gateway 是**唯一外部入口**（纯 HTTP，无 gRPC 服务端；7 服务中唯一不注册 gRPC 的部署单元）。
除转发外，gateway 自身实现四个本地端点：上传直传管道（upload.go）、任务源码全文读取
（sourcefile.go）、任务观测 WS（taskwatch.go）、健康探针（health.go）。

## 1. 通用约定

- **Base URL**：开发态 `http://127.0.0.1:8080`（`ports.gateway`，env `CODEAUDIT_GATEWAY_PORT` 覆盖）；
  模拟栈经伞仓 sim.sh 钉 18080 段；生产 `gateway.internal:8080`。
- **鉴权三链**（`cmd/main.go:74-83`）：
  | 链 | 路径 | 中间件序 |
  |---|---|---|
  | 公共 | `/health` | 无（免 JWT 免限流，compose healthcheck 依赖） |
  | 免认证链 | `/v1/auth/*` | Logging → RateLimit（**无 JWT**——登录是令牌来源） |
  | 保护链 | 其余 `/v1/*` | JWT → RateLimit → handler（JWT 外置使限流键可读 sub，ADR-212⑨） |
- **JWT**（`middleware/jwt.go`）：`Authorization: Bearer <token>`，HS256，强制 HMAC 族签名方法。
  claims：`{sub:user_id, username, role, exp, iat, type}`。**特例**：`Upgrade: websocket`
  请求允许 `?token=<JWT>` 查询参数（浏览器 WS 无法自定义头，ADR-172），日志层对该参数脱敏
  REDACTED（ADR-212⑧）。网关**不校验 type claim、不查吊销名单**（吊销仅在 project-service
  内存黑名单，且只在 GetCurrentUser 路径生效）。
  **TTL 现值**：access 1h / refresh 24h（`project-service/internal/service/user.go:19-21` 硬编码，
  `expires_in_s` 返回 3600）——与设计文档 03 §4（30min/7d）及 yaml 死键
  `gateway.jwt.access_ttl_min/refresh_ttl_day` **不一致**，见 §7 漂移表。
- **限流**（`middleware/ratelimit.go`）：令牌桶 50 req/min（`gateway.rate_limit_per_min`）。
  键 = `user:<JWT sub>`（已认证）/ 客户端 IP（未认证）；XFF 仅 `trust_proxy=true` 时取**最右**
  值（ADR-212 防伪造）。超限 429 `{"error":"rate limit exceeded","retry_after":60}` + `Retry-After: 60`。
- **响应 JSON 规则**（protojson `{EmitUnpopulated:true, UseProtoNames:true}`）：**snake_case 键、
  零值字段也输出、enum 输出名字符串、int64 输出为十进制字符串、bytes 输出 base64、
  Timestamp 输出 RFC3339 字符串**。四个本地组装端点（/health、/v1/uploads/archive、
  /v1/tasks/{id}/source-file、/v1/tools）为手写 JSON，不遵循零值全输出规则。
- **请求体**：protojson 解码 `DiscardUnknown:true`（容忍未知字段），空 body 合法；坏 JSON → 400。
  **幂等键由网关生成**（`gw-<24hex>`，`newRequestID`）——REST 客户端不携带 proto metadata；
  解码后注入（protojson.Unmarshal 会重置消息，TP12-T3 回归，transcode.go:304）。
- **调用身份贯通**：网关不向 gRPC 转发 metadata；身份只经显式字段传递——
  `CreateScanTask.created_by`（自 JWT sub 注入，ADR-199）、`ListNotifications.user_id`
  （强制 JWT 身份，ADR-212⑩）、`ChangePassword.user_id`（强制 self）、`GetCurrentUser.access_token`。

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

网关自身状态码分布：400（body/multipart 非法、超 25MB、坏 query）· 401（JWT 层）·
403（`admin role required`）· 404（未知路由/资源不存在/通知非本人）· 405（方法不符）·
409 · 413（source-file >2MiB）· 415（二进制源文件）· 429 · 500 ·
**501（未映射 `/v1/<域>`，诚实降级）** · 502（storage 上传流失败/通知归属核验失败）·
503（后端连接未配置）· 504。

## 2. 路由总表

下表为机器可读契约（守门测试解析本节表格并与代码抽取的路由面比对；新增/删除路由必须同步本表）。

| 方法+路径 | 鉴权 | 转发目标 / 实现 |
|---|---|---|
| GET /health | 公开 | 本地 health.go |
| POST /v1/auth/login | 限流 | UserService/Login |
| POST /v1/auth/register | 限流 | UserService/RegisterUser |
| POST /v1/auth/refresh | 限流 | UserService/RefreshToken |
| POST /v1/auth/logout | 限流 | UserService/Logout |
| POST /v1/uploads/archive | JWT | 本地管道 → StorageService/UploadFile（客户端流） |
| GET /v1/users/me | JWT | UserService/GetCurrentUser（Bearer 填入 access_token） |
| GET /v1/users | JWT+admin | UserService/ListUsers |
| POST /v1/users | JWT+admin | UserService/CreateUser |
| GET /v1/users/{id} | JWT | UserService/GetUser |
| PUT /v1/users/{id} | JWT | UserService/UpdateUser |
| GET /v1/users/{id}/permissions | JWT | UserService/GetUserPermissions |
| POST /v1/users/{id}/password | JWT（user_id 强制 self） | UserService/ChangePassword |
| POST /v1/users/{id}/password:reset | JWT+admin | UserService/ResetPassword |
| POST /v1/projects | JWT | ProjectService/CreateProject |
| GET /v1/projects | JWT | ProjectService/ListProjects |
| GET /v1/projects/{id} | JWT | ProjectService/GetProject |
| PUT /v1/projects/{id} | JWT | ProjectService/UpdateProject |
| DELETE /v1/projects/{id} | JWT | ProjectService/DeleteProject |
| GET /v1/projects/{id}/config | JWT | ProjectService/GetProjectConfig |
| PUT /v1/projects/{id}/config | JWT | ProjectService/UpdateProjectConfig |
| POST /v1/tasks | JWT（created_by 注入） | TaskService/CreateScanTask |
| GET /v1/tasks | JWT | TaskService/ListScanTasks |
| GET /v1/tasks/{id} | JWT | TaskService/GetScanTask |
| GET /v1/tasks/{id}/progress | JWT | TaskService/GetTaskProgress |
| GET /v1/tasks/{id}/logs | JWT | TaskService/GetTaskLogs |
| GET /v1/tasks/{id}/snapshot | JWT | 本地聚合 4 路（task+progress+logs+ai-log） |
| GET /v1/tasks/{id}/ws | JWT（?token= 特例） | 本地 WS（StreamTaskSnapshot/StreamAIInteractionLog 优先，回退轮询） |
| GET /v1/tasks/{id}/ai-log | JWT | DSHRuntimeService/GetAIInteractionLog |
| GET /v1/tasks/{id}/source-file | JWT | 本地源树读取（GetScanTask 校验 + GetProjectConfig 回退③） |
| GET /v1/tasks/{id}/context | JWT | TaskService/GetTaskContext |
| GET /v1/tasks/{id}/metrics | JWT | SASTFusionService/CalculateMetrics |
| GET /v1/tasks/{id}/comparison-report | JWT | SASTFusionService/GenerateComparisonReport |
| POST /v1/tasks/{id}/report | JWT | ReportService/GenerateReport（format 缺省 JSON） |
| POST /v1/tasks/{id}/start | JWT | TaskService/StartTask |
| POST /v1/tasks/{id}/cancel | JWT | TaskService/CancelScanTask |
| POST /v1/tasks/{id}/retry | JWT | TaskService/RetryScanTask |
| POST /v1/tasks/{id}/pause | JWT | TaskService/PauseTask |
| POST /v1/tasks/{id}/resume | JWT | TaskService/ResumeTask |
| POST /v1/tasks/{id}/complete | JWT | TaskService/CompleteTask |
| GET /v1/findings | JWT | ResultService/ListFindings |
| GET /v1/findings/{id} | JWT | ResultService/GetFinding |
| PUT /v1/findings/{id}/verdict | JWT | ResultService/UpdateVerdict |
| POST /v1/findings/verdict:batch | JWT | ResultService/BatchUpdateVerdict |
| GET /v1/reports | JWT | ReportService/ListReports |
| GET /v1/reports/{id} | JWT | ReportService/GetReport |
| GET /v1/reports/{id}/download | JWT | 本地聚合 ReportService/DownloadReport 服务端流 |
| GET /v1/tools | JWT | 本地组装（ListAvailableTools + 逐工具 ValidateToolConfig） |
| GET /v1/notifications | JWT（user_id 强制 JWT） | NotificationService/ListNotifications |
| POST /v1/notifications/{id}/read | JWT（归属核验前置） | NotificationService/MarkNotificationRead |
| GET /v1/inference/providers | JWT+admin | DSHRuntimeService/ListInferenceProviders |
| POST /v1/inference/providers | JWT+admin | DSHRuntimeService/UpsertInferenceProvider |
| GET /v1/inference/providers/{id} | JWT+admin | DSHRuntimeService/GetInferenceProvider |
| PUT /v1/inference/providers/{id} | JWT+admin | DSHRuntimeService/UpsertInferenceProvider（路径名权威） |
| DELETE /v1/inference/providers/{id} | JWT+admin | DSHRuntimeService/DeleteInferenceProvider |
| GET /v1/inference/route | JWT+admin | DSHRuntimeService/GetInferenceRoute |
| PUT /v1/inference/route | JWT+admin | DSHRuntimeService/SetInferenceRoute |

任务动作 `submit/approve/reject` 已废除（2026-09-01 人类裁定；锁定测试
`TestTP12T0_ApprovalRoutesRemoved`）。其余任意 `/v1/<域>` → 501（锁定测试
`TestTranscoder_UnknownRoute501`）。

## 3. 端点明细

### 3.1 GET /health

- 输入：无。输出 200：`{"status":"ok","service":"gateway-service"}`（常量）。非 GET → 405。

### 3.2 /v1/auth/*（仅 POST；非 POST 或路径段数≠1 → 405）

- **login**：body `{"username":str 必填, "password":str 必填}`。输出 200：
  `{"access_token":str, "refresh_token":str, "expires_in_s":"3600"}`（int64→字符串）。
  错误：用户不存在/密码错 → 401 `Unauthenticated: login failed: ...`。
- **register**：body `{"username","email","password":str, "invite_code":str}`。
  注册策略由 `auth.registration_mode` 配置（invitation 默认/open/disabled）。
  输出 200 = LoginResponse（注册即登录，自注册固定 ROLE_DEVELOPER）。错误：409 用户名冲突；
  邀请码错 → InvalidArgument/PermissionDenied → 400/403。
- **refresh**：body `{"refresh_token":str 必填}`（空 → 400）。输出新令牌对；无效/过期/type≠refresh → 401。
- **logout**：body `{"access_token":str 必填}`（空 → 400）。输出 `{}`。

### 3.3 POST /v1/uploads/archive

- 输入：`multipart/form-data`，字段名 `file`；扩展名白名单 `.zip/.tar.gz/.tgz`；上限 25MB
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
  UpdateUser 保全 role/must_change_password（proto3 bool 无 presence）。
- **GET {id}/permissions**：输出 `{"user_id","permissions":[str]}`。
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
  `SCAN_MODE_AI_ENHANCED_SAST`/`SCAN_MODE_COMPARE`）。`created_by` 网关自 JWT 注入（ADR-199）。
  输出 ScanTask：`{"task_id","project_id","scan_mode","sast_tools","status","priority","stages":[...],
  "created_at","updated_at","created_by","error_message","retry_count","config"}`。
  **task_id = 网关幂等键**（同键重放返回原任务）。创建后网关写任务→上传目录链接文件
  `.codeaudit-task-<task_id>`（失败仅日志）。
- **GET 列表**：query `pagination`/`project_id`/`filter`（filter 仅支持 scan_mode/status 字段
  与 EQ/NEQ 算子，其余 400）。输出 `{"tasks":[ScanTask],"pagination":{...}}`；稳定序 created_at 升序。
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
  错误：400 缺 path；404 任务不存在/根不可解析/文件未找到；413 >2MiB；415 前 8KB 含 NUL。
  安全：safeJoin 穿越拒绝 + EvalSymlinks 根内校验 + 软链拒绝 + `.codeaudit-task-` 标记文件拒读。
- **GET {id}/context**：输出 `{"task_id","project_config_json","cpg_storage_path","sast_finding_ids"}`；
  任务未完成/未知 → 404。
- **GET {id}/metrics**：输出 ComparisonMetrics（对比视图；precision/recall/F1/duration 等）。
- **GET {id}/comparison-report**：输出 ComparisonReport（四象限 summary + venn_data_url）。
- **POST {id}/report**：body `{"task_id"(可省,路径回填),"template_id","format"}`；
  format 缺省 `REPORT_FORMAT_JSON`（可显式 HTML）；输出 `{"result":{"report_id","report_url","format","file_size_bytes","summary"}}`。
- **POST {id}/{action}**：action ∈ start/cancel/retry/pause/resume/complete；cancel 可带
  `{"reason"}`，retry 可带 `{"scope","stage_ids"}`；输出均为 ScanTask。非法状态转移 → 409
  `FailedPrecondition: invalid state transition: X → Y`；未知 action → 404。

### 3.7 /v1/findings

- **GET 列表**：query `task_id`/`pagination`/`filter`/`sort`；输出
  `{"findings":[UnifiedFinding],"pagination":{...}}`。UnifiedFinding 核心字段：
  `finding_id/task_id/project_id/source_tool/source_rule_id/source_raw(base64)/location{...}/
  cwe_id/title/description/severity/confidence/evidence{...}/ai_verdict/ai_confidence/ai_reasoning/
  ai_fix_suggestion/diff_patch/matched_findings/is_unique/dedup_group/status/created_at/updated_at`。
  分页默认 20、上限 100（`result.page_size_default/max`）；cursor 为 base64(JSON)，坏 → 400。
- **GET {id}**：输出 `{"finding":UnifiedFinding}`；404。
- **PUT {id}/verdict**：body `{"verdict":enum,"confidence":float,"reasoning":str}`；
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
  仅列**有执行映射**的工具（当前 bandit/opengrep；解析器≠执行器，ADR-133 诚实口径）。

### 3.10 /v1/notifications

- **GET ?unread_only=true**：**user_id 强制取自 JWT**（query 里的 user_id 被忽略，ADR-212⑩ IDOR
  修复）；输出 `{"notifications":[{"notification_id","user_id","type","event","title","body","payload",
  "created_at","read"}],"pagination":{...}}`。
- **POST {id}/read**：先 List 本人通知做归属核验（ADR-212⑩），非本人 → 404
  `notification not found`；核验调用失败 → 502。输出 Notification。

### 3.11 /v1/inference/*（推理 provider/路由管理面，ADR-217）

全部 **JWT+admin**（requireAdmin；非 admin → 403）。透传链：gateway →
DSHRuntimeService → openshell-manager `/api/v1/inference/*` → OpenShell 网关
（权威存储 gateway.db）。workspace 不对外暴露（dsh-runtime 从全局配置
`dsh_runtime.sandbox.workspace` 注入）。**credentials 只进不出**：任何响应不回显凭据。

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
  客户端入站数据帧一律丢弃但续期读限。有变化才推帧；首帧必推（携带全量）。
- **双路径**：优先流式订阅（task-service `StreamTaskSnapshot` + dsh-runtime
  `StreamAIInteractionLog`，50ms 合并窗口）；流不可用/中途断流未收束 → 回退 250ms 轮询聚合
  （游标不重不漏，回退前冲刷待推增量——ADR-213②）。
- **保活**：服务端无条件每 20s ping；90s 无任何入站帧（含 pong）拆线；单帧写超时 5s。
- **连接寿命**：6h 硬上限（`wsMaxLifetime`，gw-f6a3523：32.5min 审计撞 30min 旧值与 30min
  access TTL 竞态后上调；仅作泄漏兜底，活性由 ping/pong+读限承担）。到期 close 1000。
- **收束语义**：任务终态（COMPLETED/CANCELLED/TIMEOUT/DEAD）**且** AI 日志收束
  （complete=true，或"永不会有 AI 日志"推断：纯 SAST 模式/终态后 3s 宽限仍零字节）→
  推最终帧后 close 1000 `"task settled"`。确定性错误（NotFound/InvalidArgument/PermissionDenied）
  → close 1011；瞬时错误容忍连续 8 拍（≈2s）后 1011 `"watch unstable"`（ADR-213③）。
  前端断线自动回退 snapshot 轮询（3s）。

## 5. 配置面（gateway 消费的全部键）

| 配置键 | env 覆盖 | 现值 | 用途 |
|---|---|---|---|
| `ports.gateway` | `CODEAUDIT_GATEWAY_PORT` | 8080 | 监听端口 |
| `gateway.jwt_secret_env` | — | `CODEAUDIT_JWT_SECRET` | 密钥环境变量名（值只入 env，ADR-115；空=拒启） |
| `addresses.project` | `CODEAUDIT_PROJECT_SERVICE_ADDR` | localhost:50052 | |
| `addresses.task` | `CODEAUDIT_TASK_SERVICE_ADDR` | localhost:50054 | |
| `addresses.result` | `CODEAUDIT_RESULT_SERVICE_ADDR` | localhost:50058 | |
| `addresses.storage` | `CODEAUDIT_STORAGE_SERVICE_ADDR` | localhost:50055 | |
| `addresses.sast_adapter` | `CODEAUDIT_SAST_ADAPTER_ADDR` | localhost:50051 | |
| `addresses.dsh_runtime` | `CODEAUDIT_DSH_RUNTIME_ADDR` | localhost:50057 | |
| `gateway.trust_proxy` | `CODEAUDIT_TRUST_PROXY` | false | 信任 XFF（仅可信代理后开启） |
| `gateway.rate_limit_per_min` | — | 50 | 限流 |
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
| web 控制台（codeaudit/web 仓） | 全部 /v1/*（login/refresh、projects、tasks+ws/snapshot/source-file、findings+verdict、reports+download、tools、notifications、users 管理端） |
| vscode 插件（codeaudit/vscode-plugin 仓） | login、tasks、findings、ai-log |
| e2e（tests/e2e/，真实栈） | login→projects→uploads/archive→tasks→…全链 |
| compose healthcheck | GET /health |
| dsh-pentest-sse 沙箱内 bridge（间接） | 经 gateway_dial_addr Host 头路由（见 data-flows.md §3） |

## 7. 已知文档/配置与代码漂移（以代码为准；修复须人类裁决）

| # | 漂移 | 事实（代码） | 影响 |
|---|---|---|---|
| D1 | `configs/codeaudit.yaml:48-50` `gateway.jwt.access_ttl_min(30)/refresh_ttl_day(7)` 无任何代码消费 | TTL 硬编码 access 1h / refresh 24h（user.go:19-21，注释引 03 §4 但值不符） | 死配置误导运维；03 §4（30min/7d）与实现冲突属设计粒度分歧，未裁决前以代码为准 |
| D2 | `services/gateway-service/README.md` / `IMPLEMENTATION_SUMMARY.md` / `BUILD_INSTRUCTIONS.md` 路由表/端口/中间件序/TTL 多处过时（宣称 /v1/results、/v1/storage 域、task PUT/DELETE、50053/50054 端口、per-IP 限流） | 以本文 §2 为准 | 三份历史文档不再维护路由事实；新文档即 SSOT |
| D3 | `03_接口规范.md` §1.1 宣称 gateway "gRPC 直通"与 ValidatePermission 转发 | 网关无 gRPC 服务端、无 ValidatePermission 调用；实际暴露面远超该表 | 设计文档滞后，本文为准 |
| D4 | transcode.go:550 注释宣称 snapshot 支持 `log_limit` | 代码固定 `Limit:500` 未读该参数 | 注释失真；参数未实现 |

## 8. 本文档的看护机制

- `tests/test_guardrails.py`：路由表 ↔ transcode.go 字面量双向比对；鉴权链口径；§7 漂移表存在性。
- `services/gateway-service/internal/handler/transcode_test.go`：行为级路由表（真 HTTP 请求 →
  非 404 即路由可达）、501/405/审批废除锁定。
- 历史 bug 的行为锁定测试见 `REGRESSIONS.md`（变异自检证明其有效）。
