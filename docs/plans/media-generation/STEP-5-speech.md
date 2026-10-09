# Step 5 — speech: the whole clip and the raw-audio stream

**Status:** not started

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
    - `response_format` other than `wav`/`pcm` refused;
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
    - PCM: the rate settled in step 1 and 16-bit mono;
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

(filled in when the step is done)
