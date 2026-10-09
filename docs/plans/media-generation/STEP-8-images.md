# Step 8 — image generation

**Status:** done — reviewed and committed 2026-10-09

## Intent

Serve `POST /v1/images/generations` on `vllm-omni` deployments:
- `n` is capped by `max_images_per_request` and reserved against the image limits;
- `response_format` is `b64_json` only — `file` and `url` are refused (OVERVIEW
  decision 7) — and `lora` is refused (decision 11);
- the answer's `data[]` is counted into `images` as it streams through, never held;
- it is never a stream.

## Files likely touched

- `gateway/internal/provider/`: the endpoint (path `images/generations`) and its
  format; `vllm-omni` serves it; the `model` edit; an event-stream answer is
  incomplete (as rerank's).
- `gateway/internal/server/`:
  - the row (`images_generations`, operation `generate_content`, output type image);
  - the inbound parser:
    - `n`, capped and reserved;
    - `response_format` and `lora` refused;
    - `stream` not owned.
- `gateway/internal/accounting/`:
  - the input estimate (the prompt; `negative_prompt` counted as text);
  - an images reader that counts the top-level `data[]` elements without keeping them.
    The member scanner gains a count-only mode, or the reader counts during its single
    pass.
- The fake backend: images, multi-MB base64 bodies, a layered model's answer (more
  images than `n`).
- Tests beside each.

## Decisions made during planning

- **No `usage` is expected** from vLLM-Omni's image answer. If one appears (a future
  version), it is ignored: images are what this endpoint counts.
- **The prompt's tokens are recorded** as `tokens_in`, estimated and flagged, so token
  limits still see the prompt.

## Acceptance criteria

- **Component tests:**
  - owned fields and refusals;
  - the `n` cap;
  - reservation and settle of `images`;
  - counting over a body of several MiB with no allocation growing with it;
  - a layered answer (more than `n`) settled at its real count;
  - an event-stream answer incomplete;
  - the operation and output type.
- **Phase 3 end:** `scripts/check-all.sh` green and recorded.

## Result

### What changed

- `gateway/internal/provider/`:
  - `provider.go`: the `ImagesGenerations` endpoint (path `images/generations`) and
    `FormatImages`; the `CodeModelMissing` and `CodeEndpointMissing` comments name
    vLLM-Omni's image `400` and `503`.
  - `send.go`: the wire core reads further statuses a module names —
    `wireCall.readings`, each an `answerReading` (`status`, `code`:
    `CodeModelMissing` or `CodeEndpointMissing`, `read(answer, model)`), the answer
    peeked and put back as for a `404`; any other answer of that status relayed (or
    the backend's `5xx`) as before. It replaces the `501`-only `modeMissing` field:
    llama-server's `501` on embeddings and rerank is now one reading
    (`llama_server.go`, no behaviour change). `modelMissing` builds the error for both
    the `404` and a module's reading. `notfound.go`: the peek comments.
  - `vllm_omni.go`: `vLLMOmniEndpoints` gains `ImagesGenerations`; `Send` gives image
    generation `imageReadings` — the `400` `imageModelMismatch` →
    `upstream_model_missing` (a circuit failure, retried) and the `503`
    `noDiffusionStage` → `upstream_endpoint_missing` (neutral, remembered per
    deployment, retried) — on that endpoint alone; `noDiffusionStage` takes the
    reading's signature.
  - `stream_format.go`: `jsonBodyStream` serves images too (no edit of its own, an
    event stream never complete).
- `gateway/internal/server/`:
  - `pipeline.go`: the `images_generations` row — operation `generate_content`,
    output type `image`, no output-limit key.
  - `inbound.go`: `stream` not read for images (as rerank's and the batch's);
    `parseImagesFields` — `n` an integer (absent, null or below 1: 1), above
    `max_images_per_request` → `400 invalid_value`, `param: "n"`
    (`errTooManyImages`, the embeddings cap's wording); `response_format` a string,
    `b64_json` only (null or absent pass) → `response_format_unsupported`; `lora`
    anything but null → `external_reference_unsupported`, `param: "lora"`;
    `inbound.Media = {images: n}`; `inboundFields.mediaRequest()` for the meter.
  - `attempts.go`: the meter gets `rq.inbound.mediaRequest()`. `errors.go`:
    `errTooManyImages`; `metrics_test.go`: its `errorAnswers` entry.
- `gateway/internal/accounting/`:
  - `images_usage.go` (new): the image reader — one pass as the body streams through,
    the answer's members walked, the top-level `data` list's elements counted as each
    begins and every value skipped a string run at a time (nothing kept); `data`
    named twice: the last; no `usage` read (`reported` nil: tokens always estimated,
    output 0). `media(complete)`: the count, or once cut short the larger of it and
    the request's `n`.
  - `scan.go`: `valueSkip` (one value followed in one pass, strings a run at a time,
    optionally kept up to a limit), `stringRun` and `scannedName` (a scanned member
    name decoded), moved out of the speech batch reader — shared by the batch reader,
    the image reader and the member scanner (same purpose: a one-pass JSON reader);
    the batch reader is otherwise unchanged.
  - `meter.go`: `MediaRequest{Speech, Images}` is `NewMeter`'s third argument (was
    `Speech`); `mediaReader.media(complete bool)` — the speech readers ignore it.
  - `estimate.go`: comment — an image request is its body by the rules (prompt and
    `negative_prompt` as text); no code change.
- `gateway/internal/fakebackend/`: `images.go` (new) — `/v1/images/generations` as
  vLLM-Omni answers it: `{"created", "data": [{"b64_json", "url": null,
  "revised_prompt": null}], "output_format", "size", "cot_output", "metrics"}`, no
  model, no usage, `Content-Length`; each image `Reply.ImageBytes` (default 1024) of
  PNG-signed filler; a request's `layers` answers `n × layers` images (a layered
  model); `n` outside 1–10 and a format other than `b64_json` → `400`;
  `SetNonImageModels` → `503 No diffusion stage found in multi-stage pipeline.`;
  `SetImageServedModel` → `400 Model mismatch: request specifies '<model>' but server
  is running '<name>'.`; `Reply.Fault` (Cut, Hang, End) stops the answer within image
  `At`'s base64. `speechError` became `omniError` (vLLM-Omni's error shape, used by
  all three media routes).
- No spec edit: every rule implemented is already in `docs/specs/GATEWAY.md`.

### Design choices

- **Readings are data, not fields per server.** The core reads "a status the module
  names, with the module's reading, as the model or the endpoint missing" — one
  mechanism for llama-server's `501` and vLLM-Omni's `400` and `503`, so the core stays
  type-agnostic (step 4's note) and grows no flag per server.
- **The count is the elements begun.** A whole answer's begun and closed counts are
  the same; for an answer cut short the spec takes the larger of "the elements seen"
  and `n`, and an element whose bytes began arriving was seen. Every element counts,
  whatever it holds ("the number of elements").
- **Cut short is the meter's `complete`.** The reader gets the attempt's completeness
  (`media(complete)`) rather than judging from the JSON itself, so the image count and
  the record's `partial` flag always agree: an event stream answering an image request
  (relayed, ended `upstream_incomplete`) counts `n`; a non-JSON `2xx` that ended
  cleanly counts what it carried (0).
- **`n` reaches the meter from the reservation** (`inbound.Media[images]`), one source
  for both; `MediaRequest` groups what a media request tells its meter, so the next
  media kind adds a field, not a parameter.
- **The reader shares `valueSkip` with the batch reader** (CODING-RULES §2: both skip a
  JSON value in a one-pass answer reader, strings a run at a time); the structure
  walks stay separate — the answers differ (results with kept members vs. a counted
  list).

### Spec points (reported)

- The spec and STEP-8's planning decision agree: `tokens_in` is the input estimate,
  flagged `estimated`, every other token unit 0; a `usage` the answer carries is
  ignored. The estimate is the whole body by bytes (the prompt, `negative_prompt`,
  `size`, …), as the spec's "the body by the rules above" says.
- A request sent and never answered (a response timeout before the first byte, the
  client gone during generation) records the input estimate and **0 images** — the
  spec's "no output" for any end before the first event; the images may have been
  generated. Flagged for review; the spec's rule followed.
- `data` named twice reads the last, as the batch's `results` (the client's reading);
  not written into the spec, as the batch's was not.
- `lora` is refused whatever its value (an empty object too); the message says it
  names something for the backend to read, which holds for every LoRA the server
  loads (a path).

### Tests

- `accounting/images_usage_test.go`, every case split at 1, 3, 4096, 32 KiB bytes and
  whole: one, four, a layered five (past `n`), fewer than `n`; no `data`, empty,
  null, not a list → 0; elements of any kind; `data` nested in another member or
  written in a string not counted; an escaped name; a name escaped otherwise; `data`
  named twice (the last); brackets and braces in strings; indented JSON; a `usage`
  unread (tokens still estimated); a long name; not an object. Cut short: within the
  first image (→ `n`), within the third (→ 3 with `n` 2, → 4 with `n` 4), before
  `data` (→ `n`), after `data` of a layered answer (→ 4), nothing relayed (→ `n`) —
  all estimated and partial; an event stream → `n`, partial; an error answer → zeros;
  sent, never answered → the input alone. Allocation: four images of 100 B and of
  3 MiB (a 16 MiB body) — bytes allocated within 256 KiB of each other.
- `accounting/estimate_test.go`: an image prompt and its `negative_prompt` by bytes.
- `provider/images_test.go`: passthrough through the fake (the model edit alone,
  whitespace, `stream`, unknown members kept; `Accept: application/json`; the answer
  relayed whole, complete, not a stream); completeness — whole, a `model` rewritten,
  cut short, an event stream (`ErrIncomplete`).
- `provider/vllm_omni_test.go`: `TestVLLMOmniImageAnswersThroughSend` — on images,
  the mismatch naming the deployment's model → `upstream_model_missing`, the `503`
  → `upstream_endpoint_missing` naming the URL, neither error carrying the backend's
  text; another model's mismatch, a caller's `400`, another `503` → relayed whole;
  the same answers on speech and the batch → relayed (15 subtests).
  `endpoints_test.go`: images in the media column; images' stream untouched.
- `server/images_test.go`: the pipeline (sent body, two images relayed, not flushed,
  the record — estimate, 2 images, estimated — operation, output type, cost 0,
  the token reservation the estimate alone, `gen_ai.request.stream: false`,
  `kaiak.usage.images`, the route, `kaiak_usage_images_total`, no time to first
  token); 13 refusals (the cap, `url`, `file`, another format, `lora` as an object, a
  string, empty; types; `n` named twice; model; not on `vllm-omni`) plus the config's
  cap; `n` reserved (absent, null, 0, −3 → 1; 4; 10) and never a stream; what
  passes (`b64_json`, nulls, `stream`, `layers`, unowned members) and `n` 0 relayed
  as the backend's `400` with zeros; `images_per_minute` and `images_per_hour` —
  3 reserved and settled, `429 images` naming the limit, a layered answer settling
  3 for `n` 1 (window 6 of 5), the next refused, chat and speech untouched; cut short
  (→ `n`, → the 5 seen past `n` 2), partial; the model mismatch (502, text not
  relayed, zeros, the circuit opened by two) and its retry on the other deployment
  (`kaiak.tried`, `kaiak_retries_total` `model_missing`); no diffusion stage (502
  three times, the circuit closed, remembered, one warning naming
  `images_generations`, speech to the same deployment served; on a two-deployment
  model one attempt then left out, `kaiak_retries_total` `endpoint_missing`); nothing
  of the prompt, negative prompt, images, LoRA path or the backend's texts in logs.
- Checked by mutation (each reverted): readings not wired; cut short not raised to
  `n`; `n` counted instead of `data`; the cap off; `lora` passed; `stream` read;
  `data` named twice adding both; images not reserved; another format passed; `n`
  below 1 reserved as sent; nested `data` counted; llama-server's `501` reading lost —
  each fails its tests.

### Suite (2026-10-09)

End of phase 3; no expected reds. `scripts/check-gateway.sh` green (exit 0, `gateway
checks passed`), then `scripts/check-all.sh` twice (the second after this record was
written), each `all checks passed`, exit 0:

- **Gateway:** gofmt, vet (crosshalf tag included), staticcheck 2026.2.1 on
  go1.27.1, telemetry boundary pass; `go test -race -count=1 ./...` — all 20 packages
  `ok`, `e2e` included (≈119 s).
- **Live-test kit:** gofmt, vet, staticcheck pass; `-self-test` passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends (37/36/22/21/16/16/34 passed, 0 failed).
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 0 cancelled, 1 skipped.
  **`npm run lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈70 s).
