/**
 * Decode an SSE byte stream into event `data` payloads. Framing — chunk
 * reassembly, UTF-8/CRLF/BOM handling, comment and non-data field skipping,
 * multi-`data:` joining — is `eventsource-parser`'s. Comments are reported
 * only through an optional transport-activity callback. This module keeps the
 * DeepSeek protocol: the literal `[DONE]` is yielded so the caller owns final
 * flushing, and EOF before it raises {@link LlmError}. Framing is spec-strict:
 * an event dispatches only on its blank-line terminator, so an unterminated
 * tail at EOF is truncation, not a flushable payload.
 *
 * @module dsh-llm-deepseek/sse
 */

import { EventSourceParserStream } from 'eventsource-parser/stream'
import { LlmError } from '@deepseek-ai/dsh-llm'

/** The terminal payload DeepSeek (and OpenAI) send after the last chunk. */
export const DONE = '[DONE]'

/**
 * Parse an SSE byte stream into data payloads. Yields `[DONE]` as the final
 * value and returns; throws `LlmError('STREAM_CLOSED')` when the stream ends
 * without it (truncated response — the model call cannot be trusted).
 * @param stream - raw SSE bytes; reads may split anywhere, including mid-UTF-8 sequence.
 * @param onComment - optional transport-activity callback; comments never enter the yielded payload stream.
 * @returns each event's data payload in arrival order, the `[DONE]` sentinel last.
 */
/**
 * Optional attribution context for abnormal stream termination. `context` is a
 * stable, log-safe summary of the transport boundary (provider host, HTTP
 * status, model); it is embedded verbatim in the STREAM_CLOSED error message
 * and the stderr diagnostic line so upstream cuts can be root-caused from the
 * persisted turn frames alone.
 */
export interface SseDiagnostics {
  context?: string
}

export async function* parseSse(
  stream: ReadableStream<BufferSource>,
  onComment?: (comment: string) => void,
  diag?: SseDiagnostics,
): AsyncGenerator<string> {
  const startedAt = Date.now()
  let eventCount = 0
  let comments = 0
  let lastData = ''
  let lastEventAt = startedAt
  const eventStream = stream
    .pipeThrough(new TextDecoderStream())
    .pipeThrough(
      new EventSourceParserStream({
        onComment: (comment: string) => {
          comments += 1
          onComment?.(comment)
        },
      }),
    )
  for await (const { data } of eventStream) {
    eventCount += 1
    lastData = data.length > 120 ? `${data.slice(0, 120)}…` : data
    lastEventAt = Date.now()
    yield data
    if (data === DONE) return
  }
  // STREAM_CLOSED attribution (ADR: 上游断流归因): the summary travels with the
  // error message — turn/end frames carry it into the persisted interaction
  // log, so sandbox teardown cannot destroy the evidence.
  const detail =
    `events=${eventCount} comments=${comments} ` +
    `lastAge=${Date.now() - lastEventAt}ms elapsed=${Date.now() - startedAt}ms ` +
    `last=${JSON.stringify(lastData)}${diag?.context ? ` ${diag.context}` : ''}`
  console.error(`[dsh-llm] SSE stream ended without [DONE] (STREAM_CLOSED) ${detail}`)
  throw new LlmError(`SSE stream ended without [DONE] (STREAM_CLOSED; ${detail})`, 'STREAM_CLOSED')
}
