# 增量扫描设计（上传型项目 · 服务端内容对比 · 双视图）

> 本文为功能设计文档；实现与本文冲突时先修订本文。
> **验收与测试要求**：全部功能点的逐点验收标准见配套文档 [incremental-scan-acceptance.md](incremental-scan-acceptance.md)。
> 范围：vscode-plugin 发起的**上传型项目**增量扫描；repo_url 型项目的 git commit 基线路线
> （`engine` 侧 M9 蓝图）不在本期，见 §9。
> 涉及仓：engine（主体）、vscode-plugin（入口+展示）、web（双视图，可后置切片）。

---

## 0. 关键设计决策

| # | 决策点 | 结论 | 落点 |
|---|--------|------|------|
| D1 | 新旧代码对比在哪算 | **服务端内容对比（权威）**：插件照旧全量 zip 上传，服务端 Prepare 阶段对"新树 vs 基线任务快照树"做文件级 hash diff；继承完整性必须可验证，diff 权威不放在客户端 | §4.3 |
| D5 | git 集成定位 | **B 混合路线**：插件集成 git 采集版本锚点（commit/branch/dirty/remote）+ 扫前无变更预检 + 变更预告 UX + diff hint 交叉提示；changed_files 权威计算仍在服务端。契约从 config 字符串键升级为类型化 proto 字段；非 git 工作区自动退化为纯内容 diff；仓库原生免上传路线（D）留作 repo 型演进 | §4.1 / §4.4 / §5 |
| D6 | 源码树数据面 | **混合：桶为持久 SSOT + 共享卷为可丢弃缓存**：Prepare 后把剥壳根打成 `trees/<task_id>.tar.gz` 入 MinIO；任务终态且 tar 入桶成功 → GC 删卷树（**档位感知：storage=memory 档绝不删**）；gateway source-file 按需重物化；彻底去共享卷（各服务自物化）留作独立基础设施里程碑，触发=多主机部署/节点盘压力 | §4.10 |
| D2 | 增量任务结果呈现 | **双视图可切换**：findings 落库时带来源标记（继承/新发现），完整视图为缺省，增量视图=筛选 | §4.6 / §6 |
| D3 | 增量范围作用阶段 | **仅 SAST 实扫增量；AI 全量上下文 + 增量聚焦提示词**：SAST 只扫变更文件；AI 沙箱上下文仍为全量树，但任务提示词注入变更清单与 diff，令 AI 集中审查变更的安全风险；工作对象只取新发现 | §4.5 / §4.7 |
| D4 | 插件触发方式 | **scanWorkspace 智能升级**：检测到可用基线时 QuickPick 询问「增量/全量」，不新增命令 | §5.1 |

## 1. 现状事实（设计地基）

- 服务端源码树**按任务隔离**、终态保留、无 GC：上传型解包于 `<repos_dir>/uploads-<task_id>/unpacked/`（`engine/services/task-service/internal/service/archive.go:315`），仓库型 clone 于 `<repos_dir>/<task_id>/`（`task_service.go:351`）；共享卷 `agent_repos`（compose 229/132/326 + sast `/app/data/repos`）。没有"项目级当前代码"目录。
- **存储三层分工**（"代码存哪"的口径）：①**源码树 = 共享卷目录**（`/data/repos`，文件系统）——SAST 工具（opengrep/bandit 以目录为 argv）与沙箱 tar 打包都需要真实文件系统工作目录，文件树不入库是刻意设计；②**上传归档/报告/CPG/SAST 原始输出 = 对象存储**（storage-service → MinIO，bucket：uploads/reports/cpg/sast-raw，`storage-service/internal/repo/store.go:5-15`）——上传 zip 的持久副本在 MinIO，卷上解包树是其派生物；③**PG = 结构化记录**（`tasks` 表 payload JSONB=任务 proto 全量，`task_store_pg.go:35-43`；`findings` 表）。增量设计与此对齐：diff 只在 Prepare 瞬间读卷上树，产物（baseline/changed/deleted）落 PG 随任务记录持久化；D6 后再进一步——树 tar 入桶使桶成为源码树的持久 SSOT（§4.10），卷树降级为可丢弃缓存。
- vscode-plugin 扫描链 = 全量 `findFiles('**/*')` → zip → `POST /v1/uploads/archive` → `POST /v1/tasks`（config 带 `upload_file_id`）→ start（`vscode-plugin/src/extension.ts:534-614`）；**零本地 git 集成**，打包默认排除 `**/.git/**`。
- `ScanTask` 无 baseline/parent/commit 字段；任务间唯一关联=同 `project_id`。任务 config 是 `map<string,string>`，现有被消费键仅 `project_path`/`upload_file_id`（ADR-203/209 优先级链，`task_service.go:296-366`）。
- findings 落 PG（result-service），按 `task_id` 隔离，`UNIQUE(task_id,tool_name,rule_id,file_path,line_number)`（`finding_repository.go:56-77`）；模型已有 `verdict`（人工裁决，R-30 锚）、`ai_fix_suggestion`、`diff_patch` 等列。
- SAST 契约目前**目录粒度**（`RunMultipleScansRequest{task_id,project_path,tool_ids}`，proto:1438-1443）；但 opengrep 接文件列表已被 `semgrepFilesFallback` 验证（ADR-144，`sast_adapter_handler.go:600-653`）。
- `AnalyzeCodeRequest.changed_files`（proto:1412）与 `04_工作流设计.md` §5 M9 蓝图=已规划未落地；本设计是其上传型分支，changed_files 传导骨架与其一致。
- `SCAN_MODE_COMPARE`（模式 E）是结果对比报告，与增量扫描无关，不复用其语义。

## 2. 术语与核心语义

- **基线任务（baseline）**：本任务增量对比所参照的上一次任务。选定规则（§4.2）。
- **changed_files / deleted_files**：新树相对基线树的内容差异清单（文件级，`新增∪修改` 合称 changed；删除单列）。**快照进本任务记录**，此后本任务不再依赖基线目录存活。
- **继承 finding（inherited）**：从基线任务复制到本任务的 finding（仅限"基线中位于未变更文件"的记录），落库时带 `inherited_from_task_id` 标记；连带 `verdict/reasoning`、`ai_fix_suggestion`、`diff_patch` 一起复制——**复制不是引用**：任务间 findings 相互独立（与现有每任务独立模型一致），在本任务上改裁决不回写基线。
- **新发现（new）**：本任务 SAST 实扫产出（`inherited_from_task_id` 为空）。
- **完整视图**=本任务全部 findings（缺省口径：统计卡/报告/插件树）；**增量视图**=仅新发现（按来源筛选）。

## 3. 总体数据流

```
VS Code 插件                                engine
──────────────────────────────────         ────────────────────────────────────────────────
用户 git pull/commit 后 → scanWorkspace
  ├─ 采集 git 锚点(commit/branch/dirty/remote)
  ├─ 有基线 → QuickPick「增量(基于 xxxx)/全量」
  │    （附变更预告：基线锚点后 N commits + M 未提交；
  │      锚点完全一致 → 提示"无变更"可免上传）
  ├─ 全量 zip 上传（复用现状打包）
  ├─ POST /v1/uploads/archive ────────────→ storage（MinIO uploads 桶，不变）
  ├─ POST /v1/tasks ──────────────────────→ CreateScanTask：
  │    config: { upload_file_id }             │ 类型化字段: incremental=true,
  │    + incremental / baseline_task_id?      │ baseline_task_id?(缺省自动选),
  │    + git_anchor / diff_hint?              │ git_anchor, diff_hint(仅提示)
  └─ POST /v1/tasks/{id}/start ────────────→ StartTask·Prepare（增量分支）：
                                            │ ① 解包新树（现状逻辑）
                                            │ ② 解析基线任务（§4.2 规则）
                                            │ ③ 两树各经 ResolveProjectRoot 对齐根
                                            │    → 逐文件 sha256 diff
                                            │    → changed_files / deleted_files
                                            │ ④ 快照回写 ScanTask 新字段（§4.4）
                                            ├─ SAST 阶段：changed_files → sast-adapter
                                            │    argv 传文件列表（仅变更文件）
                                            ├─ findings 继承：result-service 新 RPC
                                            │    复制基线中「未变更文件」findings
                                            │    → 本任务 task_id + 来源标记
                                            ├─ 融合/AI 阶段：AI 上下文=全量树，
                                            │    任务提示词注入变更清单+diff
                                            │    （AI 聚焦变更安全风险，§4.7）
                                            │    工作对象=新发现（继承项不重跑）
                                            └─ 终态：任务结果=完整项目问题清单
插件展示：变更 N · 继承 M · 新发现 K
```

## 4. 服务端设计（engine）

### 4.1 API 面（外部契约增量，需同步 `docs/api-external.md`）

- `POST /v1/tasks` 请求体新增**类型化字段**（不再走 config 字符串键）：
  - `incremental: bool` —— 增量意图；
  - `baseline_task_id: string`（可选）—— 显式指定基线；缺省由服务端按 §4.2 自动选定；
  - `git_anchor: {commit, branch, dirty, remote}`（可选）—— 插件采集的版本锚点，非 git 工作区为空；
  - `diff_hint: string`（可选）—— 插件本地 `git diff --name-status` 原文，仅作服务端交叉核对提示，**不作为 changed_files 依据**（服务端握有两棵完整树，权威 diff 可自证）。
  - config map 仍承载既有键（`upload_file_id`/`project_path`）与服务端生成的诊断键（`incremental_degraded_reason`）。
- 任务创建校验：显式 baseline 不存在/非本项目/非 COMPLETED → 立即 4xx（显式指定是强契约，不静默替换）。
- `GET /v1/tasks/{id}` / snapshot：透出 `changed_files`/`deleted_files`/`baseline_task_id`/`git_anchor`/`diff_source`（审计与插件展示用）。
- findings 列表/导出面：`inherited_from_task_id` 非空=继承项（筛选参数可选，web 侧也可全量拉取后本地筛）。

### 4.2 基线选定规则（钉死为契约）

同 `project_id` 下，`status=COMPLETED` **且源码目录在位**的任务中，取 `created_at` 最新者。
- "目录在位"校验失败则顺延更早者：回退到更早基线在语义上是安全的（差异面只会更大、继承面更小，结果仍正确）。
- 基线树不在位时按序重物化（D6 升级为标准机制，非可选增强）：① `trees/<baseline_task_id>.tar.gz` 在桶 → 解到 scratch 参与 diff 后清理；② 历史任务无树 tar → 从上传原件（uploads 桶）重解包+剥壳。基线可用性 = 卷树 / 树 tar / 上传原件**任一在位**。
- 全部不可用 → 降级全量（§4.8）。
- `scan_mode` 不作限制：继承的是基线终态 findings（已含融合/裁决），与本次模式正交。
- 不做并发基线锁：基线在 Prepare 时解析并快照，并发增量任务各自独立，后完成者成为下一轮候选。
- git 锚点**不参与**基线解析（基线仍按"树/归档在位"规则选定）；锚点只用于审计、插件预检与展示。基线与本次锚点 commit 相同且双方均无未提交改动时，服务端 diff 自然得出空变更（§4.5 空变更语义），无需特殊通道。

### 4.3 内容 diff（task-service Prepare 内，解包后）

1. 新树（本任务 `uploads-<task_id>/unpacked/`）与基线树各过一遍 `ResolveProjectRoot` 剥壳（`archive.go:97-107`）——**两侧都剥**，防两次上传壳层级不同导致路径错位；
2. 从对齐后的两根出发做文件级对比：路径并集 → 双侧 sha256；`新增∪修改` → changed_files，仅基线有 → deleted_files；
3. 路径口径统一为"相对项目根、正斜杠、去 `./`"——与 findings.file_path 的规范化对齐（§9-1）；
4. 产物直接写 ScanTask 字段（§4.4），diff 本身不留驻内存状态；
5. （D5 新增）若请求带 `diff_hint`：diff 完成后与 hint 做集合比对，不一致仅记任务日志一行（"客户端 git 提示与服务端 diff 不一致：…"，审计客户端健康用），不影响结果；`diff_source` 记录本次 diff 机制（V1 恒 `content`，为 D 路线预留 `git`）。

### 4.4 任务模型扩展（proto additive，字段号以实现期 proto 现状顺延）

```proto
message GitAnchor {                  // 版本锚点（D5，B 路线）
  string commit = 1;                 // HEAD 全长 hash；非 git 工作区为空
  string branch = 2;
  bool   dirty = 3;                  // 扫描时刻工作区有未提交改动
  string remote = 4;                 // origin URL，项目↔仓库关联参考
}
message CreateScanTaskRequest {
  // ... 现有 ...
  bool incremental = N;              // 增量意图
  string baseline_task_id = N+1;     // 空=按 §4.2 自动选基线
  GitAnchor git_anchor = N+2;        // 插件采集，可空
  string diff_hint = N+3;            // git diff --name-status 原文，仅提示不作依据
}
message ScanTask {
  // ... 现有 1-13 ...
  string baseline_task_id = N;     // 增量基线；空=全量任务
  repeated string changed_files = N+1;   // 新增∪修改，相对项目根
  repeated string deleted_files = N+2;
  GitAnchor git_anchor = N+3;      // 扫描时刻版本锚点快照（回写，ADR-203 哲学）
  string diff_source = N+4;        // content（V1）| git（D 路线预留）
}
message UnifiedFinding {
  // ... 现有 ...
  string inherited_from_task_id = N;  // 空=本任务实扫产出；非空=继承来源任务
}
// sast-adapter 扫描 RPC（以编排实际消费的 RunMultipleScans 为主，RunSASTScan 同步加）：
repeated string changed_files = N;   // 空=全量；非空=仅扫这些文件
```

- `incremental_degraded_reason`（string）走 config 键即可（单值字符串，适合 map）。
- task PG 镜像=protojson 全量（`task_store_pg.go:35-43`），新字段自动随 payload，无表结构变更。
- findings 表：`inherited_from_task_id VARCHAR NULL` + 索引 `(task_id, inherited_from_task_id)` 部分索引（增量视图筛选）。
- proto 改动全链：`scripts/generate-proto.sh` → `check-proto-sync.sh` → 契约三件套同步 → 新 ADR。

### 4.5 SAST 阶段传导

- 编排把 changed_files（非空）传 sast-adapter；adapter 侧 `changed_files 非空 → argv 以文件清单替换 {project} 目录参数`（复用 ADR-144 已验证的"opengrep 接文件列表"形态，含分批上限逻辑）。
- **精度口径（诚实声明）**：taint 类跨文件规则在"只给变更文件"的视野下可能漏报跨文件链——文档与任务日志明示"增量扫描以文件为边界"；需要全量精度时用户选全量。
- changed_files 为空（无变更）：SAST 阶段零调用直接过，findings 全量继承（全部标记 inherited），正常出报告——保持任务流水完整，成本近零。

### 4.6 findings 继承（result-service，写路径物化）

- 新 RPC（内部）：`InheritFindings{baseline_task_id, new_task_id, exclude_paths}`——复制基线 findings 中 `file_path ∉ (changed ∪ deleted)` 的行到新 task_id：
  - 连带全部业务列（含 verdict/reasoning、ai_fix_suggestion、diff_patch、CWE 等）；
  - `inherited_from_task_id = baseline_task_id`；
  - finding_id 新分配（`<new_task_id>-inh-<N>` 序列），不与实扫 ID 冲突；
  - 变更/删除文件的旧 findings **不继承**（整文件以新扫为准——行号漂移问题因此天然不存在）。
- 时机：Prepare 得出差异清单后、SAST 完成前即可下发（与 SAST 并行亦可，实现取串行简单优先）；融合（RESULT_FUSION）只作用于实扫新发现，继承项已是基线终态不再融合。
- 指标/统计（CalculateMetrics、web 统计卡）天然吃完整集合=完整视图；增量数=来源筛选。

### 4.7 AI 阶段口径（D3：新增增量聚焦提示词）

- 代码上下文：**全量树**进沙箱（dsh-runtime 打 tar 上传链路不变，`session.go:371-397`）；
- **增量聚焦提示词**：
  - 提示词模板全部在 engine 侧 dsh-runtime-service（沙箱 bridge.mjs 纯转发、零模板）：主审计任务卡模板 `assignmentTemplate`（`internal/sandbox/sandbox.go:1019-1077`），经 `buildTurnPrompt` 填充（`sandbox.go:1080-1082`）后由 `POST /prompt` 的自由文本直达 AI agent；
  - **注入点 = `sandbox.Task.Assignment` 字符串**（`sandbox.go:49`），落在 `ai_engine.go:75` 的 `sandboxAssignmentModeA()` 调用处按增量与否构造——该通道有两个生产先例：`sandboxAssignmentReview` 拼 findings JSON（`sandbox_analysis.go:222-228`）、补丁自纠回合拼 bad_diff_patch JSON（`fixretry.go:102-127`）；
  - **跨服务传参**：`RunAIAnalysisRequest` 现无任何 free-form 字段（proto:1286-1295），加 additive 字段 `repeated string changed_files / deleted_files` + `string incremental_diff`（patch 全文，带大小上限）——扩字段有 ADR-165 先例（`SearchMissedVulnsRequest` 补 `project_path`）；编排调用点 `orchestrator.go:575` 同步补参；
  - **大 patch 走文件、不撑爆 prompt**（ADR-187 "代码全文不进 prompt" 既定架构）：`incremental_diff` 超预算时截断 hunks，全文写为项目树内 `.codeaudit-incremental.diff`（`walkExcludes` `sandbox.go:1013-1016` 按目录名排除、不拦该文件，随 tar 进沙箱），prompt 只给清单+指路 `/sandbox/project/.codeaudit-incremental.diff`；**§4.3 diff 算法同步排除该文件名**，防其混入下一轮基线对比；
  - 提示词文案要点：交代基线任务与变更统计 → 变更/删除文件清单（必要时 diff 节选）→「请将审查重点放在上述变更的安全风险上；完整代码库位于 <沙箱路径>，可用于追踪跨文件数据流」；
  - **生效可审计**：注入内容随第一条 `/prompt` 全文进入 `.ai.log` 的「📋 [任务下发]」帧（`sandbox.go:883-898` → `ai_interaction_log.go:149-166`），e2e 与人工均可核验；
  - 范围：模式 A/B/C 主审计链注入；模式 D 逐条验证 prompt（`verifyTurnPrompt`，`sandbox_verify.go:190-202`）本就按文件定位，V1 不注入（继承项也不进模式 D 验证对象）。
- 工作对象：**仅新发现**（继承项已带 AI 建议与裁决，不重复推理——省 token 且语义自洽）；
- `AnalyzeCode.changed_files` 契约字段本期仍不启用（M9）。

### 4.8 降级矩阵（全部诚实化，不许静默）

| 场景 | 行为 | 记录 |
|------|------|------|
| 项目无可用基线（首次/全部目录缺失） | 自动降级全量 | config `incremental_degraded_reason="no_baseline"`，插件明示 |
| 显式指定 baseline 无效 | 创建即 4xx，不降级 | — |
| diff 失败（IO/超限等） | 自动降级全量 | `incremental_degraded_reason="diff_failed:…"` |
| changed_files 为空 | 正常增量任务（零扫全继承） | 不算降级 |

### 4.9 重试 / 幂等 / 生命周期

- task_id=网关幂等键重放语义不变；**重试用快照**：changed/deleted/baseline 已在任务自身，重试不重算 diff（基线树可能已被清理）；本任务新树重试时由既有 `upload_file_id` 快照链重建（ADR-203）。
- 状态机零改动（增量分支只活在 Prepare+编排内部）。
- 基线目录依赖收敛为"Prepare 瞬间一次性"——此后本任务只依赖自身快照与 DB；§4.10 的卷 GC 因此不破坏增量任务。

### 4.10 数据面：桶为持久 SSOT，共享卷为可丢弃缓存（D6）

- **树 tar 入桶**：Prepare 解析出项目树（剥壳后根）后，task-service 将树打 tar 流式上传 storage（`trees/<task_id>.tar.gz`，与 gateway 上传同款 UploadFile 通道）——tar 收录**剥壳后的根**，重物化无需再剥壳，路径口径与 §4.3 diff 直接对齐；
- **共享卷降级为缓存**：卷上树 = 可丢弃副本，事实源 = 桶（树 tar + 上传原件），丢卷不丢数据；单机零拷贝性能保留，MinIO 不上扫描关键路径（仅重物化时依赖）；
- **基线可用性升级**：见 §4.2 重物化序列；
- **卷缓存丢弃时机——三条触发线 + 硬保护，执行者=周期对账回收器**：
  - **前提**：只有**终态**任务（COMPLETED/FAILED/CANCELLED/TIMEOUT/DEAD）的树可丢弃；运行中/PAUSED 任务的树绝不碰。重试不依赖旧树（retry 自带 re-prepare：cloneRepo/archive 均先清残留再重建），驱逐不影响重试语义。
  - **触发① TTL 到期**：终态 && 树 tar 在桶（HEAD 校验通过）&& 距终态超过保留窗口（缺省 24h，`task.repo_cache_ttl` 可配，0=立即）→ 丢弃。窗口内不删：用户刚完成任务时最常点详情页看代码上下文（source-file），窗口内零重物化。
  - **触发② 容量压力**：卷用量超硬上限（`task.repo_cache_max_bytes` 可配）→ 按终态时间**旧→新**强制驱逐，仍以 tar 在桶为前提；无可驱逐（全在跑）→ 告警，新任务 Prepare 因盘满诚实失败（与现状同，不静默）。
  - **触发③ 孤儿清理**：目录在但任务记录不在（含项目级联删除后残留）→ 孤儿 TTL（缺省 7d，`task.repo_cache_orphan_ttl` 可配）后清理并记账；Prepare 中途失败、无 tar 的树走此通道——上传型可自 uploads 原件重建（非数据丢失），repo 型树是唯一副本，清理前必须记账留痕。
  - **执行者**：周期对账回收器（ADR-210 孤儿沙箱回收器同模式，缺省 10min 一轮，扫描 `<repos_dir>` 对照任务状态与桶），**不做终态钩子同步删**——状态机代码保持单一职责，回收器幂等、可独立观测。
  - **并发安全**：删除动作 = 先原子改名隔离（`<dir> → <dir>.gc-<ts>`）再 rm——已打开的文件句柄不断流（POSIX 语义），新查找 miss 后走重物化（§9-11 竞态的解法）。
  - **硬保护与可观测**：storage=memory 档 → 三条触发线**全部停用**（无持久副本，删=丢数据）；每次驱逐记结构化日志（task_id / 触发原因 TTL|容量|孤儿 / 释放字节数），卷用量与驱逐计数暴露为指标。
- **gateway source-file 适配**：随机读老任务文件时卷树可能已被驱逐 → 按需从树 tar 重物化到 gateway 本地缓存（字节上限 + LRU，可配）再读（gateway 已有 storage 客户端依赖，upload 链路同源）；memory 档下树与 tar 均失（重启后）→ 诚实 4xx"源码已不可用"，与现状 404 口径对齐（实现期钉死具体码）；
- **明确不做**：彻底去共享卷（sast/dsh/gateway 各自物化、服务无本地态、可多主机）= 独立基础设施里程碑，触发条件=多主机部署或节点盘压力；届时本节的 tar 格式与桶布局直接复用。

## 5. 插件设计（vscode-plugin）

### 5.1 入口（D4：scanWorkspace 智能升级 + D5：git 锚点集成）

`doScan` 前置两步：
- **采集 git 锚点**（D5 新增）：经 VS Code 内置 git 扩展 API（`vscode.extensions.getExtension('vscode.git')`，git 随 VS Code 捆绑、不引新依赖）读取 HEAD commit/branch/dirty/origin remote；非 git 工作区锚点留空，后续步骤自动退化为纯内容 diff 路径；
- **基线探测**：`listTasks(projectId)`（已有，`apiClient.ts:116-131`）取最近 COMPLETED 任务——
  - 有 → QuickPick 三选：「增量扫描（基于 xxxx-xxxx · 时间）」/「全量扫描」/取消；选中增量则 createTask 带 §4.1 类型化增量字段；git 工作区的 QuickPick 描述行附**变更预告**（基线锚点到当前 HEAD 的 commit 计数 + 未提交文件数）；
  - 有且**锚点完全一致**（commit 相同且双方 dirty=false）→ 先提示「代码较上次扫描无变更」，用户可跳过上传或坚持重扫（预检只是建议，服务端 diff 仍是权威）；
  - 无 → 直接全量（不询问，零摩擦）。
打包/上传/建任务/启动/跟踪链路全部复用，仅建任务请求体多增量字段。

### 5.2 展示

- 扫描完成 toast 与扫描结果树标题：`变更 N · 继承 M · 新发现 K`（数据来自 task snapshot 新字段 + findings 计数）。
- 任务标题/详情显示锚点短 hash（`abc1234`）——扫描版本可追溯（有锚点时）。
- 结果树 finding 项 description 追加「继承」角标（`inherited_from_task_id` 非空者）。
- 可选配置项（本期可不做）：`codeaudit.scanScope: ask | always-full | always-incremental`，缺省 ask。

## 6. web 双视图（D2，可后置切片）

- findings 表新增「来源」筛选（全部/新发现/继承），复用现有筛选条与 CSV 导出链（web 026a830 已有）；CSV 增 `inherited_from` 列。
- 统计卡/报告维持完整视图缺省；报告模板标注继承项为后续可选增强（本期报告不改）。

## 7. 交付切片建议

| 片 | 内容 | 门禁 |
|----|------|------|
| S1 | engine：proto 扩展（任务/请求类型化增量字段 + GitAnchor + 扫描 RPC changed_files）+ Prepare diff（含 hint 交叉核对）+ changed_files 传导 + findings 继承 + 降级矩阵 + **AI 增量提示词注入（RunAIAnalysisRequest 扩展 + Assignment 构造）** + 单测 + 契约三件套 + ADR | `make verify` 11 checks 全绿 |
| S2 | 插件：git 锚点采集 + 无变更预检 + 变更预告 + 入口询问 + 增量元数据展示 + mocha 用例 | `npm test`（201+新增）|
| S3 | web：来源筛选 + CSV 列 | `npm test` + build + ui_check |
| S4 | 模拟栈 e2e 新用例（08 为模板："两连扫"，第二次上传前改文件，断言继承/新发现/删除不继承 + ai-log「任务下发」帧含增量说明）+ dev-prod-map U7 核对 | `deploy/tests/run.sh`（需模拟栈）|
| S5 | 数据面混合（D6，可与 S1 并行或紧随）：树 tar 入桶 + 卷 GC（档位感知）+ gateway source-file 按需重物化 | make verify + e2e 04/09 回归 + `check-wiring.py` 口径同步 |

## 8. V1 明确不做

- repo_url 型项目的 git commit 基线（M9 原蓝图，需改 `--depth 1` clone 策略）；
- 仓库原生免上传路线（D：项目绑 remote、服务端 fetch pinned commit 自行 git diff——需私库凭据体系与 push-first 工作流约定，作为 repo 型演进的下一站）；
- 只传增量 zip / patch（省带宽路线，D1 已裁；若未来 repositories 体量使带宽成为硬约束可重开，代价是服务端 patch 重放重建全量树 + 客户端信任问题）；
- rename 继承（同内容异路径的 findings 迁移；V1.5 候选，可参考 git -M hint 与服务端"同 hash 异路径"判定）；
- 变更文件内旧 finding 的行号重映射与裁决迁移（V1.5 候选：按 rule_id+file_path+行号邻近匹配迁移 verdict）；
- 彻底桶中心化（去共享卷、各服务按需自物化 scratch）——独立基础设施里程碑（触发条件：多主机部署/节点盘压力），§4.10 的 tar 格式与桶布局届时直接复用；
- `AnalyzeCode.changed_files` 启用、源码树 GC、继承项二次 AI 推理。

## 9. 实现核对点

1. findings.file_path 的实际口径（sast-adapter 相对化处理现状）与 diff 输出路径规范化是否一致——不一致则在继承 RPC 内做一次规范化对齐，并加锁定测试；
2. sast-adapter 两条扫描 RPC（RunSASTScan/RunMultipleScans）中编排实际消费的是哪条（以 `data-flows.md` 与 task_service 调用点为准），changed_files 字段两条都加但先接实消费那条；
3. proto 字段号顺延（ScanTask/UnifiedFinding/扫描 RPC 三处）与 `check-proto-sync.sh`；
4. 继承 RPC 的鉴权与幂等（内部 gRPC 面沿 result-service 现有口径）；
5. 上传件两连场景的剥壳路径对齐实测（含壳层级不同的反例用例）；
6. `RunAIAnalysisRequest` additive 字段号顺延 + 编排调用点（`orchestrator.go:575`）补参链实测；
7. `.codeaudit-incremental.diff` 的双向行为实测：§4.3 diff 排除生效 + 沙箱 tar 上传后在位可见；
8. vscode.git 扩展 API 边界实测（无 git / 仓库未初始化 / 多根工作区取哪份锚点；untracked 文件在 dirty 与 diff_hint 中的口径）；
9. GitAnchor/CreateScanTaskRequest 字段号顺延 + 网关 transcode 的 REST↔gRPC 字段映射同步（`transcode.go` 手工映射面）；
10. 树 tar 与剥壳后根的一致性实测（重物化路径口径与 §4.3 diff 对齐）+ tar 体积/时限上限（500MB 解包上限对应的 tar 与上传耗时）；
11. gateway source-file 按需重物化的缓存边界、GC 与随机读的并发竞态（source-file 命中正在 GC 的老任务）、storage=memory 档下 GC 禁用与 source-file 降级口径实测。

