# Step 1 — contract

**Status:** done — reviewed and committed 2026-10-09

## Intent

Settle everything phase A builds in the documents that own it, before any code, with
the server behaviour each rule rests on read from vLLM-Omni's source at its latest
release, and the OpenTelemetry values checked at the pinned conventions.

## Files likely touched

- `docs/specs/GATEWAY.md`, each section dated:
  - **Client API:** the three endpoints and their formats, their owned fields and
    refusals (OVERVIEW decisions 5, 6, 7, 11, 13–15), and the error-table rows.
  - **Providers:** the `vllm-omni` type, its endpoint-support column, its paths, its
    wrong-endpoint reading (no core endpoints), and what passes and what is edited.
  - **Limits:**
    - the caps;
    - the six limit types: their windows, what each counts, and what is reserved
      before the request (images: `n`; audio generated: nothing, settled after);
    - the input estimate for speech (text) and images (prompt).
  - **Accounting:**
    - the three units, and what fills each (decisions 16, 17);
    - audio length — WAV, PCM, and the sample rate (decision 18);
    - estimation for raw-audio streams;
    - partial answers.
  - **Observability:**
    - the operation name and `gen_ai.output.type`;
    - the new metrics (decision 20);
    - the request line's new fields;
    - the cardinality notes.
- `docs/specs/CONTROL-PROTOCOL.md`:
  - config format 6 and protocol 6;
  - the units on the usage record;
  - the new totals windows (per-hour images and audio) and how the control plane sums
    them;
  - the new limit types and caps.
- `docs/kaiak.md`: v1 scope gains speech and image generation on `vllm-omni`.
- `protocol/schema/`:
  - `config.schema.json`: the backend type, the limit types, the caps, format 6;
  - `usage-record.schema.json`: the units;
  - `totals.schema.json`: the windows;
  - any schema that names protocol 5.
- `protocol/fixtures/`:
  - every fixture moves to format and protocol 6;
  - valid fixtures carry the new type, units, limits, caps and totals;
  - invalid cases for each new bound.
- `docs/BACKLOG.md`: "Pricing media units" (revisit trigger: a priced image or speech
  model is needed).

## Decisions made during planning

- **Read vLLM-Omni's source, not memory**, at its latest release (v0.31.0 if out,
  else v0.30.0). Record the version and commit of each file read:
  - `vllm_omni/entrypoints/openai/api_server.py`;
  - `protocol/audio.py` and `protocol/images.py`;
  - `serving_speech.py` and `speech_usage.py`;
  - the image serving code.
- **What to read there:**
  - the speech response headers;
  - the SSE events (`speech.audio.delta`, `.done`, `.error`) and their error codes;
  - the batch answer;
  - the WAV header vLLM-Omni writes when streaming (does it carry the length?);
  - the PCM sample rate and sample format per supported speech model;
  - the image answer and `file` format;
  - the route-missing answer;
  - how a `model` mismatch is answered (`400`?) — for the wrong-model rule.
- **Decision 18 is settled here, with evidence.** If every speech model vLLM-Omni lists
  natively produces 24 kHz 16-bit mono PCM (or resamples to the request's
  `sample_rate`), "24 kHz when the request sets none" is the rule, stated with its
  evidence. If any model differs, the rule is a declared rate per model, or PCM
  refused without `sample_rate` — say which and why.
- **Error classification for speech:**
  - `speech.audio.error` with `code` 500 is a backend failure after the first event.
  - Step 1 reads which other codes vLLM-Omni can send there and classifies each, as
    the Responses error events were.
- **The OpenTelemetry check:** at the pinned semantic-conventions commit, confirm
  `generate_content`, `gen_ai.output.type`'s values for image and speech, and that no
  image or audio-length metric exists. Choose the `kaiak.*` names in the existing
  style.

## Acceptance criteria

- Every rule is in its owning doc, dated, with what it rests on.
- Schemas and fixtures at format and protocol 6, with an invalid case for every new
  bound.
- `scripts/check-all.sh` run and recorded. Expected reds:
  - both halves' fixture and version tests, cleared by step 2 (`kaiak-control`) and
    step 3 (gateway);
  - the cross-half e2e, cleared by step 3.

## Result

### Rulings on the step's findings (the user, 2026-10-09)

The first pass flagged three findings against settled decisions; the user ruled, and
this revision applies the rulings:

1. **`vllm-omni` serves the media endpoints only** (decision 8 changed): speech,
   speech batch, image generation (phase B adds image edits and transcription). Chat,
   completions, embeddings, Messages, Responses and rerank to a model whose
   deployments are all `vllm-omni` answer `400 endpoint_not_served`, which closes
   the side door the source showed — vLLM-Omni's chat generates images on a
   diffusion model and speaks by default on an Omni model with a talker stage,
   counted only as tokens. Backlog: **Omni chat models**.
2. **Image `response_format` is `b64_json` only** (decision 7 changed): `file` is
   refused as `url` is (`400 response_format_unsupported`, `param:
   "response_format"`); `images` is always the answer's `data[]` count. Reserving
   `n` stays; a layered model's answer (one image per layer) can take an image
   window past its value — a per-minute limit by that one answer — and settles
   exactly. Backlog: **Image `file` responses**.
3. **`lora` refused** on image generation — confirmed.
4. **The counter bound stays 50 000** (about 1 000 groups expected at first), with
   five hour/month counts per scope; the spec states the count, no capacity claim.
5. **Decision 18** (PCM requires `sample_rate`) and the error classification stand.

### What changed

- `docs/specs/GATEWAY.md` (each rule dated 2026-10-09, with what it rests on):
  - **Client API:** the three endpoints; seven formats, speech, speech batch and
    images each its own (why, and what was rejected); errors in the OpenAI shape;
    error-table rows — `missing_required_parameter` (PCM without `sample_rate`),
    `invalid_value` (`max_new_tokens`; the two caps), new
    `response_format_unsupported` and `external_reference_unsupported`,
    `duplicate_member` (batch items, `references`), `upstream_model_missing` (the
    image `400`), `upstream_endpoint_missing` (vLLM-Omni's answers), a `429` row
    for image and audio limits (types `images`, `audio_seconds`); owned fields for
    speech (mode, formats, `sample_rate`, `max_new_tokens`, references), the batch
    and images (`n`, `response_format` `b64_json` only, `lora`); `/v1/models`
    endpoint names.
  - **Request pipeline:** stage 2's formats and refusals.
  - **Providers:** the `vllm-omni` type, serving the media endpoints only (why,
    and what was rejected); the media column and row of Endpoint support; base URL
    and paths; passthrough edits; `X-Word-Timestamps` headers relayed; audio bodies
    flushed; **Media answers** (each mode's shape); wrong model (`vllm-omni` speech
    reads as `vllm`; images' `400 Model mismatch`); wrong path (`{"detail": "Not
    Found"}` on its media endpoints, always registered); **An endpoint missing from
    a server**: `vllm-omni`'s core endpoints are all three, images' `503` reads as
    the endpoint missing, its `400`s stand; completeness per mode; every
    `speech.audio.error` a failure; probe.
  - **Routing and reliability:** retry and outcome rows; raw-audio stream on the
    stream timers.
  - **Limits:** the six types; the speech, batch and image input estimate;
    `max_new_tokens` (and its Qwen3-TTS Base cost); the two caps; reserve images by
    `n`, audio generated by nothing, audio processed from uploads; settle; no rate
    headers for media; every scope counts the three hour types; drift; counting from
    zero before totals.
  - **Accounting:** the units; **Media usage** (speech tokens per mode, audio
    length for WAV/PCM/SSE/batch and its rounding, images); estimated output 0;
    partial media answers.
  - **Observability:** `gen_ai.output.type` (request line and usage labels);
    `generate_content`; three `kaiak.usage.*` counters and their naming; routes;
    request-line fields; limit types; backend type; time to first token for speech;
    cardinality (≤ 5 operations, media counters, 20 limit-rejection series).
- `docs/specs/CONTROL-PROTOCOL.md`: protocol 6 and format 6 (why the units bump
  both, unlike types and caps); totals' window types and `used`; the backend type;
  the counter bound at five per scope; the three units, not
  price units; the record example; counted toward the hour windows; counted-units
  fixtures over every unit and type.
- `docs/specs/BACKEND-VERIFY.md`: the `vllm-omni` type, its models-list request, and
  no "use type vllm" note for it (the list cannot tell vLLM-Omni from vLLM).
- `docs/kaiak.md`: vLLM-Omni; speech and image generation in v1 scope; accounting;
  out of v1 now uploads and video.
- `docs/BACKLOG.md`: **Pricing media units**; **Omni chat models**; **Image `file`
  responses**; **vLLM-Omni quirks** (images' `503` for a speech model).
- `protocol/schema/`: `config.schema.json` (format 6; `vllm-omni`; the six limit
  types under the integer rule; `max_images_per_request` 1..2^53−1 default 10,
  `max_speech_batch_items` default 32); `usage-record.schema.json` (the three units,
  required); `totals.schema.json` (three hour window types, top-of-hour starts);
  `status.schema.json` (protocol 6); `common.schema.json` (the amount's wording).
- `protocol/fixtures/` (281 checked against the schemas with ajv, every valid one
  valid, every schema case failing for its own reason, every semantic case passing
  the schema):
  - every config, config event, resolution and duplicate-member fixture at format 6,
    every status at protocol 6 (by script, text only);
    `format-version-4.json` → `format-version-5.json` and `protocol-version-4.json`
    → `protocol-version-5.json`, each now naming 5;
  - every usage record and batch record carries the three units at 0;
    `units-unknown.json`'s unknown unit is now `video_seconds` (`images` became a
    unit);
  - valid: `config/valid/full.json` (two `vllm-omni` backends, a speech and an image
    model, both caps, media limits on global and groups) mirrored into
    `config/resolved/full.json` with its expected limits and models;
    `at-bounds.json` (caps and the six types at 2^53 − 1); `backend-types.json`
    (a `vllm-omni` backend with a speech model of its own); config event
    `full.json` (the caps);
    `usage-record/valid/speech.json`, `images.json`; `totals/valid/windows.json`
    (media hour windows); `usage/` — every unit and every non-request type in all
    four files, plus `speech.json` and `images.json`; `backend-types/vllm-omni.json`;
  - invalid, each with its `cases.json` entry: `max-images-per-request-zero`,
    `max-speech-batch-items-zero`, `limit-<type>-fractional` for each of the six
    types; `units-missing-images`, `-audio-seconds-out`, `-audio-seconds-in`;
    `images-`, `audio-seconds-out-`, `audio-seconds-in-hour-window-misaligned`,
    `per-minute-media-type`; reasons updated for `limit-type-unknown`,
    `limit-requests-fractional`, `price-unknown-unit`, `units-missing-reasoning`;
  - `counters-at-bound.json` / `counters-exceeded.json` rebuilt for five per scope
    (7 140 children; `spare` with three or four per-minute limits: 50 000 and
    50 001);
  - `config_hash` recomputed (SHA-256 of `JSON.stringify(config)`) for both valid
    config events.
- Not touched: `control/kaiak-control/schema/` (step 2 syncs it), `gateway/`,
  `control/`, `scripts/`.

### Sources read (2026-10-09)

- **vLLM-Omni v0.30.0** (latest release; v0.31.0 is only `v0.31.0rc1`), commit
  `a8576ccb725c4e21cd13c3eb5f9a546b21149d2b` (2026-09-25):
  `vllm_omni/entrypoints/openai/{api_server.py, app_state.py, errors.py,
  protocol/audio.py, protocol/images.py, serving_speech.py, speech_usage.py,
  audio_utils_mixin.py, serving_chat.py (diffusion chat), image_api_utils.py,
  images/helpers.py, lora.py, utils.py, models/serving.py,
  tts_adapters/{__init__,base,capabilities}.py and every adapter}`,
  `vllm_omni/entrypoints/serve/utils/errors.py`, `vllm_omni/config/endpoint_policy.py`,
  `vllm_omni/engine/omni_engine_base.py`, `vllm_omni/entrypoints/async_omni.py`,
  `docs/serving/speech_api.md`; `requirements/common.txt` (pins vLLM 0.30.0).
- **vLLM v0.30.0** (the version vLLM-Omni pins):
  `vllm/entrypoints/serve/exception_handling/{register.py, error_response.py,
  utils.py, handlers/http.py, handlers/validation.py, handlers/exception.py}`,
  `vllm/entrypoints/serve/engine/serving.py` (`_check_model`),
  `vllm/entrypoints/launchers/{app.py, api_server/routers.py}`,
  `vllm/entrypoints/generate/api_router.py`,
  `vllm/entrypoints/{anthropic,openai/responses,openai/completion}/api_router.py`,
  `vllm/multimodal/media/connector.py`,
  `vllm/entrypoints/speech_to_text/base/serving.py` (seconds rounding).
- **OpenTelemetry** — `open-telemetry/semantic-conventions-genai` at the pinned
  `4f85037ef86e92c510d2ef881a58f1076f6fc0e4` (2026-10-06) and `main`
  `06ec68e722c45a7218e23ea1bc1339fe4e21ecae` (2026-10-07, still `main` on
  2026-10-09): `docs/registry/attributes/gen-ai.md`, `model/gen-ai/metrics.yaml`,
  `model/gen-ai/token-metrics.yaml`.

### Findings that shaped the rules

- **Routes.** vLLM-Omni builds vLLM's app from its own task set (`generate` with a
  comprehension stage, `speech` with an audio stage, `generate` when neither — a
  pure-diffusion model), removes vLLM's chat and models routes and always adds its
  own router: chat, speech, speech batch, image generation, voices, video and the
  WebSocket routes. Speech, batch and images exist on every server — in every
  release from v0.18.0 through v0.30.0 (`api_server.py` at each tag, checked
  2026-10-09) — which is why their FastAPI `404` reads as a wrong `base_url`.
- **Endpoint-missing answers** (all read from source; the ones on vLLM's routes
  no longer matter, since `vllm-omni` serves the media endpoints only): vLLM's
  absent routes `404 {"detail": "Not Found"}`; on a pure-diffusion
  server vLLM's completions, Messages, count_tokens and Responses routes are
  registered but their state attributes never set, so `500
  {"error": {"message": "'State' object has no attribute 'openai_serving_…'",
  "type": "InternalServerError", "code": 500}}` (Starlette's `State.__getattr__`
  through vLLM's catch-all handler); vLLM's guard `501 NotImplementedError "The
  model does not support … API"` is reached only where the attributes are set to
  none (vLLM-Omni's duplex mode); image generation on a model without a diffusion
  stage `503 "No diffusion stage found in multi-stage pipeline."` (an
  `HTTPException`, rendered by vLLM's handler as `{"error": {message, type:
  "Service Unavailable", code: 503}}`). Speech on a non-speech model `400` (after
  running the generation, `TTS model did not produce audio output.`, or at once
  for a talker model); batch on pure diffusion `400`; chat on a speech-only model
  `200` with an error body (the route returns the `ErrorResponse` object).
- **Wrong model.** Speech and batch run vLLM's `_check_model`: `404 NotFoundError
  The model \`X\` does not exist.` (param `model`); image generation compares with
  the first served name: `400 Model mismatch: request specifies 'X' but server is
  running 'Y'.`; diffusion-mode speech checks no name.
- **Speech request** (`OpenAICreateSpeechRequest`): `response_format` `wav` (default),
  `pcm`, `flac`, `mp3`, `opus`; `sample_rate` > 0, "if omitted, the model's native
  sample rate", refused unless the adapter declares it (`supported_output_sample_rates`:
  Qwen3-TTS {8000, 24000}, Breeze-TTS-2 {24000}, every other adapter none);
  `stream`/`stream_format` as in the spec; `max_new_tokens` ≥ 1, adapters bound it
  (1–4096 by default; MiniMax Music 3 to 9000) and default 2048; `ref_audio` a string
  or list, `ref_audio_2`, and a `references` alias (one entry's `audio_path` →
  `ref_audio`); `extra_params`, `speaker_embedding`, `word_timestamps` (timing in
  `X-Word-Timestamps` headers only). Pydantic's default drops unknown fields.
- **Speech answers.** Whole clip: `Response(audio, media_type, headers)` with
  `X-VLLM-OMNI-INPUT-TOKENS`, `-OUTPUT-TOKENS`, `-TOTAL-TOKENS`,
  `-INPUT-TEXT-TOKENS`, `-INPUT-AUDIO-TOKENS` (none in diffusion mode); one retry
  with a new seed for a retryable generation error, only when the request set no
  `seed` and no `max_new_tokens`. Raw stream: `audio/wav` or `audio/pcm`, a 44-byte
  WAV header first with `0xFFFFFFFF` in both size fields (channels inferred, 16-bit),
  then PCM chunks; resampled to `sample_rate` when set. SSE: one
  `speech.audio.delta` per yielded chunk (the WAV header its own delta),
  `speech.audio.done` with `usage`, or `speech.audio.error` `{message, type:
  "server_error", param: null, code: 500}` plus `partial_audio: true, action:
  "discard"` once audio was sent — raised by any exception inside the stream. Batch:
  `{id, results[{index, status, audio_data, media_type, usage} | {index, status:
  "error", error}], total, succeeded, failed}` (`exclude_none`), at most
  `tts_batch_max_items` items (default 32), items merged with batch defaults.
- **Audio encoding** (`create_audio`): PCM is soundfile `RAW`/`PCM_16`
  (little-endian), mono or stereo as the model outputs; WAV is soundfile's (real
  sizes, `PCM_16`); native rates per `speech_api.md`: Qwen3-TTS 24 kHz, Fish Speech
  S2 Pro 44.1 kHz, Voxtral 24 kHz, CosyVoice3 24 kHz, Gepard 22.05 kHz mono,
  Breeze-TTS-2 24 kHz; MiniMax Music 3 "one 32 kHz stereo song" on the speech
  endpoint.
- **Speech usage** (`speech_usage.py`): `input_tokens` = text tokens of `input` +
  `instructions`, plus reference codec frames only for Qwen3-TTS Base in-context
  cloning; `output_tokens` = the AR stage's `num_tokens_out`.
- **Images.** `n` 1–10; `response_format` `b64_json` (default), `file`; `url`
  refused by a validator; `lora` `{name, path|local_path|lora_path|lora_local_path,
  scale, int_id}` → `LoRARequest(name, id, path)`; `layers` 2–10 for layered
  models; answer `{created, data[{b64_json, url, revised_prompt}], output_format,
  size, cot_output, metrics}`, no `model`; `file`: the one image's bytes or a ZIP
  (`application/zip`, `Content-Disposition` attachment).
- **Models list:** OpenAI-shaped in both modes (`_DiffusionServingModels`), owned by
  `vllm`.
- **Seconds rounding precedent:** vLLM's transcription usage reports
  `"seconds": int(math.ceil(duration_s))`.
- **OpenTelemetry:** `generate_content` — "Multimodal content generation operation
  such as Gemini Generate Content" (unchanged from `4f85037` to `06ec68e`);
  `gen_ai.output.type` — "the content type requested by the client", well-known
  `image`, `json`, `speech`, `text` (the same at both commits); no metric counts
  images or audio length in `metrics.yaml` or `token-metrics.yaml` at either
  commit; `gen_ai.token.modality` well-known `audio`, `image`, `text`, `unknown`.

### Decision 18 — ruled with evidence

**PCM requires `sample_rate`** (`400 missing_required_parameter` without it). 24 kHz
when the request names none would be silently wrong for Fish Speech S2 Pro (44.1
kHz), Gepard (22.05 kHz) and MiniMax Music 3 (32 kHz stereo), which also refuse any
`sample_rate`; a per-model declared rate is new model metadata, against decision 10.
With `sample_rate` set, vLLM-Omni either resamples (only Qwen3-TTS and Breeze-TTS-2
accept one, both mono) or refuses, so a relayed PCM answer is always 16-bit mono at
the request's rate. WAV works on every model. The PCM channel count is the residual
risk: a future model declaring rates with stereo output would be measured double —
the live kit's length check covers it at each upgrade.

### Error classification (`speech.audio.error`)

Every one is a **failure**: vLLM-Omni sends only `type: "server_error"`, `code: 500`,
from any exception inside the stream (engine, sample-rate change, the adapter's
generation check — Qwen3-TTS Base reaching its budget without EOS); every request
fault answers `400` before the stream starts. As a first event: `502
upstream_error`, retried, a circuit failure; after: relayed (message replaced), the
stream ends incomplete, `broke_off`.

### Refinements of decisions 13–21 (please review)

- **13 (speech owned fields):** adds `ref_audio_2` and `references[].audio_path`
  (other routes to the same fetch; decision 11's principle), `ref_audio` as a
  list; `sample_rate` becomes required with `pcm` (decision 18); an unknown
  `stream_format` passes and is timed as the whole clip. The raw-audio stream takes
  the stream timers (first body bytes, reads as progress) — 13 named the timers,
  this picks them. Kept: the injected `max_new_tokens` default, with its cost
  recorded (Qwen3-TTS Base loses vLLM-Omni's text-scaled budget and its one retry
  when the request names `max_new_tokens`; a model declared without
  `output_limit` keeps them — `DEPLOYMENT.md`, step 10).
- **14 (batch):** `ref_audio` is a string there; the default output limit is
  written at the batch's level; tokens are the items' usage summed, a successful
  item without usage making the record estimated.
- **15 (images):** `lora` refused (confirmed by the user); `response_format`
  `b64_json` only (the user's ruling, above).
- **8 (the type's readings):** `vllm-omni`'s core endpoints are all three media
  endpoints: vLLM-Omni registers them on every server, so their FastAPI `404` is
  a wrong `base_url` (`upstream_path_missing`, a circuit failure), and only the
  image route's `503` for a speech model reads as the endpoint missing.
- **16/17:** partial answers — a whole clip keeps its header usage; an image
  answer cut short counts the larger of `data[]` seen and `n`; a batch cut short
  counts the items relayed whole. An unmeasurable audio answer counts 0 seconds,
  flagged `estimated`. Seconds are rounded up once per record.
- **19 (names):** unchanged; new error codes `response_format_unsupported` and
  `external_reference_unsupported`; `429` types `images` and `audio_seconds`.
- **20 (metrics):** two audio counters, one per direction —
  `kaiak.usage.audio.output.duration` and `kaiak.usage.audio.input.duration` (`s`)
  — beside `kaiak.usage.images` (`{image}`); fed only by records whose unit is not
  0; `gen_ai.output.type` joins the usage labels.
- **21 (versions):** the counter bound counts five per scope — one per counted
  type — and stays at 50 000 (the user, 2026-10-09; the gateway's `countCounters`
  already derives the five from the counted types).

### Notes for later steps

- **Step 2:** `countCounters` in `control/kaiak-control/src/config/semantic.ts`
  hard-codes two per scope (message too); `COUNTED_TYPES` in `messages/totals.ts`;
  backend-verify: `vllm-omni` and no recognition note for it; the schema copy.
- **Step 3:** `TestCountedUnitsFixtures` compares the fixture's units with
  `config.TokenUnits` — the fixtures now carry every unit. Fixtures carrying the
  `vllm-omni` type (`config/valid/full.json`, `backend-types.json`,
  `config/resolved/full.json`, `backend-types/vllm-omni.json`) stay red in the
  gateway until it knows the type — step 4 by the plan, so step 3's "green" either
  takes the type into config or names those reds for step 4.
- **Step 4:** the `vllm-omni` module serves the three media endpoints only, all
  core: `{"detail": "Not Found"}` there is `upstream_path_missing`; image
  generation's `503 No diffusion stage found in multi-stage pipeline.` is
  `upstream_endpoint_missing` (matched by status and message); wrong model: vLLM's
  `404` on speech and the batch, the image `400 Model mismatch`; the probe reads
  `<base_url>/models`.
- **Step 5:** header names are upper-case in vLLM-Omni (HTTP is case-insensitive);
  `X-Word-Timestamps`/`-Omitted` relayed; flush audio bodies; the WAV header parse
  must walk chunks (soundfile may write more than `fmt `/`data`).
- **Step 10:** `DEPLOYMENT.md` — a deployment's `model` is the server's first
  `--served-model-name`; the `output_limit` ceiling within the adapter's
  `max_new_tokens` bound (4096 for most); Qwen3-TTS Base and `output_limit`; PCM only
  with `sample_rate` (Qwen3-TTS, Breeze); `b64_json` only for images; chat models
  stay on `vllm` backends. The kit's wrong-endpoint checks: chat to the image model
  is now the gateway's own `400 endpoint_not_served`; an image request to the
  speech model is the backend's `503`, read as `upstream_endpoint_missing`.
- **Step 12 (live):** confirm the image `400` and `503` texts, and what speech
  answers on the image model.

### Suite (2026-10-09, re-run after the rulings — the same results as before them)

`scripts/check-all.sh` stops at the gateway's `go test`; the later stages were run
on their own with the script's commands.

- **Gateway** (`check-gateway.sh`): gofmt, vet, staticcheck (2026.2.1 on go1.27.1)
  and the telemetry boundary pass. `go test -race -count=1 ./...` fails **74 tests
  in 7 packages**; the other 13 pass, `e2e` included.
  - `cmd/kaiak` (15), `internal/config` (23), `internal/control` (32): fixtures and
    test configs at format 6 and protocol 6 refused at 5 (`must be 5`), and the
    schema-enum checks — `TestLimitTypesAreTheSchemaEnum`,
    `TestTotalsWindowTypesAreTheCountedLimitTypes`, `TestRecordUnitsAreTheTokenUnits`,
    `TestBatchCapsRefuseZero` → **step 3**; `TestBackendTypesAreTheSchemaEnum` and
    the fixtures carrying `vllm-omni` (`TestValidFixtures`, `TestResolvedFixtures`,
    `TestValidMessageFixtures`) → **step 4**, unless step 3 takes the type into
    config.
  - `internal/limits` (1): `TestCountedUnitsFixtures` (every unit, the media
    types) → **step 3**.
  - `internal/metrics` (1): `TestFullScrapeIsTheSpecsMetricList` (the
    `gen_ai.output.type` label, the three `kaiak.usage.*` counters) → **step 3**.
  - `internal/provider` (1): `TestBackendTypeFixtures` (`vllm-omni.json`, no module
    yet) → **step 4**.
  - `internal/server` (1): `TestEveryErrorCodeHasItsClass` —
    `response_format_unsupported` and `external_reference_unsupported` are in the
    error table with no answer building them → **step 3** (the main session moves
    the two answers there, class `invalid_request`, so phase 1 can end green).
- **Live-test kit** (`scripts/live`): gofmt, vet, staticcheck and `go run .
  -self-test` pass (34 passed, 1 skipped, every kind).
- **Control `npm test`**: hangs as the rerank step's first attempt did; run with
  `--test-timeout=20000`: 647 tests, 397 pass, **238 fail, 11 cancelled** (timeouts
  waiting on statuses and batches the protocol-5 checks refuse), 1 skipped. Every
  red is `kaiak-control`'s schema copy and version constants at 5 → **step 2**:
  `scripts/sync-schemas.test.ts` (the copy differs in five schemas), the fixture
  checks (config, messages, duplicate members, usage, backend types, resolution),
  and every test that publishes a fixture config or sends protocol 6 — failing
  tests per file: `control-plane` 29, `usage` 27, `fastify` 25, `status-totals` 19,
  `config-publishing` 19, `gateways` 12, sample `config-file` 8, `config` 8,
  `usage-route` 6, sample `app` 5, `duplicate-members` 5, `limits` 5, sample page
  `events` 4, sample `page` 2, sample `main` 2, `sync-schemas` 1, `aggregate` 1,
  `messages` 1, `backend-verify` 1.
- **Control `npm run lint`**: passes.
- **Cross-half e2e** (`go test -race -tags crosshalf -run '^TestAcrossHalves'
  -count=1 ./e2e`): **passes** — it builds its config in Go and neither half's code
  moved. It turns red after step 2 (control at protocol 6, gateway at 5) until
  step 3.
