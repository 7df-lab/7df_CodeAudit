# Internal interfaces: expected input and output

English | [中文](internal-interfaces.zh.md)

One reference row per interface between components inside the repository: the Cordis plugin contract, the service and event registries, the agent-loop turn flow, the tool pipeline, the LLM adapter seam, session state, and the capability seams. Each entry states what a caller must supply and what the callee returns or emits. Boundaries visible from outside the repository are in [external-interfaces.md](external-interfaces.md).

## The Cordis plugin contract

Every component is a plugin. A plugin module exports `name`, optional `inject`, optional `Config`, and `apply` as separate named exports; `export default` is forbidden because `Loader.unwrapExports` (`vendor/loader/src/index.ts`) prefers `.default`, which would discard the namespace including `inject` (postmortem 0001).

- Input to `apply(ctx, config)`: a `Context` and the config object validated through the `Config` schema (Standard Schema; async validation is a `TypeError`, issues reject the fiber at load).
- Output: registrations are effects — `ctx.effect()`, `ctx.on()`, service classes, event listeners — each returning a disposer that unwinds when the plugin's fiber unloads.
- Failure: misconfiguration fails loud at load (`dsh: N entries did not activate`, `ValidationError`); a plugin reading a service outside its declared `inject` throws `cannot get property "<service>" without inject`, and an opportunistic read of an undeclared service must use `ctx.get(name)`, never the property proxy (the ancestor-only fiber walk fails through foreign shadows).

## Service registry (ctx keys)

| Key | Owner package | Consumed by | Notes |
|---|---|---|---|
| `ctx.sessions` | `core/session` | agent-loop, persistence, controllers | append-only log + store; see below |
| `ctx.agents` | `core/agent` | agent-loop, UI bridges | live registry + `agent/*` events |
| `ctx.agentLoop` | `core/agent-loop` | launchers, SDK/ACP | `AgentFactory`; `create`/`createAgent`/`resume` |
| `ctx.llm` | `llm/llm` | agent-loop, tools, title | adapter registry + `llm/stream` waterfall |
| `ctx.tools` | `core/tools` | agent-loop, every tool | registry + execution pipeline |
| `ctx.systemPrompt` | `core/system-prompt` | agent-loop, tools | sections/context/variables assembly |
| `ctx.sessionProjections` | `session/session-projection` | hosts, UI | mandatory projection seam |
| `ctx.sessionPersistence` | `session/session-persistence` | agent-loop resume, checkpoint policy | optional; absent → resume refuses |
| `ctx.fs` | `fs/fs` | fs tools, instructions | provider-swappable filesystem |
| `ctx.subprocess` | `subprocess/subprocess` | shell, search, lsp, terminal | process-tree spawn |
| `ctx.shell` | `shell/shell` | bash/pwsh tools | request/spec split, `resolve` then `run`/`start` |
| `ctx.terminals` | `terminal/terminal` | persistent bash/pwsh tools | backend registry + owner-scoped PTYs |
| `ctx.sandbox` / `ctx.sandboxPolicy` | `sandbox/*` | shell, fs, terminal | argv confinement + mode policy |
| `ctx.web` | `web/web` | web tools | search/fetch provider selection |
| `ctx.skills` | `skill/skill` | skill tool, `/name` invocation | scoped catalog with ranks |
| `ctx.subagents` | `subagent/subagent` | delegation tools, workflows | provider registry + continuable runs |
| `ctx.workflowEngine` | `workflow/workflow` | workflow tools | worker-thread provider |
| `ctx.compaction` | `compaction/compaction` | pressure listener, `/compact` | region transactions |
| `ctx.approval` | `interaction/user-approval` | sandboxed tools | approval waterfall + audit events |
| `ctx.userQuestions` | `interaction/user-questions` | ask-user tool, plan review | question waterfall |
| `ctx.commands` | `interaction/commands` | UI surfaces, `/name` | dispatch without a model turn |
| `ctx.permissionPresets` | `interaction/permission-presets` | web, sessions | sandbox+approval bundle switching |
| `ctx.settings` / `ctx.credentials` | `settings/*`, `credentials/*` | adapters, web Models page | layered user config + secret refs |
| `ctx.typert` | `typert/registry` | gateway, loaders | RPC descriptor registry |
| `ctx.webhookRuntime` | `webhook/webhook` | provider adapters | verified-delivery dispatch |

## Event domains and dispatch modes

Three event domains: **session events** are durable facts appended to the log and broadcast on `session/event`; **agent events** (`agent/*`) observe live work and vanish with the process; **capability events** (`fs/*`, `tools/*`, `telemetry/*`, `credentials/*`, `skills/change`, `commands/change`, `subagent/*`, `workflow/*`) attach policy or observe a seam.

Dispatch modes and their rules:

- **Waterfall**: listeners run outermost-first and MUST call `next()` to delegate; returning without `next()` vetoes the rest of the chain. Used by `agent/pre-step`, `agent/request`, `agent/request-error`, `llm/stream`, `tools/pre-execute`, `tools/execute`, `tools/post-execute`, `tools/ptc-dispatch-log`, `fs/write-intent`, `fs/edit-intent`, `approval/request`, `user-questions/request`, `session-telemetry/record`.
- **Emit**: synchronous, per-listener failures contained by the dispatcher when the contract is non-vetoing (registries), otherwise a throwing listener starves later listeners (Cordis `Array.map` dispatch).
- **Serial** (`agent/turn-stopping`): listeners run in order with no `next()`; a throw becomes the turn's error reason.

## Session events (durable log)

`Session.append(type, data, surfaceIntent?)` (`packages/core/session/src/index.ts`) validates lossless JSON (non-serializable data throws at the append site), assigns `seq = log.length` and `time`, deep-freezes, validates surface placement, then broadcasts `session/event`. Required-on-read: an unrecognized event type without `ignorable: true` must make a reader refuse reconstruction.

| Event | Payload | Surface | Notes |
|---|---|---|---|
| `turn/start` | `{turn}` | log-only | opens the turn before claiming input |
| `turn/end` | `{turn, reason: TurnEndReason}` | log-only | always the turn's last event; kinds `completed`, `aborted{reason: user\|parent\|hook\|disposed}`, `blocked`, `error{error}`, `max-tokens`, `interrupted` |
| `step/start` / `step/end` | `{turn, step}` | log-only | one model call + its tool executions; closed even on failure |
| `user/message` | `UserMessage` | append | human prompt, plugin injection, or goal round; `source` distinguishes |
| `assistant/chunk` | `{turn, step, chunk: StreamChunk}` | log-only | raw replay fidelity |
| `assistant/message` | `{turn, step, message, usage?, interrupted?}` | append, cites chunk seqs | cancelled turns finalize the delivered prefix with `interrupted: true` |
| `tool/call` | `{turn, step, callId, name, arguments}` | log-only | raw model arguments, unparsed |
| `tool/result` | `{turn, step, message, error?, meta?}` | append, cites its `tool/call` seq | `meta` is tool-private, must be JSON |
| `request/header` | `{header: EpochHeader, reason, startsSeries?}` | log-only | reasons `initial`/`resume`/`change`/`series`; latest snapshot reconstructs the request |
| `request/context` | `{provider, model, contextWindow?}` | log-only | logged only on route change |
| `session/end-seed` | `{}` | log-only | marks the constructor seed boundary; `Session` is the only writer |
| `compaction/start` / `compaction/end`, `hook/invoked` / `hook/result`, `todo/write`, `plan/mode`, `command/run` / `command/done`, `approval/policy`, `permission/preset`, `subagent/descriptor`, `session/title`, `web/deepseek-search-llm-request`, `session/title-llm-request`, `tool-workflow/*` | plugin-merged payloads | log-only or replace | merge-extensible vocabulary; see owning packages |

History derivation: `session.deriveMessages()` walks the surface — `append` nodes in order, a compaction `{op: 'replace', start, end}` node deleting the shadowed range — so the model-visible transcript is a pure function of the log (`model-visible ⟺ logged`). A `replace` node's `sourceEventSeqs` must cover every node it shadows.

## Agent-loop turn flow

Input: `agent.followup(msg)` (next turn, wake), `agent.steer(msg)` (next step, wake), `agent.inject(msg)` (next step, no wake), `agent.cancel(cause)`, `agent.runMaintenance(job)`. Event sequence per turn (`packages/core/agent-loop/src/agent.ts`):

1. `turn/start {turn}`.
2. Claim inbox input → `systemPrompt.assemble` → `agent/pre-step` waterfall may rewrite or reject; a rejected or empty first claim closes the turn with no step (`turn/end {completed}` or `{blocked}`).
3. `step/start` → entered messages appended as `user/message` → `request/header` folded (first header `initial`/`resume`, changed `change`, same-header new series `series`) → `agent/request` waterfall finalizes the call config → `llm.prepareCall` binds adapter defaults → `llm/stream` waterfall → `assistant/chunk` per delta → `assistant/message` with usage.
4. `tool/call` per requested call → the tool pipeline (below) → `tool/result` per call in model order → step owes another request while tool calls remain.
5. `agent/turn-stopping` (serial) before a turn closes with queued input absent → `step/end`, then `turn/end`.

Guarantees: turn numbering continues across seeds; `max-tokens` is sticky within a turn; cancellation mid-stream finalizes the delivered prefix as `assistant/message {interrupted: true}` and skips undispatched calls with synthetic `tool/result` errors (`TOOL_ABORTED_BEFORE_DISPATCH`); disposal closes the open turn `{aborted, reason: {kind: 'disposed'}}`; every failure is structured (`LlmError` facts verbatim, otherwise `errorChain` under code `UNKNOWN`); listener throws never unbalance the boundary events.

## Tool pipeline

Input: `ToolExecutionInput {callId, name, arguments (parsed, frozen), agent?, signal, parent?}`. Registration: `ctx.tools.register(ToolDefinition)` where a definition carries `execute(args, exec)`, a mandatory `output` declaration (JSON Schema + pure `render`, optional `presentationMeta`), optional `finalizeContent`, `timeoutMs` (cooperative, enforced by the timeout policy), `isConcurrencySafe` (pure classifier; anything but `true` is exclusive), and pure `presentCall`/`presentResult`.

Per call, in order: `tools/pre-execute` waterfall (return `{kind: 'reject', ...}` for a denial result, or dispatch) → `tools/execute` wrappers (may replace `exec.signal`; the registry fuses replacements with the caller signal) → tool body → `tools/post-execute` waterfall (`accept`, or replace `value`/`content` within the type rules — a failed result's value cannot be replaced) → `tools/result` emit → scheduler finalize commits the `tool/result` with `concludesTurn`/`additionalContexts` applied.

Scheduling: exclusive calls form barriers; `isConcurrencySafe` calls run in a rolling pool capped by `maxParallelToolCalls` (settings-live); later calls reclassify before start so a registry change creates a barrier. Results commit in model order; abort drains started calls and synthesizes ordered error results for the rest; an internal scheduler failure rejects without fabricating results.

## LLM adapter seam

Input to an adapter (`LlmAdapter`, `packages/llm/llm/src/index.ts`): one `GenerateOptions {provider, model, reasoningEffort?, messages, system?, tools?, temperature?, maxTokens?, stop?, signal?, sessionId?, purpose?}` — loop-built requests are deep-frozen and marked (`markAgentLoopRequest`).

Required output: `stream(options)` yielding the `StreamChunk` vocabulary — `block-start`, `text-delta`, `reasoning-delta`, `tool-call-delta`, `block-end` (assembled block), `usage`, `finish {reason, replayState?}` — with `usage` strictly before the terminal `finish` and nothing after. Optional overrides: `providerInfo`, `providerRetryPolicy`, `listModels` (advisory), `resolveModel`, `prepareCall` (generation binding), `imageRequestPricing`.

Registration: `ctx.llm.registerAdapter(providers, adapter)` is all-or-nothing (`DUPLICATE_ADAPTER`), atomically replaceable via `handle.replace`, and publishes `llm/adapters-updated`. `llm.prepareCall(config)` returns a `PreparedLlmCall` whose `stream` dispatches exactly once with a matching config (`INVALID_PREPARED_CALL` otherwise) and whose `adapterDefaults` the loop strips before proposing the next request. Adapter throws never escape `LlmRuntime.stream()`: they normalize to terminal `error`/`aborted` finishes.

## Session store, fork, and projections

`SessionStore.prepare/enter/announce` compose the creation transaction (`ctx.sessions.create` for simple cases); `prepare(id)` rejects duplicate ids and non-absolute `meta.cwd`. `fork(source, boundary?, childId?)` copies the prefix, stamps `parentSession`/`seedLength`, and refuses with typed codes `SESSION_NOT_FOUND`, `SESSION_NOT_LIVE`, `SESSION_ALREADY_EXISTS`, `INVALID_BOUNDARY`, `OPEN_TURN`.

`ctx.sessionProjections` folds events incrementally: a unit declares `{key, stateVersion, stateSchema, init, apply, wire?}`; uninterested events return the same state reference; readers use `stateOf(session, key)` and hosts batch client views with `snapshot()`. Same-key registration with a different `stateVersion` throws; shipped units include `turnBoundary` (agent-loop), `todos`, `plan`, `permissions`, `subagent`, `sessionStats`, `title`/`titleInput`, `timeContext`.

## Persistence seam

`SessionPersistence.prepare(id, signal)` is the load barrier (serialized per id) returning a prepared session; backends refuse foreign `SESSION_FORMAT_VERSION` with the upgrade-directed message before any structural parsing. The JSONL backend writes behind the log (`session/event` → coordinator → append), checkpoints durability per `session-checkpoint-policy` (flush before model dispatch, before top-level tool dispatch, at step boundaries), and repairs a torn tail by truncation. Crash-orphaned open turns close on reload with `turn/end {interrupted}`.

## Capability seams (provider-swappable)

Each row: Service Definition → provider registration input → consumer output.

- **fs**: `FileSystem` (`resolve/stat/readText/readBytes/listDir/writeText(target, content, intent)/editText`) with optimistic versions (`createIfAbsent`/`replaceIfVersion` → `FS_STALE_VERSION` on mismatch) and the `fs/write-intent`/`fs/edit-intent` waterfalls plus `fs/observed` recording. Providers: local, sandboxed (write fence only), e2b. Tools `read`/`write`/`edit` add remediation hints (`FS_STALE_VERSION — re-read the file, then retry`) and sandbox escalation fields only when `ctx.fs.sandboxMode` exists.
- **subprocess**: `spawn(SubprocessSpawnSpec)` → `SubprocessHandle {pid, collected, done, terminate}`; env defaults to a scrubbed parent (`KEY|PASSWORD|SECRET|TOKEN`, `DSH_*` stripped); termination is tree-scoped SIGTERM→grace→SIGKILL. Terminal primitive for PTYs.
- **shell**: `resolve(ShellExecRequest): ShellExecSpec` then `run`/`start`; model-visible fields are `command`/`workdir`/`timeoutMs` only. Providers: local bash (`['bash','-c',command]`, `NO_COLOR=1 TERM=dumb PAGER=cat`), sandboxed bash (confined argv + `{mode, denied, enforcement}` facts), pwsh mirrors. Denial marker `[sandbox: file access denied under <mode> mode]`.
- **terminals**: `registerBackend({type, spawn})`; `spawn/send/read/signal/kill/list` owner-scoped per agent; `DUPLICATE_NAME`, `NO_BACKEND`, `SEND_ACTIVE` errors; backends run confined when policy requires.
- **lsp**: `registerProvider({id, extensionToLanguage, query})` (all-or-nothing, `LSP_CONFLICT` on cross-provider extension overlap); `query({operation, filePath, position, workspaceRoot})` routes by final extension and yields `locations` or `hover`; no route → `LSP_UNAVAILABLE`.
- **web**: search/fetch providers with `WEB_PROVIDER_{CONFIGURED_MISSING,CONFIGURED_UNAVAILABLE,UNAVAILABLE,AMBIGUOUS}` selection; fetch treats non-2xx as a result; same-origin-only redirects (`WEB_REDIRECT_BLOCKED`).
- **skill**: filesystem provider roots with ranks (project 100/200, custom 300, user 400/500, bundled 600), `SKILL.md` frontmatter (`name`, `description`, optional `whenToUse`, `disable-model-invocation`, `user-invocable`), scoped precedence nearest-scope-wins.
- **compaction**: `compactIfNeeded`/`compactNow`/`compactRegion` producing `CompactionResult {summary, shadowedRange, …}`; the region must stay tool-pairing balanced; the durable `compaction/start…end` bracket is the lock.
- **subagent**: `SubagentProvider {name, capabilities, start(ResolvedSubagentStartRequest) → SubagentRun}`; results carry `stopReason` from the merge-extensible map (`completed`, `aborted`, `error`, `max-tokens`, `refusal`); `prepareContinuable` presence is the continuable capability; structured output via `outputSchema` with `completed→error` downgrade when uncaptured.
- **workflow**: `start({script, meta, args, subagentProvider, maxTotalAgents, parent, signal})` → `WorkflowRun {result (never rejects), cancel, dispose}`; closed error-code set (`SCRIPT_PARSE`, `META_INVALID`, `AGENT_CAP`, …) with `fatal` driving re-throw vs per-item null; worker wire is a closed discriminated union checked by `assertNever`.

## Interaction seams

- **approval**: `approval/request` waterfall (scoped to the agent) returns `allowed-once`/`rejected`/`cancelled`/`unavailable`; policy `never` is decided before dispatch and cannot be bypassed by a prepended listener; durable `approval/asked`/`approval/decided` pairs must be turn-enclosed (invariant-enforced).
- **user-questions**: `ask()` with typed rejections (`EMPTY_QUESTIONS`, `BAD_INTENT`, `NO_PROVIDER`, `DELEGATED_CALLER`, `ASK_ABORTED`); plan-review intent validates the approve label against its own options.
- **commands**: `/name` parsing (`/^\/([a-z][a-z0-9_-]*)/`), scoped layers (agent-scoped shadows global), durable `command/run`/`command/done` pairs, image admission only for declaring commands.
- **permission-presets**: preset table `{sandbox, approval}` appended as `permission/preset` and folded with `sandbox/mode` + `approval/policy` into the `permissions` projection; `custom` is reserved.

## Typert RPC registry (internal face of the gateway)

Services annotated `@Remote` publish `InvocationDescriptor`s (`namespace`, `method`, parameter sources `json`/`lookup`, codecs, cancellation parameter) into `ctx.typert`; lookups map wire ids to host objects (`session` → `SessionId` → live session, error `session/not-found`). The generated `TYPERT` manifest is validated at load (`face`, zod schemas, member kinds, documented invocations); the catalog gates require `@mode` tags on every declared event and reject undocumented payload params. The gateway exposes these descriptors over `POST /api/<namespace>/<method>` (see [external-interfaces.md](external-interfaces.md)) with exact-field argument validation (`args fields do not match the descriptor`) and the `gateway/*` error-code taxonomy.
