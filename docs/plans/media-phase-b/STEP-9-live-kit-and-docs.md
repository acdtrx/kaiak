# Step 9 — the live-test kit and the docs

**Status:** done — reviewed and committed 2026-10-11

## Intent

Give the live-test kit checks for the new endpoints, so step 11 can run them against
real servers. Bring every operator-facing doc to phase B.

## Files likely touched

- `scripts/live/`:
  - **`vllm-omni` kind:** an image edit with a generated input image, its `images`
    count, the refusals (`stream`, a URL, `lora`);
  - **`vllm` kind:** a transcription (a generated WAV of known length, plus an MP3 or
    FLAC), its seconds against the known length, `verbose_json`;
  - **`llama-server` kind:** a transcription of WAV, FLAC and MP3, the measured
    seconds, the refusal of an unmeasurable upload;
  - every config it writes at protocol 7 with lists;
  - **the "not found" reading against real servers** (added at step 3 review): since
    step 3 the kit's wrong-endpoint checks are refusals by the list, never sent, so
    nothing exercises a real server's route-missing answer any more. Each kind gets
    one deliberately wrong model, expecting `502 upstream_path_missing`, no retry,
    the circuit untouched:
    - `vllm`: a chat model whose list names `embeddings` (vLLM has no such route for
      it);
    - `llama-server` and `vllm-omni`: a backend with a wrong `base_url` (their servers
      register every route whatever the model, so only a wrong path gets the
      answer);
  - its self-test.
- `docs/testing/LIVE-BACKENDS.md`: the new checks; serving an ASR model on vLLM and on
  llama-server; Qwen-Image-Edit on vLLM-Omni.
- `docs/DEPLOYMENT.md`:
  - **Upgrade notes, "Next release" (protocol 7):**
    - both halves together;
    - `format_version` becomes `protocol_version: 7`;
    - a list on every model;
    - one type per model (split a model that mixed providers into two names);
    - `upstream_endpoint_missing` gone, merged into `upstream_path_missing`, which is
      no longer retried nor a circuit failure — a mistyped `base_url` fails its
      requests until fixed; update alerts matching either;
    - the new config field and environment variables;
    - the new endpoints, codes, metrics and fields;
  - **Uploads:** the cap, the budget, the deadline, sizing `GOMEMLIMIT`;
  - **Image edits:** Qwen-Image-Edit on vLLM-Omni, the edit models' input image
    limits, `mask_image` (OpenAI's `mask` is ignored by vLLM-Omni);
  - **Speech-to-text:** vLLM (seconds reported, every format), llama-server (WAV, FLAC,
    MP3; seconds measured), streaming and translations not served;
  - **Endpoints per model:** now required — what to list per kind of model, and what
    a wrong list does (decision 22).
- `README.md`, `control/kaiak-control/GUIDE.md`, `docs/ARCHITECTURE.md` (uploads in the
  data flow; the endpoint memory gone), `docs/TECH-STACK.md` (only if a library
  choice is recorded).
- `docs/BACKLOG.md`: entries resolved by phase B removed; the open ones current.

## Decisions made during planning

- **Generated test media:** the kit writes its WAV, FLAC and MP3 inputs from code (a
  tone of known length) rather than shipping binary files. If MP3 or FLAC cannot be
  written without a dependency, it carries a few KiB of fixed bytes with their known
  length.

## Acceptance criteria

- The kit's lint and self-test pass (`scripts/check-gateway.sh`).
- The docs agree with the code and with each other: grep for `format_version`,
  `upstream_endpoint_missing`, "optional" endpoints and the endpoint memory turns up
  nothing stale.
- `scripts/check-all.sh` run and recorded. Expected red: none.

## Result

### The live-test kit (`scripts/live/`)

**New flags**, in the kit's pattern (a URL, a model, a key variable; `LIVE_*`
environment defaults), each set's checks skipping cleanly without it:
- `-asr-base-url`, `-asr-model`, `-asr-api-key-env` (`LIVE_ASR_BASE_URL`,
  `LIVE_ASR_MODEL`): a speech-to-text server of the run's kind, `vllm` or
  `llama-server` (backend `live-transcriber`, model `live-asr`, list
  `audio_transcriptions`); another kind given them says `SKIP transcribe`;
- `-edit-base-url`, `-edit-model`, `-edit-api-key-env` (`LIVE_EDIT_BASE_URL`,
  `LIVE_EDIT_MODEL`): a vLLM-Omni image edit server beside a run of any kind (backend
  `live-edits`, model `live-edit`, list `images_edits`).

The reranker's and the vLLM-Omni servers' flag checks in `resolve` became one loop
with the new two (same purpose: a server beside the run's own).

**Generated media** (`uploads.go`; no binary files shipped):
- a 3.6 s 440 Hz tone, 16-bit 16 kHz mono, as WAV; as FLAC — STREAMINFO with the
  samples' MD5, VERBATIM subframes (every field byte-aligned), each frame's CRC-8 and
  CRC-16; as MP3 — 100 frames of MPEG-1 Layer III, 32 kHz, 32 kbps, mono, header then
  zeros (silence, as encoders write it). 3.6 s is a whole number of MP3 frames and
  samples, and rounds up to 4 with room either side for a decoder's delay;
- a 512×512 PNG of a lighthouse (`image/png`);
- checked locally once, then deleted: ffmpeg (`-err_detect crccheck+bitstream+explode`),
  mpg123 and afinfo decode all three at 3.600 s; the FLAC's STREAMINFO MD5 equals the
  decoded samples' and its samples equal the WAV's byte for byte.

**New checks** — what each proves:
- `edit`, `usage-log/edit`: an edit of the PNG uploaded as OpenAI's multipart form is
  answered with images that decode; the record counts the answer's `data[]`, tokens
  estimated, output type `image`.
- `edit-refused`: `stream=true` (`invalid_value`), an input image by `https` URL and
  `lora` (`external_reference_unsupported`) refused before any backend.
- `generation-to-edit`: a generation to the edit model is the list's
  `endpoint_not_served`, never sent (vLLM-Omni would answer `500`).
- `transcribe-wav`, `-flac`, `-mp3` and `usage-log/transcribe-*`: on vLLM the answer's
  `usage.seconds` is 4 — the server decoded the file as written — and the record
  carries it, tokens estimated; on llama-server the answer reports tokens, and the
  record carries the gateway's own measurement (4) with the tokens exactly. Output
  type `text`.
- `transcribe-verbose` (vLLM): `verbose_json`'s `duration` is 3.6 and the record
  counts it rounded up; skipped when the server answers `400` (models without segment
  timestamps).
- `transcribe-refused`: `stream=true` refused on both; on llama-server an Ogg upload
  `400 audio_format_unsupported` on `file`; none sent.
- `transcribe-limit`: `audio_seconds_in_per_minute` 1 — the first transcription
  settles 4 s, the second is refused `429 audio_seconds`.
- **The "not found" reading against real servers** (`pathmissing.go`):
  - `path-missing` on `vllm`: `live-path-missing` is the chat model on its own
    deployments (both, with `-base-url-2`) with the list `embeddings`, which a vLLM
    chat server has no route for; skipped when `-embeddings-model` is served by
    `-base-url`;
  - `path-missing` on `llama-server`: the chat model on `live-wrong-path`, a backend
    at `-base-url` + `/wrong-path`;
  - `omni-path-missing`, with any vLLM-Omni server: the speech model (else image, else
    edit) on `live-omni-wrong-path`, its `base_url` + `/wrong-path`;
  - each sends threshold × deployments + 1 requests (threshold 5, 2 with two
    backends): every one `502 upstream_path_missing` with the spec's message, one
    attempt, a record of zero usage not estimated; then no `circuit opened` line and
    `kaiak_circuit_state` closed for every deployment, the `path_missing` attempts
    equal to the requests, and for a wrong `base_url` the config-apply warning `the
    backend has no models list at its base_url` naming the backend. On vLLM the
    deployment is `live-chat`'s, so the chat checks after it show it still serving.
- Extended: `models` (the new models' lists), `endpoint-not-served` (image edits and
  speech-to-text sent to the chat model as uploads — any other body is refused before
  the model is known), `media-metrics` (edited images, audio processed, the
  audio-processed limit's refusals); the `live-media` group gains
  `audio_seconds_in_per_minute` 1.

**Self-test**: speech-to-text fakes on `vllm` and `llama-server`; an image edit fake on
`vllm`; the `vllm` run's embeddings on a fake of their own (one model per vLLM
process — its chat fake now has no embeddings route); each fake answers a path it has
no route for as its server does.

**Fake backend command** (`gateway/internal/fakebackend/cmd/fakebackend`), two flags:
- `-no-route-shape vllm|llama-server` — the `404` body for a path the fake has no
  route for: FastAPI's, or llama-server's `File Not Found` (before, `-no-route` gave
  FastAPI's and nothing gave llama-server's);
- `-audio-millis` — `Reply.AudioMillis`: the length vLLM's speech-to-text shape hears
  in every upload (the self-test sets the kit's 3.6 s).

**Break-it checks** (one line each, reverted with `git checkout`): `upstream_path_missing`
read as a circuit failure (`attempts.go`, `routing.Neutral` → `routing.Failure`):
`path-missing` fails at the 6th request (`503`, the circuit open); the measured upload
ignored (`transcription_usage.go`, `UploadMeasured && false`): the three
`usage-log/transcribe-*` and `transcribe-limit` fail on llama-server.

### Decisions made here

- **The vLLM case uses `embeddings`, as the step says**, so the `vllm` self-test's
  embeddings moved to a server of their own, and a live run serving
  `-embeddings-model` on `-base-url` skips `path-missing` (that server has an
  embeddings route). `rerank` would have needed neither; the step named embeddings.
- **The wrong path appends `/wrong-path`** rather than dropping `/v1`: llama-server
  serves chat and its models list without `/v1`, so a dropped `/v1` is no wrong path
  there.
- **"No usage" is the routed request's one record, zero units, not estimated** — the
  spec's one record per routed request, as the server test asserts.
- **The cloud kinds get no wrong-path check**: the step names vllm, llama-server and
  vLLM-Omni. Their signatures are documented, "not verified live"; the same code
  would check them with a fake answer per kind, if wanted.
- **One length for every upload** (3.6 s): vLLM's fake shape hears one length per
  process.
- **Added beyond the step's list** (cheap, same pattern as phase A): `transcribe-limit`,
  `generation-to-edit`, the `stream` refusal on transcriptions.
- **Docs: the memory starting point grew** by the upload budget — 1.75 GiB at the
  defaults, `GOMEMLIMIT=1600MiB` — since any key can fill the upload budget (an upload
  is read before the model's list is checked).
- **The illustrated pages** (`docs/architecture/*.html`) were stale too and were
  brought to phase B (not in the step's list).

### Docs changed

- `docs/testing/LIVE-BACKENDS.md`: intro, the generated config, the models table, the
  prerequisites' costs, the commands; new sections **Speech-to-text on a server of its
  own** (`vllm serve <model>`; `llama-server -m … --mmproj …`) and **A path the server
  does not have**; **Speech, images and image edits on vLLM-Omni**
  (`vllm serve Qwen/Qwen-Image-Edit-2511 --omni`); the self-test; the checks table (new
  rows; `models`, `chat-to-reranker`, `rerank-to-chat`, `media-metrics`,
  `endpoint-not-served` rewritten); reading failures (the endpoint-missing bullets
  gone, `upstream_path_missing` rewritten, the new checks' failures).
- `docs/DEPLOYMENT.md`: sizing (the upload budget in the memory limit), the
  environment table (`KAIAK_UPLOAD_MEMORY_BYTES`, `KAIAK_UPLOAD_READ_TIMEOUT_MS`),
  Clients (the new endpoints; passthrough with required lists; **A path the server
  does not have** replacing the endpoint-missing bullet), Config for many hosts, new
  sections **Endpoints per model** (what to list per kind of model, one type per
  model, what a wrong list does), **Uploads**, **Image edits**, **Speech-to-text**;
  Rerankers and Speech and images brought current; Observability (the Wrong model,
  path or credential alert reads `path_missing` alone, Endpoint missing on every
  attempt removed, Body or upload budget refusals, the log field); Upgrades (a type is
  an addition; the **Next release** notes for protocol 7).
- `README.md`: the intro, status (protocol 7, the config included), client APIs, one
  type per model, limits, the required lists; the image tag example unchanged.
- `docs/ARCHITECTURE.md`: the data flow (uploads; lists on one type), `config` holds
  the endpoint table, `server`'s stages (`endpoint list`, `upload measured`) and
  upload reading, `provider` (the path-missing reading, the rebuilt form),
  `accounting`, `fakebackend`, the e2e and cross-half tests, the kit.
- `docs/architecture/gateway.html`, `control-plane.html`: the stale sentences, the
  stage list, the API matrix (a speech-to-text row; edits), the footers.
- `control/kaiak-control/GUIDE.md` §7: what to list adds speech recognition and image
  edit models (the rest was moved to protocol 7 at step 2).
- `docs/BACKLOG.md`: Pricing media units names media requests and speech-to-text
  models. No entry is resolved by phase B beyond step 7's: Streaming transcription,
  Audio translations and the rest stay open and current.
- `docs/TECH-STACK.md`: unchanged — no library choice.

### Grep checklist (`git grep -i`, `*.md` and `*.html`, `docs/plans/` and `docs/reviews/` aside)

- `format_version` 4: DEPLOYMENT's 0.11.0 and 0.14.0 release notes (historical) and
  the Next release note naming the rename (2).
- "config format" 7: DEPLOYMENT's 0.11.0–0.14.0 release notes (historical);
  TECH-STACK's "Config format: JSON" (the encoding).
- `upstream_endpoint_missing` 2: the 0.14.0 note (historical), the Next release note
  naming its removal.
- "endpoint memory" 1: the Next release note naming its removal.
- "remembered" 5: the 0.13.0 note and the Next release note's history; three spec
  hits about batch cursors and gateways, unrelated.
- "optional `endpoints`" 0. "core endpoint" 0. "Wrong path to a host" 0.
- "absent" 60, none about an endpoints list but GATEWAY.md's rejected alternative
  (an optional list), recorded on purpose; DEPLOYMENT's `gen_ai_output_type` hit is
  the 0.14.0 note.
- "An endpoint missing from a server" 1: the 0.13.0 note (historical).
- `endpoint_missing` 6, all in DEPLOYMENT's release notes: 0.13.0 (2), 0.14.0 (1)
  and the Next release note (3). "endpoint-missing" 0, "no endpoint produces" 0,
  "Chat models need none" 0.

### Suite

`scripts/check-all.sh` — **all checks passed** (expected reds: none):
- gateway: gofmt, vet, staticcheck 2026.2.1, the telemetry boundary, `go test -race
  -count=1 ./...` — every package `ok`, `e2e` in 126 s;
- live-test kit: gofmt, vet, staticcheck; self-test passed for vllm (75 passed),
  llama-server (48, 1 skipped), openai (22, 1 skipped), azure-openai (21, 1 skipped),
  anthropic (16, 3 skipped), azure-anthropic (16, 3 skipped) and vllm with two
  backends (35, 3 skipped), no failure — the skips are the media and speech-to-text
  sets and embeddings/rerank where a kind has no server for them;
- control: `npm test` 675 tests, 674 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before); `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e: `ok` in 81 s.
