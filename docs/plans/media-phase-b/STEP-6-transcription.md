# Step 6 — speech-to-text

**Status:** done — reviewed and committed 2026-10-11

## Intent

Serve `POST /v1/audio/transcriptions` on `vllm` and `llama-server` deployments. It is
an upload format producing `audio_seconds_in`:
- on `vllm`, the seconds the answer reports;
- on `llama-server`, the measured upload, which is step 7's.

Tokens follow OVERVIEW decision 9.

## Files likely touched

- **The names, on both halves at once:**
  - `protocol/schema/config.schema.json`: `audio_transcriptions` in the endpoint
    names;
  - `protocol/fixtures/backend-types/vllm.json` and `llama-server.json`:
    `audio_transcriptions` in their endpoints;
  - `control/kaiak-control`: `ENDPOINT_SUPPORT`, `MODEL_ENDPOINTS`, the types, the
    packaged schema, tests;
  - `gateway/internal/config`: the name and the support table.
- `gateway/internal/provider/`:
  - the endpoint (path `audio/transcriptions`), an upload format;
  - `vllm.go` and `llama_server.go` serve it;
  - vLLM's `404` naming the model read as the model missing (check it matches the
    existing reading);
  - the answer relayed as the backend sends it, its content type kept.
- `gateway/internal/server/`:
  - the `bodyEndpoints` row: `audio_transcriptions`, operation `generate_content`,
    output type `text`, an upload endpoint, no output-limit keys;
  - the owned parts (decision 26):
    - `file`: exactly one file part, required;
    - `stream` true refused;
    - `response_format` read;
    - `vllm_xargs` refused;
    - `timestamp_granularities[]` may repeat;
  - the request counts toward `audio_seconds_in` with nothing reserved, so its limits
    refuse once the window is spent and settle after.
- `gateway/internal/accounting/`:
  - a usage reader for the answer:
    - vLLM's `usage: {type: "duration", seconds}`;
    - `verbose_json`'s `duration`, rounded up;
    - llama-server's `usage: {type: "tokens", input_tokens, output_tokens}`;
  - the record:
    - `audio_seconds_in` from the reader (on `llama-server`, from step 7);
    - tokens as reported, else `tokens_in` the prompt's estimate, flagged;
  - the input estimate: the `prompt` part's text.
- `gateway/internal/fakebackend/`: transcription handlers in vLLM's shapes (`json`,
  `text`, `verbose_json`; `srt`/`vtt` refused) and llama-server's (`json` only, token
  usage), each parsing the rebuilt body.
- Metrics and the request line: `kaiak.usage.audio.input.duration` now produced;
  `gen_ai.output.type` `text`.
- `docs/specs/GATEWAY.md`, Providers → Endpoint support: a last column
  `audio_transcriptions` — `yes` for `vllm` and `llama-server`, `—` for every other
  type — and its source in the sentence before the table ("vLLM 0.30.0; llama.cpp at
  b11526; `docs/plans/media-phase-b/RESEARCH.md` records them") — with the code, as
  `TestEndpointSupportFollowsTheSpec` reads the table.
- Tests beside each.

## Decisions made during planning

- **No output-limit keys:** neither the gateway nor OpenAI's API sets one.
  `max_completion_tokens` (vLLM) and `max_tokens` (llama-server) pass untouched.
- **Usage members are read by exact name,** as phase A's speech usage is.
- **Until step 7, a `llama-server` transcription records 0 seconds, flagged
  `estimated`.** That is the decision 29 fallback, which step 7 replaces with the
  measured upload. Not a red: phase 3 ends after step 7.

## Acceptance criteria

- **Component and server tests:**
  - each owned part and refusal: no file, two files, `stream` true, `vllm_xargs`;
  - vLLM's seconds from `json`, `text` and `verbose_json`;
  - a missing or malformed usage;
  - llama-server's tokens;
  - `tokens_in` estimated and flagged where none is reported;
  - the `audio_seconds_in` limits refusing a spent window and settling after;
  - the operation and output type on the request line and metrics;
  - a transcription to a model whose list leaves it out refused before any backend.
- The shared fixtures pass on both halves with the new name.
- `scripts/check-all.sh` run and recorded. Expected red: none (phase 3 continues).

## Result

### What changed

- **The name, on both halves at once:**
  - `protocol/schema/config.schema.json`: `audio_transcriptions` in the endpoint enum
    (last) and its description;
  - `protocol/fixtures/backend-types/vllm.json` and `llama-server.json`:
    `audio_transcriptions` in `endpoints`; `config/valid/model-endpoints.json`: a model
    `asr` on a new vLLM backend `vllm-asr`, listing `audio_transcriptions` alone, so a
    valid config exercises the name on both halves;
  - `control/kaiak-control`: `MODEL_ENDPOINTS`, `ENDPOINT_SUPPORT` (`vllm`,
    `llama-server`); the packaged schema synced (`npm run sync-schemas`);
    `config.test.ts`: speech-to-text served by `vllm` and `llama-server` alone (a
    `vllm-omni` model listing it reported at its index);
  - `gateway/internal/config`: `EndpointAudioTranscriptions`, the support table, the
    schema-order names; `TestModelEndpointsResolve` (`asr`) and the spec table's
    column (`fixtures_test.go`).
- **`docs/specs/GATEWAY.md`:**
  - Providers → Endpoint support: the last column `audio_transcriptions` (`yes` for
    `vllm` and `llama-server`, `—` elsewhere) and its sources, in the step's words;
  - the errors table's `invalid_type` row names a transcription's `response_format`
    sent as a file part (below).
- **`gateway/internal/provider`:**
  - `AudioTranscriptions` (path `audio/transcriptions`, name `audio_transcriptions`)
    and `FormatSpeechToText` — an upload, answered by a JSON body (`jsonBodyStream`: an
    event stream answering one never completes), relayed with its content type;
  - `vllm.go`, `llama_server.go`: comments only. No module code was needed: vLLM's
    transcription 404 (``The model `X` does not exist.``, `code` the number 404) is
    read by the existing whole-word rule (`missingModelNamedOrCoded`), a route
    missing by the one "not found" reading, and llama-server's `501` is a plain 5xx.
- **`gateway/internal/accounting`:**
  - `transcription_usage.go` (new): `Transcription` on `MediaRequest` —
    `VerboseJSON`, and `Upload` / `UploadMeasured`, the measured upload step 7 sets;
    `transcriptionUsage`, a media reader keeping the answer's top-level `usage` (and,
    for a `verbose_json` request, `duration`) as the body streams through
    (`memberScanner`), never the text, every member by its exact name;
  - `audio_seconds_in`: the usage's `seconds` when its `type` is `duration`, else
    `verbose_json`'s `duration`, rounded up (`roundedUpSeconds`); else the measured
    upload; else 0, flagged `estimated` — so until step 7 a `llama-server`
    transcription records 0 seconds, estimated (decision 29's fallback);
  - tokens: a `usage` of `type` `tokens` (`input_tokens`, `output_tokens`, integers);
    else the meter's estimate — `tokens_in` the input estimate, output 0, flagged;
  - `NewMeter`: `FormatSpeechToText`; `accounting.go`: `OutputType` names `text`.
- **`gateway/internal/server`:**
  - `pipeline.go`: the row — `generate_content`, output type `text`, upload format
    `transcriptionUpload`, no output-limit keys;
  - `inbound_transcription.go` (new): `timestamp_granularities[]` may repeat; the
    owned parts in the client's order — `file` (a text part `invalid_type`, absent
    `missing_required_parameter`, a second refused by the form reader as
    `duplicate_member`), `stream` (`readStreamPart`), `response_format`
    (`uploadText`; exactly `verbose_json` tells the meter), `vllm_xargs` (refused,
    text or file), `prompt` (a text part, for `EstimateUploadInput`); the media
    `{audio_seconds_in: 0}` — counted, nothing reserved;
  - `inbound.go`: `inboundFields.Transcription`, handed to the meter
    (`mediaRequest`); the `Media` comment no longer speaks of a reserved upload;
  - `errors.go`: `errMissingFile`; `errExtraParams(param)` now names its member or
    part — `extra_params`, or `vllm_xargs`;
  - `limits.go`: the audio-processed refusal reads as audio generated's ("used N");
    the "requested" and "Request too large" variants are gone with the reservation.
- **`gateway/internal/limits`:** comments say audio processed reserves nothing;
  `media_test.go`: `TestAudioProcessedLimitsSettleAfterTheRequest` replaces the
  reservation test, and the control-plane and hour-count tests use a transcription's
  need (nothing reserved).
- **`gateway/internal/fakebackend`:**
  - `transcription.go` (new): `/v1/audio/transcriptions` in vLLM's shape or
    llama-server's (`SetTranscriptionShape`, the way `SetRerankShape` picks rerank's);
    vLLM: its model check against the models list (`404` "The model `X` does not
    exist."), `json`/`text`/`diarized_json` with duration usage, `verbose_json` with
    its duration and no usage, `srt`/`vtt`/others `400`; llama-server: `json` only,
    token usage; `SetNonAudioModels` — vLLM's missing route `404
    {"detail":"Not Found"}`, llama-server's `501 not_supported_error`; a file part
    required, an upload without its `Content-Length` and `stream=true` refused; the
    audio heard `Reply.AudioMillis`; a body fault cuts the answer halfway;
  - `form.go` (new): the edits' form reader moved and named for both uploads
    (`uploadForm`, `readUploadForm`); `omniError` renamed `vllmError` (vLLM-Omni's
    errors are vLLM's);
  - `cmd/fakebackend`: `-transcription-shape` and `-non-audio-models`.
- **Tests:**
  - `accounting/transcription_usage_test.go`: vLLM's seconds from `json`,
    `diarized_json`, `verbose_json` (fractions, exponent form, a name escaped or
    repeated, usage preferred) in pieces of any size; llama-server's tokens with and
    without a measured upload; 27 missing or malformed usages (no usage, wrong types,
    other names or cases, nested, in a string, cut, tokens malformed) each falling back
    to the measured upload or 0 estimated; counts at their edges; cut short, an error
    status, an event stream, never timed to a first token;
  - `provider/transcription_test.go`: both modules' URL, the rebuilt form byte for
    byte, `Accept: application/json`, the answer relayed whole; vLLM's 404 naming the
    deployment's model `upstream_model_missing`, another relayed; completeness and the
    content type kept (JSON, plain text, cut, an event stream);
  - `server/transcription_test.go`: the whole pipeline on `vllm` and `llama-server`
    (the rebuilt form, the answer as sent, the record, the estimate reserved alone,
    no output limit written though the model declares one, `generate_content` /
    `text` on the record, request line and usage metrics, the audio-input counter and
    log field, no TTFT); response formats on both servers (`srt`, `vtt`, llama-server's
    `text` and `verbose_json` relayed `400`s, no units); 21 refusals before any backend,
    none echoing a value, including unlisted on `vllm`, `vllm-omni` and
    `openai-compatible`; the parts read (verbose, estimate, nothing reserved);
    `audio_seconds_in_per_minute` admitting below its value, passed by a running
    request, then refusing both servers' transcriptions with `429 audio_seconds`
    while chat passes; a cut answer; the model missing, vLLM's missing route and
    llama-server's `501`, none retried; nothing of the prompt, filenames, audio,
    hotwords, `vllm_xargs` or the answer's text in the logs;
  - updated: `endpoints_test.go` (the upload form carries a `file` part too),
    `messages_test.go` (a `vllm` model's list includes `audio_transcriptions`),
    `limits_test.go` (the audio-processed message), `metrics_test.go`
    (`errMissingFile`, `errExtraParams(param)`).

### Decisions made here

- **One rule for the seconds, not a branch on the backend type:** the answer's
  seconds, else the measured upload, else 0 estimated. On the servers read it is the
  spec's per-type rule — vLLM reports seconds, llama-server never does, so it always
  takes the measured upload — and the meter needs no type. Step 7 only fills
  `Transcription.Upload` / `UploadMeasured`.
- **`usage.seconds` is read as a JSON number and rounded up** (vLLM sends an
  integer; OpenAI types it a number), as `verbose_json`'s `duration`; a negative
  length is 0, as a negative count is; one past int64 saturates (the record clamps).
- **`response_format`:** only the exact `verbose_json` reads `duration`; sent as a
  file part it is `400 invalid_type`, as an edit's is — the spec's `invalid_type` row
  now names it (the spec left the case open; no disagreement).
- **`vllm_xargs` uses the `extra_params` refusal** (same reason, same code
  `external_reference_unsupported`, the spec's), its message naming the part, rather
  than the URL-or-path refusal's wording.
- **The audio-processed refusal message** drops "requested" and "Request too large":
  nothing is reserved (the spec since step 1), so neither can occur.
- **Fixture:** a separate ASR model on vLLM rather than adding the name to the
  llama-server chat model.
- **The fake:** vLLM's model check reads the models list (vLLM checks its served
  names); llama-server's `input_tokens_details` holds `text_tokens` 0 and
  `audio_tokens` the input count — a guess at the split, unread by the gateway.

### Suite

`scripts/check-all.sh` — **all checks passed** (expected reds: none):
- gateway: gofmt, vet, staticcheck 2026.2.1, the telemetry boundary, `go test -race
  -count=1 ./...` — every package `ok`, `e2e` in 121 s;
- live-test kit: lint and self-test passed for every kind;
- control: `npm test` 675 tests, 674 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before; one more than step 5: the speech-to-text rule test);
  `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e: `ok` in 76 s.
