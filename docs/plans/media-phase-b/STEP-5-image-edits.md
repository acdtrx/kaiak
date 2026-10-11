# Step 5 — image edits

**Status:** done — reviewed and committed 2026-10-11

## Intent

Serve `POST /v1/images/edits` on `vllm-omni` deployments: an upload format of its own,
counted in `images` as generations are.

## Files likely touched

- **The names, on both halves at once:**
  - `protocol/schema/config.schema.json`: `images_edits` in the endpoint names;
  - `protocol/fixtures/backend-types/vllm-omni.json`: `images_edits` in its endpoints;
  - `control/kaiak-control`: `ENDPOINT_SUPPORT`, `MODEL_ENDPOINTS`, the types, the
    packaged schema, tests;
  - `gateway/internal/config`: the endpoint name and the support table.
- `gateway/internal/provider/`:
  - the endpoint (path `images/edits`), an upload format;
  - `vllm_omni.go` serves it;
  - vLLM-Omni's image `400 Model mismatch` read as the model missing, as on
    generations;
  - the answer relayed as generations' is.
- `gateway/internal/server/`:
  - the `bodyEndpoints` row: `images_edits`, operation `generate_content`, output type
    `image`, an upload endpoint;
  - the owned parts (OVERVIEW decisions 16 and 27):
    - `n` against `max_images_per_request`, reserved;
    - `response_format` `b64_json` only;
    - `stream` true refused;
    - `lora` refused;
    - `url`, `url[]`, `mask_image`, `reference_image` strings refused unless they are
      `data:` URLs (`external_reference_unsupported`);
    - `image`, `image[]`, `url[]` may repeat; a file part is never inspected.
- `gateway/internal/accounting/`:
  - the input estimate for an edit: the prompt's text, plus each input image as a
    media item;
  - the image count reused from generations (the answer's `data[]`, never kept).
- `gateway/internal/fakebackend/`: an edits handler that parses the rebuilt body and
  checks it (the model replaced, the parts intact), answering like vLLM-Omni.
- `docs/specs/GATEWAY.md`, Providers → Endpoint support: `images_edits` joins the
  table's media column (`audio_speech, audio_speech_batch, images_generations,
  images_edits`), and the sentence before the table gains its source ("`images_edits`
  on 2026-10-11 from vLLM-Omni's source (v0.30.0; `docs/plans/media-phase-b/
  RESEARCH.md` records it)") — with the code, as `TestEndpointSupportFollowsTheSpec`
  reads the table.
- Tests beside each.

## Decisions made during planning

- **The image counter is shared with generations:** one reader, one rule (a layered
  model's answer may hold more images than `n`; the settle corrects it).
- **An edit's `n` defaults to 1** for the reservation, as vLLM-Omni's does.

## Acceptance criteria

- **Component and server tests:**
  - each owned part and refusal: `stream`, `lora`, each URL-ish field as `http…`, as
    a path-like string and as a `data:` URL, `response_format` other than `b64_json`,
    `n` over the cap;
  - the JSON form of an edit refused;
  - the rebuilt body reaching the fake backend intact;
  - `images` on the record from the answer's count; `tokens_in` estimated and
    flagged;
  - the image limits refusing and settling;
  - the operation and output type on the request line and metrics;
  - an edit to a model whose list leaves `images_edits` out refused before any
    backend.
- The shared fixtures pass on both halves with the new name.
- `scripts/check-all.sh` green — **phase 2 ends green.**

## Result

### What changed

- **The name, on both halves at once:**
  - `protocol/schema/config.schema.json`: `images_edits` in the endpoint enum (after
    `images_generations`) and its description;
  - `protocol/fixtures/backend-types/vllm-omni.json`: `images_edits` in `endpoints`;
    `config/valid/model-endpoints.json`: the image model lists `images_generations` and
    `images_edits`, so a valid config exercises the name on both halves;
  - `control/kaiak-control`: `MODEL_ENDPOINTS` and `ENDPOINT_SUPPORT["vllm-omni"]`; the
    packaged schema synced (`npm run sync-schemas`); `config.test.ts`: image edits
    served by `vllm-omni` alone (a vLLM model listing it reported at its index);
  - `gateway/internal/config`: `EndpointImagesEdits`, the endpoint-support table
    (`vllm-omni`), the schema-order names; `TestModelEndpointsResolve` and the spec
    table's column (`fixtures_test.go`).
- **`docs/specs/GATEWAY.md`:** the endpoint-support table's media column
  (`audio_speech, audio_speech_batch, images_generations, images_edits`) and its sources
  sentence, as this file specified; the `invalid_type` row of the errors table names
  the one case settled here (below): an owned text part sent as a file part.
- **`gateway/internal/provider`:**
  - `ImagesEdits` (path `images/edits`, name `images_edits`) and `FormatImageEdits` —
    an upload, answered as image generation is (`jsonBodyStream`);
  - `vllm_omni.go`: edits take image generation's readings — the `400 Model mismatch`
    naming the deployment's model is `upstream_model_missing`; the comments name edits.
- **`gateway/internal/accounting`:**
  - `NewMeter`: `FormatImageEdits` reads with `imagesUsage` — one counter, one rule
    (the answer's `data[]`, never kept; at least `n` when cut short);
  - `EstimateUploadInput(textBytes, mediaItems)` — an upload's estimate from the parts
    its format reads: text by bytes, each media item at `InlineMediaTokens`, the two
    figures equal, saturating.
- **`gateway/internal/server`:**
  - `pipeline.go`: the `images_edits` row — `generate_content`, output type `image`,
    upload format `imageEditsUpload`;
  - `inbound_image_edits.go` (new): lists `image`, `image[]`, `url[]`; the owned parts
    in the client's order — `n` (`editImages`: `strconv.ParseInt` base 10, below 1
    counted as 1, capped by `max_images_per_request`, reserved), `response_format`
    (`b64_json` only), `stream` (`readStreamPart`), `lora` (refused, text or file),
    `url`/`url[]`/`mask_image`/`reference_image` as text refused unless `data:` (file
    parts pass); the estimate: the `prompt` text part's bytes, plus each `image`/`image[]`
    file part and each `url`/`url[]` data URL;
  - `uploads.go`: `uploadText` (an owned text part; a file part is `invalid_type`) and
    `readStreamPart` (`false` in any case, else `400 invalid_value`, `param: "stream"`)
    — both upload formats' rule, for step 6 to reuse;
  - `errors.go`: `errStreamPart`; `inbound.go`: `inlineData`, speech's `inlineAudio`
    renamed and moved there — one rule (a `data:` URL, scheme in any case) for speech
    references and edit parts.
- **`gateway/internal/fakebackend`:** `/v1/images/edits` reads the rebuilt body with
  `mime/multipart` as vLLM-Omni's route does: a non-form body is an empty form (`400`,
  prompt missing), an upload without a `Content-Length` refused, the model compared with
  the served name (`400 Model mismatch`), `prompt` required, no input image `422`,
  `n` at least 1, `response_format` `b64_json`, `stream: true` `400`, `layers`; the
  answer written by the generations' writer (`writeImagesAnswer`, faults included).
- **Tests:**
  - `server/image_edits_test.go` (new): the whole pipeline (the rebuilt form part by part
    against the client's — names, filenames incl. non-ASCII, content types, every byte
    value, `model` replaced, a new boundary, an exact `Content-Length`; `images` from
    the answer; `tokens_in` estimated and flagged; the token reservation the estimate
    alone; `generate_content`/`image` on the record, request line and usage metrics,
    route `/v1/images/edits`, `gen_ai.request.stream` false, no TTFT); 54 refusals
    (each owned part; `n` fraction, word, empty, spaced, hex, underscore, past int64,
    file; each URL-ish part as `https`, `http` to an IP, a path, `file:`, `s3:`, a
    space before `data:`; `stream` in six spellings and as a file; `lora` text, empty,
    file; repeats; no model; unlisted on `vllm-omni` and on `openai-compatible`) — none
    reaches a backend or settles, no message echoes a value; the JSON form refused; the
    config's cap; what passes (`data:` in any case, masks and references as files,
    `image[]`/`url[]` repeated, `extra_params`); `n` reservation and the estimate's
    inputs read directly; image limits refusing and settling (a layered answer past
    `n`); an answer cut short; the model mismatch; nothing of the content or filenames
    in the logs;
  - `server/endpoints_test.go`: the list check on every route sends the upload route a
    form (`stream=true` skipped there: refused at the body, before the model is
    known); `tts` lists `images_edits`. `uploads_test.go`: the test row's comment;
  - `provider/vllm_omni_test.go`: `images/edits` URL; edits read the mismatch as
    generations do; `accounting`: `TestEstimateUploadInput`,
    `TestImageEditsCountAsGenerations`; `metrics_test.go`: `errStreamPart` listed;
  - `e2e/speech_test.go`: `tts` lists `images_edits` (the fixture's every endpoint).

### Decisions made here

- **`FormatImageEdits`**, a format of its own (the spec's nine formats), sharing the
  JSON-body stream rules and the images reader with generations.
- **An owned text part sent as a file part** (`n`, `response_format`, `stream`, as
  step 4 did for `model`) is `400 invalid_type`, `param` its name — the errors table's
  "wrong type" — rather than the stream rule's `invalid_value`; recorded in the
  `invalid_type` row. `lora` stays `external_reference_unsupported` either way.
- **`n` is read with `strconv.ParseInt(value, 10, 64)`:** an optional sign and decimal
  digits; anything else — spaces, `2.5`, hex, underscores, a value past int64 — is
  `invalid_type`, so no value the backend could read as a larger `n` passes the cap.
- **The estimate counts text parts only:** a `prompt` or `url` sent as a file part, and
  an `image` sent as text, are no prompt or input image (vLLM-Omni's form fields would
  refuse them); `mask_image`, `reference_image` and OpenAI's `mask` count nothing, as
  the spec lists.
- **The fake's edits leave out the "no diffusion stage" `503`:** RESEARCH.md records it
  for generations only.
- `inlineAudio` became the shared `inlineData` rather than a second copy (CODING-RULES
  §2: same purpose).

### Suite

`scripts/check-all.sh` — **all checks passed, twice** (the second run after comment
rewraps and the `invalid_type` row's wording) — phase 2 ends green:
- gateway: gofmt, vet, staticcheck 2026.2.1, the telemetry boundary, `go test -race
  -count=1 ./...` — every package `ok`, `e2e` in 122 s and 123 s;
- live-test kit: lint and self-test passed for every kind;
- control: `npm test` 674 tests, 673 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before; one more than step 4: the image-edits rule test);
  `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e: `ok` in 76 s both times.

**Phase 2 ends green** (main session's run, 2026-10-11): `scripts/check-all.sh` —
`all checks passed`; control 674 tests, 673 pass, 1 skipped; gateway e2e and the
cross-half e2e green.
