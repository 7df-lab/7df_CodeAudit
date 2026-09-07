# 内部接口:预期输入与预期输出

[English](internal-interfaces.md) | 中文

本文逐条列出仓库内部组件之间的每一个接口:Cordis 插件契约、服务与事件注册表、agent-loop 回合流、工具管线、LLM 适配器缝、会话状态,以及各能力缝。每条说明调用方必须提供什么、被调方返回或发出什么。仓库外部可见的边界见 [external-interfaces.zh.md](external-interfaces.zh.md)。

## Cordis 插件契约

每个组件都是插件。插件模块以独立命名导出提供 `name`、可选 `inject`、可选 `Config` 与 `apply`;`export default` 被禁止,因为 `Loader.unwrapExports`(`vendor/loader/src/index.ts`)优先取 `.default`,会连 `inject` 在内丢掉整个命名空间(postmortem 0001)。

- `apply(ctx, config)` 的输入:`Context` 与经 `Config` schema 校验的配置对象(Standard Schema;异步校验是 `TypeError`,issues 在加载期拒绝该 fiber)。
- 输出:注册即效果——`ctx.effect()`、`ctx.on()`、服务类、事件监听——各自返回随插件 fiber 卸载而回卷的 disposer。
- 失败:配置错误加载期响亮失败(`dsh: N entries did not activate`、`ValidationError`);插件读取未声明于 `inject` 的服务抛 `cannot get property "<service>" without inject`,对未声明服务的机会式读取必须用 `ctx.get(name)`,绝不用属性代理(仅沿祖先的 fiber 行走穿过外来 shadow 会失败)。

## 服务注册表(ctx 键)

| 键 | 所属包 | 消费方 | 备注 |
|---|---|---|---|
| `ctx.sessions` | `core/session` | agent-loop、持久化、控制器 | 只增日志 + 存储;见下文 |
| `ctx.agents` | `core/agent` | agent-loop、UI 桥 | 活跃注册表 + `agent/*` 事件 |
| `ctx.agentLoop` | `core/agent-loop` | 启动器、SDK/ACP | `AgentFactory`;`create`/`createAgent`/`resume` |
| `ctx.llm` | `llm/llm` | agent-loop、工具、标题 | 适配器注册表 + `llm/stream` 瀑布 |
| `ctx.tools` | `core/tools` | agent-loop、所有工具 | 注册表 + 执行管线 |
| `ctx.systemPrompt` | `core/system-prompt` | agent-loop、工具 | sections/context/variables 组装 |
| `ctx.sessionProjections` | `session/session-projection` | 宿主、UI | 强制投影缝 |
| `ctx.sessionPersistence` | `session/session-persistence` | agent-loop 恢复、检查点策略 | 可选;缺席 → resume 拒绝 |
| `ctx.fs` | `fs/fs` | fs 工具、指令注入 | 可换提供方的文件系统 |
| `ctx.subprocess` | `subprocess/subprocess` | shell、搜索、lsp、终端 | 进程树 spawn |
| `ctx.shell` | `shell/shell` | bash/pwsh 工具 | request/spec 拆分,先 `resolve` 后 `run`/`start` |
| `ctx.terminals` | `terminal/terminal` | 持久 bash/pwsh 工具 | 后端注册表 + 按属主隔离的 PTY |
| `ctx.sandbox` / `ctx.sandboxPolicy` | `sandbox/*` | shell、fs、终端 | argv 禁闭 + 模式策略 |
| `ctx.web` | `web/web` | web 工具 | 搜索/抓取提供方选择 |
| `ctx.skills` | `skill/skill` | skill 工具、`/name` 调用 | 带秩的分层目录 |
| `ctx.subagents` | `subagent/subagent` | 委派工具、工作流 | 提供方注册表 + 可续跑 |
| `ctx.workflowEngine` | `workflow/workflow` | workflow 工具 | worker 线程提供方 |
| `ctx.compaction` | `compaction/compaction` | 压力监听、`/compact` | 区域事务 |
| `ctx.approval` | `interaction/user-approval` | 受沙箱限制的工具 | 审批瀑布 + 审计事件 |
| `ctx.userQuestions` | `interaction/user-questions` | ask-user 工具、计划评审 | 提问瀑布 |
| `ctx.commands` | `interaction/commands` | UI 面、`/name` | 不经模型回合直接派发 |
| `ctx.permissionPresets` | `interaction/permission-presets` | web、会话 | 沙箱+审批组合切换 |
| `ctx.settings` / `ctx.credentials` | `settings/*`、`credentials/*` | 适配器、web Models 页 | 分层用户配置 + 秘密引用 |
| `ctx.typert` | `typert/registry` | 网关、loader | RPC 描述符注册表 |
| `ctx.webhookRuntime` | `webhook/webhook` | 提供方适配器 | 已验证投递派发 |

## 事件域与派发模式

三个事件域:**会话事件**是追加进日志并在 `session/event` 广播的持久事实;**agent 事件**(`agent/*`)观察进行中的工作、随进程消失;**能力事件**(`fs/*`、`tools/*`、`telemetry/*`、`credentials/*`、`skills/change`、`commands/change`、`subagent/*`、`workflow/*`)附加策略或观察某条缝。

派发模式及其规则:

- **瀑布(waterfall)**:监听者由外向内运行,必须调用 `next()` 委托;不调 `next()` 直接返回会否决链上其余监听者。用于 `agent/pre-step`、`agent/request`、`agent/request-error`、`llm/stream`、`tools/pre-execute`、`tools/execute`、`tools/post-execute`、`tools/ptc-dispatch-log`、`fs/write-intent`、`fs/edit-intent`、`approval/request`、`user-questions/request`、`session-telemetry/record`。
- **emit**:同步;契约声明为非否决时派发器逐监听者容错,否则抛错的监听者会饿死后续监听者(Cordis `Array.map` 式派发)。
- **serial**(`agent/turn-stopping`):按序运行,无 `next()`;抛出即成为该回合的错误原因。

## 会话事件(持久日志)

`Session.append(type, data, surfaceIntent?)`(`packages/core/session/src/index.ts`)校验无损 JSON(不可序列化数据在追加点抛出)、赋 `seq = log.length` 与 `time`、深冻结、校验表面位置,然后广播 `session/event`。读取即要求:未识别且无 `ignorable: true` 的事件类型必须让读取者拒绝重建。

| 事件 | 载荷 | 表面 | 备注 |
|---|---|---|---|
| `turn/start` | `{turn}` | 仅日志 | 在认领输入前打开回合 |
| `turn/end` | `{turn, reason: TurnEndReason}` | 仅日志 | 恒为该回合最后一条;kind 含 `completed`、`aborted{reason: user\|parent\|hook\|disposed}`、`blocked`、`error{error}`、`max-tokens`、`interrupted` |
| `step/start` / `step/end` | `{turn, step}` | 仅日志 | 一次模型调用及其工具执行;失败也关闭 |
| `user/message` | `UserMessage` | append | 人类提示、插件注入或目标轮;`source` 区分 |
| `assistant/chunk` | `{turn, step, chunk: StreamChunk}` | 仅日志 | 原样重放保真 |
| `assistant/message` | `{turn, step, message, usage?, interrupted?}` | append,引用 chunk seq | 被取消回合以 `interrupted: true` 固化已送达前缀 |
| `tool/call` | `{turn, step, callId, name, arguments}` | 仅日志 | 模型原始参数,不解析 |
| `tool/result` | `{turn, step, message, error?, meta?}` | append,引用其 `tool/call` seq | `meta` 为工具私有,必须可 JSON |
| `request/header` | `{header: EpochHeader, reason, startsSeries?}` | 仅日志 | reason 含 `initial`/`resume`/`change`/`series`;最新快照重建请求 |
| `request/context` | `{provider, model, contextWindow?}` | 仅日志 | 仅路由变化时记录 |
| `session/end-seed` | `{}` | 仅日志 | 标记构造种子边界;`Session` 是唯一写者 |
| `compaction/start` / `compaction/end`、`hook/invoked` / `hook/result`、`todo/write`、`plan/mode`、`command/run` / `command/done`、`approval/policy`、`permission/preset`、`subagent/descriptor`、`session/title`、`web/deepseek-search-llm-request`、`session/title-llm-request`、`tool-workflow/*` | 插件合并载荷 | 仅日志或 replace | 可扩展词表;见各属主包 |

历史推导:`session.deriveMessages()` 沿表面行走——`append` 节点按序,压缩 `{op: 'replace', start, end}` 节点删除被遮蔽区间——因此模型可见转写是日志的纯函数(`model-visible ⟺ logged`)。`replace` 节点的 `sourceEventSeqs` 必须覆盖它遮蔽的每个节点。

## Agent-loop 回合流

输入:`agent.followup(msg)`(下一回合,唤醒)、`agent.steer(msg)`(下一步,唤醒)、`agent.inject(msg)`(下一步,不唤醒)、`agent.cancel(cause)`、`agent.runMaintenance(job)`。每回合事件序列(`packages/core/agent-loop/src/agent.ts`):

1. `turn/start {turn}`。
2. 认领收件箱输入 → `systemPrompt.assemble` → `agent/pre-step` 瀑布可改写或拒绝;被拒绝或首轮为空即无 step 关闭回合(`turn/end {completed}` 或 `{blocked}`)。
3. `step/start` → 进入的消息逐条 `user/message` → 折叠 `request/header`(首条 `initial`/`resume`,变化 `change`,同 header 新序列 `series`)→ `agent/request` 瀑布定稿调用配置 → `llm.prepareCall` 绑定适配器默认 → `llm/stream` 瀑布 → 每 delta 一条 `assistant/chunk` → 带用量的 `assistant/message`。
4. 每个被请求调用一条 `tool/call` → 工具管线(下文)→ 按模型序逐条 `tool/result` → 只要还有工具调用,step 就欠一次新请求。
5. 无排队输入即将关闭回合前运行 `agent/turn-stopping`(serial)→ `step/end`,然后 `turn/end`。

保证:回合编号跨种子延续;`max-tokens` 在回合内粘滞;流中取消会把已送达前缀固化为 `assistant/message {interrupted: true}`,未派发调用以合成 `tool/result` 错误跳过(`TOOL_ABORTED_BEFORE_DISPATCH`);销毁以 `{aborted, reason: {kind: 'disposed'}}` 关闭未竟回合;一切失败均结构化(`LlmError` 事实原样,否则 `errorChain` 记 `UNKNOWN` 码);监听者抛出绝不破坏边界事件配平。

## 工具管线

输入:`ToolExecutionInput {callId, name, arguments(已解析、冻结), agent?, signal, parent?}`。注册:`ctx.tools.register(ToolDefinition)`,定义携带 `execute(args, exec)`、强制 `output` 声明(JSON Schema + 纯 `render`,可选 `presentationMeta`)、可选 `finalizeContent`、`timeoutMs`(协作式,由超时策略执行)、`isConcurrencySafe`(纯分类器;非 `true` 一律独占)与纯函数 `presentCall`/`presentResult`。

每次调用按序:`tools/pre-execute` 瀑布(拒绝返回 `{kind: 'reject', ...}` 作拒绝结果,否则派发)→ `tools/execute` 包装器(可替换 `exec.signal`;注册表把替换与调用者信号熔合)→ 工具体 → `tools/post-execute` 瀑布(`accept`,或按类型规则替换 `value`/`content`——失败结果的 value 不可替换)→ `tools/result` emit → 调度器 finalize 提交应用了 `concludesTurn`/`additionalContexts` 的 `tool/result`。

调度:独占调用构成屏障;`isConcurrencySafe` 调用进入以 `maxParallelToolCalls`(设置热更)为上限的滚动池;后继调用在启动前重新分类,注册表变化可即时构成屏障。结果按模型序提交;中止会排空已启动调用并为其余调用合成有序错误结果;调度器内部失败直接拒绝、不伪造结果。

## LLM 适配器缝

适配器输入(`LlmAdapter`,`packages/llm/llm/src/index.ts`):一条 `GenerateOptions {provider, model, reasoningEffort?, messages, system?, tools?, temperature?, maxTokens?, stop?, signal?, sessionId?, purpose?}`——loop 构建的请求深冻结并打标(`markAgentLoopRequest`)。

必备输出:`stream(options)` 产出 `StreamChunk` 词表——`block-start`、`text-delta`、`reasoning-delta`、`tool-call-delta`、`block-end`(组装后的块)、`usage`、`finish {reason, replayState?}`——`usage` 严格在终结 `finish` 之前、其后无任何块。可选覆盖:`providerInfo`、`providerRetryPolicy`、`listModels`(仅供参考)、`resolveModel`、`prepareCall`(代际绑定)、`imageRequestPricing`。

注册:`ctx.llm.registerAdapter(providers, adapter)` 全有或全无(`DUPLICATE_ADAPTER`),可经 `handle.replace` 原子换路,并发布 `llm/adapters-updated`。`llm.prepareCall(config)` 返回 `PreparedLlmCall`,其 `stream` 以匹配配置恰好派发一次(否则 `INVALID_PREPARED_CALL`),其 `adapterDefaults` 在 loop 提议下一请求前被剥离。适配器抛出绝不逃出 `LlmRuntime.stream()`:一律归一为终结的 `error`/`aborted` finish。

## 会话存储、fork 与投影

`SessionStore.prepare/enter/announce` 组成创建事务(简单场景用 `ctx.sessions.create`);`prepare(id)` 拒绝重复 id 与非绝对 `meta.cwd`。`fork(source, boundary?, childId?)` 复制前缀、盖 `parentSession`/`seedLength`,并以类型化错误码拒绝:`SESSION_NOT_FOUND`、`SESSION_NOT_LIVE`、`SESSION_ALREADY_EXISTS`、`INVALID_BOUNDARY`、`OPEN_TURN`。

`ctx.sessionProjections` 增量折叠事件:单元声明 `{key, stateVersion, stateSchema, init, apply, wire?}`;不关心的事件返回同一状态引用;读取用 `stateOf(session, key)`,宿主用 `snapshot()` 批量裁剪客户端视图。同 key 不同 `stateVersion` 的注册抛出;随机单元包括 `turnBoundary`(agent-loop)、`todos`、`plan`、`permissions`、`subagent`、`sessionStats`、`title`/`titleInput`、`timeContext`。

## 持久化缝

`SessionPersistence.prepare(id, signal)` 是加载屏障(按 id 串行),返回预备会话;后端在做任何结构解析之前,以"请升级"消息拒绝外来 `SESSION_FORMAT_VERSION`。JSONL 后端在日志之后写(`session/event` → 协调器 → 追加),按 `session-checkpoint-policy` 落检查点(模型派发前、顶层工具派发前、step 边界各刷一次),并以截断修复撕裂尾部。崩溃遗留的未竟回合在重载时以 `turn/end {interrupted}` 关闭。

## 能力缝(提供方可换)

每行:Service Definition → 提供方注册输入 → 消费方输出。

- **fs**:`FileSystem`(`resolve/stat/readText/readBytes/listDir/writeText(target, content, intent)/editText`),乐观版本(`createIfAbsent`/`replaceIfVersion`,不匹配 → `FS_STALE_VERSION`),加 `fs/write-intent`/`fs/edit-intent` 瀑布与 `fs/observed` 记录。提供方:local、sandboxed(仅写围栏)、e2b。工具 `read`/`write`/`edit` 附加补救提示(`FS_STALE_VERSION — re-read the file, then retry`)与沙箱升级字段(仅当 `ctx.fs.sandboxMode` 存在)。
- **subprocess**:`spawn(SubprocessSpawnSpec)` → `SubprocessHandle {pid, collected, done, terminate}`;env 默认为净化后的父环境(剥离 `KEY|PASSWORD|SECRET|TOKEN` 与 `DSH_*`);终止按树 SIGTERM→宽限→SIGKILL。终端原语供 PTY。
- **shell**:`resolve(ShellExecRequest): ShellExecSpec` 后 `run`/`start`;模型可见字段仅 `command`/`workdir`/`timeoutMs`。提供方:本地 bash(`['bash','-c',command]`,`NO_COLOR=1 TERM=dumb PAGER=cat`)、沙箱 bash(受限 argv + `{mode, denied, enforcement}` 事实)、pwsh 镜像。拒绝标记 `[sandbox: file access denied under <mode> mode]`。
- **terminals**:`registerBackend({type, spawn})`;`spawn/send/read/signal/kill/list` 按代理属主隔离;错误码 `DUPLICATE_NAME`、`NO_BACKEND`、`SEND_ACTIVE`;策略要求时后端受限运行。
- **lsp**:`registerProvider({id, extensionToLanguage, query})`(全有或全无,跨提供方扩展冲突 `LSP_CONFLICT`);`query({operation, filePath, position, workspaceRoot})` 按末扩展名路由,产出 `locations` 或 `hover`;无路由 → `LSP_UNAVAILABLE`。
- **web**:搜索/抓取提供方,选择语义 `WEB_PROVIDER_{CONFIGURED_MISSING,CONFIGURED_UNAVAILABLE,UNAVAILABLE,AMBIGUOUS}`;fetch 把非 2xx 视为结果而非抛错;仅同源重定向(`WEB_REDIRECT_BLOCKED`)。
- **skill**:带秩的文件系统提供方根(项目 100/200,自定义 300,用户 400/500,内置 600),`SKILL.md` frontmatter(`name`、`description`,可选 `whenToUse`、`disable-model-invocation`、`user-invocable`),作用域就近优先。
- **compaction**:`compactIfNeeded`/`compactNow`/`compactRegion` 产出 `CompactionResult {summary, shadowedRange, …}`;区域必须保持工具配对平衡;持久 `compaction/start…end` 括号即锁。
- **subagent**:`SubagentProvider {name, capabilities, start(ResolvedSubagentStartRequest) → SubagentRun}`;结果 `stopReason` 来自可扩展映射(`completed`、`aborted`、`error`、`max-tokens`、`refusal`);`prepareContinuable` 的存在即可续跑能力;结构化输出经 `outputSchema`,未捕获时 `completed→error` 降级。
- **workflow**:`start({script, meta, args, subagentProvider, maxTotalAgents, parent, signal})` → `WorkflowRun {result(绝不 reject), cancel, dispose}`;封闭错误码集(`SCRIPT_PARSE`、`META_INVALID`、`AGENT_CAP` 等),`fatal` 决定重抛还是逐项 null;worker 线为 `assertNever` 保护的封闭判别联合。

## 交互缝

- **approval**:`approval/request` 瀑布(按代理作用域)返回 `allowed-once`/`rejected`/`cancelled`/`unavailable`;策略 `never` 在派发前即决,prepend 监听者也无法绕过;持久 `approval/asked`/`approval/decided` 对必须被回合包裹(不变量强制)。
- **user-questions**:`ask()` 带类型化拒绝(`EMPTY_QUESTIONS`、`BAD_INTENT`、`NO_PROVIDER`、`DELEGATED_CALLER`、`ASK_ABORTED`);plan-review 意图校验批准标签属于自身选项。
- **commands**:`/name` 解析(`/^\/([a-z][a-z0-9_-]*)/`)、作用域分层(agent 作用域遮蔽全局)、持久 `command/run`/`command/done` 对、图像仅对声明命令开放。
- **permission-presets**:预设表 `{sandbox, approval}` 以 `permission/preset` 追加,并与 `sandbox/mode`、`approval/policy` 一起折叠进 `permissions` 投影;`custom` 保留。

## Typert RPC 注册表(网关的内面)

`@Remote` 标注的服务把 `InvocationDescriptor`(`namespace`、`method`、参数来源 `json`/`lookup`、codec、取消参数)发布进 `ctx.typert`;lookups 把线上 id 映射到宿主对象(`session` → `SessionId` → 活会话,错误 `session/not-found`)。生成的 `TYPERT` manifest 在加载期校验(`face`、zod schema、成员 kind、已文档化的 invocation);目录门禁要求每条声明事件带 `@mode` 标签并拒绝未文档化的载荷参数。网关把这些描述符经 `POST /api/<namespace>/<method>` 暴露(见 [external-interfaces.zh.md](external-interfaces.zh.md)),参数按描述符精确字段校验(`args fields do not match the descriptor`),错误码遵循 `gateway/*` 词表。
