# engine 内部接口契约 — 服务间 gRPC 预期输入 / 预期输出

> 事实源：
> `codeaudit_common.proto`（★契约 SSOT，11 个 service；**RPC 计数不在本文写死**——总数以 proto 当次输出为准，

> §0 表各 service 数值均以 proto 声明当次输出为准；守卫面 `test_proto_rpc_counts` 仅锁
> service 集合与总数，§0 分项数值漂移不在守卫内、以 proto 为准）+ 各服务
> `cmd/main.go`（注册）与 `internal/*`（实现）。本文档由守门测试 `tests/test_guardrails.py`
> 看护（proto 声明 ↔ 实现函数 ↔ 文档三向比对；显式 Unimplemented 集合锁定）。
> 字段级结构以 proto 为唯一权威（R1），本文只锁**语义契约**：必填校验、幂等三态、
> 错误码、状态机、降级口径、真实调用关系。

## 0. 注册矩阵：11 proto service → 7 部署服务

| proto service（声明行） | 部署于 | 监听端口（配置键） | RPC 数 | 显式 Unimplemented |
|---|---|---|---|---|
| TaskService (L811) | task-service | 50054 `ports.task` | 26 | WatchTaskProgress |
| ProjectService (L848) | project-service | 50052 `ports.project` | 11 | — |
| UserService (L867) | project-service | 同上 | 18 | V2.2 () +5 邀请码管理/策略读 |
| ResultService (L887) | result-service | 50058 `ports.result` | 14 | — |
| ReportService (L909) | result-service | 同上 | 6 | — |
| RuntimeService (L923) | runtime-service | 50057 `ports.runtime` | 27 | WatchAnalysisProgress |
| CodeAnalysisService (L948) | runtime-service | 同上 | 5 | GetCallGraph, GetDataFlow, GetAnalysisProgress |
| SASTAdapterService (L972) | sast-adapter-service | 50051 `ports.sast_adapter` | 6 | — |
| SASTFusionService (L986) | sast-adapter-service | 同上 | 9 | — |
| StorageService (L1005) | storage-service | 50055 `ports.storage` | 7 | — |
| NotificationService (L1019) | storage-service | 同上 | 4 | — |

- gateway-service 不注册任何 gRPC 服务端（纯 HTTP）。
- 全部实现体嵌入 `UnimplementedXXXServer`；上表"显式 Unimplemented"= 方法体存在但返回
  `codes.Unimplemented`（诚实降级）。**调用方不得依赖这 5 个 RPC**；proto 声明保留供未来。
- 各服务出站拨号一律 `insecure` 明文（无 TLS/mTLS 路径，安全边界= compose 内部隔离 + 网关
  JWT；正式部署 mTLS 属设计目标未实现）。
- 所有 gRPC server 挂 `libs/common-go/grpcrecover` 拦截器：panic → `codes.Internal
  "internal panic"`，杀请求不杀进程（②）。
- 健康服务：七个 gRPC 服务全部注册 grpc 标准健康服务（/TP19-T1 起 result/sast-adapter
  补齐——result 按服务名+全局双口径置 SERVING；网关 /health 聚合并行探测，见 api-external §3.1）。

## 1. 真实调用关系（边清单）

```
            →project  →task   →result →sast-adapter →runtime →storage
gateway        ●        ●       ●         ●              ●           ●
task           ●        —       ●         ●              ●           ●
runtime    —        ●       ●         —              —           —
sast-adapter   —        —       ●         —              —           —
result         —        —       —         —              —           ●
storage/project 纯被调方（无出站 gRPC）
```

关键边（调用方 file:line → 被调 RPC）：

- **gateway**（transcode.go 转码，清单见 docs/api-external.md §2）
- **task-service**（编排器 orchestrator.go + 服务层）：
  → `CodeAnalysisService.AnalyzeCode`(orch:507)、`SASTAdapterService.RunMultipleScans`(:539)、
  `RuntimeService.RunAIAnalysis`(:587)/`VerifySASTResults`(:619)/`SearchMissedVulns`(:643)/
  `ReviewSASTResults`(:666)/`PauseAnalysis`+`ResumeAnalysis`(task_service.go:702,737，经
  `conn.Invoke` 原始方法名)、`SASTFusionService.FuseResults`(:691)/`CompareResults`(:715)、
  `ResultService.BatchUpdateVerdict`(:769)/`GetTaskResultStats`(:819)/`DeleteFinding`(:191 补偿)、
  `ReportService.GenerateReport`(:794 Kafka 降级路径)、`StorageService.GetFileInfo`+`DownloadFile`
  (archive.go:<host-id>,125)、`ProjectService.GetProjectConfig`(project_config_fallback.go:30)+
  `GetProject`(repo_fetch.go:28)
- **runtime**：→ `ResultService.BatchCreateFindings`(store.go:58)、`GetFinding`(ai_engine.go:236)、
  `ListFindings`(ai_engine.go:253，PageSize=100 上限 50 页)、`TaskService.AppendTaskLog`(task_log.go:66，
  尽力而为)
- **sast-adapter**：→ `ResultService.BatchCreateFindings`(sast_adapter_handler.go:546)、`GetFinding`
  (fusion_handler.go:89)、`ListFindings`(:484)、`BatchUpdateFindings`(:175)
- **result**：→ `StorageService.UploadFile`(report_service.go:160 / storage_archive.go:33)

**声明且有实现但无任何内部调用方**（仅供 gateway/前端或未来用，改动无下游影响面）：
ProjectService 的成员/统计四 RPC；ResultService 的 CreateFinding/UpdateFinding(部分；R139 注：UpdateFinding
  当前零调用方且对请求未携带字段以 proto 零值整实体覆写——无"缺省保全"，未来接通
  调用方前须先裁决部分更新语义，勿直接启用)/
GetFindingsByVerdict/ExportFindings/SubmitFindingFeedback；ReportService 的 ListTemplates/
GetTemplate；Storage 的 GetPresignedUrl/ListFiles/DeleteFile；SASTAdapter 的 RunSASTScan(单工具)/
GetToolInfo/GetScanProgress；Fusion 的 AlignLocations/ClusterFindings/ResolveConflicts/
GetFusionConfig/UpdateFusionConfig。

## 2. 通用契约惯例（跨服务一致）

- **幂等键**（R4）：带 `RequestMetadata` 的写 RPC 一律先校验 `metadata.request_id != ""`，缺失 →
  `InvalidArgument "...request_id is required"`。三态语义（03 §2）：同键同体→重放缓存响应；
  同键异体→`AlreadyExists`。读 RPC 不要求幂等键。
- **幂等键派生惯例**（跨服务调用链）：task 编排每步 `"<task_request_id>-analyze|-scans|-ai|
  -verify|-missed|-review|-fuse|-verdict-<v>-<c>-|-report"`（每次重试独立命名空间
  `"<request_id>-a<N>"`）；dsh 落盘 `"<request_id>-store"`；sast 落盘 `"<reqID>-<tool>"` /
  `"<reqID>-multi"`；Kafka 消费侧报告 `kafka_<task_id>`（确定性，⑥）。
  **已知例外**：fusion 回写 `fusion-wb-<nanots>` 含纳秒时间戳，实际不具备幂等性（重放会重复
  回写，白名单补丁语义使重复无害，但非幂等）。
- **错误码分布**：InvalidArgument(必填/校验) > NotFound(实体缺) > Internal(存储故障) >
  FailedPrecondition(状态机非法转移) > AlreadyExists(同键异体) > Unauthenticated(登录/token) >
  Unavailable(下游不可达) > Unimplemented(诚实降级)。message 惯例小写英文短语带 proto 行号或
  ADR 引用。
- **降级语义是契约主体**（每条都有"任务是否失败"的明确口径，见 §3-§7 各服务）。

## 3. task-service（TaskService，26 RPC）

**状态机**是核心契约（statemachine.go:61-114 注册全部合法边；非法转移 → `FailedPrecondition
"invalid state transition: X → Y"`）：

```
CREATED ──T1──> RUNNING ──> COMPLETED │ FAILED │ TIMEOUT │ PAUSED
QUEUED  ──T4──> RUNNING     FAILED ──AutoRetry(≤2)──> QUEUED
FAILED ──耗尽──> DEAD       {任意态含 PAUSED/DEAD} ──> CANCELLED
PAUSED <──> RUNNING         （无 DEAD→QUEUED 状态机边；RetryScanTask 显式校验后绕过状态机直写）
```

- `CreateScanTask`：必填 project_id + request_id；**task_id = request_id**（幂等键即任务标识，
  且进文件系统路径——白名单 `[A-Za-z0-9._-]`/禁 `..` 与 `.`/长 ≤128，非法 → InvalidArgument，
  R137/F1；网关产 `gw-<hex>` 天然合法，仅直写 gRPC 面受影响）；
  指纹=project_id|scan_mode|sast_tools|排序后 config + 增量四字段（incremental/
  baseline_task_id/git_anchor/diff_hint 长度——同键异体判定覆盖增量意图）；创建即发 Kafka
  `task.created`。**增量契约**：显式 `baseline_task_id` 三重校验（存在/同项目/
  COMPLETED），任一不满足 → InvalidArgument（不静默替换不降级）；`git_anchor`/`diff_hint`
  （截断 64KB）落任务 config 与 proto 快照字段。
- **增量解析（StartTask 的 Prepare 包装，incremental.go）**：源码解析链产出新树根后
  ——①基线选定：显式优先；自动=同项目 COMPLETED 且源码可达（卷树或 upload_file_id 可重物化）
  的 created_at 最新者，不可达顺延更早（语义安全）；②基线树三级重物化：卷树（uploads-<id>/
  unpacked 剥壳 或 <id> clone 根）→ 上传原件重解包到 scratch（diff 后清理）；③内容 diff：两侧
  剥壳对齐 → 逐文件 sha256 → changed（新增∪修改）/deleted（排除 .git 与
  .codeaudit-incremental.diff）+ 行级 unified diff（AI 提示词素材，256KB 上限）；④快照回写
  ScanTask（baseline/changed/deleted/diff_source=content，随 PG payload），重试不重算；
  ⑤`diff_hint` 交叉核对不一致仅记任务日志（D5：不作依据）。降级矩阵（诚实，无静默）：无基线
  → `config.incremental_degraded_reason=no_baseline`；基线不可达 → `baseline_unavailable:*`；
  diff 失败 → `diff_failed:*`——降级后按全量继续（增量上下文不激活，编排零感知）。
  编排传导：InheritFindings（Execute 前置，幂等键 `<task>-inherit`）+ SAST changed_files
  （零变更=零工具调用）+ AI 增量任务卡（见 §6 RunAIAnalysis）。
- `StartTask`：CREATED/QUEUED→RUNNING；源码解析链（顺序，锁定测试 upload_priority_test.go
  三用例）：`config.project_path` 直用 > 任务级 `config.upload_file_id`（storage 拉包闭包）>
  项目级 upload_file_id（GetProjectConfig 兜底+快照回写）> 项目 repo_url clone（守卫必须含
  `r.Prepare == nil`，否则低优先级闭包覆盖高优先级——回归）。全部缺失 → FAILED
  （不空跑）。拉包解压失败 → 任务 FAILED + `FailedPrecondition`（错误文案带"压缩包下载/解压
  失败"）；解包防护：100MB 包/500MB 解包/10 万文件上限、zip-slip 与软链拒绝。编排协程异步执行。
- `FailTask`：retryable 且 retry_count<`task.max_auto_retries`(2) → FAILED→QUEUED；耗尽 → DEAD
  （仍发 `task.completed`，payload.status=DEAD）。
- `PauseTask/ResumeTask`：Pause **先**调 dsh `PauseAnalysis`（闸门先扣，fail-safe）再转 PAUSED；
  Resume 相反（先转状态再恢复，宁可不恢复不可状态错）。dsh 调用尽力而为（失败仅 WARN）。
- `CreateScanTask.authorized_targets`（切片4后半）：任务【资源】字段——
  三元组 {host, port, allowed_ips} 创建时强契约校验（host RFC1123/port 1-65535/
  allowed_ips 每项 IP 或 CIDR，拒载给字段路径 `authorized_targets[i].*`）后快照
  `ScanTask.authorized_targets`；带 pack 键任务另快照 `config.network_profile`
  （源=pack spec.network.profile，修订内容一部分）。编排经 TaskOrigin 贯穿 →
  runtime `sandboxSpecWithPolicy` 唯一构造点：profile=targeted →
  `ComposeTargetedPolicy`（configs/policy/targeted.yaml 模板+endpoints 注入，
  wire-name 特例 filesystem_policy→filesystem，组合时模板 sha256 进 launch 事件
  留痕）；restricted/空 → spec **无 policy 键**（缺省权威=镜像烘焙件）。组合失败
  fail-closed（launch 失败可见，不静默回落）。
- `AppendTaskLog`：必填 request_id/task_id/message；同键重放返回原 entry；环形缓存 500 条/任务。
- `GetTaskLogs`：after_log_id 必须可解析 int64（否则 InvalidArgument）；limit≤0 用服务端默认。
- `ListScanTasks`：filter 仅支持 scan_mode/status 字段与 EQ/NEQ（其他 → InvalidArgument
  "unsupported filter field/operator"）；稳定序 created_at **降序**（最新优先；
  R68 改排序键未改方向）+ task_id 升序决胜（锁定测试 `TestListScanTasks_NewestFirst`）。
- `ReportStageComplete/Failed`：幂等三态 + 阶段指纹；output_refs 写 `stages[].Metadata`
  （注册阶段即初始化 Metadata map——①，nil map 写入曾杀进程）。
- `GetTaskContext`：任务未完成/未知 → NotFound。
- `GetTaskUsage`（切片3后半）：per-task usage 计量聚合（totals+分模型）。
  数据链=任务终态异步归集 baton usage.recorded（共享卷 events.jsonl）→ PG `task_usage`
  表事务全量重算（幂等；无 PG 内存档=空聚合诚实降级）；计费口径
  total=in+cache_read+cache_write+out（reasoning 不重复计入）；REST 面
  `GET /v1/tasks/{id}/usage`（api-external §3.12）。
- `GetTaskGraph`：图执行视图投影——graph.log 回放折叠
  （声明序节点/attempts/计划触发史/paused+converged 态）；非图任务=空 nodes 诚实空态；
  权威源=共享卷 <interaction_dir>/<taskID>.graph.log（与 resume 同源，R116 撕裂容忍
  同纪律）；网关 `GET /v1/tasks/{id}/graph`（canAccessTask 鉴权同任务面）。
- `ListOrchestrations/CreateOrchestration/UpdateOrchestration/DeprecateOrchestration`：负载 pack 生命周期（append-only 修订+停用标记；内置 configs/packs 只读）。REST 面 `GET/POST /v1/orchestrations`（读登录/写 admin）+`PUT/DELETE /v1/orchestrations/{name}`；spec 以全量 YAML 传输（服务端 Validate，错误带字段路径）。持久面=PG `pack_revisions`/`pack_deprecated` 写穿镜像（R-31 同模式，见 data-flows §6）。
- `StreamTaskSnapshot`：服务端流；hub 订阅者模式，写路径（状态转移/日志追加/阶段变更）锁内
  notify；`proto.Equal` 变化检测，变化才发 `TaskSnapshotDelta{task,progress,logs,settled}`；
  2s 兜底 ticker；终态帧 settled=true 后关流。**WatchTaskProgress 显式 Unimplemented**
  （用 StreamTaskSnapshot 或轮询 GetTaskProgress）。
- 编排降级口径：AnalyzeCode 失败→CPG 降级不阻断；SAST 全工具失败=任务失败（模式A/D 唯一
  来源）；并行模式双侧皆败才失败；BatchUpdateVerdict 失败不中断；报告 gRPC 降级失败→任务保持
  部分结果；S8 补偿 DeleteFinding 对 NotFound 视为已清理。

## 4. project-service（ProjectService 11 + UserService 18 RPC）

- `Login/RefreshToken/GetCurrentUser`：失败 → `Unauthenticated`。JWT 签发 HS256（access
  30min / refresh 7d，代码常量对齐契约 D2 裁决，与 api-external.md §1 一致）；bcrypt；
  **Login 拒绝 state≠ACTIVE 账号**（停用即踢不可被重登绕过）；吊销=access 黑名单+
  R87 会话纪元，均 PG 写穿+启动回放（`CODEAUDIT_PROJECT_PG_DSN` 启用时——compose 默认开）。
- `RegisterUser/CreateUser/ChangePassword/ResetPassword`：经泛型 `withIdempotency`
  （user.go:121-145）——request_id 必填→InvalidArgument；同键异体→AlreadyExists；
  注册策略 invitation/open/disabled（`auth.registration_mode`）；自注册固定 ROLE_DEVELOPER；
  ResetPassword 返回服务端生成的一次性临时密码（重放一致）。
- `ListUsers`：offset 数字游标（坏 → InvalidArgument "invalid cursor (03 §5)"）；pageSize 缺省
  20/上限 100（**代码硬编码**，与 result 走配置不同口径）。
- Project CRUD：NotFound → `NotFound "project %s not found"`；重复创建 → AlreadyExists。
- **存储：内存 MemoryStore 为主，`CODEAUDIT_PROJECT_PG_DSN` 非空即启用 PG 写穿镜像+读回落**
  （HA P1-a；compose 默认开启——projects/users/members/configs/stats/revoked/
  epoch/idempotency 表族由 mirror_pg 建表；无 DSN 时回落纯内存=重启即失）。

## 5. result-service（ResultService 14 + ReportService 6 RPC）

- `BatchCreateFindings`：request_id 必填；逐条幂等重放；**单条失败不整批失败**，计入
  failed_count（不静默；调用方 sast-adapter 侧 failed_count>0 视同落库失败、不进
  幂等缓存，R137）。幂等三层（后）：①(request_id, finding_id) 重放键；②findings 表
  UNIQUE(task_id,tool_name,rule_id,file_path,line_number) 兜底；③INSERT `ON CONFLICT (id)
  DO NOTHING`——跨纪元/跨 attempt 同内容发现（沙箱侧内容稳定 ID `taskID-sbx-digest12`）
  重报=幂等 no-op 成功。`CreateFinding` 单条同口径：重复 ID no-op 后**回读存量行返回存储
  真相**（R136/F3）。
- `InheritFindings`：request_id+baseline_task_id+new_task_id 必填（自继承 →
  InvalidArgument）；复制基线中 `file_path ∉ exclude_paths`（规范化口径：正斜杠/去 ./
  消 ../，与 task-service diff 产物镜像）的 findings 到新任务——finding_id 新分配
  `<new_task>-inh-<N>`（List 稳定序 → 序号确定）、`inherited_from_task_id=baseline`、
  verdict/reasoning/ai_fix_suggestion/diff_patch 等终态列随行复制；命中排除清单计入
  skipped_count。幂等：同 (request_id, finding_id) 重放跳过——编排侧用任务稳定键
  `<task_id>-inherit`（自动重试不重复继承）。PG 列 `inherited_from_task_id` 启动幂等迁移
  （ADD COLUMN IF NOT EXISTS）。
- `ListFindings`：pageSize≤0→`result.page_size_default`(20)、>max→`result.page_size_max`(100)
  （配置驱动）；cursor=base64(JSON)，坏 → InvalidArgument。
- `UpdateVerdict/BatchUpdateVerdict`：verdict 实际变化才发 Kafka `finding.verdict.updated`；
  Batch 跳过不存在条目（不计入 updated_count）。
- `GenerateReport`：request_id+task_id 必填；同键同 task→重放 report_id；同键异 task→
  AlreadyExists；**FAILED 报告不占键**（删旧同 ID 重建，+212⑥，锁定测试
  TestGenerateReport_FailedReportRetry_SameID）；报告 ID 确定性 `report_<task>_<request_id>`；
  storage 归档失败仅 WARN（PG 本体完好）；Kafka 消费侧幂等键 `kafka_<task_id>`（重投递幂等，
  TestHandleTaskCompleted_Redelivery_Idempotent）。
- `DownloadReport`：服务端流 64KiB 分块。
- 存储：PostgreSQL（`CODEAUDIT_PG_DSN`，表 findings/finding_feedback/reports/report_templates
  启动自建；`CODEAUDIT_STORE=memory` 降级内存）。

## 6. runtime-service（RuntimeService 27 + CodeAnalysisService 5 RPC）

- `ListAgents`（TP20-T2 切换 Agent 收口）：读 RPC（无幂等键）；descriptor 注册表
  （`configs/agent_providers.yaml`）如实投影——首条=缺省（is_default）；文件缺失=
  合成 DSH 单条目（现网兼容）；解析/校验失败 → `FailedPrecondition`（消费面 web
  降级，不回落硬编码）。capabilities 五布尔=条目如实声明（未声明=false）。
- `RunAIAnalysis`：request_id+task_id 必填；幂等三态；五 Agent 流水线；AI 发现经
  `BatchCreateFindings` 落盘（幂等键 `<request_id>-store`），落盘失败 → `Unavailable
  "persist N ai findings to result-service: ..."`（不静默，）。
- **Maka 方言钩子**：maka 回合
  收束即 execIn python3(sqlite3 只读) 导 runtime_events 表→`<tasklog>/<taskID>/
  maka-events.jsonl`（teardown 前抓取——沙箱回收即失档）；`batonProjectTurn` 分叉：
  该文件存在→`baton project -maka`（金丝雀 park 在 baton 侧落 PARKED）。
  导出失败=投影禁用诚实降级（不影响回合产物）。Task.TasklogDir 传递位。
- `RunGraphTask`（切片 5+6 图执行模式）：request_id+task_id 必填；幂等三态；
  `graph`=GraphSpec 快照（seed_intents/constraints/timeouts，pack execution:graph 创建
  时钉）；GraphRunner 串行图（节点=同沙箱独立 session g-<nodeId>）；`<task>.graph.log`
  域中立事件族落盘（A1 唯一持久真相，resume 回放重建 open 集）；响应只报图状态
  （converged/paused/nodes_*——域中立 C4，findings 走既有 submit_findings→
  BatchCreateFindings 链路，幂等键 `<request_id>-store`）。
  **增量（D3）**：`changed_files/deleted_files/incremental_diff` 非空 → 沙箱任务卡
  （Assignment）注入变更清单+diff 聚焦段（全量代码树照常进沙箱，跨文件追踪指路保留）；
  `incremental_diff` 超 24KB 任务卡预算 → 全文落项目树 `.codeaudit-incremental.diff`
  （随 tar 进沙箱），任务卡截断+指路（纪律）。注入内容随第一条 /prompt 全文
  进入 `.ai.log`「任务下发」帧（可审计）。
- `VerifySASTResults`：先从 result 拉实体；**沙箱不可用 → 如实全批 NEEDS_MANUAL
  （confidence 0.3）**，绝不冒充 AI 判定（07 §10）。进沙箱前同段去重（±2 行容差）。
- `SearchMissedVulns/ReviewSASTResults`：兼容分支；沙箱→RuleScan 降级；落盘失败仅日志
  （与 RunAIAnalysis 上抛口径**不同**，调用方须区分）。project_path 取值 request 优先、
  env 兼容回落（⑯）。
- `GetSessionStatus`：session 不存在不报错，返回 `state="not_found"`。
- `PauseAnalysis/ResumeAnalysis`：task_id 必填；无活动会话也接受"预约"（闸门注册表）。
- `GetAIInteractionLog/StreamAIInteractionLog`：字节游标增量；内存 miss 回查磁盘
  `<interaction_dir>/<task>.ai.log|.sse.log`（进程重启可读；yaml 相对路径=本地开发，
  容器部署经 `CODEAUDIT_INTERACTION_DIR` 指向共享卷 `/data/repos/ai-interaction`，R36）；
  条目未出现时 complete=false
  诚实等待（非错误）。读 RPC 无幂等键。sharedAILogs 内存态 LRU 双上限（64 条/256MB，
  ）。图任务通道增量 `AILogChannel`（node_cursors 游标）与 b4 到达粒度
  `AILogChannel.granularity`/主面同字段（enum `AILogGranularity`：真源=方言——
  dsh-sse=TOKEN 逐字；acp-stdio/maka=TURN 整回合；盘回落条目=UNSPECIFIED）；
  条目淘汰后 Get 走 channelDeltas 盘回落（旧图任务通道不回空）。
- `WatchAnalysisProgress` **显式 Unimplemented**。
- **推理管理面 6 RPC**：`List/Get/Upsert/DeleteInferenceProvider` +
  `Get/SetInferenceRoute`——纯管道，经 openshell-manager `/api/v1/inference/*` 透传
  OpenShell 网关（权威存储 gateway.db，本服务零状态）。workspace 请求级可选
  （per-team 命名空间，空=全局配置缺省注入）；
  写 RPC 三个（Upsert/Delete/SetRoute）request_id 必填（upsert/删除天然幂等，无响应
  缓存）；错误映射 manager 400→InvalidArgument / 404→NotFound / 401,403→
  PermissionDenied / 不可达→Unavailable。**credentials 只进不出**（读路径不回流，
  manager 侧按省略脱敏）。SetInferenceRoute 透传网关连通性验证回执
  （validation_performed/validated_endpoints）。唯一内部调用方=gateway（/v1/inference/*）。
- `ListWorkspaces`（切片3b team 上车）：workspace 清单，只读透传 manager
  `/api/v1/workspaces`（limit/offset；负值 InvalidArgument）。REST 面
  `GET /v1/workspaces`（admin 门禁）。
- `CreateWorkspace`/`DeleteWorkspace`：web
  工作区管理页通道。R4 幂等键网关注入；名字空 InvalidArgument；manager
  409（重名/资源守卫 details 原文）→ FailedPrecondition（REST 409）、
  400 → InvalidArgument。REST 面 `POST /v1/workspaces`、`DELETE /v1/workspaces/{name}`
  （admin 门禁，路径名权威）。
- `AnalyzeCode`：request_id+project_path 必填且路径可 os.Stat（否则 InvalidArgument
  "project_path inaccessible"）；产出 AST 级摘要 `<project>/.codeaudit/cpg.json`。
  `GetCallGraph/GetDataFlow/GetAnalysisProgress` **显式 Unimplemented**（CPG 后端未接）。

## 7. sast-adapter-service（SASTAdapterService 6 + SASTFusionService 9 RPC）

- `RunMultipleScans`：request_id/project_path/tool_ids 必填（空工具列表 → InvalidArgument）；
  多工具并行、按声明序汇装；单工具失败产出 FAILED ToolScanResult 仍正常返回（04 §6），
  仅系统性错误返回 err；扫描超时 `sast_adapter.scan_timeout_s`(120s) → DeadlineExceeded；
  落盘幂等键 `<reqID>-multi`。
  **增量**：`changed_files` 非空 → 工具按 `sast_adapter.tools.<id>.files_argv`
  模板执行（`{files}` 占位展开，40/批，results 数组合并——同款形态）；文件数
  >500 或工具未配 files_argv → **诚实降级全目录扫描**（WARN 留痕）。`RunSASTScan`
  同语义（changed_files=7）。配置键 `files_argv` 为可选键（区别于必填 argv，）。
- `ListAvailableTools`：仅列有执行映射的工具（bandit/opengrep）。
- `FuseResults`：request_id 必填；实体解析三级链（本地 store→扫描存储→result GetFinding
  兜底）；ID 解析不到 → `InvalidArgument "findings not found (result-service also
  unreachable/missing)"`；五阶段流水（FP 过滤→位置合并→去重→冲突→置信度），conflict/
  confidence 两阶段成员索引**必须取全量 FilteredSAST+FilteredAI 输入**（锁定测试
  stage_semantics_test.go）；融合失败→buildFallbackResult 未融合降级；融合后白名单回写
  dedup_group/is_unique/matched_findings。
- `CompareResults`：四象限+双向 precision/recall/F1。`CalculateMetrics/GenerateComparisonReport`：
  按 task 从 result 翻页拉全量；result 地址未配 → FailedPrecondition，拨号失败 → Unavailable。

## 8. storage-service（StorageService 7 + NotificationService 4 RPC）

- `GetStorageMode`（S5）：只读档位探测，mode="s3"（MinIO 持久档）|"memory"
  （07 §10 降级档，重启即丢）。唯一消费方=task-service 卷缓存 GC 的硬保护
  （memory 档绝不删卷树）。判据=presigner 注入与否（MinIO 档必注入）。
- `UploadFile`：客户端流；首块必须带 file_path+content_type（缺失 → InvalidArgument）；
  成功返回 StoredFile{file_id(`file-` 前缀),size_bytes}；数据对象按路径前缀分派域桶
  （reports/ cpg/ sast-raw/ uploads/ trees/——源码树 tar 域），元数据 sidecar 恒落默认桶 `meta/files/<file_id>`。
- `DownloadFile`：file_id 必填；64KiB 服务端流；不存在 → NotFound。
- `GetPresignedUrl`：MinIO 模式签 PUT/GET URL；memory 模式占位。
- `ListNotifications`：user_id 必填；`MarkNotificationRead`：notification_id 必填 + 归属校验
  （跨用户一律 NotFound，⑩）。
- Kafka 事件→通知的幂等由 Redis `ca:idem:<request_id>`（TTL 24h）兜底。
- 存储：`CODEAUDIT_STORE=s3`（MinIO+Redis）| `memory`（全内存）双模。

## 9. proto 同步与共享库

- **SSOT**：`proto/codeaudit_common.proto`。
  `scripts/check-proto-sync.sh`（R1）备份 libs/proto-gen → protoc 重生成 → diff；工具缺失
  跳过不删生成物。
- **libs/proto-gen**：生成物入库；7 服务 go.mod `replace github.com/codeaudit/proto-gen =>
  ../../libs/proto-gen/go` 引用。已知残留：`libs/proto-gen/go/go.mod:1` 模块名仍为
  `github.com/auditmind/proto-gen`（改名未净，目录级 replace 不校验模块名故可编译）。
- **libs/common-go**：`grpcrecover` 拦截器（②），六个 gRPC 服务统一挂载。
- **libs/go-config**：全局配置加载（env > yaml，缺键 fail-fast），7 服务共用。
- **libs/utils、libs/common-python**：空占位（仅 .gitkeep）。
