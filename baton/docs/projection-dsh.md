# DSH 方言投影规范（`.sse.log` → `.tasklog/`）v2

> 投影器族的第一个适配器（实现：`internal/project/projector.go`）。
> 输入**只认** `.sse.log` 原始 SSE 帧；`.ai.log` 是人性化纯文本，禁作输入。

> （`services/runtime-service/internal/sandbox/sandbox.go` sseParser/consume/
> sessionEvent/assistantChunk/assistantAttempt，700–1100 行）——v1 规范遗漏的
> `session.stream`（/R93，0.1.5 逐字实时通道）与 `assistant/message` 内嵌
> stream 语义已在 v2 补齐，以源码为准。

## 帧协议（与 engine sseParser 同构，sandbox.go:705-718）

`event: <名>` 行暂存帧名 + `data: <JSON>` 行成帧 + 空行清空；`: ping` 等注释跳过——
除 `: ts <RFC3339Nano>`（帧接收时刻，engine writeRaw 在 event: 行前写入，
TP17-T3 起；只作用紧随其后的一个帧）。单 data 行假设（bridge.mjs 发送侧）。
主会话常量 `main`（mainSessionID）。

**ts 语义（落地后）**：帧驱动事件 ts=帧接收时刻；扫描前合成事件
（task.created/spine.updated）与 EOF 合成事件（未闭合 tool.result）恒投影时刻
（裁决语义）；旧文件无注释 → 全部降级投影时刻（向后兼容）；非法注释 → 降级+Warning。

## 映射表

| DSH SSE | baton 事件 | 规则 |
|---|---|---|
| `bridge.hello` | —（元数据） | `{provider, model}` 记入 envelope `pen.model`——**模型名唯一来源**；task.created/spine（扫描前发射）无模型名=诚实降级 |
| `session.status{idle}`（main） | — | 主会话收束=回合干净闭合（同 turn/end stop）；子会话 idle 不收敛（对齐 engine 口径） |
| `runtime.exit` | `task.status{FAILED}` | reason 记 data 原文；正常完成走 engine 状态流 |
| `subagent.started` / `subagent.finished` | `task.subagent` | **SDK 协议面真实形状**：started=`{parentSessionId, childSessionId}`；finished=`{provider, agentId, parentSessionId, childSessionId, status: ok|error, stopReason: completed|aborted|error|max-tokens|refusal, lastAssistantMessage?}`——均为顶层通知帧（不入 session.event envelope）；envelope `session=childSessionId`；status/stop_reason 直通（schema 自由 string）；lastAssistantMessage 不投影（子会话消息经 session.event 同通道覆盖）；仅 in-process 子代理有 finished（remote 不报告）。运行时样本待 S2 场景 |
| `session.event{turn/start}` | — | 回合开始（内部状态）；L1 冻结允许点=回合边界 |
| `session.event{user/message}` | `model.message{role:user}` | content 文本块聚合；**系统上下文（"Current runtime context"/`<system-reminder>` 前缀）保留投影**——与人性化流的静默分歧是有意的：中立日志记录模型实见，人性化流面向人 |
| `session.event{assistant/chunk}` | 聚合/`usage.recorded` | `reasoning-delta`/`text-delta` 入缓冲（回合闭合或无 message 兜底时提交为一条 assistant 消息）；`usage` chunk 即时发射（模型名取 bridge.hello；input=未缓存，计费输入=input+cache_read+cache_write）；`finish` chunk=步骤收敛信号，**不再触发发射**（S1 实证修正：finish 后必有 assistant/message，此处发射必与 content 版双记——缓冲保留至 message 重置或回合收束兜底）；`block-start/end`、`tool-call-delta` 忽略 |
| `session.stream{frame.type=chunk}` | 同 assistant/chunk | **0.1.5 逐字实时通道**：chunk 同款 StreamChunk 复用同处理；`start`/`end` 忽略；子会话流静默（对齐 engine）。S1 实证：真实流 usage 唯一载体即本通道的 `usage` chunk（data.message.usage 同值不重复读） |
| `session.event{assistant/attempt}` | 聚合 | 0.1.5 持久层记录（`data.stream`）：`text-chunks`/`reasoning-chunks` 的 `texts[]` 入缓冲；内嵌 `chunk` 记录按 assistant/chunk 处理 |
| `session.event{assistant/message}` | `model.message{role:assistant}` | **message.content 为权威**；内嵌 `data.stream` 与实时通道是同一正文的重复表达，**防双记**（对齐 engine 防双渲染）；content 空则缓冲保留至回合收束兜底。content 块 type=text→text，含 reason/think→reasoning。S1 实证形状：`data.{turn,step,message{content[]},usage{…}}`（usage 在 data 层，与 message 平级） |
| `session.event{tool/call}` | `tool.call` | **call_id 保真真实 `data.callId`（`call_…`，S1 实证；合成 `<session>-t<turn>-c<n>` 仅无 callId 时兜底）**；arguments JSON >阈值(2048B 可配) 或非法 JSON → `args_ref` blob；**S2 审计对回填**：持久层序=tool/call → approval/asked{callId} → approval/decided → tool/result（agent-loop appendToolCall 先于 prepare/审批门）——call 构造后入会话待发缓冲，decided 按 callId 回填挂 `call.approval`，tool/result/回合收束触发发射（保 seq=因果序；帧 ts 快照不随 flush 漂移）；`permission/preset` 会话级预授权**粘性**挂后续所有 call（S1 实证形状 `{preset:"danger-full-access"}`，decided 到达则压过一次） |
| `session.event{tool/result}` | `tool.result` | **真实结果帧**：形状=`data.message.content[]` 块 `type=tool-result{toolCallId,isError,content[].text}`→ outcome completed/error 按 isError、输出文本内联或 blob（Bytes 计字节数）、按 toolCallId 闭合 openCalls；**所属 call 及其之前的待发缓冲先发射**（因果序）。**回合收束合成（v0 近似）降级为兜底**：仅真正未闭合的调用在 turn/end（非 error/main idle）→ `completed`；`error`/中断（EOF 中途、撕裂行）→ `indeterminate`（公理：缺失结果≠没跑过）；被拒（approval denied）→ `denied`（真实帧路径同理：审批拒绝事实压过帧级 isError→error，与收束兜底同口径） |
| `session.event{turn/end}` | —（闭合判定） | **干净收束白名单**：reason.kind ∈ stop/completed/tool-calls → 回合内未闭合调用 completed；**其余一律 indeterminate**（engine 枚举 completed\|blocked\|max-tokens\|interrupted\|aborted\|error，payload-validation.ts turnEndReasonValue——黑名单时代 interrupted/max-tokens/blocked 被误记 completed，公理方向性错误） |

| `permission/preset` | 挂 tool.call.approval | preset→`preset`（会话级粘性） |
| 子会话 `user/message` | `model.message{user, session=child}` | 子任务任务文本 |
| `agent/inbox/spliced`、`todo/write`、`step/*`、`request/*`、`session/title`、`sandbox/mode`、`session.status{running}` | v0 不投影 | 未映射词表；v1 按需评估（todo/write 是 spine.next 候选输入） |

## 投影规则

1. **输出**：`.tasklog/{events.jsonl, spine.json, blobs/, WORKSPACE.json}`；
   seq=行号=因果序=游标；撕裂行（末行 JSON parse 失败）丢弃并记 Warning（含行号）。
   **双段语义（TP17-T2 联动）**：events.jsonl = SSE 派生段（project 全量重写，
   可重算）+ 尾部 handoff.* 生命周期段（append-only；project 重投影原样续写、
   seq 连续重排、ts/载荷保历史原值；中段残留 handoff=脏档案，尾段不保留并
   Warning）。`baton handoff -phase checkpointed` 在 gate 冻结点 append 基线锚
   （log_seq=锚定前行数；幂等防重：尾部锚已覆盖当前全量=SSE 无增长的重复锚定
   则跳过）。
2. **幂等**：events.jsonl/spine.json 全量重写（固定时钟下逐字节一致，测试锁定；

   blobs 只增去重（内容寻址）；WORKSPACE.json 已存在则原样保留——**重跑不换身份**。
   落盘原子性：events/spine/WORKSPACE/blobs 均 tmp+rename。
3. **blob 阈值** 2048B（可配 `-blob-threshold`）；引用 `blob:<sha256 前 16 位>`。
4. **spine v0 浅投影**：goal/constraints 来自任务元数据（CLI 旗标，engine 集成后由
   R7 注入）；facts/decisions/open 由 M3 生成器与 Maka 任期回填。**失败要点机械提炼
   **：projector 扫描完成后从失败 Result
   零 token 提炼 lesson 条目入 spine.facts（kind=lesson；session+要点去重、上限 20
   保最新、错误首行 120 字节截断、FAILED 终态回带 evidence seq；确定性=全量重算
   幂等）。末 K 工具调用观察（observation）**不提炼**——brief 认论纪律=贫 spine，
   kind 留 schema 占位（设计档 §2 同批回填弃做）。
5. **WORKSPACE.json**：首次投影生成（UUID+git_anchor+parent 链），重挂前核对，

   修复静默分叉）。
6. **可观测性**：词表白名单外的未映射帧/事件名按名计数汇总 ⚠
   （不零痕迹丢弃）；`tool/call` arguments 非字符串形状原值入库+告警；旧档案前缀
   他任期事件计数告警（多任期 append 未实现的现状约束）。

## 已知缺口（诚实账，S1 实证清单）

- ~~`.sse.log` 帧无时间戳~~ 已解决：engine writeRaw 帧首
  `: ts` 注释 + baton 帧时刻透传；存量旧文件降级投影时刻（兼容）。

  gw-dc46007…）**：真实流有显式 tool/result 帧（输出文本+isError），已按事实
  映射；回合收束合成降级为未闭合调用的兜底。同批修正：session.stream finish
  提前发射导致的 assistant 双记、真实 callId 保真、permission/preset 粘性挂载。
- ~~**approval/asked·decided 字段形状仍未实证**~~ **S2 已按源码权威定档
  **：S1 假设形状证伪（字段=decision→实为 outcome；启发式归一化
  对 cancelled/unavailable 语义反转）；投影器已按 DSH v1 审计对真实形状修正


  当前依据=发射端源码 schema（payload-validation.ts 校验器+SDK 协议 types）+
  S1/S2 夹具，强度高于运行前假设。
- **16MB 截断**作用在 `.ai.log`（ai_interaction_log.go:157）；`.sse.log` 无截断

  销注原文「待 S1」）；若有轮转/上限，blob 抽取天然免疫（正文已离开原始流）。
- 夹具 `internal/project/testdata/*.sse.log`：basic/torn 为依源码构造的合成流；

  最小化，回归锁=TestRealStreamCalibration）。
