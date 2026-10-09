# Step 6 — speech: the SSE stream

**Status:** done — reviewed and committed 2026-10-09

## Intent

Serve speech's SSE mode (`stream: true` or `stream_format: "sse"`):
- `speech.audio.delta` events relay, their base64 audio counted toward
  `audio_seconds_out`;
- `speech.audio.done` carries the token usage and completes the stream;
- `speech.audio.error` is classified as step 1 settled.

## Files likely touched

- `gateway/internal/provider/stream_format.go`: a speech stream format —
  - completeness at `speech.audio.done`;
  - the error event's classification;
  - no request edits beyond the model and the output-limit default.
- `gateway/internal/accounting/`:
  - the speech reader's stream events: usage from `done`, the delta's audio length
    from its base64 size and the format's sample rate;
  - a WAV header inside the first delta read as in step 5.
- `gateway/internal/server/inbound.go`: SSE accepted.
- The fake backend: the SSE mode (delta, done, error).
- Tests beside each.

## Decisions made during planning

- **The base64 is never decoded whole.** The audio length is the decoded size of each
  delta, computed from its encoded length and padding. Only a leading WAV header is
  decoded.

## Acceptance criteria

- **Component tests:**
  - completeness;
  - a stream broken before `done` (partial: the seconds sent, tokens estimated);
  - the error event (backend failure versus the caller's);
  - usage from `done`;
  - the delta length (PCM, WAV);
  - the timers (first event, stall).
- `scripts/check-all.sh` run and recorded. Expected red: none.

## Result

### What changed

- `gateway/internal/provider/`:
  - `stream_format.go`: `speechStream` reads the SSE stream by its data's `type`:
    complete once it carried `speech.audio.done`; every `speech.audio.error` an error
    event of kind failure, whatever type or code it names (an `error` that is not an
    object, or none, too), its code the error's `code` — vLLM-Omni's integer `500`,
    which `backendIdentifier` logs as `"500"`. Relayed through `withGatewayMessage`
    (type, code, `partial_audio`, `action` kept, the message the gateway's); the plain
    form is vLLM-Omni's own event, `{"type":"speech.audio.error","error":{"message",
    "type":"server_error","param":null,"code":500}}`. No request edit beyond the model
    and the output limit; no nested model.
  - `provider.go`: `FormatSpeech`'s comment (an audio answer, or its audio in events).
- `gateway/internal/accounting/`:
  - `audio.go`: the count-only entry, `audioClip.count(n)` — a clip takes by number
    the bytes it does not read (samples; a chunk it skips) and never a WAV header's,
    which come through `write`; `wavClip.write` now goes through it for skipped chunks
    and data. `formatClip` (by `response_format`: `wav`, `pcm` at the request's rate)
    under `newAudioClip`. `emptyClip`: no audio relayed, 0 s exactly. `writeBase64`:
    the decoded size from length and padding (`base64Text` strips at most two `=`,
    `base64Size`: 3 bytes per 4 characters, 1 or 2 for a last group of 2 or 3, a lone
    character none); the bytes the clip must read decoded 48 at a time from their
    four-character groups alone (`base64Bytes`), the rest counted.
  - `speech_usage.go`: `streamEvent` — a `speech.audio.delta`'s audio into the clip of
    the first delta's `response_format` (a later delta naming another, or a WAV header
    that is not base64, leaves the answer unmeasured); `speech.audio.done`'s usage
    (`speechEventUnits`: `input_tokens` and `output_tokens` both JSON integers, else no
    report). `answered(header, stream)`: an event stream is read by its events, its
    headers not at all — the whole clip's usage headers on an SSE answer count nothing.
    `mediaSeen` turns true on the first delta carrying samples (not the header's, not
    an empty one).
  - `meter.go`: `mediaReader.answered` takes the meter's `stream`.
- `gateway/internal/server/`:
  - `inbound_speech.go`: `stream_format: "sse"` sets `inbound.Stream` (`stream: true`
    already did): the SSE stream takes the stream timers and `gen_ai.request.stream`.
  - `errors.go`: `errSpeechEventStream` removed; `metrics_test.go`: its
    `errorAnswers` entry removed.
- `gateway/internal/fakebackend/`:
  - `speech.go`: the SSE mode (`writeSpeechEvents`), as vLLM-Omni's
    `_generate_audio_sse_events` writes it: `event: speech.audio.delta` per piece —
    `{"type","audio": base64 (padded),"response_format"}` — a WAV stream's 44-byte
    header (placeholder sizes) a delta of its own first, then `len(Chunks)` PCM pieces;
    `speech.audio.done` with `usage` (`input_tokens`, `output_tokens`, `total_tokens`,
    `input_token_details`), none with `OmitUsage`. Faults before piece `At` (At 0 before
    the header): Hang, Cut, End, and ErrorEvent — `speech.audio.error`, `server_error`
    (or `Code` as the error's type), code 500, `partial_audio`/`action: "discard"` once
    a delta was sent, its message a distinctive text ("CUDA error on device 3…") that
    tests prove never reaches the client or the log.
  - `fakebackend.go`: an `ErrorEvent` fault's event is named by its data's `type`
    (`error` for Messages and Responses, `speech.audio.error` for speech); comments.
- `docs/specs/GATEWAY.md` (Accounting → Media usage, the SSE bullet, dated
  2026-10-09): the WAV header read however the deltas split it; the stream's format
  is its first delta's `response_format`; a later delta naming another, or a WAV
  header that is not base64, leaves the answer unmeasured.

### Design choices

- **One method on the clip, not a second clip type.** `count` takes what a clip would
  not read anyway, so `writeBase64` only asks the clip; the WAV walk is not duplicated
  and the raw bytes and the base64 paths share it.
- **Header bytes are decoded in 48-byte windows**, written through `write`, which may
  carry a few data bytes past the header — counted as `write` counts them. The loop
  decodes only while the clip is in a header state; a skipped chunk (e.g. `LIST`) is
  counted, not decoded.
- **The reader is told the answer is a stream** (`answered(header, stream)`) rather
  than guessing from the content type: an SSE answer reads no usage headers, an audio
  body still reads its content type.
- **An SSE answer that relayed no delta counts 0 s, measured** (`emptyClip`): no audio
  was relayed, which the spec counts as 0, not as unmeasurable.
- **Usage from `done` follows the JSON usage readers**: a count past int64 or a
  fraction is malformed (no report), a negative one clamps to 0 (`nonNegative`, as
  every usage object the gateway reads) — the whole clip's headers saturate past
  int64 instead, as step 5 settled.
- **A lenient size, a strict header**: PCM and data bytes are counted from the length
  whatever their characters (never decoded); a WAV header that does not decode makes
  the answer unmeasured.
- **The error event's code is the error's `code`** (500), not its `type`: the format
  names a code.
- **`withSpeechModels` applies its edits after the speech backends and models are
  in**, so a test can lower `omni-slow`'s first-event timeout or add a deployment;
  the two existing callers' edits are unaffected.

### Spec points (reported)

- vLLM-Omni's SSE generator read at the pinned commit `a8576ccb`
  (`serving_speech.py`, `_generate_audio_sse_events` and `_generate_audio_chunks`):
  the delta's `response_format` is the validated, lower-cased format; the WAV header
  is always yielded as a chunk of its own before the first audio (a header split
  across deltas cannot happen there — handled and tested anyway, at no extra code);
  an empty delta (`"audio":""`) can be sent; `done` carries `usage.model_dump()`;
  `error` is `{"type":"speech.audio.error","error":{message, type: "server_error",
  param: null, code: 500[, partial_audio: true, action: "discard"]}}`.
- **A first-event `speech.audio.error`'s record is zeros flagged `partial`**: the
  existing rule for an attempt answered by refusing nothing processed (`Refused`,
  then no answer). "None bills anything" holds; the flag is the generic one.
- The clarification above is the only spec edit; the rest of the SSE contract was
  already written at step 1.

### What step 7 needs

- The batch item's audio: `newAudioClip(item.media_type, item rate)` (vLLM-Omni's
  `media_type` is `audio/wav` or `audio/pcm`) and `writeBase64(clip, audio_data)` —
  one clip per item; `media()` must sum the items' exact durations
  (`clip.length()`) and round once (`wholeSeconds`), as the spec says.
- The item's `usage` has the SSE `done` usage's shape: `speechEventUnits` reads one;
  the batch sums them, an item without one leaving no report.
- The speech batch is its own format (OVERVIEW decision 14): its stream format must
  not be `speechStream`, which completes on `speech.audio.done` — an event stream
  answering a batch is never complete (spec, Providers: media answers).

### Tests

- `provider/error_event_test.go`: every `speech.audio.error` a failure — `server_error`
  500, a caller-like type, a busy-like code, an `error` that is a string, none; deltas,
  `done`, a Messages-shaped `error` and invalid JSON no error event; the code is `500`.
  The relayed event keeps type, code, `partial_audio`, `action`, drops the host and
  model in the message; a message named twice gets the plain form.
- `provider/speech_test.go`: completeness — `done` (with deltas, alone, named by its
  data alone) complete; deltas alone, `done` named by its event line alone, an
  unknown event incomplete. The SSE stream through the fake (WAV via `stream`, PCM
  via `stream_format`): relayed event by event, the header its own first delta,
  44 + 120 000 and 40 000 bytes of audio, `done` with usage; sent with the model and
  `max_new_tokens` edits alone and `Accept: text/event-stream`. Error events: first →
  `CodeErrorEvent`, failure, code 500; after two deltas → relayed with the gateway's
  message, `ErrorEventEnd`, incomplete.
- `accounting/speech_usage_test.go`: usage from `done` — reported (with details),
  zero, negative → 0; missing, null, not an object, one count, a null count, a count
  as text, fractional, past int64 → estimated; usage headers on an SSE answer not
  read. Delta length (exact durations) — PCM; PCM unpadded; padded by one and two;
  PCM counted not decoded; empty; WAV header its own delta; WAV unpadded; header split
  across four deltas; header and audio in one delta; split mid-header; a `LIST` chunk
  before the data; at most the declared size; the header alone (0 s); no delta (0 s,
  measured). `base64Size` over every length and padding. Unmeasurable: mp3, no
  format, PCM without a rate, formats mixed, a data-chunk header that is not base64
  (followed by a readable one), an unreadable WAV header — 0 s, estimated, tokens
  kept. Broken before `done` (cut, or an error event): 2 s for 1.25 s, tokens
  estimated, partial. First samples: not the header delta, not an empty delta.
- `server/speech_test.go`: the pipeline in six modes (SSE WAV and PCM added): events
  relayed, audio decoded to the right size, record tokens from `done` (7, 4, not
  estimated), 3 s, cost, `gen_ai.request.stream: true`, time to first token observed;
  refusals without the two SSE rows. `TestSpeechEventStreamTimers`: first-event
  timeout (`omni-slow` at 150 ms) → `504 upstream_timeout`, "no first event within
  150ms", the prompt billed estimated and partial; stall after the header and one
  piece → relay cut, 1 s, tokens estimated, partial, `upstream_stalled`.
  `TestSpeechEventStreamEnds`: cut (`upstream_failed`) and ended without `done`
  (`upstream_incomplete`) after two pieces → 2 s, estimated, partial, `broke_off`;
  `done` without usage → complete, estimated, 3 s. `TestSpeechEventStreamErrorEvents`:
  after the first event — relayed (type, code 500, `partial_audio`, `action`, the
  gateway's message; no backend text), connection cut, 2 s, estimated, partial,
  `upstream_incomplete`, `broke_off`; as the first event — retried on a second
  deployment (`kaiak.tried` `…:upstream_error,…:200`, `kaiak_retries_total` 1), one
  record, the answer's; alone — `502 upstream_error`, `kaiak.upstream.error.code`
  `"500"`, zeros, no backend text in the log. Logs: an SSE answer and an SSE error
  event leave no base64 audio (`UklGR`) or backend text (`CUDA`) in the log.
- Checked by mutation (each reverted): `complete` never true; the error event read
  as the caller's; the error event not seen; usage headers read on SSE; size ignoring
  padding; formats mixed kept; a header decode failure ignored; `sse` not setting the
  stream flag; `done`'s usage ignored; an SSE answer with no delta unmeasured — each
  fails its tests (a `count` taking nothing in a skipped chunk loops, caught by the
  test timeout).

### Suite (2026-10-09)

Phase 2 continues; no expected reds. `scripts/check-gateway.sh` (a first run stopped
at gofmt on one test file; fixed, re-run green), then `scripts/check-all.sh` twice
(the second after this record was written) — every run `all checks passed` /
`gateway checks passed`, exit 0:

- **Gateway:** gofmt, vet (crosshalf tag included), staticcheck 2026.2.1 on
  go1.27.1, telemetry boundary pass; `go test -race -count=1 ./...` — all 20 packages
  `ok`, `e2e` included (≈119 s).
- **Live-test kit:** gofmt, vet, staticcheck pass; `-self-test` passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends (37/36/22/21/16/16/34 passed, 0 failed).
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 0 cancelled, 1 skipped.
  **`npm run lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈70 s).
