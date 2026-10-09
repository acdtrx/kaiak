# Step 5 — speech: the whole clip and the raw-audio stream

**Status:** done — reviewed and committed 2026-10-09

## Intent

Serve `POST /v1/audio/speech` on `vllm-omni` deployments in two modes:
- **the whole clip** (no `stream`, no `stream_format`): tokens read from the response
  headers;
- **the raw-audio stream** (`stream_format: "audio"`): tokens estimated from the input.

In both modes `audio_seconds_out` is measured on the audio relayed to the client. The
SSE mode is step 6.

## Files likely touched

- `gateway/internal/provider/`:
  - the endpoint (path `audio/speech`) and a speech format;
  - `vllm-omni` serves it;
  - the request edits: the model, and the output-limit default under
    `max_new_tokens`;
  - the response headers made available to the meter (they are not relayed);
  - the answer is relayed byte for byte (its content type kept).
- `gateway/internal/server/`:
  - the `bodyEndpoints` row (`audio_speech`, operation `generate_content`, output type
    speech, output-limit key `max_new_tokens`);
  - the inbound parser:
    - the owned fields (OVERVIEW decision 13);
    - `response_format` other than `wav`/`pcm` refused; `pcm` without `sample_rate`
      refused (OVERVIEW decision 18);
    - `ref_audio` with a URL or path refused;
    - the mode from `stream`/`stream_format`, which sets the timers and
      `gen_ai.request.stream`;
    - SSE is refused until step 6.
- `gateway/internal/accounting/`:
  - the input estimate (text, plus a reference audio counted as one media item);
  - a usage reader:
    - tokens from the `x-vllm-omni-*` headers;
    - none on the raw stream, so estimated and flagged;
  - an audio-length reader:
    - WAV: the header's sample rate, channels and bit depth, then the bytes actually
      received — never the header's size fields;
    - PCM: 16-bit mono at the request's `sample_rate`;
    - `audio_seconds_out` rounded as step 1 says.
- The fake backend:
  - speech, both modes, with the usage headers;
  - WAV and PCM bodies of a known length;
  - its route-missing answer for a non-speech model.
- Tests beside each.

## Decisions made during planning

- **The audio length is read in one pass** as the body streams through. Nothing is held
  beyond a header's bytes.
- **A client gone mid-clip settles the seconds sent**, as a broken-off chat settles the
  tokens sent.

## Acceptance criteria

- **Component tests:**
  - owned fields and each refusal;
  - the output-limit default and ceiling under `max_new_tokens`;
  - usage from headers (present, missing → estimated, malformed);
  - WAV length (whole, streamed with a placeholder size, odd chunking);
  - PCM length (with and without `sample_rate`);
  - the raw stream's estimate flagged;
  - the operation and output type on the request line and metrics;
  - the new units on the record;
  - the `audio_seconds_out` limits counting it.
- **A non-`vllm-omni` model** answers `400 endpoint_not_served`.
- `scripts/check-all.sh` run and recorded. Expected red: none (phase 2 continues).

## Result

### What changed

- `gateway/internal/provider/`:
  - `provider.go`: the `AudioSpeech` endpoint (path `audio/speech`) and
    `FormatSpeech`; `Response.BackendHeader()` — the backend's response headers as
    sent, for the gateway's own reading (accounting), never relayed; `Request.Stream`
    is speech's mode (the raw-audio stream takes the stream timers).
  - `response.go`, `send.go`: the response keeps the backend's headers; a module
    names headers it relays beside the core's allowlist (`wireCall.relayed`), so the
    core names no server.
  - `vllm_omni.go`: `vLLMOmniEndpoints = {AudioSpeech}`; a speech answer relays
    `X-Word-Timestamps` and `X-Word-Timestamps-Omitted` (never logged).
  - `stream_format.go`: `speechStream` — no request edit beyond the model and the
    output limit; an event stream answering speech is not read and never complete
    (step 6 replaces it).
- `gateway/internal/accounting/`:
  - `audio.go` (new): `audioClip`, measured in one pass as the body streams through.
    WAV: the chunks walked up to `data` (others skipped by size and pad byte), the
    `fmt ` chunk's rate, channels and block size (format tags PCM `1` and extensible
    `0xFFFE`; block size must be channels × bytes per sample), then the data bytes
    received, at most the declared size — never a size field; at most 16 header bytes
    held. PCM: 16-bit mono at the request's `sample_rate`. Anything else, PCM without a
    rate, or a WAV header it cannot read (incl. cut short before its data chunk) is
    unmeasured. The exact length in nanoseconds (128-bit division, saturating),
    rounded up to whole seconds once per record.
  - `speech_usage.go` (new): `Speech` (the request's `SampleRate`, `RawStream`) and
    the speech reader — the whole clip's tokens from `x-vllm-omni-input-tokens` /
    `-output-tokens` (both present, each once, digits alone; past int64 saturates,
    then the protocol bound clamps), none on the raw stream; output estimate 0.
  - `meter.go`: `NewMeter(ep, input, speech)`; an optional `mediaReader` (a format
    whose answer is media) gets the answer's headers (`Answered(status, stream,
    header)`), the body pieces, adds its media units at settlement — unmeasured media
    count 0 and flag the record estimated — and says when samples were first seen.
- `gateway/internal/server/`:
  - `pipeline.go`: the `audio_speech` row — operation `generate_content`, output type
    `speech`, output-limit key `max_new_tokens`.
  - `inbound_speech.go` (new), `inbound.go`: the owned fields — mode from
    `stream`/`stream_format` (raw-audio stream → `inbound.Stream`, the stream timers,
    `gen_ai.request.stream`), `response_format` `wav`/`pcm` (else
    `response_format_unsupported`), `sample_rate` (required with `pcm`), `ref_audio`
    (string or each list entry), `ref_audio_2`, each `references[].audio_path` must
    start `data:` in any case (else `external_reference_unsupported`, `param` naming
    it; wrong shapes `invalid_type`; a references entry naming a member twice
    `duplicate_member`); `inbound.Media = {audio_seconds_out: 0}`; `inbound.Speech`
    for the meter.
  - `errors.go`: `errMissingSampleRate` (`400 missing_required_parameter`, `param:
    "sample_rate"`) and `errSpeechEventStream` (the SSE refusal, below).
  - `relay.go`: an audio body (`audio/*`) is flushed after each read and gets
    `X-Accel-Buffering: no`; the raw-audio stream's time to first token is its first
    samples (not a WAV header alone).
  - `attempts.go`: the meter built with `inbound.Speech`, told the backend headers.
  - `params.go`: comment only (speech among the output-limit endpoints).
- `gateway/internal/fakebackend/`: `speech.go` (new) — `/v1/audio/speech`, the whole
  clip (WAV with real sizes, or PCM; `Content-Length`; `X-VLLM-OMNI-*` usage headers
  upper-case unless `OmitUsage`) and the raw-audio stream (`stream_format: "audio"`:
  a 44-byte WAV header with `0xFFFFFFFF` sizes flushed alone, then `len(Chunks)`
  paced pieces; `Fault` Hang/Cut/End; no usage), 16-bit mono silence at the request's
  `sample_rate` (24 kHz without one), `Reply.AudioMillis` long (default 2500);
  `SetNonSpeechModels` — vLLM-Omni's `400 BadRequestError "TTS model did not produce
  audio output."`; an unknown `stream_format` refused `400`; the SSE mode not played
  (`400`). `fakebackend.go`: the route, `AudioMillis`, the pacing factored into
  `streamWriter.await`.
- `docs/specs/GATEWAY.md` (two clarifications, dated 2026-10-09): `X-Accel-Buffering:
  no` on every body flushed as it is read, audio included; the whole clip's usage
  headers each sent once, digits alone.

### Design choices

- **The meter gets the headers at `Answered`**, from `Response.BackendHeader()` — the
  whole upstream header, never relayed (`Header()` stays the client's). The core
  stays type-agnostic; accounting knows the speech format's header names.
- **`mediaReader` is an optional extension of `usageReader`** (a type assertion in
  `NewMeter`), not new methods on every reader; steps 6–8 extend the same reader.
- **`Speech` is `NewMeter`'s third argument** (zero for other endpoints); step 7 can
  extend it with per-item values.
- **The raw-audio stream rides the existing body path with the stream timers**:
  `Request.Stream` true gives the first-event timer to the first body bytes and the
  stall timer to each read — no new timer code.
- **SSE (step 6) fails closed**: `stream: true` or `stream_format: "sse"` →
  `400 invalid_value`, `param` naming the member, "speech as server-sent events …
  is not served"; the inbound part that tells SSE apart is built
  (`readSpeechMode`). The fake refuses SSE too.

### Spec points (reported, not changed)

- **SSE is refused until step 6**, against the spec, which already serves it.
- **`Accept`**: the core sends `Accept: text/event-stream` for the raw-audio stream
  (`Request.Stream`) and `application/json` for the whole clip. vLLM-Omni ignores
  `Accept` and the spec names no value; left as it is.
- **Input estimate of an upper-case `DATA:` reference**: inbound admits it (schemes
  compare case-insensitively), but the estimate's data-URL test reads `data:` only,
  so it counts as text bytes — an over-estimate, never an under-count.
- **WAV formats**: IEEE float (tag 3) is unmeasured (the spec names PCM and the
  extensible format; vLLM-Omni writes `PCM_16`). A WAV declaring `0` as its
  streamed data-size placeholder would count 0 s by "at most the size it declares";
  vLLM-Omni and OpenAI write `0xFFFFFFFF`.
- **Rule order of the mode**: `stream: true` with an unknown `stream_format` is the
  SSE stream (refused now); an unknown `stream_format` without `stream` passes, timed
  as the whole clip, and the backend refuses it.

### Tests

- `accounting/audio_test.go`: WAV length — whole, streamed with placeholder sizes,
  chunks before and between `fmt`/`data` with pad bytes, the extensible format,
  stereo 24-bit, a `fmt` longer than its fields, a chunk after `data`, an empty and a
  one-sample `data` — each read whole and split at 1, 3, 43, 44, 45 and 4096 bytes;
  cut short; every unreadable header; PCM with and without a rate; other content
  types; the exact duration and the rounding.
- `accounting/speech_usage_test.go`: usage from headers (present, zero, leading
  zeros, past int64; missing, one of two, not a number, signed, fractional, empty,
  repeated → estimated); the raw stream estimated (WAV, PCM); unmeasurable audio →
  0 s, estimated; a clip broken off (partial, header usage kept); an error answer;
  the first samples as the stream's first content.
- `provider/speech_test.go`: passthrough (model edit, `max_new_tokens`, the rest
  untouched) and the audio relayed byte for byte for the whole clip (WAV, PCM) and the
  raw stream; the headers relayed (content type, word timing) and kept back (usage,
  request ID, cookie, server), the backend headers whole, no word timing on another
  endpoint; completeness by framing, cut short, an event stream never complete.
  `endpoints_test.go`: speech in the media column; speech's stream untouched.
- `server/speech_test.go`: the pipeline in four modes (whole clip WAV/PCM, raw
  stream WAV/PCM): sent body, relayed body, headers, flush, the record (tokens,
  `audio_seconds_out` 3 for 2.5 s, flags, operation, output type, cost), the request
  line, the usage metrics (`kaiak_usage_audio_output_duration_seconds_total`), the
  route, time to first token on the raw stream only; 35 refusals (every
  reference-audio variant, formats, PCM without a rate, wrong types, duplicates, the
  output limit out of range, SSE, a non-`vllm-omni` model → `400
  endpoint_not_served`); inline references and unowned fields passing untouched, an
  unknown `stream_format` relayed and timed as the whole clip; the output-limit
  default, ceiling, a kept value, the unowned `max_tokens`, no limit on a model
  without one, the reservation; usage headers missing or malformed → estimated; the
  input estimate; a client gone mid-clip (2 s for 1.25 s relayed, partial); the
  timers (whole clip: response timeout, not retried; raw stream: stalled, partial);
  the `audio_seconds_out_per_minute` and `…_per_hour` limits counting and refusing
  (`429 audio_seconds`), chat untouched; a model that does not speak (`400` relayed,
  zero units); nothing of the text, reference audio, word timing or audio in logs.
- `server/metrics_test.go`: the two new error answers listed.
- Checked by mutation (each reverted): dropping the media hook, relaying the usage
  headers, timing the raw stream not, flushing audio not, counting the WAV's declared
  size, reading headers on the raw stream — each fails its test.

### Left for step 6

- Remove `errSpeechEventStream` (and its `errorAnswers` entry) and the refusal in
  `readSpeechMode`; SSE sets `inbound.Stream`.
- `speechStream`: complete at `speech.audio.done`; every `speech.audio.error` a
  failure; its relayed error's plain form.
- `speechUsage.streamEvent`: usage from `done`; each delta's audio by its decoded
  size; the clip from the delta's `response_format` (today `answered` builds it from
  the content type, which for SSE is `text/event-stream` — unmeasured); the leading
  WAV header decoded into `wavClip`, the data counted by length (`wavClip` needs a
  count-only entry for base64-sized data); `mediaSeen` on the first delta carrying
  samples.
- The fake backend's SSE mode (it refuses SSE with `400` today).

### Notes for later steps

- Step 7: extend `accounting.Speech` with the items' effective formats and rates;
  sum item durations into one record before rounding (`audioDuration`,
  `wholeSeconds`).
- Steps 9–10: `cmd/fakebackend` has no flag for `SetNonSpeechModels` or
  `AudioMillis` yet.

### Suite (2026-10-09)

Phase 2 continues; no expected reds. `scripts/check-gateway.sh` once, then
`scripts/check-all.sh` twice (the second after this record was written) — every run
`all checks passed`, exit 0:

- **Gateway:** gofmt, vet (crosshalf tag included), staticcheck 2026.2.1 on
  go1.27.1, telemetry boundary pass; `go test -race -count=1 ./...` — all 20 packages
  `ok`, `e2e` included (≈118 s).
- **Live-test kit:** gofmt, vet, staticcheck pass; `-self-test` passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends (37/36/22/21/16/16/34 passed, 0 failed).
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 0 cancelled, 1 skipped.
  **`npm run lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈70 s).
