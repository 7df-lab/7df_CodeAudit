# Data flows and cross-boundary interactions

English | [中文](data-flows.zh.md)

Flows that cross several interfaces or live outside any single seam. Each flow lists its stages with the expected input and output at every hop, the invariant that guards it, and the tests that pin it. Per-interface contracts are in [external-interfaces.md](external-interfaces.md) and [internal-interfaces.md](internal-interfaces.md).

## Model-visible ⟺ logged reconstruction

Stages: prompt assembly (`systemPrompt.assemble`) → request build from `deriveMessages()` → `request/header` fold → provider call → append. Invariant: everything a model request contains is reconstructable from the session log — prompt text and tool schemas live in the latest `request/header`, messages in the surface, route facts in `request/context` — and a runtime invariant refuses unbalanced or unlogged input. consequence: a new model-visible input requires a new session event. Tests: `packages/core/session/tests/request-header.spec.ts`, `packages/core/agent-loop/tests/contract-regressions.spec.ts`, invariant companions `packages/core/*/src/invariant.ts`.

## Persistence and crash recovery

Stages: `Session.append` → `session/event` broadcast → persistence coordinator write-behind buffer → JSONL/SQLite artifact → checkpoint flushes (before model dispatch, before top-level tool dispatch, at step boundaries) → reload. Expected behavior per hop: the in-memory log is authoritative; a failed append truncates partial bytes so a retry has no seq gap; a torn final line is repaired by truncation to the last committed record; an open turn in a reloaded log closes with `turn/end {interrupted}` while earlier events stay intact; corruption before a committed `turn/end` refuses the load. Tests: `packages/session/session-persistence-jsonl/tests/jsonl.spec.ts` (crash recovery, torn tails, seq gaps), `packages/session/session-checkpoint-policy/tests/`, `packages/core/session/tests/repair.spec.ts`.

## SDK event streaming to RunResult

Stages: session log append → `session/event` broadcast → SDK server `session.event` notification → client subscription queues → `Session.run()` activity interval → `RunResult`. Expected data: the notification carries the full durable event envelope verbatim; the run interval spans from the prompt's durable inbox receipt to the next whole-agent idle; `final_response` selects the last committed root-session assistant text; `finish_reason` mirrors the last root `turn/end` reason kind; descendant events reach `notifications` (via `subagent.started` ancestry) but never the root response. Failure mapping: a `turn/end` without a string reason kind is a protocol violation (`SdkProtocolError`), a dead runtime rejects with exit code plus a bounded stderr tail. Tests: `packages/sdk/server/tests/server.spec.ts`, `packages/sdk/client/tests/sdk-client.spec.ts`, `python/sdk/tests/`.

## Snapshot record / replay pipeline

Stages: a recorded `session.jsonl` fixture (headers and payloads, body envelopes omitted) → `snapshot.yml` declaring profile, composition/header class, and workspace facts → replay through the shipped profile with a scripted model → comparison against the committed expected output (`workspace.expected/` for mutating scenarios, byte-identical outside mutated files). Rules: replay synthesizes stripped envelopes; fixtures use canonical packed rows (migrated by `scripts/migrate-packed-session-fixtures.ts`); typed tokens preserve parent/child session identity; `DSH_SNAPSHOT=refresh` re-derives expected outputs only when replay input stays valid; the snapshot harness rejects structured `UNKNOWN_TOOL` results in fresh runs and fixtures (postmortem 0002). Tests: `packages/test-support/session-snapshot/tests/`, `snapshots/**/*.snapshot.ts`.

## Compaction surface rewrite

Stages: pressure measurement (threshold ratio over the context window) or a `context-overflow` error → region selection (tool-pairing balanced) → summarizer call (`purpose: 'compaction'` request) → one durable transaction appending `compaction/start`, the summary `user/message` with a `replace` surface op covering `shadowedSeqs`, `compaction/end`. Expected data: `CompactionResult {compactionId, startSeq, summarySeq, endSeq, shadowedRange, shadowedSeqs, shadowedTokenCount}`; the replace node's `sourceEventSeqs` covers every shadowed surface node; `deriveMessages()` rebuilds from the rewritten surface; the manual path is serialized by the `compaction/start…end` bracket (concurrent attempts fail with `ManualCompactionError`). Tests: `packages/compaction/compaction-basic/tests/compaction-basic.spec.ts`, `packages/compaction/compaction/tests/tool-pairing.spec.ts`, `packages/core/session/tests/surface.spec.ts`.

## Subagent delegation and continuation

Stages: parent tool call (`subagent`) → provider `start` → child session creation (`parentSession`, `delegationDepth + 1`, `subagent/descriptor` appended on the child's first accepted step) → driven run (`followup` + `whenIdle`) → `subagent/end` event + `subagent.finished` notification. Expected data: stop reasons come from the merge-extensible map; `lastAssistantMessage` is omitted when empty; depth limits enforce the recursion budget from the persisted header; continuation routes child output upward via the child-scoped `report` tool and parent steering via `send_message`/`interrupt_agent`, with authority checks rejecting forged senders. Tests: `packages/subagent/*/tests/`, `packages/subagent/subagent/tests/invariant.spec.ts`.

## Webhook delivery to session creation

Stages: HTTP POST → HMAC verification → bounded body read → JSON object validation → `VerifiedWebhookDelivery` snapshot (deep-frozen, lossless JSON) → every kind-matching rule's `run()` → optional `WebhookSessionRequest` validation → creation transaction (workspace resolve/create, `webhook-<uuid>` session, preset application, title rename, `followup` prompt with `source.kind: 'webhook'`) → `202` already returned. Expected data: the audit chain in the session log traces delivery id, rule id, and provider; rollback on failure detaches the workspace and disposes the agent without masking the original error; repeated delivery ids re-run (no built-in deduplication). Tests: `packages/webhook/webhook/tests/session.spec.ts`, `packages/webhook/webhook-github/tests/handler.spec.ts`.

## Credential resolution and redaction

Stages: adapter requests a `CredentialRef` (e.g. `DEEPSEEK_API_KEY`) → ladder: inherited env → managed `.credentials.yaml` → project `.env` → home `.env` → value (or absent). Expected data: inherited env shadows writes with an explicit remediation message; empty values cannot be stored; the document enforces `version: 1` and rejects unknown keys fail-loud; file mode 0600 is enforced at boot and before every write. Redaction: diagnostics name references and codes, never values; subprocess environments are scrubbed of credential-shaped and `DSH_*` names. Tests: `packages/credentials/credentials-local/tests/local.spec.ts`, `packages/credentials/credentials-local/tests/migration.spec.ts`, `packages/subprocess/subprocess/src` scrub helpers in `packages/subprocess/subprocess/tests/service.spec.ts`.

## Worker and code-runtime boundaries

Stages: host validates the request → spawn worker with a scrubbed environment → closed wire protocol in both directions → bounded result materialization. Workflow: `WorkerInit {meta, body, args, limits}` in, `WorkerToHostType`/`HostToWorkerType` discriminated frames out (`assertNever` on both ends), results pass the plain-JSON realm projection (functions, symbols, bigints, cycles, exotic prototypes rejected). Code runtime JS: `workerData` boot + `call`/`reply` pairs with eager log streaming so output survives termination. Code runtime Python: the same frames on fd 3 (`PROTOCOL_FD`), mirrored by a constant-equality test with the Python side. Expected failure: worker death, cancel grace expiry, or output limit settles the run as a structured error, never a hang. Tests: `packages/workflow/workflow-worker-thread/tests/`, `packages/code-runtime/code-runtime-worker-thread/tests/`, `packages/code-runtime/code-runtime-python/tests/protocol-mirror.e2e.ts`.

## Telemetry capture

Stages: session lifecycle and event listeners → per-record capture (`ledger`/`ops` channels, severity pre-mapped from tool errors, turn errors, agent errors) → `session-telemetry/record` waterfall (the redaction extension point; a throwing listener withholds that record, fail-closed) → OTLP export with the anonymous user id attached. Expected data: the canonical log is never rewritten; one failing backend never starves peers. Tests: `packages/session/session-telemetry/tests/`, `packages/session/session-telemetry-otel/tests/`.

## Snapshot-of-record: the test world's own data flow

Stages: `MockAdapter` scripts `StreamChunk` responses → the loop records the transcript through the real session/registry/pipeline → invariants oracle re-derives balance from the log. Rule: mock only the expensive or non-deterministic boundary (model, network, clock); a keyword probe on the agent's own output never substitutes for re-reading the world (files on disk, re-run commands). Tests: `packages/core/agent-loop/tests/mock-adapter.ts`, `docs/testing.md`.
