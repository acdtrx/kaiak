# Plan: media generation — phase A (speech and image generation on vLLM-Omni)

## Goal

Serve speech and image generation from self-hosted models through the full request
pipeline: `POST /v1/audio/speech` (whole clip, SSE stream, raw-audio stream),
vLLM-Omni's `POST /v1/audio/speech/batch` and `POST /v1/images/generations`, passed
through to vLLM-Omni backends, counted in their own units — images and seconds of
audio — and limited in them.

Phase B (uploads, then image edits and transcription) follows in its own plan after
this one is released. The research and the direction settled with the user are in
`RESEARCH.md` beside this file; this plan builds on them and does not repeat them.

## Scope

- **A backend type, `vllm-omni`:** vLLM-Omni's server, claiming the media endpoints
  only, all three of them core endpoints.
- **Three client API endpoints:** speech, speech batch, image generation — each a format
  of its own, through every pipeline stage.
- **Three usage units:** `images`, `audio_seconds_out`, `audio_seconds_in` (the last one
  is produced in phase B).
- **Six limit types:** images and the two audio units, per minute and per hour.
- **Two global caps:** images per request and speech batch items.
- **Both halves:** config format 6 and protocol 6 — the specs, schemas, fixtures, the
  gateway and `kaiak-control`.
- **kaiak measures speech audio length** (WAV and PCM) as it relays it.
- **Live-test kit:** a `vllm-omni` kind with speech and image checks; docs for serving
  Qwen3-TTS and Qwen-Image behind kaiak.
- **Endpoints per model** (step 13, added 2026-10-11): an optional per-model list
  narrowing the endpoints a model serves, so a request a loaded model cannot serve is
  refused before any backend.

## Out of scope

- **Phase B:** uploads (`multipart/form-data`), image edits, transcription — their own
  plan, after this one is released.
- **Speech output in MP3, FLAC, Opus or AAC:** refused (`docs/BACKLOG.md`, measuring
  compressed speech audio).
- **Streaming image generation:** vLLM-Omni does not stream generations.
- **Prices for the new units:** a priced model's token prices still apply to speech
  tokens; images and audio seconds cost nothing. Backlog (step 1).
- **Video, WebSocket endpoints, the voices endpoints, other servers.**

## Decisions

Settled with the user (2026-10-09; `RESEARCH.md` → Direction and Settled for
planning):

1. **vLLM-Omni only**, a backend type of its own: `vllm-omni`.
2. **Phase A is speech (whole clip, SSE stream, raw-audio stream, batch) and image
   generation**, released before phase B.
3. **Units:**
   - `images` — images generated;
   - `audio_seconds_out` — audio generated;
   - `audio_seconds_in` — audio processed (phase B).

   All three enter in this plan's version bump, so phase B needs none.
4. **Limits:** images and audio seconds per minute (each gateway) and per hour (shared
   through the control plane's totals), as tokens are.
   - Images reserve `n` before the request runs.
   - Audio generated settles after it.
   - Audio processed (phase B) is reserved from the measured upload.
5. **Speech output is WAV or PCM only.** kaiak measures the seconds; the other formats
   are refused.
6. **Raw-audio streaming is accepted.**
   - Its seconds are measured.
   - Its tokens are estimated from the input text and flagged `estimated`.
7. **Image `response_format` is `b64_json` only** (changed at step 1 review,
   2026-10-09): `file` answers a ZIP for several images, and layered models answer one
   image per layer, so a count from `n` would be wrong; `file` and `url` are refused,
   and `images` is always the answer's `data[]` count. Backlog: image `file` responses.
8. **`vllm-omni` claims the media endpoints only**, and they are its core endpoints
   (changed at step 1 review, 2026-10-09). Every vLLM-Omni server registers all three
   routes whatever model it loaded, so a route-missing `404` there is a wrong
   `base_url`; a model that cannot serve one answers in the server's own words
   (`GATEWAY.md`). Chat is not claimed because vLLM-Omni's chat on a diffusion model
   generates images,
   and on an Omni model audio, which no media limit would see. Chat, completions,
   embeddings, Messages, Responses and rerank to a model on `vllm-omni` deployments get
   kaiak's `400 endpoint_not_served`. Backlog: Omni chat models.
9. **`gen_ai.operation.name` is `generate_content`**, OpenTelemetry's well-known value,
   with `gen_ai.output.type` naming the kind.
10. **Model metadata is unchanged and required.**
    - `capabilities.vision` means the model takes images in.
    - `context_length` bounds the prompt and, for speech, the output limit.
11. **URLs and file paths are refused** in fields that make the server fetch or read:
    speech's reference audio (`ref_audio` and its variants, step 1) and image
    generation's `lora` (an adapter path, confirmed at step 1 review). Inline data
    only. **Speech's `extra_params` is refused outright** (step 11 review,
    2026-10-10): vLLM-Omni hands it to the model, and IndexTTS2 fetches
    `extra_params.emo_audio` like `ref_audio`; the gateway cannot know which keys a
    model fetches, and judging values by how they look is rejected.
12. **New global caps**, as `max_embedding_inputs`:
    - images per request, default 10;
    - speech batch items, default 32.

Made while planning (confirm in review):

13. **Speech's owned fields:**
    - `model`;
    - `stream` and `stream_format`, which together pick the mode: whole clip, SSE, or
      raw audio;
    - `response_format` (`wav` or `pcm`; anything else refused);
    - `max_new_tokens`, the output-limit key: a client's value is refused above the
      model's ceiling (bounded by `context_length`), but **no default is injected**
      (step 11 review, 2026-10-10) — an injected value switches off vLLM-Omni's
      text-scaled budget and retry, and a reached `max_new_tokens` on Qwen3-TTS voice
      cloning answers `500`, a circuit failure the gateway's own default would cause.
      Limits still reserve the model's default figure;
    - `ref_audio` (no URLs or paths);
    - `sample_rate`, read to measure PCM.

    Everything else passes untouched.
14. **The speech batch's owned fields:**
    - `model`;
    - `items`, counted against the cap;
    - each item's and the batch's `response_format`, `ref_audio` and `max_new_tokens`,
      with the same rules as speech (no injected default);
    - each errored item's `error` text replaced with the gateway's own message, its
      `index` and `status` kept (step 11 review, 2026-10-10), as 5xx bodies and error
      events are: vLLM-Omni's text can name GPU errors and the backend-side model.
15. **Image generation's owned fields:**
    - `model`;
    - `n`, against the cap and reserved;
    - `response_format` (`b64_json` only; decision 7);
    - `lora` refused (decision 11).

    It is never a stream: an event-stream answer is incomplete, as for rerank.
16. **Usage on a speech answer:**
    - whole clip: tokens from the `x-vllm-omni-*-tokens` response headers;
    - SSE: tokens from `speech.audio.done`;
    - batch: the items' usage summed;
    - raw audio: tokens estimated (decision 6);
    - every mode: `audio_seconds_out` measured on the audio relayed to the client. A
      client gone mid-answer settles the seconds sent, as tokens do today.
17. **Image usage:** `images` is the answer's `data[]` count, read while the body streams
    through without keeping it. Reserving `n` undercounts a layered model's answer; the
    settle corrects it, and a per-minute image limit can be overshot by that answer.
18. **PCM output requires `sample_rate`** (settled at step 1 from vLLM-Omni v0.30.0's
    sources): without it, `400 missing_required_parameter`. Native rates differ (Fish
    Speech 44.1 kHz, Gepard 22.05 kHz, MiniMax Music 32 kHz stereo), and only the
    models that accept `sample_rate` (Qwen3-TTS, Breeze-TTS-2, both mono) can answer
    PCM through kaiak. WAV works on every model.
19. **The names:**
    - limit types: `images_per_minute`, `images_per_hour`,
      `audio_seconds_out_per_minute`, `audio_seconds_out_per_hour`,
      `audio_seconds_in_per_minute`, `audio_seconds_in_per_hour`;
    - global caps: `max_images_per_request`, `max_speech_batch_items`;
    - endpoint names: `audio_speech`, `audio_speech_batch`, `images_generations`;
    - new error codes: `response_format_unsupported`, `external_reference_unsupported`.
20. **Metrics for the new units:**
    - OpenTelemetry's GenAI conventions have no image or audio-length metric (checked
      at the pinned commit and `main`), so they get `kaiak.*` counters:
      `kaiak.usage.images` (`{image}`), `kaiak.usage.audio.output.duration` and
      `kaiak.usage.audio.input.duration` (`s`), beside the token usage counter.
    - The labels match the usage metrics, plus `gen_ai.output.type`.
21. **Versions:** config `format_version` 6 and protocol 6 — the new units on the usage
    record, the new limit types and totals windows, the new backend type and caps.
    The 50 000-counter bound stays: each scope now keeps five hour and month counters,
    and the user expects about 1 000 groups initially (step 1 review, 2026-10-09).

## Constraints

- **One pipeline, no side doors:** every new endpoint passes every stage.
- **Passthrough** preserves what it does not understand; owned fields only.
- **Nothing sensitive in logs:** no prompt text, no audio or image bytes.
- **Audio and images are never held whole:** measured and counted as they stream
  through.
- **Protocol changes land on both halves at once.**
- No new dependency (audio-length reading is the standard library).

## Risks

- **vLLM-Omni moves fast** (195 commits in the two weeks after v0.30.0).
  - Mitigation: step 1 pins the version read, the fake backend follows it, and the
    live run uses the latest release.
- **Measuring audio while relaying it** (a streamed WAV may carry an unknown length;
  SSE carries base64 chunks).
  - Mitigation: the length is read from the bytes actually sent, never from a header's
    size field; tests over each shape.
- **The PCM sample rate** (decision 18).
- **Audio generated settles after the request**, so a per-minute audio limit can be
  overshot by one request, as USD can.
  - Mitigation: documented; images and tokens still reserve.
- **The DGX's memory:** Qwen-Image and a TTS model beside the user's serving model.
  - Mitigation: the live run stops and restores the user's model, as the rerank run
    did.

## Tag

`v0.13.1` on `main`, "before the media generation plan, phase A", immediately before
step 1 starts. It is not pushed to `github`: an anchor, not a release.

## Branch and worktree

Branch `media-a`, worktree `.claude/worktrees/media-a`. It rebases onto `main` before
the ff merge.

Steps are implemented by subagents (the same model as the main session), one step per
brief. The main session reviews each against its acceptance criteria and commits at
the step boundary. The release after the merge is the user's call.

## Phases and steps

- **Phase 1 — Contract, units, limits and the type** (steps 1–4). Green at the end: both
  halves at format and protocol 6, the units and limits counted end to end, the
  `vllm-omni` type serving vLLM's endpoints.
  1. `STEP-1-contract.md` — research pinned, specs, schemas, fixtures, versions.
  2. `STEP-2-kaiak-control.md` — units, limit types, totals and storage, the backend
     type, backend-verify, the sample.
  3. `STEP-3-gateway-units-and-limits.md` — the units on records and metrics, the limit
     types and their reservations, the caps, versions.
  4. `STEP-4-gateway-vllm-omni-type.md` — the module, its endpoints and probe.
- **Phase 2 — Speech** (steps 5–7). Green at the end.
  5. `STEP-5-speech.md` — the whole clip and the raw-audio stream; usage headers; audio
     length.
  6. `STEP-6-speech-sse.md` — the SSE stream.
  7. `STEP-7-speech-batch.md` — the batch endpoint.
- **Phase 3 — Image generation** (step 8). Green at the end.
  8. `STEP-8-images.md`
- **Phase 4 — End to end, live kit, docs, review, live run** (steps 9–12). Green at the
  end.
  9. `STEP-9-e2e.md` — gateway e2e and cross-half e2e.
  10. `STEP-10-live-kit-and-docs.md`
  11. `STEP-11-review-and-green.md`
  12. `STEP-12-live-run.md` — the DGX, with vLLM-Omni.
- **Phase 5 — Endpoints per model** (step 13; added 2026-10-11, with the user, after
  the live run). Green at the end.
  13. `STEP-13-endpoints-per-model.md` — an optional per-model `endpoints` list that
      narrows what a model serves: speech sent to an image model, chat to a
      reranker, refused before any backend.

Expected reds inside phase 1:
- After step 1, both halves fail the new fixtures and the version checks.
- Step 2 clears `kaiak-control`'s; step 3 the gateway's, the two new error codes
  included; step 4 the fixtures that name the `vllm-omni` type.
- The cross-half e2e is red from step 2 until step 3.

## Verification

- **Component tests:**
  - each format's owned fields and refusals;
  - the caps;
  - the reservations (images by `n`, audio settled after);
  - usage from headers, from the SSE `done` event and from batch items;
  - audio length for WAV (whole, streamed with unknown size) and PCM (with and without
    `sample_rate`), base64 included;
  - image counting over a multi-MB answer without holding it;
  - the new limit types per minute and per hour;
  - metrics and the request line for the new units.
- **Gateway e2e** against the fake backend:
  - each endpoint and mode, through the whole pipeline;
  - usage records with the new units;
  - each new limit refusing;
  - chat to a model on `vllm-omni` refused with `400 endpoint_not_served`.
- **Cross-half e2e:**
  - a speech request and an image request settle at the sample;
  - the hourly image and audio totals limit a second gateway.
- **`scripts/check-all.sh`** green at every phase end, and 3× in a row at the end.
- **Live (step 12):** vLLM-Omni on the DGX with Qwen3-TTS and Qwen-Image.
  - The kit's `vllm-omni` kind passes.
  - The real answers match the fake's shapes.
  - The user's model is restored.

**Review outcome** (step 11, 2026-10-09 to 10-11): two agent reviews of the worktree
and an independent Codex review of a plain copy. The user ruled three findings —
`extra_params` refused (speech, then images), no `max_new_tokens` of the gateway's
making on speech, batch item error text replaced — and the rest were fixed with
tests: image `Retry-After` while a request runs, absurd WAV byte rates, untested
paths, the batch cap counted before decoding, speech members read by exact name, JSON
escapes in batch base64. `STEP-11-review-and-green.md` has the table.

**Verification status:** component, e2e and cross-half tests green, `check-all.sh` 3×
in a row (2026-10-11). Live on the DGX (step 12, 2026-10-09/10): vLLM-Omni 0.30.0 with
Qwen3-TTS and Qwen-Image — the kit's 50 checks pass, the real answers match the fake
backend in every field the gateway reads, the measured audio seconds equal the decoded
clips; speech sent to an image model answers `400` after a full image generation
(`docs/BACKLOG.md`, Endpoints per model).
