# 数据流转与跨界交互

[English](data-flows.md) | 中文

本文收录跨越多个接口、或不属于任何单条缝的数据流。每条流列出各阶段在每个跳点上的预期输入与输出、守护它的不变量,以及钉住它的测试。逐接口契约见 [external-interfaces.zh.md](external-interfaces.zh.md) 与 [internal-interfaces.zh.md](internal-interfaces.zh.md)。

## 模型可见 ⟺ 已记录的重建流

阶段:提示组装(`systemPrompt.assemble`)→ 从 `deriveMessages()` 构建请求 → `request/header` 折叠 → 提供方调用 → 追加。不变量:模型请求包含的一切都能从会话日志重建——提示文本与工具模式在最新 `request/header`,消息在表面,路由事实在 `request/context`——运行时不变量拒绝失衡或未记录的输入。推论:新的模型可见输入必须新增会话事件。测试:`packages/core/session/tests/request-header.spec.ts`、`packages/core/agent-loop/tests/contract-regressions.spec.ts`、不变量伴随 `packages/core/*/src/invariant.ts`。

## 持久化与崩溃恢复流

阶段:`Session.append` → `session/event` 广播 → 持久化协调器 write-behind 缓冲 → JSONL/SQLite 工件 → 检查点刷写(模型派发前、顶层工具派发前、step 边界)→ 重载。逐跳预期:内存日志是权威;追加失败会截断残缺字节,使重试无 seq 缺口;撕裂的末行按截断修复到最后一条已提交记录;重载日志中的未竟回合以 `turn/end {interrupted}` 关闭且更早事件原样保留;已提交 `turn/end` 之前的损坏拒绝加载。测试:`packages/session/session-persistence-jsonl/tests/jsonl.spec.ts`(崩溃恢复、撕裂尾、seq 缺口)、`packages/session/session-checkpoint-policy/tests/`、`packages/core/session/tests/repair.spec.ts`。

## SDK 事件流到 RunResult

阶段:会话日志追加 → `session/event` 广播 → SDK 服务器 `session.event` 通知 → 客户端订阅队列 → `Session.run()` 活动区间 → `RunResult`。预期数据:通知逐字携带完整持久事件包;运行区间从 prompt 的持久收件回执跨到下一次整代理空闲;`final_response` 选取最后一条已提交的根会话助手文本;`finish_reason` 镜像最后一条根 `turn/end` 的原因 kind;子孙事件经 `subagent.started` 血缘进入 `notifications`,但绝不进入根响应。失败映射:缺字符串原因 kind 的 `turn/end` 是协议违约(`SdkProtocolError`),运行时死亡以退出码加有界 stderr 尾拒绝。测试:`packages/sdk/server/tests/server.spec.ts`、`packages/sdk/client/tests/sdk-client.spec.ts`、`python/sdk/tests/`。

## 快照录制/回放管线

阶段:已录制的 `session.jsonl` fixture(保留头与载荷、省略体包络)→ 声明 profile、组合/header 类与工作区事实的 `snapshot.yml` → 经脚本化模型从发行 profile 回放 → 与已提交的期望输出比对(可变场景对 `workspace.expected/`,未变更文件字节全等)。规则:回放合成被剥离的包络;fixture 使用规范打包行(由 `scripts/migrate-packed-session-fixtures.ts` 迁移);类型化 token 保全父子会话身份;`DSH_SNAPSHOT=refresh` 仅在回放输入仍有效时重导期望输出;快照 harness 在全新运行与 fixture 中拒绝结构化 `UNKNOWN_TOOL` 结果(postmortem 0002)。测试:`packages/test-support/session-snapshot/tests/`、`snapshots/**/*.snapshot.ts`。

## 压缩表面重写流

阶段:压力测量(上下文窗口的阈值比)或 `context-overflow` 错误 → 区域选择(工具配对平衡)→ 摘要调用(`purpose: 'compaction'` 请求)→ 一条持久事务追加 `compaction/start`、带 `replace` 表面操作覆盖 `shadowedSeqs` 的摘要 `user/message`、`compaction/end`。预期数据:`CompactionResult {compactionId, startSeq, summarySeq, endSeq, shadowedRange, shadowedSeqs, shadowedTokenCount}`;replace 节点的 `sourceEventSeqs` 覆盖它遮蔽的每个表面节点;`deriveMessages()` 从重写后的表面重建;手动路径由 `compaction/start…end` 括号串行(并发尝试以 `ManualCompactionError` 失败)。测试:`packages/compaction/compaction-basic/tests/compaction-basic.spec.ts`、`packages/compaction/compaction/tests/tool-pairing.spec.ts`、`packages/core/session/tests/surface.spec.ts`。

## 子代理委派与续跑流

阶段:父工具调用(`subagent`)→ 提供方 `start` → 子会话创建(`parentSession`、`delegationDepth + 1`、子代理首个被接受的 step 上追加 `subagent/descriptor`)→ 驱动运行(`followup` + `whenIdle`)→ `subagent/end` 事件与 `subagent.finished` 通知。预期数据:stop reason 来自可扩展映射;`lastAssistantMessage` 为空时省略;深度上限按持久化头部执行递归预算;续跑经子作用域 `report` 工具向上送达子代理输出、经 `send_message`/`interrupt_agent` 向下传达父指令,并带拒绝伪造发送者的权限校验。测试:`packages/subagent/*/tests/`、`packages/subagent/subagent/tests/invariant.spec.ts`。

## Webhook 投递到会话创建流

阶段:HTTP POST → HMAC 验签 → 有界体读取 → JSON 对象校验 → `VerifiedWebhookDelivery` 快照(深冻结、无损 JSON)→ 每条 kind 匹配规则的 `run()` → 可选 `WebhookSessionRequest` 校验 → 创建事务(工作区 resolve/create、`webhook-<uuid>` 会话、预设应用、标题重命名、带 `source.kind: 'webhook'` 的 `followup` 提示)→ 早已返回的 `202`。预期数据:会话日志中的审计链可追溯投递 id、规则 id 与提供方;失败回滚分离工作区并销毁代理,且不掩盖原始错误;重复投递 id 重跑(无内建去重)。测试:`packages/webhook/webhook/tests/session.spec.ts`、`packages/webhook/webhook-github/tests/handler.spec.ts`。

## 凭据解析与脱敏流

阶段:适配器请求 `CredentialRef`(如 `DEEPSEEK_API_KEY`)→ 阶梯:继承 env → 托管 `.credentials.yaml` → 项目 `.env` → home `.env` → 值(或缺席)。预期数据:继承 env 对写入遮蔽并给出明确的补救消息;空值不可存储;文档强制 `version: 1` 并响亮拒绝未知键;文件 0600 权限在启动时与每次写入前强制。脱敏:诊断只点名引用与代码,绝不点名值;子进程环境剥除凭据形状与 `DSH_*` 名字。测试:`packages/credentials/credentials-local/tests/local.spec.ts`、`packages/credentials/credentials-local/tests/migration.spec.ts`、`packages/subprocess/subprocess/src` 净化助手见 `packages/subprocess/subprocess/tests/service.spec.ts`。

## Worker 与代码运行时边界流

阶段:宿主校验请求 → 以净化环境 spawn worker → 双向封闭线协议 → 有界结果物化。Workflow:`WorkerInit {meta, body, args, limits}` 输入,`WorkerToHostType`/`HostToWorkerType` 判别帧输出(两端 `assertNever`),结果经纯 JSON realm 投影(函数、symbol、bigint、循环、异质原型全拒绝)。代码运行时 JS:`workerData` 启动 + `call`/`reply` 对,日志急切流出使输出在终止后仍可取。代码运行时 Python:相同帧走 fd 3(`PROTOCOL_FD`),由与 Python 侧的常量相等测试镜像。预期失败:worker 死亡、取消宽限到期或输出超限都以结构化错误收束运行,绝不悬挂。测试:`packages/workflow/workflow-worker-thread/tests/`、`packages/code-runtime/code-runtime-worker-thread/tests/`、`packages/code-runtime/code-runtime-python/tests/protocol-mirror.e2e.ts`。

## 遥测采集流

阶段:会话生命周期与事件监听 → 逐记录采集(`ledger`/`ops` 通道,severity 按工具错误、回合错误、agent 错误预映射)→ `session-telemetry/record` 瀑布(脱敏扩展点;抛错的监听者扣下该记录,失败关闭)→ 附匿名用户 id 的 OTLP 导出。预期数据:规范日志永不重写;一个后端故障绝不饿死同伴。测试:`packages/session/session-telemetry/tests/`、`packages/session/session-telemetry-otel/tests/`。

## 测试世界自身的数据流

阶段:`MockAdapter` 以脚本化 `StreamChunk` 应答 → loop 经真实的会话/注册表/管线记录转写 → 不变量预言机从日志重导配平。规则:只在昂贵或非确定性边界(模型、网络、时钟)上 mock;对代理自身输出的关键词探针永远不能替代重读世界(磁盘文件、重跑命令)。测试:`packages/core/agent-loop/tests/mock-adapter.ts`、`docs/testing.md`。
