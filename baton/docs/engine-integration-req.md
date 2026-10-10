# engine 集成需求清单（R1–R8）



> 需求提出、未实施」为首屏旧口径——R1–R8 已全部实施，进度见下方实施进度块与
> 主仓库 dev-prod-map §2.8；本文 R 段落正文保留为需求原文，读时以进度块为准。）**
> 任何一条落地前须：engine 子仓认领（主仓库 U3）→ 遵守 engine/AGENTS.md 工作循环 → ADR → 门禁。

>
> 联调场景与验收对应关系见 `integration-test-plan.md`。
>
> **实施进度**：
> - R1+R7 最小批已实施——engine TP17-T1（62b27ca5，）：回合收束投影钩子 +
>   BATON_BIN 子进程解耦 + goal=任务卡摘要注入；
> - R2+R3 最小批已实施——engine TP17-T2（08df9d57，）+ baton 717f889：
>   WaitFrozen 冻结回调（进入挂起=真实定格点，重投影+checkpointed 锚定）+
>   handoff 子命令 + project 双段语义；git-anchor 锚已接，**tree_ref 仍缺**
>   （沙箱工作区 tar 导出链路）；
> - R5 已实施——engine TP17-T3（996fc71b，）+ baton 74472df：writeRaw
>   帧首 ': ts' 注释（SSE 合法注释双向兼容）+ 投影 ts 透传（帧驱动事件=发生时刻，
>   旧文件降级投影时刻）；
> - R7 完整版已实施——engine TP17-T4（b4f388b9，）：proto TaskOrigin
>   （tenant/user/constraints）四请求贯通 → baton -tenant/-user/-constraint 旗标
>   （user=created_by；constraints=config.ai_constraints 约定键；tenant 单租户=空）；
>   R6 首批：agent_id/agent_bridge_cmd cfg 驱动（-agent 贯通）；
> - R6 unblock 前提达成——Maka 本体已下载实测（Apache Maka Incubating，npm
>   nightly 钉 0.2.0-dev.39.20260916）：headless `maka run`/ACP 双面、
>   RuntimeEventLog↔baton 九族映射（含原生 handoff 同构）、凭据经网关纪律、
>   session-export 档案交换面——设计定稿 baton/docs/maka-integration.md
>   。实施排 S2 Maka 沙箱化实证后（先证一单再固化接口）；
> - R4+R6 完整版 v0+tree_ref 已实施——engine 切片 1：
>   R6 descriptor 化（configs/agent_providers.yaml 条目驱动 4 处消费点，mock-stdio
>   方言全路径闭环验收；Maka 条目待 S2 沙箱化实证）；R4 handoff hold（teardown/对账
>   跳过+TTL 缺省 30m 后对账回收）；tree_ref（冻结点工作区 tar.gz sha256 内容寻址
>   →本 CLI -tree-ref 传入；v0 落点=tasklog 换手种子卷，跨主机桶引用随 manager API
>   评估升档——handoff 事件 tree_ref 可离线解包回源码树，验收达成）；

>   `BATON_HOST_BIN_DIR`（缺省宿主目录 `/opt/codeaudit-sim/bin` 投放静态二进制，
>   缺失=钩子诚实降级禁用）；产出落 `agent_repos` 卷

>   三投影缺陷随批修正=baton 52b8338；证据 `.agent/evidence/baton-s1-20260917/`
>   本机）；

>   （缺省 `/opt/codeaudit/bin`）；供给链=主仓库 `deploy/internal/baton-provision.sh`

>   构建批末尾双入口，`BATON_PROVISION=0` 旁路，无 Go 或无 baton 检出台=警告
>   跳过不阻塞）。活口径以主仓库 `docs/dev-prod-map.md` §2.8 为准；
> - S1 两开放项已收口：approval 审计对+subagent SDK 形状按发射端
>   源码权威修正入投影器（S1 假设形状证伪——decided 字段实为 outcome、
>   cancelled/unavailable 曾误归 granted；时序实证=审计对在所属 tool/call 之后
>   到达，改缓冲回填。详见 projection-dsh.md）；运行时样本待场景编排；




## R1 投影器触发钩子（S1/S2 前置）

- **现状**：`baton project` 是离线 CLI，无人调用。
- **需求**：engine 在两个时机调用投影器——①任务分析回合结束后（增量投影，
  `.sse.log` 只增不改，幂等重跑即可）；②换手冻结时（产出 handoff.checkpointed 基线）。
- **建议形态**：`go run`/子进程调用 baton CLI（解耦，baton 不进 engine 依赖树）；
  调用点候选 `dsh_agent.go` 分析收束处与 gate 冻结路径（R2）。
- **验收**：sim 栈跑一单真实任务，`.tasklog/` 自动产出且 events.jsonl 与 CLI 手跑一致。

## R2 gate 冻结点接换手流程（S2 前置）

- **现状**：`pause.go:69-102` PauseSession/GateFor + `session.go:65-83` 每回合前
  `gate.Wait`；上游 `task_service.go:938-1002` PauseTask→PauseAnalysis 链路已通。
- **需求**：gate 挂起处插入换手序列：收笔（pen.revoked 语义）→ 触发投影（R1）→
  工作区快照（R3）→ 等待新执行者。换手期间任务状态用 PAUSED。
- **验收**：sim 栈上 DSH 任务可在此点冻结且恢复后续跑不丢回合。

## R3 工作区快照引用（S2 前置）

- **现状**：`task_service.go:499-516` uploadTreeTar 已把源码树 tar 入桶。
- **需求**：快照的桶引用作为 `handoff.checkpointed.workspace.tree_ref` 传入投影器
  （`-git-anchor` 同理）；新 agent 重挂工作区以此为准（快照为真相，Q14 共识）。
- **验收**：handoff 事件里的 tree_ref 可被离线解析回源码树。

## R4 沙箱保活标记（S2 优化项，非正确性前提）

- **现状**：`session.go:364-394` RunSession teardown 必删沙箱；`reconciler.go:37-67`
  孤儿对账器删非活跃沙箱。
- **需求**：换手窗口内加 "handoff hold" 注册项，teardown/对账跳过——**仅为免重解包
  的性能优化**；正确性由 R3 快照保证（沙箱是易失资源，架构不许依赖其实例连续性）。
- **验收**：换手窗口内沙箱存活；窗口后正常回收。

## R5 writeRaw 帧时间戳（时间线精度前置）

- **现状**：`ai_interaction_log.go:169-173` writeRaw 落原始帧，**无时间戳**——baton v0
  的 `ts` 只能记投影时刻（projection-dsh.md 已知缺口）。
- **需求**：writeRaw 每帧附接收时刻（帧前缀注释行或行内字段，格式需与 baton 约定，
  向后兼容旧无时间戳文件）。
- **验收**：新 .sse.log 投影出的 `ts` 为事件发生时刻；旧文件仍可投影（降级）。

## R6 可插拔 agent provider descriptor（S2 的 Maka 接入前置）

- **现状**：换一个 agent 要改 4 处硬编码——bridge 拉起脚本（`session.go:161-171`）、
  bridgeSession SSE 协议（`sandbox.go:558-665`）、结果解析 parseAuditResult/
  parsePatchFixResult（`sandbox.go:416,455`）、任务卡 prompt 模板
  （`sandbox_analysis.go:219`）。服务名 dsh-runtime-service 绑死 DSH。
- **需求**：4 处硬编码改为 config 驱动的 provider descriptor（launch/方言标识/结果
  解析器引用/prompt 模板引用）；服务演进为通用 runtime（更名含 proto/compose 联动）。
  DSH 是第一个 descriptor 条目；Maka 第二个（headless CLI 驱动）。
- **验收**：加一个 descriptor 条目即可拉起新 agent，4 处代码零改动。
- **状态**：已实施（engine 切片 1/）——验收经 mock-stdio 条目全
  路径闭环（设计允许口径）；Maka 条目接入待 S2 沙箱化实证（本仓 maka-integration.md 设计已定稿）。

## R7 任务元数据供给（S1/S2 前置）

- **现状**：baton 的 goal/constraints/tenant/user 目前靠 CLI 旗标手工传。
- **需求**：engine 调用投影器时从任务 config 自动注入（goal=任务卡摘要、
  constraints=任务配置约束、tenant/user=任务发起者）。
- **验收**：投影产物脊柱字段与 web 任务详情一致。

## R8 usage 静默丢弃——无需改动（说明条目）

- **现状**：`sandbox.go:974-976` 将 usage/finish chunk 从人性化流静默丢弃。
- **说明**：**不改**。原始帧完整保留在 `.sse.log`，baton 投影器已补收
  （`usage.recorded`，模型名取 bridge.hello）。本条仅备案，防后人误判为缺口。
