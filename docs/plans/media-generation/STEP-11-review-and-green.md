# Step 11 — review and green

**Status:** done — reviewed and committed 2026-10-11

## Intent

An independent review checks the plan's code, and the branch is green and ready to
merge except for the live run (step 12).

## Files likely touched

- Fixes from the review, each with its test.
- `docs/BACKLOG.md`: what the review leaves open, each with its revisit trigger.
- `docs/plans/media-generation/OVERVIEW.md`: the review outcome and the verification
  status.

## Decisions made during planning

- **The review runs as for the rerank plan:** two agent reviews of the worktree and a
  Codex review of a plain copy (`/tmp/kaiak`, run by the user), over the branch diff
  against `v0.13.1`. Findings are re-checked against the code, and reproductions are
  run.
- **Findings are triaged by frequency.**

## Acceptance criteria

- Every finding is fixed or answered.
- `scripts/check-all.sh` green 3× in a row.

## Result

Two agent reviews of the worktree over `v0.13.1..media-a` ("speech"; "images,
limits, control"), the user's rulings on three of their findings (2026-10-10), each
finding re-checked against the code, its repro turned into a test. vLLM-Omni read at
the pinned `a8576ccb` (v0.30.0) and, for the batch's fields, on `main` at `b65107d`
(2026-10-09). Then the Codex review of a plain copy (2026-10-10): three medium
findings, each with a reproduction under the build tag `auditb` that failed on the
code it reviewed (rows 17–19); no finding in its other areas. Then the user's
decision on what the live run (step 12) found (row 20).

| # | Severity | Source | Finding | Done / recorded |
|---|---|---|---|---|
| 1 | ruling | the user's ruling (2026-10-10) | Speech `extra_params` was passed: vLLM-Omni hands it to the model's adapter, and IndexTTS2 resolves `extra_params.emo_audio` through the media connector that fetches `http(s)` and reads `file:` — `{"model":"tts","input":"hi","extra_params":{"emo_audio":"http://169.254.169.254/latest/meta-data/x.wav"}}` reached the backend | Refused on speech, any presence and value (`null`, `{}` included): `400 external_reference_unsupported`, `param: "extra_params"`, the message naming the member only (`server/inbound_speech.go` `parseSpeechFields`, `errors.go` `errExtraParams`). **Not refused on the batch**: `BatchSpeechRequest` and `SpeechBatchItem` declare no `extra_params` (plain Pydantic models, unknown fields dropped) and `_merge_batch_item` sets none, at `a8576ccb` and on `main` `b65107d` — vLLM-Omni never reads it there, so it passes untouched as `ref_audio_2` and `references` do. `GATEWAY.md` → errors table, owned fields: speech (dated, the reason, rejected: judging values by their look, an allowlist) and speech batch; `DEPLOYMENT.md` → What the gateway accepts; `BACKLOG.md` → vLLM-Omni `extra_params` |
| 2 | ruling | the user's ruling (2026-10-10) | The gateway wrote the model's default `max_new_tokens` into speech and batch requests, which switches off vLLM-Omni's text-scaled budget and its retry; a Base clone reaching it raises `Qwen3TTSCodecLimitError` → `500` / `speech.audio.error`, a circuit failure under legitimate use | The default is reserved, never sent (`server/pipeline.go` `writesNoOutputLimit` on speech and the batch; `params.go`); a value above the ceiling is refused (row 15). Unchanged: a client's value out of range is refused (below 0, above `context_length`). Reservation for a request (or item) that sets none: the model's `output_limit.default` fitted to the context — `min(default, max(context_length − largest prompt estimate, 256))`, as the injected default was — and no output on a model without `output_limit`. `GATEWAY.md` → Passthrough edits, Output-limit keys (dated, the reason, rejected), Injected default fits the context; `DEPLOYMENT.md` → Output limit, Limits; `BACKLOG.md` → vLLM-Omni: a reached `max_new_tokens` answers 500 |
| 3 | ruling | the user's ruling (2026-10-10) | A batch item's `error` text was relayed verbatim (the fake's: a CUDA error naming the backend-side model) | In a successful batch answer, every `error` member of each object in the top-level `results` that is a string, object or array becomes `"The model backend could not generate this item."`; `null`, numbers and booleans pass; index and status kept. Edited as the body streams, in the same pass as the `model` rewrite (`provider/model.go`: the rewriter watches the list's object elements; `response.go` `batchItemError`, wired for `FormatSpeechBatch` 2xx JSON only); bounded memory (one key capture, no value held). A result naming `error` twice gets both replaced, as a body naming `model` twice gets both rewritten — a streamed body has no whole to fall back on. `maxKeyCapture` 32 → 48: a fully escaped `results` is 42 bytes (and `message`/`response`, 42/48, did not match before, against "keys match exactly after decoding"). `GATEWAY.md` → Media answers (dated, the cost — a client loses vLLM-Omni's validation text, e.g. an unknown voice — and rejected alternatives); `DEPLOYMENT.md` → Usage |
| 4 | low | agent review: speech | Paths mutants survived in the batch: the item's rate over the batch's; the 4 KiB base64 window ending in padding; a negative `index`; data after the answer object | Tests added (below); each mutant now fails |
| 5 | low | agent review: speech | `rq.inbound.Sequences = int64(len(items))` in `parseSpeechBatchFields` never read | Removed; `inbound.go` `Sequences` doc says the batch's items are counted by `ItemOutputLimits` |
| 6 | low | agent review: speech | The fake's raw-audio WAV stream wrote and flushed its header before any pacing or fault; vLLM-Omni yields it with the first audio chunk (`_generate_audio_chunks`) | The fake writes the header with the first piece, after piece 0's fault and pacing (`fakebackend/speech.go`); a hang before the first audio is now a first-event timeout (`TestSpeechRawAudioHangBeforeTheFirstAudio`). The SSE stream's header delta is still paced apart from the first audio delta — not changed (not in scope; vLLM-Omni yields them back to back there too) |
| 7 | low | agent review: speech | "Audio relayed to the client" said less than the rule the relay keeps (a piece is read before it is written) | Worded as "the audio the backend sent while the client was there, the last piece read included": `GATEWAY.md` → Media usage: Audio length and Partial; `DEPLOYMENT.md` → Usage; comments in `accounting/audio.go`, `speech_usage.go`, `server/relay.go`. The relay's order unchanged |
| 8 | low | agent review: speech | An absurd WAV header (rate `0xFFFFFFFF`) measured relayed samples as a few ns — 0 s, "measured"; `audioDuration`'s comment ("no audio byte is shorter than a nanosecond") was false above 1 GB/s | `maxAudioByteRate` = 100 MB/s (768 kHz × 32 ch × 32-bit = 98.3 MB/s, the most any real format needs; vLLM-Omni's models need at most 128 KB/s): a WAV byte rate above it is an unreadable header, PCM at a `sample_rate` above it unmeasured — 0 s, `estimated`. Below it a byte lasts ≥ 10 ns, so the comment is now true. `GATEWAY.md` → Audio length |
| 9 | low | agent review: images, limits, control | An image limit blocked only by a running request answered the reservation slot's expiry; the images are counted again at settlement, in a newer bucket, so the promise failed (repro: `images_per_minute` 4, 4 images reserved at 0 s and settled at 30 s, a request refused at 10 s told 50 s, refused again at 60 s) | `blockedByRunning` covers image limits: the same 2 s short guess as tokens (`limits/limits.go`, `headers.go`); a limit blocked by settled images keeps its time, which holds. `GATEWAY.md` → Refusal (dated). Not done: audio processed (phase B) reserves and re-counts the same way — to apply there when an endpoint produces it |
| 10 | low | agent review: images, limits, control | Two "escaped name" tests held no escape (tooling had unescaped `\u` in the source) | Real escapes built from a backslash constant (`images_usage_test.go`, `scan_test.go`), the bytes checked with `od`; the mutants `string(u.key) == "data"` and `s.captures[string(s.key)]` now fail |
| 11 | low | agent review: speech | `examples/config.json`: `qwen3-tts` `context_length` 32768, while vLLM-Omni v0.30.0's `deploy/qwen3_tts.yaml` runs the talker at `max_model_len: 4096` | 4096 (the ceiling of 4096 still fits); both halves' example tests pass |
| 12 | low | agent review: images, limits, control | The kit's image model-name check could never fail (vLLM-Omni's image answer has no `model`); the self-test's image count was 1 | Check dropped; the self-test's image check asks its fake for `n` 2 with 2 layers (`options.imageCheckParams`, set in `startMediaFakes`) — `usage-log/image` checks 4. `LIVE-BACKENDS.md` → Self-test |
| 13 | low | agent review: images, limits, control | `DEPLOYMENT.md` said a `vllm-omni` deployment's `model` is "the name the server lists"; image generation compares against the first `--served-model-name` only | `DEPLOYMENT.md` → One backend per model: the first served name, a second one answers `400 Model mismatch` read as a missing model; `LIVE-BACKENDS.md` → `-image-model` the same |
| 14 | low | agent review: images, limits, control | `CONTROL-PROTOCOL.md` → Matching totals to limits: "an hour and a month count for every scope and type" | The four hour counts and the month count, by type, as `GATEWAY.md` → Every scope is counted |
| 15 | ruling | the main session (2026-10-10), from ruling 2 | Speech `max_new_tokens` above the model's ceiling was lowered to it — a value of the gateway's making, which a Base clone needing more than the ceiling reaches and answers `500` | Refused, not lowered, on speech and the batch — the batch's value (even when every item sets its own) and each item's: `400 invalid_value`, `param` naming the member, in the out-of-range wording ("… at most 1000 tokens (its output-limit ceiling) …"; `errors.go` `errOutputLimitAboveCeiling`, `params.go`). Chat, completions, Messages and Responses still lower. With nothing ever written into a speech request or item, the provider's item-edit mechanism (`ItemParams`, `elementEdits`, `editElements`, `indexArray`, the pipeline's `itemParams`) had no user and is removed, with its test; `editObject`'s comma fix for several inserts stays, tested on its own. `GATEWAY.md` → errors table, Passthrough edits (speech gets the model edit alone), Output limit, Output-limit keys (dated, the reason, rejected), Output limit out of range; `DEPLOYMENT.md` → Output limit |
| 16 | ruling | the main session (2026-10-10), from ruling 1 | Image generation passed `extra_params` into the diffusion pipeline's `extra_args` (`api_server.py`, `generate_images`, at `a8576ccb`); which keys a pipeline fetches or reads the gateway cannot know | Refused as speech's is, any presence and value: `400 external_reference_unsupported`, `param: "extra_params"` (`server/inbound.go` `parseImagesFields`; the error renamed `errExtraParams`, shared by both). `GATEWAY.md` → errors table, owned fields: images (dated); `DEPLOYMENT.md` → What the gateway accepts; `BACKLOG.md` entry renamed vLLM-Omni `extra_params`, covering both; the kit's `image-refused` check and `LIVE-BACKENDS.md` |
| 17 | medium | Codex review | The speech batch's items cap was checked after `json.Unmarshal` had decoded every item into a `[]json.RawMessage`: 200 000 empty items (600 001 bytes, within the default body cap) allocated about 28 MB before the refusal — outside the body budget, and before authorization and the limits | Counted without decoding: `server/inbound.go` `listElements` (the count `rerankDocuments` made, now shared), used by `inbound_speech.go` `speechBatchItems` — a value that is not a list is refused by its first byte, a list over the cap by its count, and only a list within the cap is decoded. `GATEWAY.md` → Limits → Output multiplicity: batch caps (dated) |
| 18 | medium | Codex review | The speech readers decoded into structs, whose tags `encoding/json` matches case-insensitively: an SSE delta's `"Audio":""` after its `audio` counted 0 s while the client got the audio; a usage's `"OUTPUT_TOKENS":0` counted 0 output tokens as reported (the SSE stream and each batch result); a `speech.audio.error` also naming `"Type":"speech.audio.delta"` was not taken for an error — its backend text relayed, and a `done` after it made the stream complete | Read by exact names after decoding: `accounting/speech_usage.go` `exactMembers` and `memberValue` — the event's `type`, `audio`, `response_format` and `usage`, and the usage's counts, which each batch result's usage goes through too; `provider/stream_format.go` `speechStream.observe` — the type and the error's `code`, from maps. The plan's other readers already compare decoded names exactly: the batch and image scanners (`scannedName`) and the batch error rewriter (`keyIs`). `GATEWAY.md` → Complete responses (speech), Media usage (dated) |
| 19 | medium | Codex review | A batch item's `audio_data` holding any JSON escape but `\/` — `\u0041`, a valid way to write `A` — left the whole clip unmeasured: 0 s, `estimated`, while the client decodes all its audio. The test case "an escape" expected that | Escapes are decoded into the same window (`accounting/speech_batch_usage.go` `itemAudio.escape`, no allocation): `\/` and a `\uXXXX` naming a base64 character or `=` enter it as that character. Chosen: every other escape — `\n`, `\"`, `\\`, a `\uXXXX` naming any other character, either half of a surrogate pair — leaves the item unmeasured, as before: base64 has no place for them (a line break, which lenient base64 decoders skip, included); a `\u` digit that is not hex (invalid JSON) ends the escape, unmeasured, and is read as the string's own character, as the skip readers read it. The SSE delta's `audio` was already decoded as a JSON string (`encoding/json`): tested, unchanged. `GATEWAY.md` → Media usage: Audio length (dated) |
| 20 | ruling | the user's decision (2026-10-10), from the live run (step 12) | Speech sent to a vLLM-Omni image model's name runs a whole image generation (about 60 s of the GB10) before its `400`, and is recorded as nothing | Documented; the code unchanged. `GATEWAY.md` → Providers (vLLM-Omni): the live answer in place of "the source does not decide", the batch's `400` and image-to-speech's `503` both "at once"; `BACKLOG.md`: the `/v1/models` entry retitled **Endpoints per model**, holding this cost and the recommendation — an optional per-model endpoint list in config, the others refused `400 endpoint_not_served` before any backend, `/v1/models` listing it — its trigger the user's decision or a wasted generation seen; `DEPLOYMENT.md` → Speech and images, One backend per model: the warning to let only the keys meant for image models reach them |

The sources of findings 4–14 are given by their subject.

**Asked of step 12** (answered there; row 20): what vLLM-Omni answers to a speech
request sent to an image model was unknown: `_create_diffusion_speech` turns a
`ValueError` into `400` (neutral) but any other exception into `500` (a circuit
failure). The kit's `speech-to-image` check recorded it live.

**Tests added or changed** (each new test fails without its fix — checked by
reverting each fix in turn):
- `server`: `TestSpeechRefusals` + five `extra_params` cases (the repro, no URL, `{}`,
  `null`, a string; the message never echoes the value);
  `TestSpeechPassesInlineReferencesAndWhatItDoesNotOwn` passes an unknown object in
  place of `extra_params`; `TestSpeechBatchPassesWhatItDoesNotOwn` + `extra_params`
  at both levels; `TestSpeechOutputLimit` rewritten — the sent body and the
  reservation read from `x-ratelimit-remaining-tokens` (default 300 reserved unsent,
  1000 lowered, 200 kept, an unknown `max_tokens`, none on a model without a limit);
  `TestSpeechThroughThePipeline`, `TestSpeechBatchThroughThePipeline`,
  `TestSpeechBatchOutputLimits` expect no default sent; new
  `TestSpeechBatchItemErrorIsTheGateways` (the gateway's text, index and status
  kept, the backend's text in neither the client's body nor the log);
  `TestSpeechBatchLogsHoldNoContent` no longer expects the backend text relayed; new
  `TestSpeechBatchItemRateWinsOverTheBatchs` (24 kHz batch, 8 kHz PCM item, 2.5 s →
  3 s; the mutant gives 1); new `TestSpeechRawAudioHangBeforeTheFirstAudio` (504 at
  the first-event timeout, the prompt billed, no audio); `TestEveryErrorCodeHasItsClass`
  learns `errExtraParams` and `errOutputLimitAboveCeiling`. Rows 15–16:
  `TestSpeechRefusals` + `max_new_tokens` 1001 over a ceiling of 1000 (the message
  names the ceiling); `TestSpeechBatchRefusals` + an item's and the batch's value
  over the ceiling; `TestSpeechOutputLimit` keeps a value at the ceiling and one on a
  model with no limit; `TestSpeechBatchThroughThePipeline` and
  `TestSpeechBatchOutputLimits` send values at the ceiling, never lowered;
  `TestImagesRefusals` + four `extra_params` cases (a path, no path, `{}`, `null`);
  `TestImagesPassWhatTheyDoNotOwn` passes an unknown object instead.
- `provider`: `TestPassthroughBodyEditsElements` removed with the item-edit
  mechanism; the speech and batch passthrough tests send no parameters and expect
  the model edit alone; new `TestSpeechBatchRewriterReplacesEachResultsError` (string, object,
  array, quotes and braces in text, null/number/boolean kept, escaped names — one
  character and every character — `error` and `results` named twice, the top-level
  `model` rewritten and a result's kept, `error` elsewhere untouched, results not a
  list; every split and byte by byte); `TestSpeechBatchAnswerCompleteness` expects the
  edited body and an error answer relayed untouched; `TestRerankPassesThroughWithTheModelEditAlone`
  keeps a rerank result's `error`.
- `accounting`: `TestSpeechBatchAudioLength` + a whole 4096-character window ending
  in `==` (3070 bytes, not 3072); `TestSpeechBatchUnmeasurableAudio` + a negative
  index; `TestSpeechBatchReadsWhereTheAnswerPutsThem` + data after the answer;
  `TestUnreadableWAVIsNotMeasured` + an absurd rate, one past the bound, one at it
  (measured); `TestPCMAndOtherContentTypes` + PCM past the bound;
  `TestImagesCountTheAnswersData` and `TestScannerFindsTopLevelMembers` with real
  escapes.
- `limits`: new `TestImageLimitBlockedOnlyByARunningRequestAnswersAShortRetry`
  (minute and hour: the repro → 2 s; once settled, the settled wait, which holds).
- `e2e` `TestSpeech`: no `max_new_tokens` sent; the batch's errored item carries the
  gateway's text, the backend's in neither the body nor the log.
- Mutants reverted one at a time, each killed by its test: the `extra_params` check
  removed; the default sent; the batch rewriter unwired, or wired on error answers;
  `model` matched at a result's depth; containers not replaced; `maxKeyCapture` 32;
  the batch's rate winning; the window read when full; a negative index read; reading
  past the answer; the fake's header before pacing; either byte-rate bound removed;
  image limits back on the slot's expiry; either name read undecoded; the speech
  ceiling lowered again, an item's ceiling unchecked, image `extra_params` passed.

**Tests for rows 17–19** (each new or changed test fails without its fix — checked
by reverting each fix in turn):
- `server`: new `TestSpeechBatchCapCountsItemsBeforeDecodingThem` — refusing 200 000
  empty items allocates as often as refusing 1 000 (`testing.AllocsPerRun`; the old
  code: 200 044 against 1 015), the message naming the count; a list within the cap
  decoded into its three items.
- `provider`: new `TestSpeechEventsAreReadByExactNames` — an error event naming
  `"Type":"speech.audio.delta"`: as the first event an error event, code 500; after a
  delta, relayed with the gateway's message (the backend's text gone) and ending the
  stream incomplete though `done` follows; deltas naming `"Type"` done leave it
  incomplete; a `done` naming `"TYPE"` a delta completes it.
- `accounting`: new `TestSpeechEventsAreReadByExactNames` — `Audio`,
  `RESPONSE_FORMAT`, `Type`, `Usage`, `OUTPUT_TOKENS` beside the API's members change
  nothing, and named alone are absent; `speechEventUnits` with `OUTPUT_TOKENS` 0 beside
  `output_tokens` 40 reports 40. `TestSpeechBatchUsage` + a count in another case
  beside it, and the counts in another case alone; `TestSpeechBatchReadsWhereTheAnswerPutsThem`
  + names in another case. New `TestSpeechBatchEscapedAudioReadsAsItsCharacters`
  (the Codex reproduction, widened) — PCM and WAV of every byte value, the first
  character or every character escaped (`\uXXXX` in either case, `\/`), the same
  JSON value as the plain answer, 1 s measured in every split. `TestSpeechBatchUnmeasurableAudio`:
  the case "an escape" (`\u0041`, which encoded the defect) replaced by escapes base64
  has no place for — `\n`, `\"`, `\u0021`, `\u00e9`, `\u0141` (past one byte, its
  last byte `A`), a surrogate pair — and an escape cut short by a quote.
  `TestSpeechDeltaLength` + escaped characters in PCM and in a WAV header.
- Fuzz functions, their seed corpora run by `go test`: `FuzzWAVLengthIsTheSameHoweverRead`
  (a WAV read whole, a byte at a time and as base64 by `writeBase64` gives the same
  length, measured flag and samples seen; the seeds walk chunk boundaries) and
  `FuzzImagesCountIsTheDecodersCount` (the image count against `encoding/json`'s, in
  pieces of 1, 7 and 32 KiB) — differential checks no existing test made. Fuzzed
  10 s each here: 1.5 M and 1.3 M executions, no failure.
- The Codex reproductions (`auditb`) copied into the packages and run with `-race`:
  all pass; removed again.
- Mutants reverted one at a time, each killed by its test: the cap checked after
  decoding; the usage, the delta and the speech stream's `observe` read by struct;
  escapes back to `\/` alone; a `\uXXXX` naming no base64 character written; a code
  past one byte read by its last byte; a digit that is not hex taken into the escape.

**Suite (2026-10-10):** `scripts/check-gateway.sh`, then `scripts/check-all.sh`
twice, each green, exit 0 — and again after rows 15–16: `scripts/check-gateway.sh`,
then `scripts/check-all.sh` twice (the second after this record was written), each
green, exit 0, with the same counts:
- **Gateway:** gofmt, vet, staticcheck 2026.2.1 on go1.27.1, telemetry boundary;
  `go test -race -count=1 ./...` — every package `ok`, `kaiak/e2e` ≈120 s.
- **Live-test kit:** gofmt, vet, staticcheck; `-self-test passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends` (59/37/22/21/16/16/34 passed, 0 failed); `usage-log/image` counted 4;
  `image-refused` includes `extra_params`.
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 1 skipped; **`npm run
  lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈76 s); `all checks passed`.

**Green 3× in a row after the Codex fixes (2026-10-11, run by the main session; the
fixing subagent was cut off by a usage limit as its own runs started):**
`scripts/check-all.sh` three times — each exit 0, `all checks passed`; gateway
`kaiak/e2e` 125.2 s, 120.3 s, 123.9 s; control `npm test` 655 tests, 654 pass, 0 fail,
1 skipped; cross-half e2e `ok` 75.4 s each.
