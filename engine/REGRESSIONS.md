# REGRESSIONS.md — 缺陷档案与防回归索引

> 本档案是 engine 防回归机制的索引层：**每条已修复缺陷一行，绑定具名锁定测试与变异条目**。
> 守门测试 `tests/test_guardrails.py` 校验本档案引用的测试真实存在（档案不腐烂）；
> 变异自检 `tests/mutation/run_mutations.py` 把历史 bug 等价再引入源码临时副本，断言锁定测试
> 必须变红——证明"测试通过"不是牙齿被拔掉的通过。

## 防回归机制（三层，全部在门禁 `make verify` 自动执行）

1. **契约钉住**——`docs/api-external.md`（外部 REST/WS 面）、`docs/api-internal.md`（内部
   gRPC 面）、`docs/data-flows.md`（数据流/存储/配置）三份契约文档，由
   `tests/test_guardrails.py` 与代码做结构级双向比对（路由表、proto 声明↔实现、端口表、
   台账引用）。改代码破坏契约文档 → 门禁红。
2. **回归台账**（本文件）——每个已修复的 bug 登记一行：症状→根因→钉住它的测试→能杀死
   该测试的变异。修 bug 必须同 commit 补测试 + 登记，否则"修复无凭据"。
3. **变异自检**（`tests/mutation/run_mutations.py`）——把台账里每条变异**等价地再引入**
   源文件的临时修改，跑对应锁定测试，断言必须红。它防的是另外两种腐化：
   - 有人弱化/删除/改名锁定测试 → 变异存活（跑不红）→ 门禁红；
   - 源码重构使变异锚点失配 → 锚点恰配性检查红 → 强制同步条目。
   即：**测试本身也被测试**。"门禁绿"因此自证牙齿还在。

修 bug 的工作循环：复现 → 最小修复 → 写/更新一个先红后绿的回归测试 → 本档案登记一行 →
`tests/mutation/run_mutations.py` 的 MUTANTS 追加一条变异（给出能杀死它的测试 pattern）→
`make verify` 全绿。

## 缺陷档案

> 依据：`.agent/decisions.md` ADR-192~215 与 gw-* 生产实例修复记录。测试名一律可 grep
> （Go：`services/**/*_test.go`；守门：`tests/test_guardrails.py`）。

| ID | 日期 | 症状 | 根因 | 锁定测试 | 变异 |
|---|---|---|---|---|---|
| R1 | 2026-09-06 | e2e 用例04 四连败：任务级/项目级 upload_file_id 被 repo clone 静默覆盖（ADR-209） | repo 分支守卫缺 `r.Prepare == nil`，第三档兜底覆盖高档闭包 | `TestStartTask_TaskUploadWinsOverRepoURL`、`TestStartTask_ProjectUploadWinsOverRepoURL`、`TestStartTask_RepoURLStillFallback`（upload_priority_test.go） | M1 |
| R2 | 2026-09-06 | ReportStageComplete 带 output_refs 即 panic 杀整个 task-service（ADR-212①） | 注册阶段不带 Metadata map，对 nil map 赋值 | `TestReportStageComplete_ThreeState`、`TestRegisterStages_AIEnhancedSast`（task_service_test.go） | M2 |
| R3 | 2026-09-06 | 任一 gRPC handler panic 杀进程（minio/存储 panic 连锁，ADR-212②） | grpc-go 无内建 recover | `TestUnaryInterceptorRecoversPanic`、`TestStreamInterceptorRecoversPanic`（libs/common-go/grpcrecover） | —（拦截器本体测试） |
| R4 | 2026-09-06 | fusion 首阶段 panic 时降级路径二次 panic（ADR-212③） | runStage panic 恢复返回 nil ctx，buildFallbackResult(nil) 解引用 | `TestExecute_FirstStagePanic_FallbackNoPanic`（pipeline_fallback_internal_test.go） | M3 |
| R5 | 2026-09-06 | findingsOf 并发读写 → Go runtime fatal 全进程（ADR-212④） | 读路径无锁与写路径并发 | `findings_race_internal_test.go`（-race 与 runtime fatal 双路径） | M4 |
| R6 | 2026-09-06 | task.created/completed 事件自上线即被消费端静默丢弃，offset 照常提交（ADR-212⑤） | producer 不带 event_type 头+载荷字段与消费端不齐 | `TestBuildTaskEvent_HeaderAndPayloadAligned`（event_publisher_test.go） | M5 |
| R7 | 2026-09-06 | FAILED 报告重试必 500；Kafka 重投递必产重复报告（ADR-212⑥） | 重试同键裸 INSERT 撞 PK + 消费侧 request_id 含 UnixNano | `TestGenerateReport_FailedReportRetry_SameID`、`TestHandleTaskCompleted_Redelivery_Idempotent`（report_service_test.go） | M6 |
| R8 | 2026-09-06 | 网络中断截断的列表页冒充完整页（ADR-212⑦） | 三个列表循环缺 rows.Err() 检查 | **缺锁定测试**（repo 层 PG 断连注入未建，如实记录） | — |
| R9 | 2026-09-06 | WS 免鉴权通道 ?token=JWT 整条落网关日志（ADR-212⑧） | logging 中间件打 RequestURI 全文 | `TestRedactToken`（logging_redact_test.go） | M7 |
| R10 | 2026-09-06 | 任意垃圾 Authorization 头每请求换新桶，限流对最该限的对象失效（ADR-212⑨） | 限流键=原始 Authorization 头且位于 JWT 之外 | `TestRateLimit_KeyedByJWTSub`（ratelimit_key_test.go） | M8 |
| R11 | 2026-09-06 | 通知中心 IDOR：任何登录用户可读/标任意用户通知（ADR-212⑩） | user_id 取 query 且 MarkRead 无归属校验 | `TestNotifications_UserIdFromJWTNotQuery`（regression_locks_test.go，本档案随建）；storage 侧归属校验为评审钉 | M9 |
| R12 | 2026-09-06 | 沙箱创建失败泄注册表条目，真孤儿被永久屏蔽（ADR-212⑪） | 注册先于创建但失败路径不注销 | `TestRun_CreateFailure_DeregistersActiveEntry`（sandbox_test.go） | M10 |
| R13 | 2026-09-06 | reconciler 归属标签第二重圈定恒不命中（ADR-212⑫） | 把标签 VALUE 当 KEY 查 | `TestOrphanNames`（sandbox/reconciler_test.go） | M11 |
| R14 | 2026-09-06 | expired 会话只增不减（ADR-212⑭） | StartJanitor 零调用方未接线 | 语义锁 `TestGetSessionExpired`（session_test.go）；**main 接线无离线测试**（如实记录） | — |
| R15 | 2026-09-06 | yaml-only 部署下验证/审核静默空转 200；ReviewSASTResults 无视请求 project_path（ADR-212⑮⑯） | result 地址 env-only 与持久化侧 yaml 双口径；env 优先无视 request | **缺锁定测试**（部署拓扑类，留待 e2e；如实记录） | — |
| R16 | 2026-09-06 | WS 流式路每次回退泄 1-2 个 pump goroutine 与上游流（ADR-213①） | 流生命周期未挂独立 ctx，弃置后 Recv 挂死 | `TestStreamWatch_FallbackCancelsUpstreamStream`（taskwatch_stream_test.go） | M12 |
| R17 | 2026-09-06 | 断流回退丢最后一窗报错行（游标已越过，轮询取不到，ADR-213②） | 回退前不冲刷 pend 增量 | `TestStreamWatch_FallbackFlushesPendingLogs` | M13 |
| R18 | 2026-09-06 | 后端一次抖动=观测页全员断线+重连风暴（ADR-213③） | 轮询路任何错误立即 1011 拆链 | `TestPollWatch_TransientErrorTolerated` | M14 |
| R19 | 2026-09-06 | SAST-only 部署流式路恒回退轮询（主路径对半数部署不可达，ADR-213 死路径） | AI 断流判定缺 `ais != nil` 守卫恒真 | `TestStreamWatch_NoDSH_StreamsStayOnStreamPath` | M15 |
| R20 | 2026-09-06 | fusion 冲突/置信度阶段恒空转，04 §3.3 对合并组从未生效（ADR-214） | 组员索引建自 dedup 后输出（只剩 primary，AI 成员已移除） | `TestConflictResolve_SeesAIMember_AndWritesBackVerdict`、`TestConfidenceFusion_MultiSourceBoost`、`TestPipeline_ConflictAndConfidenceLive`（stage_semantics_test.go） | M16 / M17 |
| R21 | 2026-09-06 | sharedAILogs 无淘汰内存无界；禁用态每任务白建条目（ADR-215①③） | 无 LRU；Enabled 判定未前移 | `TestAILogStore_LRUEviction`、`TestAILogStore_IncompleteEntriesProtected`、`TestWireAILog_DisabledModeNoEntry`（ai_interaction_log_test.go） | M18 |
| R22 | 2026-09-06 | AI 交互日志恒空（ADR-215 回归） | 回调接线落后于 runner 构造（runner 拷贝 cfg 后再接线无效） | `TestWireAILog_WiresCallbacksBeforeRunnerCopy` | M19 |
| R23 | 2026-09-06 | 布局迁移后上传流任务源码全文 404（gw-f6a3523①） | source-file 根解析缺①b uploads-<task_id>/unpacked 流 | `TestSourceFile_UploadsUnpackedFlow`（sourcefile_test.go） | M20 |
| R24 | 2026-09-06 | unpacked/<壳> 根错位，7/7 补丁被误杀+17min fixretry 白跑（gw-f6a3523②） | 解包后缺剥壳降入（唯一子目录逐层降入封顶 3 层） | `TestResolveProjectRoot`、`TestResolveProjectRootDescentCap`（project_root_test.go） | M21 |
| R25 | 2026-09-06 | 32.5min AI 审计撞 30min WS 硬断，观测页中途断流（gw-f6a3523③） | wsMaxLifetime 30min 与长任务竞态 | `TestTaskWatch_LifetimeCoversLongAudit`（regression_locks_test.go，本档案随建：寿命下界必须 > 1h） | M22 |
| R26 | 2026-08-2x | 写路由幂等键恒空（TP12-T3 旅程回归） | protojson.Unmarshal 重置消息，注入先于解码被清空 | `TestCreateProject_IdempotencyInjectedAfterDecode`（regression_locks_test.go，本档案随建） | M23 |
| R27 | 2026-09-05 | 成功结果落地前 3 秒任务被对账误杀（ADR-196） | 判活单证 updated_at | `TestReconciler_AILogActivityKeepsTaskAlive`（task reconciler_test.go） | —（注入式活跃度，变异面在接线层） |
| R28 | 2026-09-04~06 | 推理断流整轮报废 / 巨型 tool-call 断流 / 空发现误判报废（ADR-192/194/211/193 族） | 上下文窗口虚构/单批过大/判据 len>0 | `TestRun_MainTurnTransientRetry`、`TestBuildTurnPrompt_BatchContractTieredByPatchMass`、`TestRun_BatchedSubmitMergeAcrossRetry`、`TestParseAuditResult_EmptyFindingsViaToolIsValid`（sandbox_test.go） | —（prompt 契约类，锚点为模板串，不设文本变异） |
| R29 | 2026-09-07 | 仓库拉取模式部署形态恒 DEAD：`prepare: git clone …: exec: "git": executable file not found in $PATH`（sim e2e 07 实证；上传流任务不受影响故长期隐形） | task 镜像运行时层只装 ca-certificates tzdata，repo_fetch.go 却在本容器内 exec git（ADR-163） | `TestTaskImageContainsGit`（image_contract_test.go，双向锚：代码 exec git ⇔ 镜像层装 git） | M24（变异面=Dockerfile 文本） |
| R30 | 2026-09-07 | 风险详情 UI：ADR-195 链路点选（定位 sink 链）从未渲染 + 人工裁决理由提交后消失（sim 实证：PUT /verdict 200 且 verdict 落库，GET 回读 ai_reasoning 恒空；5 条 ai_agent 发现 reason 全空） | findings 表 DDL 有 reasoning 列（ADR-135 迁移亦补列）但 PostgresFindingRepository 三路径全漏：INSERT 不写/行投影 SELECT 不读/UPDATE SET 不更；memory 仓整结构体拷贝故单测全绿（部署形态 PG 才暴露） | `TestFindingRepoReasoningWired`（finding_reasoning_contract_test.go，文本面契约：三类语句必须携带 reasoning + Scan/Exec 参数接线 + 读路径 COALESCE 防 NULL Scan 崩） | M25/M26/M27（SELECT/INSERT/UPDATE 三变异面） |

## 已知未覆盖缺口（如实记录，非缺陷）

- **R8/R15 两行无锁定测试**：repo 层 rows.Err 需 PG 断连注入；地址双口径属部署拓扑，归 e2e
  （模拟栈）覆盖，离线门禁不伪造。
- **R14 main 接线**：StartJanitor 接线在 cmd/main.go，离线单测不可达；TTL 语义已有锁。
- **变异自检的边界**：变异只证明"锁定测试对**已知等价缺陷**有牙齿"，不能证明对新缺陷形态
  有牙齿——新缺陷仍依赖"先红后绿"纪律（新 bug 必须先写红测试再修）。
- e2e（tests/e2e/，真实沙箱+真实栈）与模拟栈回归（伞仓 pb-A）仍是行为级最终裁决，本档案
  只锁离线可复现面。

## 维护纪律

1. 锁定测试删除或改名 → 必须同 commit 更新本档案（守门测试会红）；
2. 源码重构使变异锚点失配 → `tests/mutation/run_mutations.py` 锚点检查红，先同步 MUTANTS
   再谈门禁；
3. 禁止删除/改名本档案历史行（只增；纠正用追加行）。
