# External interfaces: expected input and output

English | [中文](external-interfaces.zh.md)

One reference row per boundary where something outside the repository enters or leaves a running harness. Each interface lists the expected input, the expected output, the failure behavior a caller must expect, and the tests that pin it. Behavior internal to the process is in [internal-interfaces.md](internal-interfaces.md); flows that cross several interfaces are in [data-flows.md](data-flows.md).

## The `dsh` launcher command line

- Entry: `apps/cli/src/bin.ts`, `apps/cli/src/args.ts`; grammar tests: `apps/cli/tests/args.spec.ts`, built-bin acceptance: `apps/cli/tests/built-bin.e2e.ts`.
- Input: `dsh [--profile <name>] [--patch <path>]... [--dump-config | --dump-default-config] [args...]`, the `web` subcommand (alias of `--profile web`), and the `plugin` subcommand (`dsh plugin --profile <name> <pnpm args...>`). Launcher flags come first; the first token the launcher does not own starts the booted app's argv verbatim, including its `-h`.
- Output: a booted application for `profile`/`web`; a YAML entry tree on stdout for `--dump-config` (grouped under `# == <origin>` provenance comments, `!!js` expressions printed verbatim, never evaluated); bundle layers only for `--dump-default-config`; a pnpm forwarding run for `plugin`.
- Failure: exit 1 with `error: …` on missing or empty `--profile`, `--patch ''`, both dump flags together, app arguments alongside a dump, or `plugin` with no pnpm arguments. Bare `dsh -h` prints launcher help with exit 0. Unknown profiles that match no template fail with `dsh: profile "<name>" does not exist; create it with 'dsh plugin --profile <name> add <package>'`.
- Exit codes: launcher/app usage errors 1; boot failure 1; `SIGTERM` 0; first `SIGINT` 130 (a second forces exit); graceful disposal force-exits after 5 s; `plugin` returns the pnpm status, or 127 with `dsh: pnpm not found on PATH — install pnpm to manage profile plugins` when pnpm is absent.

## Profiles and composition files

- Entry: `packages/boot/app-boot/src/profile.ts`, `apps/cli/src/profile-boot.ts`; tests: `packages/boot/app-boot/tests/profile.spec.ts`, `apps/cli/tests/built-bin.e2e.ts`.
- Input: `$DSH_HOME` (default `~/.dsh`; empty/whitespace `DSH_HOME` means unset) holding `profiles/<name>/` with `package.json` (`dsh.profile.bundles`, `dsh.profile.patchReload`), `cordis.patch.yml` (a top-level YAML array of patch entries), and `pnpm-workspace.yaml`. Shipped template profiles: `web`, `headless`, `sdk`, `sdk-minimal`, `acp`.
- Layer order, later wins per row id: bundle layers in listed order → the profile's user `cordis.patch.yml` → `$DSH_HOME/cordis.patch.yml` → `--patch` overlays in argv order → the telemetry-disable patch when `DSH_TELEMETRY_DISABLED` is non-empty. `patchReload: live` re-composes on user-layer edits; `startup` applies all layers once.
- Output: a composed Cordis entry tree; `!!js` expressions are legal only under plugin `config` and entry `disabled` (`docs/cordis-primer.md#loader-configuration`), and `verify-cordis-config` rejects them anywhere else.
- Failure: fail loud at boot — `dsh: patches <file> must be a top-level YAML array of loader patch entries`, `dsh: profile bundle "X" declares no dsh.bundle in its package.json`, `dsh: N entries did not activate … (waiting for services: …)` for rows that never resolve.

## Environment variables

- Entry: `packages/boot/app-boot/src/index.ts` (`loadLayeredEnv('dsh')`); tests: `packages/boot/app-boot/tests/app-boot.spec.ts`.
- Trust order: inherited `process.env` → the invoking directory's `.env` (project layer) → `$DSH_HOME/.env` (user layer). A `.env` value never overrides an inherited variable. Bootstrap-only names (`PATH`, `HOME`, `NODE_OPTIONS`, `DEEPSEEK_BASE_URL`, `HTTP_PROXY`, every `DSH_*`-prefixed name, and the rest of the reserved list) are rejected from any `.env` with `dsh: <path> sets "<NAME>", which only the launching environment may set …`, failing the launch.
- Key variables consumed downstream: `DSH_HOME` (home override), `DEEPSEEK_API_KEY` (credential fallback; requests fail with `LlmError` `MISSING_CREDENTIAL` when unresolvable), `DSH_TELEMETRY_DISABLED` / `DSH_TELEMETRY_MODE` / `DSH_TELEMETRY_OTLP_URL`, `DSH_TOOLS_MODE` (`native|ptc|both`), `DSH_PERMISSION_MODE` (default `workspace-write`; `danger-full-access` implies approval `never`), `DSH_MAX_TOKENS_AS_SUCCESS` (sdk profile; invalid JSON fails the boot), `DSH_CONTEXT_WINDOW` and `DSH_SYSTEM_PROMPT` (sdk-minimal defaults), `DSH_WEB_URL` (output-only, bash-visible), `DSH_SNAPSHOT=replay` (test seam swapping `cordis.yml` for `cordis.snapshot.yml`), `E2B_API_KEY` (e2b POC), `EXA_API_KEY` / `PERPLEXITY_API_KEY` (search providers).

## `dsh --profile headless "<task>"` — one-shot run

- Entry: `packages/bundle/headless/src/`; tests: `packages/bundle/headless/tests/`, `apps/cli/tests/built-bin.e2e.ts`.
- Input: positional task words (joined by spaces); an empty or whitespace task is a usage error, `error: a task is required, for example: dsh --profile headless "run the tests"`.
- Output: reasoning text prefixed `dsh: reasoning:` on stderr; the final assistant text plus a newline on stdout.
- Exit codes: 0 iff the last `turn/end` reason kind is `completed`; otherwise 1, with `dsh: <error.code>: <error.message>` on stderr for the `error` kind.

## `dsh web` — browser application

- Entry: `packages/bundle/web-app/src/`, `packages/host/webserver/`; tests: `apps/cli/tests/web-auth.e2e.ts`, `packages/bundle/web-app/tests/`.
- Input: `--host <host>` (loopback default; `0.0.0.0` refused with a safety message), `--port <port>` (default 3080), `--no-open`, `--trusted-host <authority...>`.
- Output: stdout `dsh web: <authenticatedUrl>` (plus ` (LAN: <url>)` for `0.0.0.0` binds), the managed `DSH_WEB_URL` environment for spawned shells, and a browser handoff unless suppressed.
- Auth: the launch URL carries a one-time `?token=` which a GET exchanges via `303` for an `HttpOnly; SameSite=Strict` cookie (`dsh-auth-<authority-hash>`); unauthenticated requests get `401 dsh web authentication required; reopen the URL printed by dsh web.`; untrusted `Host`/`Origin` get `403`.
- RPC carrier: `POST /api/<namespace>/<method>` with JSON body `{ type: 'client-request', rpcId, method, payload }`, answered by `{ type: 'server-response', rpcId, result: { ok: true, value } | { ok: false, error: { code, message, details } } }`; `415` for a non-JSON content type, `413` above the 300 MiB body cap, `404` for foreign methods on claimed endpoints. Streams multiplex over one WebSocket at `/api/remote.mux` with text frames `{ type: 'open'|'cancel'|'item'|'error'|'end', streamId, … }`; close codes `1003` (binary frame), `1008` (invalid frame/duplicate id), `1011` (undeliverable terminal error).

## `dsh --profile sdk` / `sdk-minimal` — JSON-RPC stdio runtime

- Entry: `packages/bundle/sdk-app/`, `packages/sdk/server/`, `packages/sdk/protocol/`; tests: `packages/sdk/protocol/tests/transport.spec.ts`, `packages/sdk/server/tests/server.spec.ts`, `apps/cli/tests/profiles/sdk/keyless-smoke.e2e.ts`.
- Input: newline-delimited JSON-RPC 2.0 on stdin (no Content-Length headers; malformed lines ignored; non-object `params` normalized to `{}`). Requests: `initialize` `{cwd, provider, model, reasoningEffort?, maxTokens?}`, `session/prompt` `{sessionId, contentBlocks}` (unknown ids lazily create the session; image blocks carry canonical base64 `data` and `mimeType` png/jpeg/webp/gif), `shutdown` (no params). Request ids are client-minted.
- Output: on stdout only framed JSON-RPC. `initialize` → `{ serverInfo: { name: 'deepseek-harness-sdk-runtime', version } }`; `session/prompt` → `{ messageId }`; `shutdown` → `{}` then exit 0. Notifications: `session.event` `{sessionId, event}` (full session-log envelope for every session), `session.status` `{sessionId, status: 'idle'|'running'}`, `subagent.started` `{parentSessionId, childSessionId}`, `subagent.finished` `{provider, agentId, parentSessionId, childSessionId, status, stopReason, lastAssistantMessage?}`.
- Failure: error frames `-32601 method not found: <m>` and `-32603` carrying handler messages; `session/prompt` before `initialize` → `SDK server is not initialized`; unknown provider → `no adapter registered for provider "<p>"`; stdin EOF before ready → startup failure exit 1.
- Version pin: the TypeScript client refuses a `dsh` dependency whose version differs from its own (`dsh SDK client <v> requires the same dsh version, got <v>`).

## Python SDK (`deepseek-harness-sdk`)

- Entry: `python/sdk/`, `python/sdk-runtime/`; tests: `python/sdk/tests/`, snapshot counterpart `scripts/snapshots/python-sdk-single-exe/`.
- Input: `DeepSeekHarness(dsh_home=…, cwd=…, profile='sdk'|'sdk-minimal', provider=…, model=…, reasoning_effort=…, max_tokens=…, patches=(…), base_url=…, api_key=…, initialize_timeout_seconds=30, …)`. `dsh_home` is required — the SDK never discovers `~/.dsh`; it launches the bundled `dsh --profile <profile>` subprocess.
- Output: `harness.run("task", session_id=…)` → `RunResult(session_id, final_response, finish_reason, events, notifications)`; `final_response` is the last committed root-session assistant text in the run interval and `finish_reason` the last root `turn/end` reason kind.
- Failure: handshake timeout names the profile and retains runtime diagnostics; a `turn/end` without a string `data.reason.kind` raises `SdkProtocolError`; a profile without an SDK server row fails at boot with no fallback.

## `dsh --profile acp` — Agent Client Protocol

- Entry: `packages/acp/acp/src/index.ts`; tests: `packages/acp/acp/tests/bridge.spec.ts`, `…/turns.spec.ts`, `apps/cli/tests/built-bin.e2e.ts`.
- Input: ACP over newline-delimited JSON on stdio: `initialize`, `authenticate`, `session/new` (absolute `cwd`; `additionalDirectories` rejected), `session/list` (keyset cursor over `[createdAt, sessionId]`, canonical encoding enforced), `session/resume`, `session/close`, `session/set_session_config_option` (`model`, `reasoning_effort`), `session/prompt` (text, `resource_link`, advertised `image`; audio rejected; one prompt in flight per session), `session/cancel`.
- Output: `initialize` → `agentInfo.name === 'deepseek-harness-acp'` plus fixed capabilities; ordered `session/update` notifications (`agent_thought_chunk`, `agent_message_chunk`, `tool_call`/`tool_call_update`, `usage_update`, `config_option_update`); `session/request_permission` waterfalls with `allow-once`/`reject-once` options; prompt responses `{ stopReason }` mapped from turn ends (`completed→end_turn`, `max-tokens→max_tokens`, `interrupted→cancelled`).
- Failure: `invalidParams`/`internalError` request errors preserving the detail; unknown-session cancellation is silently ignored; exit 0 on client disconnect; stdout carries protocol frames only.

## Webhook ingress (GitHub adapter)

- Entry: `packages/webhook/webhook-github/src/handler.ts`, `packages/webhook/webhook/src/`; tests: `packages/webhook/webhook-github/tests/handler.spec.ts`, `packages/webhook/webhook/tests/`.
- Input: `POST <configured path>` with `Content-Type: application/json`, `x-hub-signature-256` (HMAC-SHA256 over the raw body with the credential-resolved secret), `x-github-delivery`, `x-github-event`, and a JSON-object body within `maxBodyBytes`.
- Output: `202` with an empty body once the verified delivery is dispatched in memory (`{kind: 'github', source, deliveryId, event: {name, payload}, receivedAt}`); rules run asynchronously and repeated delivery ids intentionally re-run.
- Failure: `405` non-POST (`allow: POST`), `415` wrong content type, `400` missing/blank header or non-JSON payload, `413` over-cap body, `401 invalid webhook signature`, `503` when the secret or the runtime is unavailable. No request data is echoed in error bodies.

## Session storage artifacts

- Entry: `packages/session/session-persistence-jsonl/src/format.ts`, `…/session-persistence-sqlite/src/schema.ts`; tests: `packages/session/session-persistence-jsonl/tests/jsonl.spec.ts`.
- Layout: `<root>/<projectKey>/<encodedSessionId>/session.jsonl` (`.jsonl.zstd` when compression is on). `projectKey` is a bounded readable slug of the cwd (`--` prefixed/suffixed, `_no-cwd` when absent); `encodeSegment` maps every session id injectively to one safe path segment (`~XXXX` escapes, `.` → `~002E`), so ids cannot traverse or collide.
- Header line: first record `{type: 'session', version, id, createdAt, cwd?, parentSession?, seedLength?, origin?, delegationDepth, agentPreset?}`; `version` must equal `SESSION_FORMAT_VERSION` (currently `0`) or load fails with the upgrade-directed refusal, never a corruption message. Retired fields (`sandboxMode`, `approvalPolicy`) are rejected.
- Event lines: one JSON record per line (or packed `text-chunks`/`reasoning-chunks`/`tool-call-chunks` rows for delta runs) with `seq` contiguous from 0 and `sourceEventSeqs` range-encoded. A torn final line is repaired by truncation; a seq gap or malformed record before a committed `turn/end` is corruption and refuses to load.
- SQLite: monotonic `SCHEMA_VERSION`; incompatible databases are rejected, not migrated.

## LLM provider wire (DeepSeek adapters)

- Entry: `packages/llm/llm-deepseek/src/sse.ts`, `…/adapter.ts`, `…/translate.ts`; tests: `packages/llm/llm-deepseek/tests/`, real-API `packages/llm/llm-deepseek/tests/*.e2e.ts` (self-skipping without `DEEPSEEK_API_KEY`).
- Input: `POST /chat/completions`-style requests with `Authorization: Bearer <key>`, attribution headers from `attributionHeaders()` on every request, `stream: true` for turns; tools mapped to the provider `tools` field; `stop` sequences honored.
- Output: SSE frames ending in the literal `[DONE]`; deltas translated to the `StreamChunk` vocabulary with `usage` before the terminal `finish` and nothing after.
- Failure: EOF before `[DONE]` → `LlmError('STREAM_CLOSED')` whose message embeds the transport diagnostics (event count, comment heartbeats, last-frame age, duration) and whose stderr line names the provider host, HTTP status, and model; credentials that no HTTP header can carry are refused with `INVALID_CREDENTIAL` naming the credential reference, never echoing the value. Stable `LlmError` codes include `AUTH`, `RATE_LIMIT`, `NO_ADAPTER`, `MISSING_CREDENTIAL`, `INVALID_CREDENTIAL`, `INVALID_ADAPTER`, `DUPLICATE_ADAPTER`, `REGISTRATION_DISPOSED`, `INVALID_PREPARED_CALL`, `STREAM_CLOSED`.

## Hook bridges (Claude Code / Codex dialects)

- Entry: `packages/hooks/hook-protocol/src/`, `packages/hooks/hooks-claude-code/src/`, `packages/hooks/hooks-codex/src/`; tests: `packages/hooks/*/tests/`.
- Input: a hook command receives one JSON payload on stdin (trailing newline in the Claude dialect, none in Codex), cwd = the session workspace, plus dialect-specific env (`CLAUDE_PROJECT_DIR`). Exit code 2 means block; exit 0 with a `{`-leading stdout may carry `continue`, `stopReason`, `decision`, `reason`, `systemMessage`, or a `hookSpecificOutput` block (`hookEventName` must match the expected event or its fields are discarded; `permissionDecision` overrides the top-level `decision`).
- Output: one merged outcome per point — `deny > ask > allow`, the first `continue: false` sticky with its `stopReason`, `additionalContext` accumulated in hook order; durable `hook/invoked` and `hook/result` session events record the run with a stderr summary capped at 500 characters.
- Failure: spawn failure or timeout (default 600 s) is a non-blocking error; invalid regex matchers make a matcher never match; unknown decisions are ignored rather than guessed.

## Sandbox runner CLI (native)

- Entry: `native/landlock-run/docs/cli-contract.md`, `native/landlock-run/packages/entry/src/index.ts`; tests: `native/landlock-run/test/`, `packages/sandbox/sandbox-local/tests/landlock.e2e.ts`.
- Input: `landlock-run [--ro <path>]... [--rw <path>]... -- <argv>...` (the `--` separator is mandatory) or `landlock-run --probe`.
- Output: after a successful exec the child's exit status passes through unchanged; the probe prints exactly `landlock: fully enforced` or `landlock: partially enforced (older ABI)` with exit 0.
- Failure: every launcher-level failure exits 125 with a `landlock-run: ` fatal stderr line; attribution therefore requires exit 125 **and** that line (postmortem 0004). A partial-ABI confined run prints the informational `landlock-run: partial enforcement (older Landlock ABI)` line, which is excluded from fatal classification.
