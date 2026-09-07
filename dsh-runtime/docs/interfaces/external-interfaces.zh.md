# 外部接口:预期输入与预期输出

[English](external-interfaces.md) | 中文

本文逐条列出仓库之外的调用方进入或离开运行中 harness 的每一个边界。每个接口给出预期输入、预期输出、调用方必须预期的失败行为,以及钉住该行为的测试。进程内部行为见 [internal-interfaces.zh.md](internal-interfaces.zh.md);跨越多个接口的数据流见 [data-flows.zh.md](data-flows.zh.md)。

## `dsh` 启动器命令行

- 入口:`apps/cli/src/bin.ts`、`apps/cli/src/args.ts`;语法测试:`apps/cli/tests/args.spec.ts`,构建产物验收:`apps/cli/tests/built-bin.e2e.ts`。
- 输入:`dsh [--profile <name>] [--patch <path>]... [--dump-config | --dump-default-config] [args...]`,`web` 子命令(`--profile web` 的别名),以及 `plugin` 子命令(`dsh plugin --profile <name> <pnpm args...>`)。启动器旗标必须在前;启动器不认识的第一个 token 起属于被启动应用的 argv,原样透传(包括它自己的 `-h`)。
- 输出:`profile`/`web` 启动一个应用;`--dump-config` 在 stdout 打印 YAML 条目树(按 `# == <origin>` 出处注释分组,`!!js` 表达式原样打印、不求值);`--dump-default-config` 只打印 bundle 层;`plugin` 转发执行一次 pnpm。
- 失败:缺失或空的 `--profile`、`--patch ''`、两个 dump 旗标同用、dump 携带应用参数、`plugin` 无 pnpm 参数时,以 `error: …` 退出码 1 结束。裸 `dsh -h` 打印启动器帮助,退出码 0。不匹配任何模板的未知 profile 以 `dsh: profile "<name>" does not exist; create it with 'dsh plugin --profile <name> add <package>'` 失败。
- 退出码:启动器/应用用法错误为 1;启动失败为 1;`SIGTERM` 为 0;第一次 `SIGINT` 为 130(第二次强制退出);优雅收尾 5 秒后强制退出;`plugin` 返回 pnpm 的退出码,pnpm 缺失时为 127 并打印 `dsh: pnpm not found on PATH — install pnpm to manage profile plugins`。

## Profile 与组合文件

- 入口:`packages/boot/app-boot/src/profile.ts`、`apps/cli/src/profile-boot.ts`;测试:`packages/boot/app-boot/tests/profile.spec.ts`、`apps/cli/tests/built-bin.e2e.ts`。
- 输入:`$DSH_HOME`(默认 `~/.dsh`;空/空白 `DSH_HOME` 视为未设置)下的 `profiles/<name>/`,含 `package.json`(`dsh.profile.bundles`、`dsh.profile.patchReload`)、`cordis.patch.yml`(补丁条目的顶层 YAML 数组)和 `pnpm-workspace.yaml`。随发行模板 profile:`web`、`headless`、`sdk`、`sdk-minimal`、`acp`。
- 层序(同一行 id 后者覆盖):按列出顺序的 bundle 层 → 该 profile 的用户 `cordis.patch.yml` → `$DSH_HOME/cordis.patch.yml` → 按序的 `--patch` 覆盖层 → 当 `DSH_TELEMETRY_DISABLED` 非空时的禁用遥测补丁。`patchReload: live` 随用户层编辑重新组合;`startup` 一次性应用全部层。
- 输出:组合后的 Cordis 条目树;`!!js` 表达式仅在插件 `config` 与条目 `disabled` 下合法(`docs/cordis-primer.md#loader-configuration`),出现在其他位置会被 `verify-cordis-config` 拒绝。
- 失败:启动即响亮失败——`dsh: patches <file> must be a top-level YAML array of loader patch entries`、`dsh: profile bundle "X" declares no dsh.bundle in its package.json`、无法解析的行报 `dsh: N entries did not activate … (waiting for services: …)`。

## 环境变量

- 入口:`packages/boot/app-boot/src/index.ts`(`loadLayeredEnv('dsh')`);测试:`packages/boot/app-boot/tests/app-boot.spec.ts`。
- 信任顺序:继承的 `process.env` → 调用目录的 `.env`(项目层)→ `$DSH_HOME/.env`(用户层)。`.env` 的值永不覆盖已继承的变量。仅限启动环境的名字(`PATH`、`HOME`、`NODE_OPTIONS`、`DEEPSEEK_BASE_URL`、`HTTP_PROXY`、一切 `DSH_*` 前缀名等保留名单)出现在任何 `.env` 中都会以 `dsh: <path> sets "<NAME>", which only the launching environment may set …` 拒绝并使启动失败。
- 下游消费的关键变量:`DSH_HOME`(home 覆盖)、`DEEPSEEK_API_KEY`(凭据回退;无法解析时请求以 `LlmError` `MISSING_CREDENTIAL` 失败)、`DSH_TELEMETRY_DISABLED` / `DSH_TELEMETRY_MODE` / `DSH_TELEMETRY_OTLP_URL`、`DSH_TOOLS_MODE`(`native|ptc|both`)、`DSH_PERMISSION_MODE`(默认 `workspace-write`;`danger-full-access` 隐含审批 `never`)、`DSH_MAX_TOKENS_AS_SUCCESS`(sdk profile;非法 JSON 使启动失败)、`DSH_CONTEXT_WINDOW` 与 `DSH_SYSTEM_PROMPT`(sdk-minimal 默认值)、`DSH_WEB_URL`(仅输出,对 bash 可见)、`DSH_SNAPSHOT=replay`(测试缝,把 `cordis.yml` 换成 `cordis.snapshot.yml`)、`E2B_API_KEY`(e2b POC)、`EXA_API_KEY` / `PERPLEXITY_API_KEY`(搜索提供方)。

## `dsh --profile headless "<task>"` — 单发运行

- 入口:`packages/bundle/headless/src/`;测试:`packages/bundle/headless/tests/`、`apps/cli/tests/built-bin.e2e.ts`。
- 输入:位置参数任务词(以空格连接);空或纯空白任务是用法错误 `error: a task is required, for example: dsh --profile headless "run the tests"`。
- 输出:stderr 前缀 `dsh: reasoning:` 的推理文本;stdout 打印最终助手文本加换行。
- 退出码:当且仅当最后 `turn/end` 原因 kind 为 `completed` 时 0;否则 1,`error` kind 另在 stderr 打印 `dsh: <error.code>: <error.message>`。

## `dsh web` — 浏览器应用

- 入口:`packages/bundle/web-app/src/`、`packages/host/webserver/`;测试:`apps/cli/tests/web-auth.e2e.ts`、`packages/bundle/web-app/tests/`。
- 输入:`--host <host>`(回环默认;`0.0.0.0` 以安全消息拒绝)、`--port <port>`(默认 3080)、`--no-open`、`--trusted-host <authority...>`。
- 输出:stdout 打印 `dsh web: <authenticatedUrl>`(`0.0.0.0` 绑定追加 ` (LAN: <url>)`),为派生 shell 注入托管的 `DSH_WEB_URL` 环境,除非抑制否则移交默认浏览器。
- 认证:启动 URL 携带一次性 `?token=`,GET 以 `303` 兑换为 `HttpOnly; SameSite=Strict` cookie(`dsh-auth-<authority-hash>`);未认证请求得 `401 dsh web authentication required; reopen the URL printed by dsh web.`;不受信任的 `Host`/`Origin` 得 `403`。
- RPC 载体:`POST /api/<namespace>/<method>`,JSON 体 `{ type: 'client-request', rpcId, method, payload }`,应答 `{ type: 'server-response', rpcId, result: { ok: true, value } | { ok: false, error: { code, message, details } } }`;非 JSON content type 得 `415`,超过 300 MiB 体积上限得 `413`,被认领端点上的异己 method 得 `404`。流复用于 `/api/remote.mux` 上的单条 WebSocket,文本帧 `{ type: 'open'|'cancel'|'item'|'error'|'end', streamId, … }`;关闭码 `1003`(二进制帧)、`1008`(非法帧/重复 id)、`1011`(终态错误无法送达)。

## `dsh --profile sdk` / `sdk-minimal` — JSON-RPC stdio 运行时

- 入口:`packages/bundle/sdk-app/`、`packages/sdk/server/`、`packages/sdk/protocol/`;测试:`packages/sdk/protocol/tests/transport.spec.ts`、`packages/sdk/server/tests/server.spec.ts`、`apps/cli/tests/profiles/sdk/keyless-smoke.e2e.ts`。
- 输入:stdin 上的换行分隔 JSON-RPC 2.0(无 Content-Length 头;畸形行忽略;非对象 `params` 归一为 `{}`)。请求:`initialize` `{cwd, provider, model, reasoningEffort?, maxTokens?}`、`session/prompt` `{sessionId, contentBlocks}`(未知 id 惰性创建会话;图像块携带规范 base64 `data` 与 `mimeType` png/jpeg/webp/gif)、`shutdown`(无参数)。请求 id 由客户端铸造。
- 输出:stdout 只输出成帧的 JSON-RPC。`initialize` → `{ serverInfo: { name: 'deepseek-harness-sdk-runtime', version } }`;`session/prompt` → `{ messageId }`;`shutdown` → `{}` 后退出码 0。通知:`session.event` `{sessionId, event}`(每个会话的完整会话日志事件包)、`session.status` `{sessionId, status: 'idle'|'running'}`、`subagent.started` `{parentSessionId, childSessionId}`、`subagent.finished` `{provider, agentId, parentSessionId, childSessionId, status, stopReason, lastAssistantMessage?}`。
- 失败:错误帧 `-32601 method not found: <m>` 与携带处理器消息的 `-32603`;`initialize` 之前 `session/prompt` → `SDK server is not initialized`;未知 provider → `no adapter registered for provider "<p>"`;就绪前 stdin EOF → 启动失败退出码 1。
- 版本钉:TypeScript 客户端拒绝与其自身版本不同的 `dsh` 依赖(`dsh SDK client <v> requires the same dsh version, got <v>`)。

## Python SDK(`deepseek-harness-sdk`)

- 入口:`python/sdk/`、`python/sdk-runtime/`;测试:`python/sdk/tests/`,快照对应物 `scripts/snapshots/python-sdk-single-exe/`。
- 输入:`DeepSeekHarness(dsh_home=…, cwd=…, profile='sdk'|'sdk-minimal', provider=…, model=…, reasoning_effort=…, max_tokens=…, patches=(…), base_url=…, api_key=…, initialize_timeout_seconds=30, …)`。`dsh_home` 必填——SDK 绝不探测 `~/.dsh`;它以子进程启动绑定的 `dsh --profile <profile>`。
- 输出:`harness.run("task", session_id=…)` → `RunResult(session_id, final_response, finish_reason, events, notifications)`;`final_response` 是本次运行区间内最后一条已提交的根会话助手文本,`finish_reason` 是最后一条根 `turn/end` 的原因 kind。
- 失败:握手超时的报错点名 profile 并保留运行时诊断;`turn/end` 缺少字符串 `data.reason.kind` 抛 `SdkProtocolError`;缺少 SDK server 行的 profile 在启动期失败,无回退。

## `dsh --profile acp` — Agent Client Protocol

- 入口:`packages/acp/acp/src/index.ts`;测试:`packages/acp/acp/tests/bridge.spec.ts`、`…/turns.spec.ts`、`apps/cli/tests/built-bin.e2e.ts`。
- 输入:stdio 上换行分隔 JSON 的 ACP:`initialize`、`authenticate`、`session/new`(绝对 `cwd`;`additionalDirectories` 被拒)、`session/list`(`[createdAt, sessionId]` 键集游标,强制规范编码)、`session/resume`、`session/close`、`session/set_session_config_option`(`model`、`reasoning_effort`)、`session/prompt`(text、`resource_link`、已宣告的 `image`;audio 拒绝;每会话同时至多一个 prompt)、`session/cancel`。
- 输出:`initialize` → `agentInfo.name === 'deepseek-harness-acp'` 与固定能力;有序 `session/update` 通知(`agent_thought_chunk`、`agent_message_chunk`、`tool_call`/`tool_call_update`、`usage_update`、`config_option_update`);`session/request_permission` 瀑布(选项 `allow-once`/`reject-once`);prompt 应答 `{ stopReason }`(由 turn 结束映射:`completed→end_turn`、`max-tokens→max_tokens`、`interrupted→cancelled`)。
- 失败:保留细节的 `invalidParams`/`internalError` 请求错误;未知会话的取消静默忽略;客户端断连退出码 0;stdout 只承载协议帧。

## Webhook 入口(GitHub 适配器)

- 入口:`packages/webhook/webhook-github/src/handler.ts`、`packages/webhook/webhook/src/`;测试:`packages/webhook/webhook-github/tests/handler.spec.ts`、`packages/webhook/webhook/tests/`。
- 输入:`POST <配置路径>`,`Content-Type: application/json`,`x-hub-signature-256`(以凭据解析出的密钥对原始体做 HMAC-SHA256)、`x-github-delivery`、`x-github-event`,以及 `maxBodyBytes` 内的 JSON 对象体。
- 输出:已验证的投递在内存中派发后返回 `202` 空体(`{kind: 'github', source, deliveryId, event: {name, payload}, receivedAt}`);规则异步运行,重复投递 id 有意重跑。
- 失败:非 POST `405`(`allow: POST`),content type 错误 `415`,缺失/空白头或非 JSON 体 `400`,超限体 `413`,`401 invalid webhook signature`,密钥或运行时不可用 `503`。错误体不回显任何请求数据。

## 会话存储工件

- 入口:`packages/session/session-persistence-jsonl/src/format.ts`、`…/session-persistence-sqlite/src/schema.ts`;测试:`packages/session/session-persistence-jsonl/tests/jsonl.spec.ts`。
- 布局:`<root>/<projectKey>/<encodedSessionId>/session.jsonl`(开启压缩时为 `.jsonl.zstd`)。`projectKey` 是 cwd 的有界可读 slug(`--` 前后缀,缺省 `_no-cwd`);`encodeSegment` 把任意会话 id 单射映射为一个安全路径段(`~XXXX` 转义,`.` → `~002E`),id 因此无法穿越或碰撞。
- 头行:首条记录 `{type: 'session', version, id, createdAt, cwd?, parentSession?, seedLength?, origin?, delegationDepth, agentPreset?}`;`version` 必须等于 `SESSION_FORMAT_VERSION`(当前为 `0`),否则加载以"请升级"的拒绝失败,绝不报损坏。已退役字段(`sandboxMode`、`approvalPolicy`)被拒绝。
- 事件行:每行一条 JSON 记录(delta 游程打包为 `text-chunks`/`reasoning-chunks`/`tool-call-chunks` 行),`seq` 从 0 连续,`sourceEventSeqs` 区段编码。撕裂的末行以截断修复;已提交 `turn/end` 之前的 seq 缺口或畸形记录按损坏处理并拒绝加载。
- SQLite:单调 `SCHEMA_VERSION`;不兼容数据库被拒绝,不迁移。

## LLM 提供方线缆(DeepSeek 适配器)

- 入口:`packages/llm/llm-deepseek/src/sse.ts`、`…/adapter.ts`、`…/translate.ts`;测试:`packages/llm/llm-deepseek/tests/`,真实 API `packages/llm/llm-deepseek/tests/*.e2e.ts`(无 `DEEPSEEK_API_KEY` 自跳过)。
- 输入:`POST /chat/completions` 风格请求,`Authorization: Bearer <key>`,每个请求携带 `attributionHeaders()` 的归因头,回合用 `stream: true`;工具映射到 provider `tools` 字段;`stop` 序列被尊重。
- 输出:以字面 `[DONE]` 结束的 SSE 帧;delta 翻译为 `StreamChunk` 词表,`usage` 在终结 `finish` 之前、其后无任何帧。
- 失败:`[DONE]` 前 EOF → `LlmError('STREAM_CLOSED')`,消息内嵌传输诊断(事件数、注释心跳、末帧年龄、时长),stderr 行点名 provider 主机、HTTP 状态与模型;HTTP 头无法承载的凭据以 `INVALID_CREDENTIAL` 拒绝并点名凭据引用,绝不回显值。稳定的 `LlmError` 代码包括 `AUTH`、`RATE_LIMIT`、`NO_ADAPTER`、`MISSING_CREDENTIAL`、`INVALID_CREDENTIAL`、`INVALID_ADAPTER`、`DUPLICATE_ADAPTER`、`REGISTRATION_DISPOSED`、`INVALID_PREPARED_CALL`、`STREAM_CLOSED`。

## Hook 桥(Claude Code / Codex 方言)

- 入口:`packages/hooks/hook-protocol/src/`、`packages/hooks/hooks-claude-code/src/`、`packages/hooks/hooks-codex/src/`;测试:`packages/hooks/*/tests/`。
- 输入:hook 命令在 stdin 收到一条 JSON 载荷(Claude 方言带尾换行,Codex 不带),cwd = 会话工作区,外加方言专属 env(`CLAUDE_PROJECT_DIR`)。退出码 2 表示阻断;退出码 0 且 stdout 以 `{` 开头可携带 `continue`、`stopReason`、`decision`、`reason`、`systemMessage` 或 `hookSpecificOutput` 块(`hookEventName` 必须匹配预期事件,否则其字段被丢弃;`permissionDecision` 覆盖顶层 `decision`)。
- 输出:每个挂点一条合并结果——`deny > ask > allow`,首条 `continue: false` 连同其 `stopReason` 粘滞,`additionalContext` 按钩子顺序累积;持久的 `hook/invoked` 与 `hook/result` 会话事件记录运行,stderr 摘要截断到 500 字符。
- 失败:spawn 失败或超时(默认 600 秒)是非阻断错误;非法正则匹配器永不匹配;未知 decision 被忽略而非猜测。

## 沙箱 runner CLI(原生)

- 入口:`native/landlock-run/docs/cli-contract.md`、`native/landlock-run/packages/entry/src/index.ts`;测试:`native/landlock-run/test/`、`packages/sandbox/sandbox-local/tests/landlock.e2e.ts`。
- 输入:`landlock-run [--ro <path>]... [--rw <path>]... -- <argv>...`(`--` 分隔符强制)或 `landlock-run --probe`。
- 输出:exec 成功后子进程退出状态原样透传;probe 精确打印 `landlock: fully enforced` 或 `landlock: partially enforced (older ABI)` 并以退出码 0 结束。
- 失败:一切 launcher 级失败以退出码 125 加 `landlock-run: ` 致命 stderr 行结束;因此归因要求退出码 125 **且**该行同时成立(postmortem 0004)。部分 ABI 下的受限运行会打印信息行 `landlock-run: partial enforcement (older Landlock ABI)`,该行被排除在致命分类之外。
