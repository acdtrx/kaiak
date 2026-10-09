# Step 7 — speech batch

**Status:** done — reviewed and committed 2026-10-09

## Intent

Serve vLLM-Omni's `POST /v1/audio/speech/batch` on `vllm-omni` deployments:
- the items are counted against `max_speech_batch_items`;
- each item follows speech's rules for `response_format`, `ref_audio` and
  `max_new_tokens`;
- the answer's per-item usage is summed;
- each successful item's base64 audio is measured toward `audio_seconds_out`.

## Files likely touched

- `gateway/internal/provider/`: the endpoint (path `audio/speech/batch`) and its format;
  `vllm-omni` serves it.
- `gateway/internal/server/`:
  - the row (`audio_speech_batch`, operation `generate_content`, output type speech);
  - the inbound parser: items counted and capped; per-item and batch-level owned
    fields, with the batch's defaults applying where an item sets none.
- `gateway/internal/accounting/`:
  - the input estimate over the items;
  - a batch reader: the `results[]` scanned once, summing each item's `usage`, and
    measuring each item's `audio_data` from its encoded size and its format (the
    item's `media_type`);
  - errored items add nothing.
- The output limit: a batch reserves the output limit once per item (the items run as
  that many sequences), as completions' prompts do.
- The fake backend: the batch endpoint, mixed success and error items.
- Tests beside each.

## Decisions made during planning

- **The answer is relayed as vLLM-Omni sends it**, with its top-level `model`
  rewritten if present. Its base64 audio is counted, never held.

## Acceptance criteria

- **Component tests:**
  - the items cap;
  - per-item refusals (format, `ref_audio`), naming the item's position;
  - usage summed;
  - audio length per item;
  - errored items;
  - the reservation per item.
- **Phase 2 end:** `scripts/check-all.sh` green and recorded.

## Result

### What changed

- `gateway/internal/provider/`:
  - `provider.go`: the `AudioSpeechBatch` endpoint (path `audio/speech/batch`) and
    `FormatSpeechBatch`; `Request.ItemParams` (`ItemParam`: a parameter the pipeline
    sets in an element of a top-level list — a batch item's output limit).
  - `body.go`: element edits — `indexArray`, `elementEdits` (one edit per list),
    `editElements` (each element's members spliced with `editObject`, every other
    byte kept; an index past the list, an element that is not an object or a list
    that is absent or not one is a gateway fault). `editObject`: several members
    inserted into an empty object are now comma-separated — latent until now (a
    top-level body always carries `model`), reachable through an empty `{}` batch
    item.
  - `stream_format.go`: `rerankStream` became `jsonBodyStream`, shared by rerank and
    the speech batch: no edits of the format's own, an event stream answering either
    never complete.
  - `vllm_omni.go`: `vLLMOmniEndpoints = {AudioSpeech, AudioSpeechBatch}`; the batch
    relays no word-timing headers (vLLM-Omni sends none there).
- `gateway/internal/server/`:
  - `pipeline.go`: the `audio_speech_batch` row (operation `generate_content`, output
    type `speech`, output-limit key `max_new_tokens`); `request.itemParams` and
    `request.outputReserve` (the most the request may generate in all).
  - `inbound.go`: the batch's `stream` is not read (as rerank's); `ItemOutputLimits`;
    the format's parser.
  - `inbound_speech.go`: `parseSpeechBatchFields` — `items` (absent or null: none; not
    a list: `invalid_type`; above `max_speech_batch_items`: `400 invalid_value`,
    `param: "items"`, counted before any item's members are read); each level's
    `response_format`, `sample_rate` and `ref_audio` (`readSpeechBatchLevel`), an
    item's `max_new_tokens`; the merged format and rate held to speech's rules
    (`mergedSpeechAudio`), the refusal naming the level that set the value
    (`items[2].response_format` or `response_format`; PCM without a rate:
    `items[2].sample_rate` when the item set `pcm`, `sample_rate` when it inherited
    the batch's); `inbound.Sequences` = the items; `inbound.Media` audio out; each
    item's effective rate for the meter.
  - `params.go`: `applyItemOutputLimits` — an item's own `max_new_tokens` refused
    below 0 or above the context (`items[3].max_new_tokens`), lowered to the ceiling
    in the item (an `ItemParam`), else the batch's effective limit (the default
    written at the batch's level when the batch sets none); the reservation the
    items' effective limits summed, the effective output limit the largest item's.
    Every other endpoint's `outputReserve` is its effective limit × its sequences.
  - `limits.go`: reserves `outputReserve`. `attempts.go`: passes `ItemParams`.
  - `errors.go`: `errTooManyItems` (in the embeddings cap's wording);
    `metrics_test.go`: its `errorAnswers` entry.
- `gateway/internal/accounting/`:
  - `speech_batch_usage.go` (new): the batch reader — one pass over the answer as it
    streams through, a dedicated scanner of the answer's members, the `results`
    list and each result's members (others skipped, strings passed a run at a time).
    A result read whole adds, when its `status` is `success`, its `usage`
    (`speechEventUnits`, summed; one missing or malformed → no report) and its
    audio's exact length; an errored result adds nothing. No result read whole → no
    report. `index`, `status`, `media_type` and `usage` kept, bounded; `audio_data`
    never kept (`itemAudio`, below). `results` named twice: the last.
  - `itemAudio`: the audio read both ways while it streams — as WAV (the header
    decoded, the data counted, `wavClip`) and by its decoded size — then the
    `media_type` picks one: WAV from its header, PCM at the rate of the item the
    result's `index` names (`formatClip`), anything else unmeasured. Characters go
    through a 4 KiB window of whole base64 groups; `\/` reads as `/`, any other escape
    leaves the item unmeasured.
  - `audio.go`: `audioFormat` (the format an audio content type names) under
    `newAudioClip`; the base64 helpers generic over `string | []byte`
    (`readBase64` under `writeBase64`), so a window is read without a copy.
  - `estimate.go`: a batch's `items` are its prompts (`speechBatchItems`), as a
    completion batch's — the total, and the input one item sees.
  - `meter.go`: the batch's reader; `speech_usage.go`: `Speech.ItemSampleRates`.
- `gateway/internal/fakebackend/`: `speech_batch.go` (new) — `/v1/audio/speech/batch`
  as vLLM-Omni answers it: each item merged with the batch, a WAV file with real
  sizes or PCM at the item's rate, `AudioMillis` long, base64; results in
  vLLM-Omni's member order (`audio_data` before `media_type`), absent members left
  out; usage per item (none with `OmitUsage`); `Reply.FailItems` — an item's
  `status: "error"` with a distinctive text (`SpeechBatchItemError`); no items or
  more than 32 → 400; a non-speech model → `400 Batch speech is not supported in
  diffusion mode`. `fakebackend.go`: the route, `FailItems`.
- `docs/specs/GATEWAY.md` (three clarifications, settled 2026-10-09): a batch value
  no item inherits is not judged, `ref_audio` inline wherever set, `items` shapes;
  an item is successful when its `status` is `success`, and an answer with no item
  read whole has no report; the batch's audio by `media_type` wherever the item
  puts it, PCM at the rate its `index` names, an unmeasurable item 0 and the record
  estimated while the others count, a successful item without audio relayed none.

### Design choices

- **The audio is measured before its format is known.** vLLM-Omni writes
  `media_type` after `audio_data`, and the audio must not be held: each result's
  audio is read as WAV and by size at once (both O(1) in memory), and its
  `media_type` picks at the result's end. Any member order reads the same.
- **A scanner of its own, not the member scanner layered three deep.** The answer's
  structure is stepped byte by byte, but strings — the audio above all — are passed
  a run at a time (`bytes.IndexByte` to the next quote or backslash), so a 16 MiB
  answer costs one pass and a copy into the window, no allocation that grows with
  it (about 26 KB allocated metering a 26 KB answer, 30 KB a 16 MiB one).
- **The reservation sums the items' own limits** (`outputReserve`), not items × the
  largest: the spec's "each item's effective output limit". `outputLimit` stays the
  per-sequence figure (the largest item's).
- **Item edits are generic in the provider core** (`ItemParams`, the pipeline names
  the list), like `Params`; the batch adds no format edit, so it shares rerank's
  stream format.
- **What is judged where:** `response_format` and `sample_rate` by the values each
  item gets (a batch value every item overrides passes); `ref_audio` wherever set
  (the errors table names "a batch's or item's `ref_audio`"); `max_new_tokens`
  wherever set, each on its own.
- **An unmeasurable item counts 0 and flags the record; the others still count** —
  the batch reading of "an answer it cannot measure counts 0".
- **A batch every item of which errored reports zeros** (each errored item adds
  nothing); a complete answer with no result reports nothing (estimated).

### Spec points (reported)

- vLLM-Omni read at the pinned `a8576ccb` (`protocol/audio.py`: `SpeechBatchItem`,
  `BatchSpeechRequest`, `SpeechBatchItemResult`, `BatchSpeechResponse`;
  `serving_speech.py`: `_merge_batch_item`, `create_speech_batch`;
  `audio_utils_mixin.py`; `api_server.py`'s route): the batch's own fields are
  `model` (optional there), `items` (≥ 1), `voice`, `instructions`,
  `response_format` (default `wav`), `sample_rate`, `speed`, `task_type`,
  `language`, `ref_audio` (a string), `ref_text`, `x_vector_only_mode`,
  `max_new_tokens`, `initial_codec_chunk_frames`, `non_streaming_mode`; an item has
  the same less `model` and `items`, `input` required. Unknown fields — `stream`,
  `ref_audio_2`, `references` — are dropped by Pydantic, never read: they pass
  untouched as unowned, as the spec lists `ref_audio` alone for the batch. Results
  are in item order, `index` the item's position; the answer is
  `model_dump(exclude_none=True)` through Starlette's `JSONResponse` (no `\/`), its
  audio `base64.b64encode`, padded, `media_type` `audio/wav` or `audio/pcm`.
- **Input estimate:** a batch's own `instructions` and `ref_audio` count once, as the
  rest of a completion batch does, though vLLM-Omni applies them to every item that
  inherits them; the backend's usage settles the record. The spec's rule is
  followed; flagged for review.
- The three clarifications above are the only spec edits.

### Tests

- `accounting/speech_batch_usage_test.go`, every case split at 1, 3, 4096, 32 KiB
  bytes and whole: usage summed; errored results add nothing, their usage unread;
  every result errored → zeros; a negative count → 0; a result without usage, usage
  null, a count as text, a count missing → estimated; no results, results null, not
  a batch answer → estimated; seconds rounded once (3 × 0.4 s → 2). Audio length
  (exact): WAV summed; PCM at each item's rate, by index not by place; WAV and PCM;
  `media_type` before `audio_data`; with parameters; unpadded; `\/` in PCM, in WAV,
  in a WAV header; no `audio_data`, `null`, named twice (the last); an errored or
  non-success result's audio not counted. Unmeasurable (others still counted,
  record estimated): PCM at no rate, no index, a null index, an index past the
  items; MP3; no `media_type`; a WAV that is not one; a WAV header not base64; an
  escape; PCM of any characters counted by length. Members where the answer puts
  them: nested and top-level look-alikes, escaped names, brackets and quotes in
  text, non-object elements, results named twice, a long name; indented JSON. Cut
  short: within the first result → estimated; within the second → the first;
  after the results → both; all partial. An error answer → zeros; an event stream
  → estimated. Allocation: 4 results of 0.1 s and of 65.5 s (16 MiB) → bytes
  allocated within 256 KiB of each other (allocated bytes, not counts: under
  `-race` the longer run picked up a few background allocations, failing a count
  comparison 96 to 99 in the first `check-all` run); 263 s.
- `accounting/estimate_test.go`: a batch's items as prompts (total, the largest
  item), the largest item counting its media; `items` on speech is text.
- `provider/speech_batch_test.go`: passthrough through the fake (model, the batch's
  `max_new_tokens`, an item's lowered in the item, every other byte — whitespace,
  `stream` — kept; `Accept: application/json`); the answer relayed, complete;
  completeness — whole, a `model` rewritten, cut short, an event stream.
  `provider/body_test.go`: element edits (replace, add, several into an empty item,
  other elements and whitespace kept; six faults); two inserts into an empty
  object. `endpoints_test.go`: the media column; the batch's stream untouched.
- `server/speech_batch_test.go`: the pipeline (sent body, results with WAV and PCM
  audio, record 21/12 tokens and 4 s for 3 × 1.25 s, cost, operation, output type,
  `gen_ai.request.stream: false`, the route, the audio metric, no time to first
  token); 22 refusals naming the position (the cap, formats from the item or the
  batch, PCM without a rate at either level, `ref_audio` at either level, types,
  output limits of the batch and an item, shapes, duplicates, model, not on
  `vllm-omni`) plus the config's cap; what passes (overridden batch defaults,
  inherited rates, inline references, nulls, unowned fields, `stream`); output
  limits (the default at the batch's level, the batch's lowered or kept, a model
  with none) and the reservation read from `x-ratelimit-remaining-tokens`, too
  large for 4 × 300; an errored item, every item errored, no usage; 5 MiB through
  the pipeline (80 s); the audio limit counting 8 s; a model that does not speak
  (400 relayed, zeros); nothing of the texts, reference audio, audio or item error
  in the log.
- Checked by mutation (each reverted): errored results counted; seconds rounded
  per item; the audio held; missing usage ignored; results named twice kept both;
  members matched at the wrong level; any escape accepted; an unfinished result
  counted at settlement; the cap off; the reservation not per item; `ItemParams`
  not passed; the item's value not lowered; the batch's format judged over the
  item's; the batch's `stream` read; the estimate's items not as prompts — each
  fails its tests.

### Notes for later steps

- Step 9: the fake's batch takes `Reply.FailItems` and `OmitUsage`; `cmd/fakebackend`
  has no flag for `FailItems` (nor for `AudioMillis`, step 5's note).
- Step 10: `DEPLOYMENT.md` — `global.max_speech_batch_items` against the server's
  `tts_batch_max_items` (a larger cap lets the server refuse what the gateway
  admits); the kit's batch check (a WAV and a PCM item, the seconds summed).
- Step 12: confirm the live answer's member order (`audio_data` before
  `media_type`) and the pure-diffusion server's batch `400` text.

### Suite (2026-10-09)

End of phase 2; no expected reds. `scripts/check-gateway.sh` green (exit 0, `gateway
checks passed`). The first `scripts/check-all.sh` failed one test,
`TestSpeechBatchCountsWithoutHolding`, under `-race` — allocation counts 96 against
99, background allocations over the longer run — so the test now compares allocated
bytes (Tests, above); then `scripts/check-all.sh` twice (the second after this
record was written), each `all checks passed`, exit 0:

- **Gateway:** gofmt, vet (crosshalf tag included), staticcheck 2026.2.1 on
  go1.27.1, telemetry boundary pass; `go test -race -count=1 ./...` — all 20 packages
  `ok`, `e2e` included (≈120 s).
- **Live-test kit:** gofmt, vet, staticcheck pass; `-self-test` passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends (37/36/22/21/16/16/34 passed, 0 failed).
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 0 cancelled, 1 skipped.
  **`npm run lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈70 s).
