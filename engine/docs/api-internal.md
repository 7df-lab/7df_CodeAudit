# engine 内部接口契约 — 服务间 gRPC 预期输入 / 预期输出

> 事实源（以代码为准，逐项核对于 2026-09-07，基线 commit `5bc736fd`）：
> `codeaudit_common.proto`（★契约 SSOT，11 个 service / 110 个 RPC）+ 各服务
> `cmd/main.go`（注册）与 `internal/*`（实现）。本文档由守门测试 `tests/test_guardrails.py`
> 看护（proto 声明 ↔ 实现函数 ↔ 文档三向比对；显式 Unimplemented 集合锁定）。
> 字段级结构以 proto 为唯一权威（R1），本文只锁**语义契约**：必填校验、幂等三态、
> 错误码、状态机、降级口径、真实调用关系。

## 0. 注册矩阵：11 proto service → 7 部署服务

| proto service（声明行） | 部署于 | 监听端口（配置键） | RPC 数 | 显式 Unimplemented |
|---|---|---|---|---|
| TaskService (L811) | task-service | 50054 `ports.task` | 19 | WatchTaskProgress |
| ProjectService (L848) | project-service | 50052 `ports.project` | 11 | — |
| UserService (L867) | project-service | 同上 | 13 | — |
| ResultService (L887) | result-service | 50058 `ports.result` | 13 | — |
| ReportService (L909) | result-service | 同上 | 6 | — |
| DSHRuntimeService (L923) | dsh-runtime-service | 50057 `ports.dsh_runtime` | 18 | WatchAnalysisProgress |
| CodeAnalysisService (L948) | dsh-runtime-service | 同上 | 5 | GetCallGraph, GetDataFlow, GetAnalysisProgress |
| SASTAdapterService (L972) | sast-adapter-service | 50051 `ports.sast_adapter` | 6 | — |
| SASTFusionService (L986) | sast-adapter-service | 同上 | 9 | — |
| StorageService (L1005) | storage-service | 50055 `ports.storage` | 6 | — |
| NotificationService (L1019) | storage-service | 同上 | 4 | — |

- gateway-service 不注册任何 gRPC 服务端（纯 HTTP）。
- 全部实现体嵌入 `UnimplementedXXXServer`；上表"显式 Unimplemented"= 方法体存在但返回
  `codes.Unimplemented`（诚实降级）。**调用方不得依赖这 5 个 RPC**；proto 声明保留供未来。
- 各服务出站拨号一律 `insecure` 明文（无 TLS/mTLS 路径，安全边界= compose 内网隔离 + 网关
  JWT；正式部署 mTLS 属设计目标未实现）。
- 所有 gRPC server 挂 `libs/common-go/grpcrecover` 拦截器：panic → `codes.Internal
  "internal panic"`，杀请求不杀进程（ADR-212②）。
- 健康服务：project/task/dsh-runtime/storage 注册 grpc 标准健康服务；result/sast-adapter 未注册。

## 1. 真实调用关系（边清单）

```
            →project  →task   →result →sast-adapter →dsh-runtime →storage
gateway        ●        ●       ●         ●              ●           ●
task           ●        —       ●         ●              ●           ●
dsh-runtime    —        ●       ●         —              —           —
sast-adapter   —        —       ●         —              —           —
result         —        —       —         —              —           ●
storage/project 纯被调方（无出站 gRPC）
```

关键边（调用方 file:line → 被调 RPC）：

- **gateway**（transcode.go 转码，清单见 docs/api-external.md §2）
- **task-service**（编排器 orchestrator.go + 服务层）：
  → `CodeAnalysisService.AnalyzeCode`(orch:507)、`SASTAdapterService.RunMultipleScans`(:539)、
  `DSHRuntimeService.RunAIAnalysis`(:587)/`VerifySASTResults`(:619)/`SearchMissedVulns`(:643)/
  `ReviewSASTResults`(:666)/`PauseAnalysis`+`ResumeAnalysis`(task_service.go:702,737，经
  `conn.Invoke` 原始方法名)、`SASTFusionService.FuseResults`(:691)/`CompareResults`(:715)、
  `ResultService.BatchUpdateVerdict`(:769)/`GetTaskResultStats`(:819)/`DeleteFinding`(:191 补偿)、
  `ReportService.GenerateReport`(:794 Kafka 降级路径)、`StorageService.GetFileInfo`+`DownloadFile`
  (archive.go:107,125)、`ProjectService.GetProjectConfig`(project_config_fallback.go:30)+
  `GetProject`(repo_fetch.go:28)
- **dsh-runtime**：→ `ResultService.BatchCreateFindings`(store.go:58)、`GetFinding`(ai_engine.go:236)、
  `ListFindings`(ai_engine.go:253，PageSize=100 上限 50 页)、`TaskService.AppendTaskLog`(task_log.go:66，
  尽力而为)
- **sast-adapter**：→ `ResultService.BatchCreateFindings`(sast_adapter_handler.go:546)、`GetFinding`
  (fusion_handler.go:89)、`ListFindings`(:484)、`BatchUpdateFindings`(:175)
- **result**：→ `StorageService.UploadFile`(report_service.go:160 / storage_archive.go:33)

**声明且有实现但无任何内部调用方**（仅供 gateway/前端或未来用，改动无下游影响面）：
ProjectService 的成员/统计四 RPC；ResultService 的 CreateFinding/UpdateFinding(部分)/
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
  `"<reqID>-multi"`；Kafka 消费侧报告 `kafka_<task_id>`（确定性，ADR-212⑥）。
  **已知例外**：fusion 回写 `fusion-wb-<nanots>` 含纳秒时间戳，实际不具备幂等性（重放会重复
  回写，白名单补丁语义使重复无害，但非幂等）。
- **错误码分布**：InvalidArgument(必填/校验) > NotFound(实体缺) > Internal(存储故障) >
  FailedPrecondition(状态机非法转移) > AlreadyExists(同键异体) > Unauthenticated(登录/token) >
  Unavailable(下游不可达) > Unimplemented(诚实降级)。message 惯例小写英文短语带 proto 行号或
  ADR 引用。
- **降级语义是契约主体**（每条都有"任务是否失败"的明确口径，见 §3-§7 各服务）。

## 3. task-service（TaskService，19 RPC）

**状态机**是核心契约（statemachine.go:61-114 注册全部合法边；非法转移 → `FailedPrecondition
"invalid state transition: X → Y"`）：

```
CREATED ──T1──> RUNNING ──> COMPLETED │ FAILED │ TIMEOUT │ PAUSED
QUEUED  ──T4──> RUNNING     FAILED ──AutoRetry(≤2)──> QUEUED
FAILED ──耗尽──> DEAD       {任意态含 PAUSED/DEAD} ──> CANCELLED
PAUSED <──> RUNNING         （无 DEAD→QUEUED 状态机边；RetryScanTask 显式校验后绕过状态机直写）
```

- `CreateScanTask`：必填 project_id + request_id；**task_id = request_id**（幂等键即任务标识）；
  指纹=project_id|scan_mode|sast_tools|排序后 config；创建即发 Kafka `task.created`。
- `StartTask`：CREATED/QUEUED→RUNNING；源码解析链（ADR-209 顺序，锁定测试 upload_priority_test.go
  三用例）：`config.project_path` 直用 > 任务级 `config.upload_file_id`（storage 拉包闭包）>
  项目级 upload_file_id（GetProjectConfig 兜底+快照回写）> 项目 repo_url clone（守卫必须含
  `r.Prepare == nil`，否则低优先级闭包覆盖高优先级——ADR-209 回归）。全部缺失 → FAILED
  （不空跑）。拉包解压失败 → 任务 FAILED + `FailedPrecondition`（错误文案带"压缩包下载/解压
  失败"）；解包防护：25MB 包/200MB 解包/3000 文件上限、zip-slip 与软链拒绝。编排协程异步执行。
- `FailTask`：retryable 且 retry_count<`task.max_auto_retries`(2) → FAILED→QUEUED；耗尽 → DEAD
  （仍发 `task.completed`，payload.status=DEAD）。
- `PauseTask/ResumeTask`：Pause **先**调 dsh `PauseAnalysis`（闸门先扣，fail-safe）再转 PAUSED；
  Resume 相反（先转状态再恢复，宁可不恢复不可状态错）。dsh 调用尽力而为（失败仅 WARN）。
- `AppendTaskLog`：必填 request_id/task_id/message；同键重放返回原 entry；环形缓存 500 条/任务。
- `GetTaskLogs`：after_log_id 必须可解析 int64（否则 InvalidArgument）；limit≤0 用服务端默认。
- `ListScanTasks`：filter 仅支持 scan_mode/status 字段与 EQ/NEQ（其他 → InvalidArgument
  "unsupported filter field/operator"）；稳定序 created_at 升序 + task_id 决胜。
- `ReportStageComplete/Failed`：幂等三态 + 阶段指纹；output_refs 写 `stages[].Metadata`
  （注册阶段即初始化 Metadata map——ADR-212①，nil map 写入曾杀进程）。
- `GetTaskContext`：任务未完成/未知 → NotFound。
- `StreamTaskSnapshot`：服务端流；hub 订阅者模式，写路径（状态转移/日志追加/阶段变更）锁内
  notify；`proto.Equal` 变化检测，变化才发 `TaskSnapshotDelta{task,progress,logs,settled}`；
  2s 兜底 ticker；终态帧 settled=true 后关流。**WatchTaskProgress 显式 Unimplemented**
  （用 StreamTaskSnapshot 或轮询 GetTaskProgress）。
- 编排降级口径：AnalyzeCode 失败→CPG 降级不阻断；SAST 全工具失败=任务失败（模式A/D 唯一
  来源）；并行模式双侧皆败才失败；BatchUpdateVerdict 失败不中断；报告 gRPC 降级失败→任务保持
  部分结果；S8 补偿 DeleteFinding 对 NotFound 视为已清理。

## 4. project-service（ProjectService 11 + UserService 13 RPC）

- `Login/RefreshToken/GetCurrentUser`：失败 → `Unauthenticated`。JWT 签发 HS256（access 1h /
  refresh 24h 硬编码——D1 漂移见 api-external.md §7）；bcrypt；吊销=内存黑名单（仅服务重启前
  有效）。
- `RegisterUser/CreateUser/ChangePassword/ResetPassword`：经泛型 `withIdempotency`
  （user.go:121-145）——request_id 必填→InvalidArgument；同键异体→AlreadyExists；
  注册策略 invitation/open/disabled（`auth.registration_mode`）；自注册固定 ROLE_DEVELOPER；
  ResetPassword 返回服务端生成的一次性临时密码（重放一致）。
- `ListUsers`：offset 数字游标（坏 → InvalidArgument "invalid cursor (03 §5)"）；pageSize 缺省
  20/上限 100（**代码硬编码**，与 result 走配置不同口径）。
- Project CRUD：NotFound → `NotFound "project %s not found"`；重复创建 → AlreadyExists。
- **存储为内存 MemoryStore——无持久化**（重启即失；PG 化为后续任务）。

## 5. result-service（ResultService 13 + ReportService 6 RPC）

- `BatchCreateFindings`：request_id 必填；逐条幂等重放；**单条失败不整批失败**，计入
  failed_count（ADR-198 不静默）。findings 表 UNIQUE(task_id,tool_name,rule_id,file_path,
  line_number) 兜底。
- `ListFindings`：pageSize≤0→`result.page_size_default`(20)、>max→`result.page_size_max`(100)
  （配置驱动）；cursor=base64(JSON)，坏 → InvalidArgument。
- `UpdateVerdict/BatchUpdateVerdict`：verdict 实际变化才发 Kafka `finding.verdict.updated`；
  Batch 跳过不存在条目（不计入 updated_count）。
- `GenerateReport`：request_id+task_id 必填；同键同 task→重放 report_id；同键异 task→
  AlreadyExists；**FAILED 报告不占键**（删旧同 ID 重建，ADR-135+212⑥，锁定测试
  TestGenerateReport_FailedReportRetry_SameID）；报告 ID 确定性 `report_<task>_<request_id>`；
  storage 归档失败仅 WARN（PG 本体完好）；Kafka 消费侧幂等键 `kafka_<task_id>`（重投递幂等，
  TestHandleTaskCompleted_Redelivery_Idempotent）。
- `DownloadReport`：服务端流 64KiB 分块。
- 存储：PostgreSQL（`CODEAUDIT_PG_DSN`，表 findings/finding_feedback/reports/report_templates
  启动自建；`CODEAUDIT_STORE=memory` 降级内存）。

## 6. dsh-runtime-service（DSHRuntimeService 18 + CodeAnalysisService 5 RPC）

- `RunAIAnalysis`：request_id+task_id 必填；幂等三态；五 Agent 流水线；AI 发现经
  `BatchCreateFindings` 落盘（幂等键 `<request_id>-store`），落盘失败 → `Unavailable
  "persist N ai findings to result-service: ..."`（不静默，ADR-134）。
- `VerifySASTResults`：先从 result 拉实体；**沙箱不可用 → 如实全批 NEEDS_MANUAL
  （confidence 0.3）**，绝不冒充 AI 判定（07 §10）。进沙箱前同段去重（±2 行容差）。
- `SearchMissedVulns/ReviewSASTResults`：兼容分支；沙箱→RuleScan 降级；落盘失败仅日志
  （与 RunAIAnalysis 上抛口径**不同**，调用方须区分）。project_path 取值 request 优先、
  env 兼容回落（ADR-212⑯）。
- `GetSessionStatus`：session 不存在不报错，返回 `state="not_found"`。
- `PauseAnalysis/ResumeAnalysis`：task_id 必填；无活动会话也接受"预约"（闸门注册表）。
- `GetAIInteractionLog/StreamAIInteractionLog`：字节游标增量；内存 miss 回查磁盘
  `data/ai-interaction/<task>.ai.log|.sse.log`（进程重启可读）；条目未出现时 complete=false
  诚实等待（非错误）。读 RPC 无幂等键。sharedAILogs 内存态 LRU 双上限（64 条/256MB，
  ADR-215）。
- `WatchAnalysisProgress` **显式 Unimplemented**。
- **推理管理面 6 RPC（ADR-217）**：`List/Get/Upsert/DeleteInferenceProvider` +
  `Get/SetInferenceRoute`——纯管道，经 openshell-manager `/api/v1/inference/*` 透传
  OpenShell 网关（权威存储 gateway.db，本服务零状态）。workspace 从全局配置注入；
  写 RPC 三个（Upsert/Delete/SetRoute）request_id 必填（upsert/删除天然幂等，无响应
  缓存）；错误映射 manager 400→InvalidArgument / 404→NotFound / 401,403→
  PermissionDenied / 不可达→Unavailable。**credentials 只进不出**（读路径不回流，
  manager 侧按省略脱敏）。SetInferenceRoute 透传网关连通性验证回执
  （validation_performed/validated_endpoints）。唯一内部调用方=gateway（/v1/inference/*）。
- `AnalyzeCode`：request_id+project_path 必填且路径可 os.Stat（否则 InvalidArgument
  "project_path inaccessible"）；产出 AST 级摘要 `<project>/.codeaudit/cpg.json`。
  `GetCallGraph/GetDataFlow/GetAnalysisProgress` **显式 Unimplemented**（CPG 后端未接）。

## 7. sast-adapter-service（SASTAdapterService 6 + SASTFusionService 9 RPC）

- `RunMultipleScans`：request_id/project_path/tool_ids 必填（空工具列表 → InvalidArgument）；
  多工具并行、按声明序汇装；单工具失败产出 FAILED ToolScanResult 仍正常返回（04 §6），
  仅系统性错误返回 err；扫描超时 `sast_adapter.scan_timeout_s`(120s) → DeadlineExceeded；
  落盘幂等键 `<reqID>-multi`。
- `ListAvailableTools`：仅列有执行映射的工具（bandit/opengrep）。
- `FuseResults`：request_id 必填；实体解析三级链（本地 store→扫描存储→result GetFinding
  兜底）；ID 解析不到 → `InvalidArgument "findings not found (result-service also
  unreachable/missing)"`；五阶段流水（FP 过滤→位置合并→去重→冲突→置信度），conflict/
  confidence 两阶段成员索引**必须取全量 FilteredSAST+FilteredAI 输入**（ADR-214，锁定测试
  stage_semantics_test.go）；融合失败→buildFallbackResult 未融合降级；融合后白名单回写
  dedup_group/is_unique/matched_findings。
- `CompareResults`：四象限+双向 precision/recall/F1。`CalculateMetrics/GenerateComparisonReport`：
  按 task 从 result 翻页拉全量；result 地址未配 → FailedPrecondition，拨号失败 → Unavailable。

## 8. storage-service（StorageService 6 + NotificationService 4 RPC）

- `UploadFile`：客户端流；首块必须带 file_path+content_type（缺失 → InvalidArgument）；
  成功返回 StoredFile{file_id(`file-` 前缀),size_bytes}；数据对象按路径前缀分派域桶
  （reports/ cpg/ sast-raw/ uploads/），元数据 sidecar 恒落默认桶 `meta/files/<file_id>`。
- `DownloadFile`：file_id 必填；64KiB 服务端流；不存在 → NotFound。
- `GetPresignedUrl`：MinIO 模式签 PUT/GET URL；memory 模式占位。
- `ListNotifications`：user_id 必填；`MarkNotificationRead`：notification_id 必填 + 归属校验
  （跨用户一律 NotFound，ADR-212⑩）。
- Kafka 事件→通知的幂等由 Redis `ca:idem:<request_id>`（TTL 24h）兜底。
- 存储：`CODEAUDIT_STORE=s3`（MinIO+Redis）| `memory`（全内存）双模。

## 9. proto 同步与共享库

- **SSOT**：`proto/codeaudit_common.proto`（根目录同名文件为副本，生成脚本只认 proto/ 下）。
  `scripts/check-proto-sync.sh`（R1）备份 libs/proto-gen → protoc 重生成 → diff；工具缺失
  跳过不删生成物。
- **libs/proto-gen**：生成物入库；7 服务 go.mod `replace github.com/codeaudit/proto-gen =>
  ../../libs/proto-gen/go` 引用。已知残留：`libs/proto-gen/go/go.mod:1` 模块名仍为
  `github.com/auditmind/proto-gen`（ADR-208 改名未净，目录级 replace 不校验模块名故可编译）。
- **libs/common-go**：`grpcrecover` 拦截器（ADR-212②），六个 gRPC 服务统一挂载。
- **libs/go-config**：全局配置加载（env > yaml，缺键 fail-fast），7 服务共用。
- **libs/utils、libs/common-python**：空占位（仅 .gitkeep）。
