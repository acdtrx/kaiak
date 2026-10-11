# Plan: media phase B — protocol 7, uploads, image edits and speech-to-text

## Goal

Serve image edits and speech-to-text from self-hosted models through the full request
pipeline — `POST /v1/images/edits` on vLLM-Omni and `POST /v1/audio/transcriptions` on
vLLM and llama-server — with the uploads they need (`multipart/form-data`), counted and
limited in their units: images generated, seconds of audio processed.

First, protocol 7 simplifies the model config the endpoints rest on: one version for
everything, a mandatory `endpoints` list on every model, and one backend type per
model.

Phase A (`docs/plans/media-generation/`) is released as v0.14.0. The facts this plan
rests on, read from source, are in `RESEARCH.md` beside this file.

## Scope

- **Protocol 7, one version:** the config's `format_version` becomes `protocol_version`
  and carries the protocol version; one version constant per half.
- **Mandatory `endpoints`** on every model, and **one backend type per model**; routing
  and refusals simplified to match; the endpoint memory removed; one "not found"
  reading, neutral and not retried (decision 22).
- **Uploads:** a multipart reader, the upload cap, its own memory budget and read
  deadline, the body rebuilt for the backend.
- **Image edits** on `vllm-omni`.
- **Speech-to-text** on `vllm` and `llama-server`, producing `audio_seconds_in`.
- **Both halves:** the specs, schemas, fixtures, the gateway, `kaiak-control`, the
  sample, the live-test kit and the docs.
- **Live run** on the DGX: Qwen-Image-Edit on vLLM-Omni, an ASR model on vLLM and on
  llama-server.

## Out of scope

- **Streaming transcription** — refused; backlog (trigger: someone asks for live
  transcripts).
- **`/v1/audio/translations`** — vLLM reports no usage for it; backlog.
- **Streaming image edits** (multi-stage pipelines only) — refused.
- **OpenAI's JSON form of image edits** — no backend takes it; refused.
- **Transcription on cloud backends and on vLLM-Omni** (it never serves it).
- **Prices for the media units** (`docs/BACKLOG.md`, Pricing media units).
- **Accepting the previous protocol version** during a rollout — multi-version
  support waits until the project ships to real users.

## Decisions

Settled with the user (2026-10-11):

1. **Phase B is uploads, image edits and speech-to-text**, behind protocol 7, released
   together.
2. **One version, the protocol version.** It covers the config document, the usage
   record, the messages and the endpoints. It travels in `Kaiak-Protocol`, and the
   config states it in `protocol_version` (renamed from `format_version`), which must
   equal it — a config file or seed config has no connection to carry a header. Any
   change that makes the other half or an existing document unreadable or invalid
   bumps it; additions bump nothing. It becomes **7**. Rejected: a separate config
   format version — every bump so far moved both numbers together (2 to 6), and no
   mix of an older and a newer half works across either bump.
3. **`endpoints` is mandatory** on every model: non-empty, unique. A model serves
   exactly what it lists; `/v1/models` shows the list.
4. **One backend type per model:** every deployment of a model is on a backend of the
   same type — the same engine and the same model config on every instance, differing
   only in capacity (each backend's `max_in_flight`). A new config rule on both
   halves. The cost: one model name no longer fails over between providers (Claude on
   Anthropic and Foundry, a GPT model on OpenAI and Azure): those become two names.
5. **Each listed endpoint is one the model's type serves** (`endpoint-not-served`,
   judged against the one type).
6. **The endpoint memory is removed** (per-deployment memory of an endpoint missing,
   `server/endpointmemory.go`): mandatory lists stop a client's wrong-endpoint request
   before any backend, and identical instances leave nothing to route around.
7. **Speech-to-text runs on `vllm` and `llama-server`.** vLLM-Omni never serves it;
   cloud types stay out (local focus).
8. **`audio_seconds_in`:** the backend's seconds when it reports them; otherwise kaiak
   measures the upload (WAV, FLAC, MP3); a request neither can count is refused before
   it is sent. Counted after the request, as audio generated is — this replaces phase
   A's "reserved from the measured upload". With one type per model the source is the
   model's type: `vllm` reports seconds (`usage.seconds`, or `verbose_json`'s
   `duration` rounded up); `llama-server` reports none, so its uploads are measured,
   and an upload in another format is refused (llama-server decodes only those three).
9. **Tokens on speech-to-text:** reported tokens when present (llama-server's
   `usage`); otherwise as images — `tokens_in` the prompt's estimate, flagged
   `estimated`, the other token units 0. No output limit of the gateway's making.
10. **Speech-to-text is non-streaming:** `stream: true` refused; backlog entry.
11. **Response formats pass through;** the gateway reads `verbose_json`'s `duration`,
    and the backend refuses what it cannot answer (vLLM: `srt`, `vtt`; llama-server:
    anything but `json`).
12. **`/v1/audio/translations` stays out;** backlog entry.
13. **Image edits run on `vllm-omni`** (it serves them; nothing else does).
14. **Image edits count as generations:** `images` is the answer's `data[]` count; `n`
    is reserved and capped by `global.max_images_per_request`; input images have no
    unit and are bounded by the upload cap.
15. **Streaming edits are refused.**
16. **Edit fields:**
    - `url`, `url[]`, `mask_image` and `reference_image` given as strings must be
      inline `data:` URLs, else refused (`external_reference_unsupported`) — the
      phase A ruling, inline data only;
    - `lora` refused, as on generations;
    - OpenAI's `mask` passes through — vLLM-Omni ignores it (its field is
      `mask_image`), documented, not translated;
    - the JSON form of edits is refused (multipart only).
17. **Upload cap:** `global.max_upload_bytes`, default 25 MiB (vLLM's and OpenAI's audio
    limit), one cap for both upload endpoints; the JSON cap
    (`max_request_body_bytes`) stays for JSON bodies.
18. **Upload memory budget:** `KAIAK_UPLOAD_MEMORY_BYTES`, default 256 MiB, separate
    from the body budget, so uploads cannot starve chat. Uploads stay in memory: the
    gateway writes nothing to disk, and a retry needs the whole body.
19. **Upload read deadline:** `KAIAK_UPLOAD_READ_TIMEOUT_MS`, default 300 000 (25 MiB
    in 60 s needs about 3.5 Mbit/s of uplink, which mobile clients often lack).
20. **The multipart body is rebuilt, not edited:** every part kept byte for byte with
    its name, filename and content type, `model` replaced, a new boundary — so the
    backend gets exactly what the gateway parsed (the reasoning behind refusing
    duplicate JSON members; llama-server's router mode rebuilds the same way).
21. **Part rules:** a repeated single-value part is refused, as duplicate members are;
    list parts may repeat (`image`, `image[]`, `url[]`, `timestamp_granularities[]`);
    a text part is capped at 1 MiB and a body at 1000 parts (Starlette's limits);
    filenames never appear in logs.

22. **One "not found" reading on every type, neutral and not retried** (settled with
    the user at plan review, 2026-10-11): a backend's route-missing answer on a listed
    endpoint answers the client `502 upstream_path_missing` at once — not a circuit
    failure, not retried, its message naming both causes left with mandatory lists and
    identical instances (a wrong `base_url`, or a list naming an endpoint the server
    does not serve). Both are operator errors: the gateway errors out and logs it, and
    does not work around them. This is the fewest lines: the endpoint-missing class
    (`upstream_endpoint_missing`) goes, with each type's core endpoints, `vllm`'s "no
    core endpoints" rule, and the wrong path's retry rule (every deployment on that
    backend refused for the request); llama-server's `501` and vLLM-Omni's image `503`
    lose their special readings and are plain `5xx`, as any server error. The cost: a
    mistyped `base_url` — on any type, where today it trips the circuit on the OpenAI
    and Anthropic types — fails its share of requests until fixed; the config-apply
    warning (the backend has no models list at its `base_url`) and the log show it.
    This replaces the 2026-10-08 rulings on the endpoint-missing reading and on the
    wrong path as a circuit failure.

Made while planning (confirm in review):

23. **One refusal for an unlisted endpoint:** `400 endpoint_not_served`, the message
    "The model is not served on /v1/…: its configured endpoints do not include this
    API." The type-level refusal ("none of its backends serves this API") goes: config
    validation makes it impossible.
24. **Names:** endpoints `images_edits` (`/v1/images/edits`) and `audio_transcriptions`
    (`/v1/audio/transcriptions`); the config field `global.max_upload_bytes`; the
    environment variables `KAIAK_UPLOAD_MEMORY_BYTES` and
    `KAIAK_UPLOAD_READ_TIMEOUT_MS`; new error codes named at step 1 (a non-multipart
    upload; an upload whose audio cannot be measured where it must be).
25. **OpenTelemetry:** speech-to-text is `generate_content` with `gen_ai.output.type`
    `text`; image edits `generate_content` with `image`, as generations.
26. **Speech-to-text's owned parts:** `model`; `file` (exactly one file part,
    required); `stream` (true refused); `response_format` (read for `verbose_json`);
    `vllm_xargs` refused outright, as phase A's `extra_params` (it reaches the model,
    whose keys the gateway cannot judge). The `prompt` part feeds the input estimate.
    Everything else passes untouched.
27. **Image edits' owned parts:** `model`; `n`; `response_format` (`b64_json` only);
    `stream` (true refused); `lora`; `url`, `url[]`, `mask_image`, `reference_image`
    (decision 16); `image` and `image[]` file parts, each an input media item in the
    estimate; `prompt` for the estimate. Everything else passes untouched.
28. **The rebuilt body is written as it is sent,** from slices of the client's body,
    with its `Content-Length` computed from the parts — no second copy held, and
    servers that check an upload's size from its length (vLLM) can.
29. **A `vllm` answer without seconds** (none expected) falls back to the measured
    upload when it can be measured, else 0 flagged `estimated`.
30. **Measuring an upload:** the whole file is in memory, so:
    - WAV: the format chunk's rate, channels and bit depth, and the data chunk's bytes
      actually present (its declared size bounded by the file);
    - FLAC: STREAMINFO's total samples and sample rate (a total of 0, "unknown", is
      unmeasurable);
    - MP3: a frame scan — ID3v2, ID3v1 and APE tags skipped, samples per frame by MPEG
      version and layer;
    - rounded up to a whole second once per record, as `audio_seconds_out`.

## Constraints

- **One pipeline, no side doors:** both new endpoints pass every stage.
- **Passthrough** preserves what the gateway does not own: every part survives the
  rebuild.
- **Nothing sensitive in logs:** no prompt text, no audio or image bytes, no
  filenames.
- **No new dependency:** `mime/multipart` and the audio-length readers are the
  standard library.
- **Protocol changes land on both halves at once.**

## Risks

- **The protocol 7 churn:** every config fixture, test document, example and seed in
  both halves gains a version rename and endpoints lists.
  - Mitigation: step 1 moves the shared fixtures in one pass; each half's test
    helpers build models with a list once, centrally.
- **Upload memory:** a 25 MiB body plus the provider's rebuilt stream.
  - Mitigation: the separate budget (decision 18); the rebuilt body is never held
    (decision 28).
- **Measuring hostile audio** (truncated headers, absurd sizes, endless frame
  chains).
  - Mitigation: bounded single passes over the in-memory file; fuzz-style tests, as
    phase A's WAV reader had.
- **A mistyped `base_url` is no longer taken out of rotation** (decision 22): its
  share of requests fails until it is fixed.
  - Mitigation: the config-apply warning, the `502 upstream_path_missing` on the
    request line and in the error metrics; documented in DEPLOYMENT.
- **The DGX's memory and disk:** Qwen-Image-Edit-2511 with its text encoder is about
  60 GB.
  - Mitigation: the live run stops and restores the user's model, as before; the
    weights stay for re-tests.

## Tag

`v0.14.1` on `main`, "before the media plan, phase B", immediately before step 1
starts. It is not pushed to `github`: an anchor, not a release.

## Branch and worktree

Branch `media-b`, worktree `.claude/worktrees/media-b`. It rebases onto `main` before
the ff merge.

Steps are implemented by subagents (the same model as the main session), one step per
brief. The main session reviews each against its acceptance criteria and commits at
the step boundary. The release after the merge is the user's call.

## Phases and steps

- **Phase 1 — Protocol 7** (steps 1–3). Green at the end: both halves at protocol 7,
  every model listing its endpoints on one type, routing and refusals simplified.
  1. `STEP-1-contract.md` — the specs for all of phase B; protocol 7's schemas and
     fixtures.
  2. `STEP-2-kaiak-control-protocol-7.md` — the version, the rules, the sample, the
     guide.
  3. `STEP-3-gateway-protocol-7.md` — the version, the rules, routing and refusals,
     the endpoint memory and the endpoint-missing class removed.
- **Phase 2 — Uploads and image edits** (steps 4–5). Green at the end.
  4. `STEP-4-uploads.md` — the multipart reader, the cap, the budget, the deadline,
     the rebuilt body.
  5. `STEP-5-image-edits.md` — the endpoint, on both halves.
- **Phase 3 — Speech-to-text** (steps 6–7). Green at the end.
  6. `STEP-6-transcription.md` — the endpoint on `vllm` and `llama-server`, on both
     halves; vLLM's seconds.
  7. `STEP-7-audio-measuring.md` — WAV, FLAC and MP3 upload lengths; llama-server's
     path.
- **Phase 4 — End to end, live kit, docs, review, live run** (steps 8–11). Green at the
  end.
  8. `STEP-8-e2e.md` — gateway e2e and cross-half e2e.
  9. `STEP-9-live-kit-and-docs.md`
  10. `STEP-10-review-and-green.md`
  11. `STEP-11-live-run.md` — the DGX.

Expected reds inside phase 1:
- After step 1, both halves fail the moved fixtures and the version checks.
- Step 2 clears `kaiak-control`'s; step 3 the gateway's.
- The cross-half e2e is red from step 2 until step 3.

Each later step lands its endpoint's names in the shared schema, the backend-type
fixtures and both halves at once — and its rows in `GATEWAY.md`'s endpoint-support
and errors tables, which gateway tests read — so phases 2 and 3 stay green step by
step.

## Verification

- **Component tests:**
  - the config rules on both halves (mandatory lists, one type, the version);
  - the multipart reader: owned parts, repeats, limits, hostile bodies;
  - the rebuilt body (byte-for-byte parts, the model replaced, the length);
  - the upload cap, budget and deadline;
  - each endpoint's owned parts and refusals;
  - usage: edit images, vLLM's seconds (`usage.seconds`, `verbose_json` duration),
    llama-server's tokens and measured seconds;
  - the WAV, FLAC and MP3 readers over valid, odd and hostile files;
  - the one "not found" reading on every type: `502` at once, neither retried nor
    counted by the circuit.
- **Gateway e2e** against the fake backend: each endpoint through the whole pipeline,
  the usage records, the new limits refusing, an unlisted endpoint refused before any
  backend.
- **Cross-half e2e:** an edit and a transcription settle at the sample; the hourly
  `audio_seconds_in` total limits a second gateway.
- **`scripts/check-all.sh`** green at every phase end, and 3× in a row at the end.
- **Live (step 11):** Qwen-Image-Edit-2511 on vLLM-Omni; an ASR model on vLLM and on
  llama-server; the kit passes, the real answers match the fake's shapes, the
  measured seconds match the backend's; the user's model restored.
