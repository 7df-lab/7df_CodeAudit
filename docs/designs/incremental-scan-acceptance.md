# 增量扫描验收与测试要求（配套 [incremental-scan.md](incremental-scan.md)）

> 逐功能点给出**可观察的验收标准**与**测试要求**（层级+关键用例+门禁归属）。
> 实现发现验收标准不可达/不合理时，先修订本文再改码。
> 诚实性总红线：一切降级路径可见可查（config 诊断键 + 任务日志），无静默全量、无静默丢继承、无静默删缓存。

## 0. 门禁总口径（每层用什么工具、何时必跑）

| 层级 | 工具/门禁 | 必跑时机 |
|------|-----------|----------|
| engine 单测/契约/守门/变异 | `make verify`（G1 SSOT 7 检 / G2 逐 module 单测 / G4 契约经 fixture_server / G6 守门 12 用例 / G7 变异） | engine 每次交付；G7 对新增核心逻辑（diff/继承过滤/缓存决策）必须配套变异条目 |
| vscode-plugin | `npm test`（Mocha + vscode 桩） | 插件每次交付 |
| web | `npm test`（Vitest）+ `npm run build` + `deploy/tests/ui_check.py` | web 切片交付 |
| 模拟栈 e2e | `bash deploy/tests/run.sh`（新用例见 F23） | S1+S2 完成后、S5 完成后各跑全量 |
| 缺陷纪律 | engine `REGRESSIONS.md`：开发/测试中发现的 bug 必登记+补锁定测试+变异（ADR-216） | 随发现随登记 |

## 1. 功能点总览（F# → 切片 → 主要测试层级）

| F# | 功能点 | 切片 | 主要测试层级 |
|----|--------|------|--------------|
| F1 | 增量任务创建契约（类型化字段） | S1 | G4 契约 + 单测 |
| F2 | 基线自动选定规则 | S1 | 单测 |
| F3 | 基线三级重物化 | S1/S5 | 单测 + e2e |
| F4 | 内容 diff（剥壳对齐/路径口径/hint 核对） | S1 | 单测矩阵 + G7 变异 |
| F5 | changed/deleted 快照回写与重试复用 | S1 | 单测 + G4 |
| F6 | SAST 增量传导 | S1 | 单测 + e2e 04 回归 |
| F7 | findings 继承物化（双视图数据底座） | S1 | 单测 + G7 变异 + e2e |
| F8 | AI 增量聚焦提示词 | S1 | 单测 + e2e（ai-log 帧） |
| F9 | 降级矩阵 | S1 | 单测 + e2e |
| F10 | 空变更语义 | S1 | e2e |
| F11 | 幂等/状态机零侵入 | S1 | G6/G7 回归 |
| F12 | 树 tar 入桶 | S5 | 单测 + 集成 |
| F13 | 卷缓存丢弃（三触发线+回收器） | S5 | 单测 + 集成 + 并发用例 |
| F14 | gateway source-file 按需重物化 | S5 | 单测 + e2e 09 回归 |
| F15 | 部署口径同步（check-wiring/U7） | S1/S5 | check + 静态审计 |
| F16 | 插件 git 锚点采集 | S2 | mocha |
| F17 | 无变更预检 | S2 | mocha |
| F18 | 变更预告 | S2 | mocha |
| F19 | 入口询问（scanWorkspace 智能升级） | S2 | mocha |
| F20 | 增量元数据展示 | S2 | mocha |
| F21 | web 来源筛选 + CSV 列 | S3 | vitest + ui_check |
| F22 | 契约三件套/ADR/proto 同步链 | 全程 | G1 |
| F23 | e2e 两连扫全链 | S4 | run.sh 新用例 |

## 2. 逐点验收与测试

### A. engine 增量核心（S1）

#### F1 增量任务创建契约（设计 §4.1/§4.4）
**验收**
- A1.1 `POST /v1/tasks` 带 `incremental=true`（无 baseline_task_id）→ 创建成功，幂等键语义不变；任务启动后 `GET /v1/tasks/{id}` 透出解析出的 `baseline_task_id`；
- A1.2 显式 `baseline_task_id` 必须同时满足：存在、同 project_id、COMPLETED——任一不满足立即 4xx，**不静默替换、不建任务**；
- A1.3 `git_anchor`/`diff_hint` 原样落任务记录并在 snapshot 透出；空 anchor（非 git 工作区）不报错；
- A1.4 旧客户端（无增量字段的纯全量请求）行为与现状完全一致——增量字段全部缺省不改变现有路径。
**测试**：task-service 单测（A1.2 三种非法各一例 + A1.4 回归）；G4 契约用例（api-external.md 字段同步后）；transcode REST↔proto 字段映射单测（§9-9）。

#### F2 基线自动选定（§4.2）
**验收**
- A2.1 同项目多个 COMPLETED：取 created_at 最新且**源码可达**（卷树/tar/上传原件，§4.2 重物化序列）者；
- A2.2 最新者不可达 → 顺延更早可达者，任务日志记一行基线选择结果；
- A2.3 全部不可达 → 走 F9 降级（no_baseline）；
- A2.4 `git_anchor` 与基线选择无关（锚点相同/缺失不改变选择结果）。
**测试**：单测构造三任务（最新无树/中间有/最旧有）断言选中中间者；A2.4 边界用例。

#### F3 基线三级重物化（§4.2/§4.10）
**验收**
- A3.1 卷树在位 → 直接 diff，零下载；
- A3.2 卷树不在、`trees/<id>.tar.gz` 在 → 重物化到 scratch 参与 diff，**结束后 scratch 清理无残目录**；
- A3.3 tar 也不在、上传原件在 → 重解包+剥壳后参与 diff（路径口径与 A3.1 一致）；
- A3.4 三级全缺 → 视为不可达，回到 F2 规则。
**测试**：单测三级各一例 + scratch 清理断言；集成（模拟栈删卷树后增量任务仍成功）。

#### F4 内容 diff（§4.3）
**验收**
- A4.1 输出 changed（新增∪修改）/ deleted 两清单；路径=相对剥壳根、正斜杠、无 `./` 前缀，**与 findings.file_path 口径一致**（§9-1）；
- A4.2 同内容 rename = delete+add 两条（V1 不做 rename 继承，§8）；
- A4.3 两次上传壳层级不同（裸根 vs 包一层目录）→ 剥壳对齐后 diff 结果与同壳场景一致；
- A4.4 `.codeaudit-incremental.diff` 及 walkExcludes 同名规则文件不计入 diff；
- A4.5 `diff_hint` 与服务端结果不一致 → 仅任务日志一行（含双方差异摘要），changed/deleted 依服务端；`diff_source=content`；
- A4.6 文件数达解包上限（10 万）量级时 diff 可完成或诚实降级（不 panic 不挂死）。
**测试**：单测矩阵（增/删/改/rename/壳错位/空树/大文件数）；hint 不一致注入；路径口径锁定测试（对齐 findings.file_path 的规范化函数共享）；G7 变异：删掉"剥壳对齐"或"排除文件"条件必须被测试杀死。

#### F5 快照回写与重试复用（§4.4/§4.9）
**验收**
- A5.1 baseline/changed/deleted/git_anchor/diff_source 落 ScanTask 字段并随 PG payload 持久；snapshot/列表 API 透出；
- A5.2 重试**不重算 diff**：删基线目录后 retry，changed_files 与首次一致且任务可走完；
- A5.3 同幂等键重放返回原任务（含全部增量字段）。
**测试**：单测（A5.2 场景）；G4 契约（snapshot 字段）。

#### F6 SAST 增量传导（§4.5）
**验收**
- A6.1 changed 非空 → sast-adapter 仅对清单内文件执行（argv 文件列表；超分批上限走 ADR-144 同款分批合并）；
- A6.2 changed 为空 → SAST 零工具调用，阶段正常 COMPLETE；
- A6.3 增量任务的执行日志含一行视野声明（taint 类跨文件规则以文件为边界，需全量精度请选全量）；
  - `emitIncrementalScopeNotice` 于编排启动时发 WARN 日志（锁定测试 `TestIncrementalScopeNotice_InTaskLog`；非增量/零变更不发）；
- A6.4 全量任务（无增量字段）SAST 行为与现状逐字节一致。
**测试**：sast-adapter 单测（argv 构造/分批/空清单短路）；e2e 04 全量回归；两连扫 e2e 断言第二次仅触变更文件。

#### F7 findings 继承物化（§4.6，双视图数据底座）
**验收**
- A7.1 继承集 = 基线 findings 中 `file_path ∉ (changed ∪ deleted)`；变更/删除文件的旧 findings **不继承**；
- A7.2 继承行：task_id=新任务、`inherited_from_task_id`=基线、finding_id 新分配（`<task>-inh-N`）无冲突；verdict/reasoning、ai_fix_suggestion、diff_patch、CWE 等业务列**原样复制**；
- A7.3 继承项不进融合（RESULT_FUSION 只作用实扫新发现）、不进 AI 工作对象；
- A7.4 空变更任务 → findings 全量继承（全部带继承标记）；
- A7.5 继承写入不破坏 `UNIQUE(task_id,tool,rule,file_path,line_number)`；
- A7.6 web/插件看到的本任务 findings 集合 = 继承 ∪ 新发现（完整视图口径，统计卡/报告无需感知增量）。
**测试**：result-service 单测（过滤矩阵/列复制/ID 分配/UNIQUE 负例/空变更全继承）；G7 变异：删继承过滤条件必须被杀；e2e 两连扫三集合断言（继承/新发现/删除不继承）。

#### F8 AI 增量聚焦提示词（§4.7）
**验收**
- A8.1 增量任务（changed 非空）模式 A/B/C：Assignment 含基线信息+变更/删除清单+聚焦指令+全量代码库指路；`.ai.log`「📋 [任务下发]」帧**全文可见**注入内容；
- A8.2 `incremental_diff` 超预算 → hunks 截断 + `.codeaudit-incremental.diff` 落项目树随 tar 进沙箱 + prompt 指路路径正确；
- A8.3 非增量任务提示词与现状逐字节一致（回归快照）；
- A8.4 模式 D `verifyTurnPrompt` 不注入；`sast_finding_ids` 仅含新发现（继承项不进 AI）。
**测试**：dsh-runtime-service 单测（Assignment 构造/截断策略/文件写入）；e2e 断言任务下发帧含增量段；模式 D 提示词快照回归。
  **实现偏离与补齐** ①"任务下发帧含增量段"的 e2e 断言由 dsh 单测承担（S4 已记录于 run.sh:328 注释）；②A8.3"非增量与现状一致"补齐锁定测试 `TestSandboxAssignment_NonIncrementalUnchanged`（判据函数 `isIncrementalAssignment` 四边界 + 增量任务卡以 ModeA 全文为前缀的基底锚）。

#### F9 降级矩阵（§4.8）
**验收**
- A9.1 无可用基线 → 自动全量 + config `incremental_degraded_reason="no_baseline"` + 插件端明示；
- A9.2 diff 失败（IO/超限注入）→ 全量 + `diff_failed:…` 原因；
- A9.3 显式 baseline 无效 → 4xx（**不降级**，与 A1.2 一致）；
- A9.4 降级任务的结果完整性与全量任务一致（降级路径不引入增量代码的副作用）。
**测试**：单测两降级路径；e2e（新项目首扫带 incremental=true 断言降级原因在 config 可见）。

#### F10 空变更语义（§4.5/§4.6）
**验收**
- A10.1 零变更 → 任务正常 COMPLETED、SAST 零调用、findings 全继承、报告正常生成；
- A10.2 插件端显示"无变更"口径（与 F17 预检提示衔接，预检只是建议，服务端 diff 是权威）。
**测试**：e2e（同包三连扫第三遍零改动）；空清单传导单测。

#### F11 幂等/状态机零侵入（§4.9）
**验收**
- A11.1 状态机转移表零改动（G6 守门用例原样通过）；增量分支只存在于 Prepare+编排内部；
- A11.2 网关幂等键重放、AutoRetry、Pause/Resume 行为不变。
**测试**：G6 12 用例回归；G7 变异抽查。

### B. engine 数据面（S5，D6）

#### F12 树 tar 入桶（§4.10）
**验收**
- A12.1 Prepare 成功后 `trees/<task_id>.tar.gz` 存在，内容=**剥壳后根**（重物化出的路径与直接读卷树逐路径一致）；
- A12.2 tar 入桶失败 → 任务**不因此失败**（主流程不受阻），卷树保留 + 任务日志记账；
- A12.3 tar 体积/上传时长有护栏（超限按 F9 诚实降级路径处理，不静默跳过）。
**测试**：单测 + 集成（模拟栈任务完成后断言桶内对象与卷树一致性）；入桶失败注入用例。

#### F13 卷缓存丢弃——三触发线 + 回收器（§4.10）
**验收**
- A13.1 **TTL 线**：终态 && tar 在桶（HEAD 校验）&& 超 `task.repo_cache_ttl`（缺省 24h）→ 回收器删除卷树；TTL=0 → 终态后下一轮回收即删；
- A13.2 **绝不误删**：运行中/PAUSED/_CREATED 任务的树在任意触发线下都不被删（容量压力下宁可告警）；
- A13.3 **容量线**：卷用量超 `task.repo_cache_max_bytes` → 按终态时间旧→新强制驱逐（tar 前提不变）；无可驱逐对象 → 告警而非删除在跑任务的树；
- A13.4 **孤儿线**：目录在但任务记录不在（含项目级联删除残留）→ 孤儿 TTL（缺省 7d）后清理 + 记账；无 tar 的树（Prepare 中途失败）走孤儿通道，repo 型树清理前必须记账留痕；
- A13.5 **并发安全**：删除=改名隔离（`<dir>→<dir>.gc-<ts>`）再 rm；source-file 正在读的文件句柄不断流，读毕正常；删除期间新请求 miss → 走 F14 重物化；
- A13.6 **memory 档硬保护**：storage=memory 时三条触发线全部停用（构造终态任务断言树仍在）；
- A13.7 **重试无扰**：先驱逐基线/本任务旧树再 retry → retry 重建树并完成（re-prepare 语义）；
- A13.8 **可观测**：每次驱逐有结构化日志（task_id/触发原因/释放字节）；卷用量与驱逐计数暴露为指标。
**测试**：单测（决策函数纯逻辑：TTL/容量排序/孤儿/memory 档矩阵）；集成（模拟栈 `repo_cache_ttl=0` 观察回收闭环 + 桶对象仍在）；并发用例（source-file 循环读同时触发 GC）；G7 变异（删"终态才可删"守卫必须被杀——这是最危险的误删面）。

#### F14 gateway source-file 按需重物化（§4.10）
**验收**
- A14.1 卷树被驱逐后 source-file 仍 200（经重物化+LRU 缓存）；同任务重复读命中缓存不再下载；
- A14.2 缓存字节上限生效（LRU 驱逐最旧）；
- A14.3 memory 档下树与 tar 均失（重启后）→ 诚实 4xx"源码已不可用"（具体码实现期钉死并与现状 404 口径对齐）；
- A14.4 未被驱逐的老任务 source-file 读路径与现状一致（零回归）。
**测试**：gateway 单测（miss→物化→命中→上限驱逐）；e2e 09（source-file 面回归）。

#### F15 部署口径同步
**验收**
- A15.1 `check-wiring.py` 吸收新口径（trees 桶、回收器配置键、env 覆盖）且对存量部署不误报；
- A15.2 `docs/dev-prod-map.md` 同 commit 同步（U7）：新配置键（repo_cache_ttl/max_bytes/orphan_ttl、回收器开关）、桶口径；
- A15.3 sim/prod overlay 配置齐备，`sandbox-deploy.sh check` 全绿。
**测试**：check-wiring 静态审计；sandbox-deploy check。

### C. vscode-plugin（S2）

#### F16 git 锚点采集（§5.1）
**验收**
- A16.1 git 工作区 → anchor={commit 全长, branch, dirty, origin}；请求体正确携带；
- A16.2 非 git / 仓库未初始化 → 空 anchor，无报错无阻塞（增量入口仍可用，纯内容 diff 路径）；
- A16.3 多根工作区取根的规则实现期钉死并写入 README（§9-8）；
- A16.4 untracked 文件计入 dirty 的口径与 diff_hint 一致（同一判定函数）。
**测试**：mocha（vscode.git 桩：正常/无 git/未初始化/dirty 含 untracked 各例）。

#### F17 无变更预检（§5.1）
**验收**
- A17.1 当前 commit == 基线锚点 commit 且双方 dirty=false → 提示"代码较上次扫描无变更"，允许跳过或坚持重扫；
- A17.2 commit 相同但任一方 dirty=true → **不**给无变更提示（内容可能不同）；
- A17.3 非 git 工作区无预检（不报错）。
**测试**：mocha（锚点比对矩阵：同/异 commit × dirty 组合）。

#### F18 变更预告（§5.1）
**验收**
- A18.1 git 工作区选增量时，QuickPick 描述行含"基线锚点后 N commits + M 个未提交文件"；
- A18.2 非 git 工作区无此行，布局不塌陷。
**测试**：mocha。

#### F19 入口询问（D4）
**验收**
- A19.1 有可用基线 → QuickPick 三选（增量(基于 xxxx·时间)/全量/取消）；选中增量 → 建任务请求带 §4.1 全部增量字段；
- A19.2 无基线 → 直接全量，零询问；
- A19.3 取消 → 无上传、无任务、无状态残留（scanning 标志复位）。
**测试**：mocha（doScan 分支 + 请求体断言）。
  doScan 分支用例（QuickPick 三选/选中增量载荷/取消零残留/无基线直全量/无变更预检）见 vscode-plugin test/。

#### F20 增量元数据展示（§5.2）
**验收**
- A20.1 完成提示显示"变更 N · 删除 D · 继承 M · 新发现 K"（数值取平台 snapshot，不做本地推算）；
  **实现口径** "结果树标题"按文首纪律正式修订：树视图经 `registerTreeDataProvider` 注册、无动态 title 通道（需迁移 `createTreeView`），V1 以完成 toast 承担该信息，树标题动态化列为后续增强；
- A20.2 继承项在树视图 description 带「继承」角标（inherited_from_task_id 非空者）；
- A20.3 任务标题/详情显示锚点短 hash（有锚点时）。
  **实现口径** 同 A20.1：锚点短 hash（`@abc1234`）现于完成 toast 呈现，任务标题/详情侧随树标题动态化列为后续增强。
**测试**：mocha（渲染层 + 数据映射）。

### D. web（S3）

#### F21 来源筛选 + CSV（§6）
**验收**
- A21.1 findings 表新增来源筛选（全部/新发现/继承），过滤结果与 `inherited_from_task_id` 空/非空精确对应；
- A21.2 CSV 导出含 `inherited_from` 列（沿用 buildFindingsCsv 纯函数，BOM+RFC4180 口径不破）；
- A21.3 统计卡维持完整视图口径（继承+新发现合计），不因筛选联动统计卡语义（或明确联动——实现期钉死并写进用例）。
**测试**：vitest（筛选/CSV/统计三组）；ui_check 增量任务路径视觉断言。

### E. 契约与端到端（贯穿）

#### F22 契约/文档/proto 同步链
**验收**
- A22.1 `generate-proto.sh` + `check-proto-sync.sh` 绿（根 proto 与 proto/ 副本一致）；
- A22.2 契约三件套（api-external/api-internal/data-flows）含：任务创建增量字段、降级 config 键、inherited_from_task_id、trees 桶与重物化流、回收器配置——断言以文档实测为准（guardrails 文档实测化同款纪律）；
- A22.3 新 ADR 记录增量扫描设计决策（D1-D6 摘要+偏离处）；开发中发现的 bug 入 REGRESSIONS.md。
**测试**：G1 7 检查；文档断言用例。

#### F23 e2e 两连扫全链（S4）
**验收**
- A23.1 模拟栈全链：项目首扫（全量，含 findings）→ 修改 1 文件+新增 1 文件+删除 1 文件 → 二扫（增量）→ 断言：changed=2/deleted=1；继承集=未变更文件的基线 findings（含 verdict 复制）；新发现仅来自变更文件；被删文件 findings 不继承；ai-log 任务下发帧含增量段；报告生成且为完整视图；
    ①"verdict 复制"由标签改为实证断言（继承项 ai_verdict 与基线 keep.py finding 逐值比对，e2e c11）；②"报告生成且为完整视图"补 c11 断言（reports 按 task 过滤非空）；③"ai-log 帧"偏离已在 run.sh 注释与本文件 A8.3 记录（单测承担）。
- A23.2 三扫（零改动）→ 全继承、SAST 零调用、COMPLETED；
- A23.3 降级链：清基线卷树+tar 后发起增量 → 自动全量 + 原因可见；
- A23.4 时长观测：记录同项目全量 vs 增量端到端时长对比（V1 只观测不设阈值硬门）；
- A23.5 存量回归：e2e 01-10 全量用例在增量代码上全绿（60/60 基线不破）。
**测试**：`deploy/tests/run.sh` 新用例（08 为模板）；证据归档本机（不入 git）。

## 3. 全局验收底线（任一切片交付必须满足）

1. **兼容性**：不带增量字段的旧客户端在整套增量代码上行为与现状一致（F1.4/F6.4/F8.3/F23.5 四重回归）；
2. **诚实性**：所有降级（无基线/diff 失败/入桶失败/源码不可用）都有机器可查的痕迹（config 键或任务日志），无静默路径；
3. **误删零容忍**：F13.2/F13.6 是数据面红线——运行中任务树与 memory 档树的误删视为最高级缺陷，必须配 G7 变异条目；
4. **可审计**：AI 提示词注入、hint 不一致、驱逐动作三类关键行为都有日志/指标可查证。
