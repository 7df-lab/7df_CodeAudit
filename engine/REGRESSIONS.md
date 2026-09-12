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
| R31 | 2026-09-08 | task-service 任务全内存存储：容器重启后任务蒸发（dind 全新部署实测），链路态/发现/审计历史随之失联，`codeaudit-task` 单点重启即数据事故 | NewTaskService 只建 `map[string]*pb.ScanTask`，无任何持久化；config 无持久化旋钮 | `task_store_pg.go`（lib/pq，`tasks` 表 JSONB payload protojson 写穿：创建/transitionLocked/重试回 QUEUED 三写点 + 启动 `hydrateTasks` 回放）；旋钮 `task.pg_dsn` / env `CODEAUDIT_TASK_PG_DSN`（空=纯内存，测试/单机形态不受影响；compose 生产形态已注入）。upsert 失败只记日志不反噬任务流（持久化降级≠业务失败） | `TestTaskStorePayloadRoundTrip`（protojson 往返 proto.Equal）、`TestNewPGTaskStoreBadDSN`（坏 DSN fail-loud）、`TestPersistTaskNilStoreNoop`、`TestPayloadJSONNoSchemeRegression`（防 snake_case 污染 payload） | —（需真 PG；持久化实证归部署验收：建任务→重启 task 容器→任务仍在） |
| R32 | 2026-09-08 | POST /v1/projects 载荷缺 `project` 包装键时静默创建全空项目（dind 实测：裸 `{"name":...}` 顶层 → 201 空壳，name/repo 均丢） | handler 未校验包装形状即透传子字段；proto 对缺失包装键静默得零值 Project | `CreateProject` 顶部：`project == nil || name 全空白 → codes.InvalidArgument`（提示正确包装形状） | `TestCreateProjectRejectsMissingProject`（4 用例：缺键/nil/空名/全空白名；校验先于依赖触达，零值 handler 离线可验） | M28（变异面=校验块文本） |
| R33 | 2026-09-08 | `.tar.gz` 上传任务 prepare 必挂：`压缩包解压失败: 不支持的格式（仅 .zip/.tar.gz/.tgz）: …/archive-<ts>.gz`（gw-e295b637 sim 实证；ADR-200 起潜伏 4 天，E2E 只用 zip 未暴露） | `downloadArchive` 落盘名用 `filepath.Ext` 取扩展名——`.tar.gz` 是双段后缀只取到 `.gz`，自产路径不满足自家解包 switch；白名单（放行原始名）与落盘名（改名后）两套后缀口径漂移 | `TestFetchUploadArchive_TarGzFullChain`（fake storage 全链：元数据→落盘名→解包→剥壳，红测试先于修复复现 gw-e295b637 同款报错）；`TestFetchUploadArchive_TgzAndZip`（单段两档不回退）、`TestFetchUploadArchive_UnsupportedExt`（裸 .gz 仍拒收）（archive_test.go） | M29 |

| R34 | 2026-09-08 | 补丁失败反馈再生成整轮报废：修复轮模型按契约分 4 批调用 `submit_patches`、正文无 JSON，Run 报 `no JSON in DSH output` 判废（gw-d331089f sim 实证：13 补丁合规提交全丢，9 分钟再生成沙箱白跑；ADR-184 工具参数提取在错误返回后永不执行） | `ManagerRunner.Run` 对所有回合无条件套用审计回合的 findings 解析（`parseAuditResult`），再生成回合的结果契约是 `submit_patches` 工具参数/正文 `{"patches":[...]}`——回合类型未传给 runner，解析语义错位 | `TestRun_PatchFixRoundToolBatchesMerged`（fake bridge 全生命周期：4 批 submit_patches+纯正文无 JSON，Run 必须按 patches 语义合并且 OK）、`TestParsePatchFixResult`（工具批合并同 index 后批覆盖/围栏降级/空列表合法/双空报错）（sandbox_test.go）；`retryFailedPatches` 闭包签名改 `[]sandbox.PatchFix`（fixpatch_test.go 4 用例同步） | M30 |
| R35 | 2026-09-08 | diff_patch 校验 13/13 全拒：补丁段路径带沙箱挂载前缀 `project/…`（模型看到的根是 `/sandbox/project`），校验端按工作区根解析 → `read workspace file: no such file or directory`（gw-d331089f sim 实证，全批进 fixretry 又被 R34 判废） | `fixpatch` 对段路径只做防穿越清洗，无沙箱挂载视角容错；prompt 约定"仓库相对路径"但模型在沙箱内的 CWD 视角天然多一层挂载前缀（gw-f6a3523 已统一三方根，未覆盖模型产出侧） | `TestNormalizeDiffPatch_SandboxMountPrefixRewrite`（`project/`、`sandbox/project/` 双形态解析+产出路径同步改写+幂等）、`TestNormalizeDiffPatch_GenuineProjectDirNotStripped`（真有顶层 project/ 目录不误剥）、`TestNormalizeDiffPatch_AddFileWithMountPrefix`（add 按父目录存在判定+不可解时错误携带原路径）（fixpatch_test.go） | M31 |

| R37 | 2026-09-08 | diff_patch 主轮校验失败率过高且按任务"全有全无"：@@ 锚点双写形态整补丁被拒（gw-61200b8b 2/2、gw-5a7393ed≥1 实证；锚点写得规范的任务 0 失败——模型书写风格决定成败） | 解析器把 "@@ 定义行" 物化为 hunk 首条上下文行（注释误标 Cline 同款）+1 空格去重只看紧邻行——锚点丢缩进/双写/夹新增三种自然书写形态全部生成文件中不存在的连续序列；Cline 上游（插件 applyPatch.ts 逐字移植可证）defStr 是寻位指令不进内容，canonTrim/trim 两级容错 | `TestNormalizeDiffPatch_AnchorDoubleWriteUnindented`（gw-61200b8b 原样）、`...UnindentedEof`（同任务第 2 项）、`...WithAddition`（gw-5a7393ed 插入式双写）、`...HallucinatedHintIgnored`（锚点幻觉降级内容锚定）、`...PureAdditionAnchoring`（纯新增 defStr 锚定/裸 @@ 仍拒）（fixpatch_test.go，红测试先于修复复现线上同款报错） | M33 |

| R37a | 2026-09-08 | R37 上线后首任务再漏 2/4：文件顶（idx==0）带删除行——@@ 即被删首行（gw-2ff81ebf 实证：`@@ import os`/`-import os`/`+import os/subprocess`），初版修复误判"不可表达"整补丁拒 | 该形态在 Cline 摄入语义下合法（defStr 指向首条变更行本身，消费端插件 findContext 全文回扫兜底层覆盖）；引擎初版对 idx==0 带删除行直接拒绝，属容错面缺一角而非新缺陷 | `TestNormalizeDiffPatch_AnchorAtFileTopDeletion`（gw-2ff81ebf patch#1 原样，红先于修；重建 @@ 承载首条变更行，writeHunk anchorLine 通道防双写） | —（M33 变异面已覆盖物化回归，anchorLine 分支由同测试锁定） |
| R38 | 2026-09-09 | 同一文件同一行的漏洞显示成两条（用户报障）：opengrep 的 location 是沙箱绝对路径、ai_agent 引用的是裸文件名/相对路径，融合 MergeStage 按 file_path 精确字符串比对建索引 → 两形态永不命中，SAST+AI 各出一条（is_unique 双真），融合去重形同虚设 | 匹配键未做路径归一——两条流水线的路径基准不同（沙箱挂载根 vs 模型书写习惯）， gw-f6a3523 统一三方根只覆盖了任务源目录，未覆盖发现位置比对 | `TestMergeStage_PathSuffixAlignment_MergesSameVuln`（裸文件名/相对子目录/`./` 前缀三形态必须合并成组+matched_findings 记录+不标 unique，红先于修）、`TestMergeStage_PathSuffixAlignment_NegativeControls`（异名文件/异行/部分同名不误并）（stage_merge_test.go）；修复=sameVulnFile 后缀对齐（AI 路径归一后为 SAST 路径尾部或相等）+ start_line 相等 | M34 |
| R36 | 2026-09-08 | 长审计任务被误判 TIMEOUT：62 分钟 AI 回合进行中，对账器按 updated_at 判死（30m），任务提前进终态→前端提前渲染发现 Tab（此时发现未落库=空）且状态机无 TIMEOUT→COMPLETED 边，成功跑完也无法翻正（gw-d331089f sim 实证：12:27 误判超时，12:43 阶段全部收敛） | ADR-196 保活探针在容器化部署恒 miss——task 与 dsh-runtime 两容器对相对路径 `interaction_dir` 各自解析到本容器 FS（不落共享卷），`latestInteractionLogMtime` 恒 ok=false 静默回退 updated_at；"同宿主同 CWD"假设被容器化打破 | `TestInteractionDir_EnvOverrideWins`（CODEAUDIT_INTERACTION_DIR 覆盖口锁定，ai_interaction_log_test.go）；ADR-196 keep-alive 本体已有 reconciler 层锁。main.go 接线与 compose env 注入归部署面（同 R14 先例如实记缺） | M32 |
| R40 | 2026-09-10 | **预注册不变量（ADR-225 S5 数据面交付，非线上缺陷；验收全局底线③“误删零容忍”）**——①回收器隐藏目录前缀守卫（.gateway-cache=gateway 重物化缓存/.gc-* 中转态；删除=缓存被清空）②isTerminalStatus 排除 FAILED（自动重试在途任务的卷树与重试 re-prepare 并发竞态；FAILED→QUEUED 是在途边） | 卷缓存 GC 新逻辑（repo_cache.go 三触发线），交付期即锁定 | `TestRepoCacheGC_HardProtections`（repo_cache_test.go 三子测：memory 档绝不删/非真终态 RUNNING·PAUSED·FAILED 绝不删/邻居目录 ai-interaction·.gateway-cache·.gc-* 绝不触碰）、`TestRepoCacheGC_TTLLine`（驱逐/TTL 内/无锚点/锚点校验失败四态） | M38/M39 |
| R39 | 2026-09-10 | **预注册不变量（ADR-225 增量扫描交付，非线上缺陷）**——按 ADR-216"新增核心逻辑必须配变异牙齿"纪律为三条红线面预置守卫：①继承排除过滤（守卫删除=已修复/已删除文件的旧 findings 复活为新任务有效项——过期漏报被当有效历史，最高危）②基线选定 COMPLETED 守卫（删除=RUNNING/FAILED 半成品 findings 被当基线继承）③files_argv 占位守卫（删除=配置错误静默通过，增量扫描空跑） | 增量扫描新逻辑（incremental.go/inherit_service.go/sast_incremental.go），交付期即锁定，无历史缺陷形态 | `TestInheritFindings_MatrixAndCopy`（继承_service_test.go：排除矩阵+终态列复制+ID 形态+ListFindings 回读标记）、`TestSelectBaseline_Rules`（incremental_test.go：最新可达者胜/跨项目与非终态不入选/不可达顺延）、`TestBuildFilesArgv_MissingFilesPlaceholder`（sast_incremental_test.go：缺 {files} 占位必报配置错误） | M35/M36/M37 |

| R41 | 2026-09-11 | 审计修复批次：gateway /v1/auth/* 免认证链限流失效——main.go `rateLimited(authMux)` 在请求 handler 内每请求构造，而 RateLimitMiddleware 每次调用 newRateLimiter 建独立计数表→登录面每请求新桶恒放行（2026-09-10 限流 100/min 调整后 e2e 实测只证了保护链，auth 链从未真被限过） | 中间件实例内含计数表状态，per-request 包装=状态归零；protected 链启动时构造一次故正常 | main.go 改启动时一次性构造 `authLimited := LoggingMiddleware(RateLimit(authMux))`，handler 内只 `authLimited.ServeHTTP` | 守门 `test_gateway_auth_ratelimit_built_once`（静态断言 per-request 包装禁现）|
| R42 | 2026-09-11 | 审计修复批次：HTML 报告代码片段列未转义——`snippetOf` 取自被扫源码（攻击者可控），:423 直写 `<pre>%s</pre>`，web 报告窗口渲染即存储型 XSS；同函数 docstring 自称"go html/template 免注入"与实现（手写拼接）不符 | 逐字段 htmlEsc 清单漏了 snippet 一项；注释抄了模板引擎的安全声明 | snippet 过 htmlEsc + 注释如实化（手写拼接+逐字段转义） | `TestRenderHTMLReport_SnippetEscaped` + M41 |
| R43 | 2026-09-11 | 审计修复批次：ListUsers 游标超界 panic——`end` 有钳制而 `offset` 无，cursor>len(recs) 时 `make(0, end-offset)` 负容量 + slice 越界（admin 面 DoS）；ListProjects/ListMembers 同位置均有钳制 | 复制 ListProjects 分页代码时漏抄 offset 钳制两行 | 补 `if offset > len(recs) { offset = len(recs) }` 对齐 | `TestListUsers_CursorBeyondEnd_EmptyPage`（红时 panic=makeslice cap out of range）+ M40 |
| R44 | 2026-09-11 | 审计修复批次：GetReport 读路径丢弃归档 Url——modelToProto 恒造 `report://<id>` 伪协议，已归档报告的真实 storage 地址不再可取 | modelToProto 写死伪协议未查 r.Url | 归档 Url 优先（reportURL helper），空则回落伪协议 | `TestGetReport_ReturnsArchivedUrl` |
| R45 | 2026-09-11 | 审计修复批次：PG 档 GetTaskResultStats 缺 by_severity/by_cwe——SQL 只算 total+两 verdict 计数，两 map 恒空（统计卡在 PG 档丢维度）；内存实现齐全 | PG 实现只抄了 verdict 聚合，severity/cwe 两 GROUP BY 漏写（CWE 由 rule_id 承载，表无独立 cwe 列） | 补两条 GROUP BY 查询对齐内存口径 | 内存口径既有测试+台账注明：repository 层无 DB 基建，SQL 正确性归 sim e2e（06 快照聚合面）回归 |
| R46 | 2026-09-11 | 审计修复批次：fetchFindingsByIDs 静默缩水——dial 失败返回空集、逐条 GetFinding 失败 continue，AI 验证对象无声变少（验证覆盖率假象）；调用方拿不到错误 | 函数签名无错误通道，历史实现吞错 | 签名改 (findings, error)：连接/查询错误传播（NotFound 跳过=已删竞态合法），两调用点（RunAIVerify/RunSASTVerify 路径）转 codes.Internal | dsh-runtime-service 单测全绿+编译期签名锁定 |
| R47 | 2026-09-11 | 审计修复批次：StartTask 持 s.mu 拨 project-service RPC——fetchProjectConfigValue/fetchProjectRepo 在锁内同步 gRPC，project-service 慢/挂时 StartTask 卡锁→全服务任务面（创建/列表/快照/流）冻结；且 fetchProjectRepo 失败静默跳过，最终报"project_path 未配置"误导排障 | 锁内做网络 I/O（ADR 无明文，实现期引入）；错误被 `gerr == nil &&` 条件静默吞 | 解析链两 RPC 移锁外（锁内快照→解锁 RPC→重锁校验状态仍 RUNNING 再应用）；fetch 失败原因追加进 FAILED ErrorMessage | task-service 全量单测绿（含 MissingProjectPath/优先级链/重试回归）|
| R48 | 2026-09-11 | 审计修复批次：任务读路径出活引用——CreateScanTask 幂等回放/创建返回、ListScanTasks 列表元素、progressOf.Stages 均为锁内 map/slice 原引用，RPC 序列化（锁外）与编排状态机并发读写同 message（-race 必红面；TestOrchestrationFailure 曾依赖活引用读 retry_count，恰是反模式实证） | proto message 含 MessageState 非并发安全，cloneLocked 已有但三条路径未用 | 全部改 cloneLocked/proto.Clone 出锁；retry 测试改读最新快照 | task-service 全量单测绿 + M 框架回归；锁内克隆=语义快照 |
| R49 | 2026-09-11 | 审计修复批次：FAILED 的 ErrorMessage 先 persist 后赋值——transitionLocked 内部 persistTaskLocked 落库时 ErrorMessage 还是旧值，重启回放/PG 查询的 FAILED 任务无失败原因（内存态有、持久层无） | 赋值行放在 transitionLocked 之后（persist 在 transition 内部） | 五处（StartTask×3/runOrchestration×2）赋值提到 transition 之前 | task-service 全量单测绿 |
| R50 | 2026-09-11 | 审计修复批次：AppendTaskLog 幂等回放 miss 回落追加——logIdem 命中但原条目已被环形丢弃（>500 条）时 fall through 再追加，同 request_id 二次入账 | 回放分支 miss 无兜底出口 | 回空壳回执（原 log_id+task_id），不再追加 | `TestAppendTaskLog_ReplayAfterRingEviction_NoRefill` |
| R51 | 2026-09-11 | 审计修复批次：orchestrator 补偿路径 NotFound 判断走字符串比较（`status.Code(err).String() == "NotFound"`） | 历史写法绕开 codes 枚举 | 直写 `status.Code(err) == codes.NotFound` | 编译期+既有补偿单测 |
| R52 | 2026-09-11 | 审计修复批次（D5 复核点确认为真缺陷，未被 S5 吸收）：卷缓存容量线记账删后量恒 0——enforceCapacity 在 evictIfTarInBucket（内部已删目录）之后才 `total -= dirSize(c.dir)`（恒 0），水位 break 永不触发→超容量线会逐出全部合格候选而非驱至 90% 水位；注释自称"按驱逐前体积记账"与代码相反 | evict 封装吞了体积信息，调用方删后补量 | evictIfTarInBucket 改返回 (释放字节, ok)，容量线用返回值记账 | `TestRepoCacheGC_CapacityStopsAtWatermark`（修复前 t-new 也被逐出）+ M42 |
| R53 | 2026-09-11 | 审计修复批次：ExportFindings task_id 直拼落盘文件名/对象键（`findings-%s.json`）——含 `../` 的 task_id 路径穿越出导出目录 | filepath.Join 不消毒 `..`；task_id 语义上是服务端生成但 API 面直接受控 | task_id 白名单消毒（`[A-Za-z0-9._-]`、禁 `..`、长度上限） | validateTaskIDForPath + result-service 单测绿 |
| R54 | 2026-09-11 | 审计修复批次：QueryCPG 按 cpg_storage_path 直接 os.ReadFile——请求面任意文件读取（内网 RPC 面，越权读仍成立） | 合法形态只有 `<projectPath>/.codeaudit/cpg.json`（AnalyzeCode :53 生成）但读路径无形态断言 | 路径消毒：Base(dir)==.codeaudit 且 Base==cpg.json，否则拒绝（诚实错误 JSON） | `TestQueryCPG_RejectsNonCpgPaths` |

| R55 | 2026-09-11 | 用户报障"Provider 切换路由报错，网关只认大写"：前端预置小写键（base_url/api_key）与 OpenShell 网关大写约定键（OPENAI_BASE_URL/OPENAI_API_KEY，anthropic 为 BASE_URL/API_KEY）不匹配；引擎三层（gateway protojson→dsh-runtime→manager）对 map 键零校验逐字透传，小写键静默存储不被识别，切路由连通性验证（no_verify=false 默认开）时才失败，用户无从归因 | proto map 键自由字符串+透传链哲学=无人持有键名契约；e2e 用例 10 永久 no_verify=true+假凭据，验证连通性路径覆盖为零 | dsh-runtime-service UpsertInferenceProvider 入口拦已知别名键（大小写不敏感命中、精确约定键放行）→ InvalidArgument 指路约定键；api-external §3.11 契约化键约定；web 预置键改约定键+提示 | `TestInference_UpsertRejectsLowercaseAliasKeys` + e2e c10 别名键 400 断言 |
| R56 | 2026-09-11 | 用户报障"AI 未生效仍显示 AI 检测完成"：沙箱不可达走 RuleScan 兜底（ADR-175 设计降级）时 RunAIAnalysisResponse success 返回且无降级标志→编排照发 done:ai→阶段绿勾+任务已完成；summary["ai_degraded"] 仅在 gRPC 报错时置位（降级路径 gRPC 成功不置位）且 storeContextLocked 不搬运=死信号 | 降级事实只在 finding 级标注（NEEDS_MANUAL/rulescan-fallback 前缀），任务/阶段层零信号；阶段事件 msg 参数被 stageRecorder 整体丢弃 | proto additive RunAIAnalysisResponse.degraded=3；runFiveAgentPipeline 按沙箱路径失败置位（注意 :117 的 fallbackUsed 被"是否零发现"语义覆盖，改用 sbxErr 直填）；orchestrator 发 "[降级]" 前缀 stage 行；stageRecorder/completeStage 把 msg 落 TaskStage.metadata（message+degraded），web 渲染降级 Alert | `TestRunFiveAgentPipeline_DegradedFlag` + `TestStageRecorder_MsgAndDegradedLandInMetadata` + e2e c07 降级分支 metadata 断言 |
| R57 | 2026-09-11 | 附随 R56 审码发现：stageRecorder 的 msg 参数被整体丢弃（不落 TaskStage），编排进度行/降级行只进执行日志不进看板 | StageRecorder(func(eventKey,msg)) 签名有 msg 但实现未消费 | 非 done 分支写 metadata["message"]+"[降级]"前缀置 metadata["degraded"]=true；done 分支经 completeStage 写 metadata["message"]（degraded 不被覆盖） | `TestStageRecorder_MsgAndDegradedLandInMetadata` |

| R59 | 2026-09-11 | 审计批次二（四路审计）：A6.3 视野声明生产不可达——emitIncrementalScopeNotice 原挂在 runOrchestration（Execute 之前），此刻 IncrementalContext 是 StartTask 新建空壳，Snapshot() 恒 active=false → WARN 永不落真实任务日志；原测试直调 helper 恰绕开生产时序（与 R58 同模式第二例） | 调用点在填充之前（inc.Set 只在 Execute→Prepare 内发生）；测试未经生产链 | 调用移至 runIncrementalDiff 的 inc.Set 激活点之后 | `TestIncrementalScopeNotice_ViaProductionChain`（经 runIncrementalDiff 生产入口；零变更不发负例） |
| R60 | 2026-09-11 | 审计批次二：继承部分失败静默当成功——InheritFindings 逐行 Create 失败仅 FailedCount++，编排消费方不检查 → 缺继承行的任务照常 COMPLETED，违背"完整性优先"承诺 | FailedCount 字段无消费方 | runIncrementalInherit 对 FailedCount>0 返回 error 走既有 FAILED→QUEUED 重试链（幂等重放只补缺） | `TestRunIncrementalInherit_PartialFailureFails`（进程内 gRPC 假体）+ M43 |
| R61 | 2026-09-11 | 审计批次二：继承后缀排除误杀未变更文件——excludedBy 的 HasSuffix 无前缀校验，变更 pkg/util/keys.py 连带排除 vendor/pkg/util/keys.py 的旧 findings（不重扫不继承，漏洞从增量报告永久消失）；R38 后缀语义在 merge 面方向安全，复用到排除面翻转为漏报 | 排除判定原样复用 R38 merge 语义未评估方向差异 | 两段校验：后缀命中后追加前缀段判定（空=已归一相对；/unpacked/=上传树根；其余不排除） | `TestExcludedBy_TwoSegmentCheck`（嵌套负例+树根正例+相对精确）+ M44 |

| R62 | 2026-09-11 | 审计批次二：三处无上限内存缓冲（OOM 向量）——storage AssembleChunks 全量入内存、树 tar 打包无体积上限、沙箱 tarProject 先全量缓冲再查 100MB（且 multipart 再缓冲≈2×）；gateway 100MB 是唯一有闸入口，gRPC 直连/大仓库并发可冲垮 storage/task/dsh 服务 | 全缓冲实现简单但无预算控制 | AssembleChunks 边收边量超 512MB（env CODEAUDIT_STORAGE_MAX_OBJECT_BYTES）ResourceExhausted；writeTreeTarGz 累计字节上限（独立计数器，勿混文件计数）超限放弃入桶任务不失败；tarProject 流式拷贝按缓冲水位即时中止 | 各服务单测（roundtrip 回归+水位判定）；集成归 sim e2e |
| R63 | 2026-09-11 | 审计批次二（P3 六项打包）：①归档报告 URL 双前缀畸形（minio://reports/reports/…，R44 后被顶到前台）②semgrep CWE 提取 s[:end] 带（Improper Neutralization (CWE-89) 类）前缀垃圾串 ③StartTask 三处早失败路径阶段看板悬挂全 PENDING/0% ④rehydrateBaselineTar/uploadReportToStorage 裸 ctx 无限阻塞（对齐 archive.go 120s 口径）⑤fetchProjectConfigValue 吞错致静默降级 repo clone 扫错代码 ⑥网关通知归属核验直调裸 r.Context() 可悬挂 | 单项均轻但分散 | 逐项修复见 commit；③ finalizeStagesLocked×3；④⑤ 错误/超时通道带回 StartTask ErrorMessage | 各服务单测回归 |

| R64 | 2026-09-11 | 审计批次二（跨仓 D5）：finding.created 死契约——storage 消费端就绪（HIGH_SEVERITY_FOUND 映射完整）但全伞仓零生产者，高危发现站内通知永远不触发（静默死路径） | 事件生产分散在 result-service（无收件人上下文）与 task-service（无 severity），谁都不全 | task-service 成功收尾拉取本任务 findings（翻页），HIGH/CRITICAL 者经 TaskEventProducer 补发 finding.created（收件人=created_by；非致命，无收件人跳过与消费端同口径）；R55 补遗=守卫增约定键全小写形态四键；D4=proto 根副本删除+四锚点改指 proto/+verify G1 防回潮 | `TestBuildFindingCreatedEvent_Shape`（载荷四字段+event_type 头）+ `TestInference_UpsertRejectsLowercaseCanonicalForms` + e2e c04 verdict:batch 断言 + G1/check-wiring 改指后全绿 |

| R65 | 2026-09-12 | 待办收尾·账号面三件：①ID 生成 UnixNano&0xFFFFFFFF 截断 32 位（相差 4.295s 必同 ID，时间戳形态可预测，proj-/user- 共用）②jwtSecret 未配置静默落开发缺省密钥（签发侧 fail-open，与 gateway 同键位 fail-fast 不一致；缺省密钥已知=伪造令牌面）③登录"user not found"vs"invalid password"文案区分（网关 401 透传→用户名枚举） | 三处孤立缺口 | ①crypto/rand 8 字节 hex ②未配置 panic（fail-fast，compose/env 均已要求该键）③文案合一 "invalid username or password" | `TestGenerateID_CryptoRandom`/`TestJwtSecret_ExplicitValueWins`/`TestLogin_UnifiedFailureMessage`（测试装配显式供密钥） |
| R66 | 2026-09-12 | 待办收尾：FAILED 报告对调用方 success 返回——编排照发 done:report（阶段绿勾），与 R56 降级可感知精神相悖 | 生成失败落 FAILED 行但 RPC 成功（重试同 ID 语义保留过度外溢） | FAILED 路径返回 FailedPrecondition（错误携带 report_id 供重试同 ID；FAILED 行仍落库 R7 语义不变）；编排 err 分支既有"degraded"标注接住 | `TestGenerateReport_ContentFailureHonest` |
| R67 | 2026-09-12 | 待办收尾：取消不传播——CancelScanTask 只改状态，在途编排继续跑（阻塞 RPC 逐次超时+重试循环继续；编排协程恒 Background ctx） | 编排无取消通道 | per-task cancels registry：StartTask 注册 WithCancel→runOrchestration(orchCtx)，循环顶 select 取消检查（状态已 CANCELLED 由覆盖守卫保持），CancelTask 转移后触发；退出注销防泄漏 | `TestCancelTask_PropagatesToOrchestration`（取消后状态稳定 CANCELLED+registry 无泄漏） |
| R68 | 2026-09-12 | 待办收尾：①idem 表内存而任务 PG 持久（R-31）——重启后同 request_id 重放走新建覆盖既有任务（状态归 CREATED）；②任务列表按 updated_at 排序+offset 游标——updated_at 高频变更令翻页重复/跳页 | ①无跨重启重放判定 ②排序键可变 | ①指纹随任务 Config 落库（_idem_fp 内部键），task_id=request_id 任务在即重放（同体回克隆/异体 AlreadyExists）②排序改 created_at desc+task_id 决胜（不可变） | `TestCreateScanTask_PostRestartReplayNoReset`；排序变更经列表用例回归 |
| R69 | 2026-09-12 | 待办收尾：三服务内存态无界增长——task-service logs/incrementalCtx/cancels/idem/logIdem（终态任务辅助态永留）、sast-adapter byTask+idempotency（任务粒度累积最重）、project-service revokedTokens（map[string]struct{} 无 TTL） | 长生命周期服务只增不减 | task-service：GC tick 挂 sweepTerminalAux（终态超 24h 清 logs/incrementalCtx/cancels；idem>50k/logIdem>100k 整体重置=重启遗忘语义）；sast-adapter：byTask FIFO 200 任务+idempotency 10k 护栏；project-service：revokedTokens 改 map[string]time.Time+撤销时惰性清扫 24h（access TTL 30min 足裕） | 各服务全量单测回归（行为对正常负载无感知） |
| R70 | 2026-09-12 | 待办收尾：①gateway 重物化 LRU 驱逐直接 RemoveAll（读缓存请求瞬断）+并发 miss 双方同写互删半成品 ②task-service cmd 未 Close TaskEventProducer（停机丢在途事件批） | ①无隔离/原子性 ②资源收尾遗漏 | ①驱逐改名 .gc-<ts> 隔离+异步删（POSIX 句柄不断流）；miss 解包入 .tmp 目录成功后 rename 原子就位、失败只清己方 tmp ②defer producer.Close() | `TestEnforceRehydrateLRU_EvictsOldest`（水位语义保持）；Close 编译期 |
| R71 | 2026-09-12 | 用户报障 gw-a00057d9691e0097f1b546da："还在执行审计却抛错立刻回收沙箱"——上游(glm-5.3-flash)流式推理 2m39s 后一帧 SSE JSON.parse 失败（dsh llm-deepseek MALFORMED_RESPONSE），被判非瞬态→回合立即失败→恒 teardown 回收→2m39s 分析全丢→RuleScan 降级 | `isTransientStreamErr` 关键字表只含连接级断流族（SSE stream ended/EOF/connection 等），流中途**数据级损坏**不在内；而它与连接级断流同族可恢复（请求合法、会话历史完好，续跑指令即可让模型重发提交）——DSH 提供商级重试不接（MALFORMED_RESPONSE 不在 retryableCodes，重发请求有增量重复顾虑），回合级续跑是唯一安全杠杆但分类漏了它 | `isTransientStreamErr` 增关键字 `malformed SSE payload`（dsh translate 稳定消息前缀，跨仓契约同 ADR-192 补遗口径）→ 走既有 ADR-192 续跑重试 ≤2 次（maxTurnStreamRetries，重试耗尽才失败回收）；决策 ADR-227（人类指令 2026-09-12"此类错误应该重试两次，而不是直接回收沙箱"） | `TestRun_MainTurnMalformedPayloadRetry`（畸形帧→继续指令→新回合 submit_findings 落袋；promptCount==2+缩批要求+ADR-192 事件）+ M45 |

## 已知未覆盖缺口（如实记录，非缺陷）

- **Kafka 无 DLQ**（R70 附带登记）：storage event_consumer 处理失败即提交 offset（事件永久丢，代码注释自认）；DLQ 需 topic 拓扑决策，单独立项。
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
