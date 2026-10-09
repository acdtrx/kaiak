# Step 10 — live kit and docs

**Status:** done — reviewed and committed 2026-10-09

## Intent

- **One kit run checks a real vLLM-Omni server** for speech and one for images.
- **An operator can serve Qwen3-TTS and Qwen-Image behind kaiak** from
  `DEPLOYMENT.md` alone.

Only the kit's `-self-test` runs in the suite.

## Files likely touched

- `scripts/live/`: a `vllm-omni` kind, or flags for a speech server and an image server
  beside a run of another kind — whichever fits the kit's structure. Checks:
  - **speech:**
    - each mode answers audio;
    - usage exact for the whole clip and SSE, estimated for raw audio;
    - `audio_seconds_out` close to the clip's real length (decoded locally);
    - a compressed format refused;
    - `ref_audio` with a URL refused;
  - **batch:** usage summed, the cap;
  - **images:** an image answered, `images` counted, `n` capped, `file`, `url` and
    `lora` refused;
  - **limits:** an image limit and an audio limit refusing;
  - **endpoints:** chat to the image model is kaiak's `400 endpoint_not_served`; speech
    sent to the image model (vLLM-Omni's answer, which step 1 could not settle from
    source) is recorded;
  - the self-test against the fakes.
- `docs/testing/LIVE-BACKENDS.md`: the new kind and checks; vLLM-Omni's start commands.
- `docs/DEPLOYMENT.md`:
  - a Speech and images section: install vLLM-Omni (its own venv, the vLLM version it
    pins);
  - `vllm serve <model> --omni` for Qwen3-TTS and Qwen-Image;
  - one backend per model;
  - the formats accepted;
  - the units, limits and caps;
  - the upgrade note for format and protocol 6 (both halves together).
- `README.md`, `docs/ARCHITECTURE.md`, `docs/architecture/gateway.html`: the endpoints
  and the type.

## Decisions made during planning

- **Real audio length in the live check** is decoded on the Mac (WAV header and bytes)
  and compared with the gateway's `audio_seconds_out`, within rounding.

## Acceptance criteria

- `go -C scripts/live run . -self-test` passes, covering every new check.
- `scripts/check-all.sh` green.

## Result

### What changed

- `gateway/internal/fakebackend/cmd/fakebackend/main.go`: three flags for the media
  setters the self-test needs — `-non-speech-models LIST` (`SetNonSpeechModels`:
  vLLM-Omni's `400 TTS model did not produce audio output.`, the batch's `400`),
  `-non-image-models LIST` (`SetNonImageModels`: `503 No diffusion stage found in
  multi-stage pipeline.`), `-image-served-model NAME` (`SetImageServedModel`: `400
  Model mismatch` for another model). Their defaults change nothing. Audio length and
  image size need no flag: the fake's defaults (2.5 s, 1024 bytes) serve the kit.
- `scripts/live/`:
  - **Flags** (`main.go`): `-speech-base-url` (env `LIVE_SPEECH_BASE_URL`),
    `-speech-model` (env `LIVE_SPEECH_MODEL`), `-speech-api-key-env`,
    `-image-base-url` (env `LIVE_IMAGE_BASE_URL`), `-image-model` (env
    `LIVE_IMAGE_MODEL`), `-image-api-key-env`. Each URL and model go together; a key
    flag needs its URL and a set variable; the URLs get the note when they lack
    `/v1`. The run's header names both servers.
  - **Config** (`config.go`): `vllm-omni` backends `live-speech` and `live-images`;
    models `live-tts` (context 4096, output limit 2048 / 4096, streaming) and
    `live-image` (context 1024), priced like the others; `max_speech_batch_items` 3
    and `max_images_per_request` 2 (above every request but the cap checks'); with
    either server, a group `live-media` (`images_per_minute` 1,
    `audio_seconds_out_per_minute` 1) and its key `k-live-media`.
  - **Checks** (`media.go`, new), on the servers given, between the Responses
    checks and `endpoint-not-served`:
    - `speech-wav`, `speech-pcm` (`sample_rate` 24000), `speech-raw`
      (`stream_format: "audio"`, WAV), `speech-sse` (`stream: true`, WAV): audio of
      the right content type, measured here — the WAV's chunks walked to `data`,
      the data bytes received (at most the declared size, so a streamed WAV's
      placeholder reads), PCM by its bytes; no `x-vllm-omni-*` header reaching the
      client; SSE: deltas joined into a WAV stream, `speech.audio.done` with usage,
      last, no other event.
    - `usage-log/speech-wav`, `-pcm`, `-raw`, `-sse`: `generate_content`, `speech`,
      `gen_ai.request.stream`, not partial; tokens reported (WAV, PCM, SSE — SSE's
      equal to `done`'s) or estimated and flagged (raw); `kaiak.usage.audio.output.
      duration` equal to the length decoded here rounded up.
    - `speech-refused`: `mp3` → `400 response_format_unsupported`, a `ref_audio`
      URL → `400 external_reference_unsupported`, `pcm` without `sample_rate` →
      `400 missing_required_parameter`, each with its `param`, none sent.
    - `speech-batch` (a WAV and a PCM item) and `usage-log/batch`: both results
      successful, each item measured by its `media_type`, usage summed exactly, the
      exact lengths summed before the one rounding. `speech-batch-cap`: 4 items →
      `400 invalid_value` on `items`, not sent.
    - `image` (one 512×512 image, 10 steps): every `b64_json` decodes to a PNG, JPEG
      or WebP; a `model` member, if any, is the public name. `usage-log/image`:
      `kaiak.usage.images` equal to the answer's `data[]`, `image`, tokens
      estimated. `image-cap`: `n` 3 → `400 invalid_value` on `n`. `image-refused`:
      `response_format` `url` and `file` → `400 response_format_unsupported`, `lora`
      → `400 external_reference_unsupported`, none sent.
    - `chat-to-image`: `400 endpoint_not_served`, not sent.
    - `audio-limit`, `image-limit` (the `live-media` key): the first request served;
      after its log line (written after settlement) the second → `429` type
      `audio_seconds` / `images`, `rate_limit_exceeded`, `Retry-After`, no
      `x-ratelimit-*` header, the log line's `kaiak.limit.type`
      `audio_seconds_out_per_minute` / `images_per_minute`.
    - `image-to-speech`: an image request to `live-tts` → `502
      upstream_endpoint_missing`, the endpoint-missing warning for `images_generations`
      on `live-speech`, a speech request right after → `200`.
    - `speech-to-image`: a speech request to `live-image`, **recorded** — status,
      content type, error type, code and message (clipped), and the log line's
      `error.type`, `kaiak.upstream.error.code`/`type` and seconds; it fails only
      when no answer came. The last request sent to the image server.
    - `media-metrics`: `kaiak_usage_audio_output_duration_seconds_total` and
      `kaiak_usage_images_total` by model and output type, and
      `kaiak_limit_rejections_total` for each media limit type.
    - Without either server: one `SKIP speech, image` line; with one, a SKIP for the
      other.
  - **Existing checks**: `models` lists `live-tts` and `live-image` with the three
    media endpoints each; `endpoint-not-served` refuses `/v1/audio/speech`,
    `/v1/audio/speech/batch` and `/v1/images/generations` to `live-chat` on every
    kind — so it never skips now (llama-server's run had skipped it).
  - **Self-test** (`process.go`): the vllm run gets two vLLM-Omni fakes with bearer
    keys (`LIVE_SELF_TEST_SPEECH_KEY`, `LIVE_SELF_TEST_IMAGE_KEY`, covering the key
    flags): a speech server whose model does not draw (`-non-image-models`) and an
    image server with its served name set whose model does not speak
    (`-non-speech-models`, `-image-served-model`). The self-test clears the speech
    and image flags first, so `LIVE_SPEECH_*`/`LIVE_IMAGE_*` cannot point it at a
    real server. The fakes of a run are stopped together on every path.
- `docs/testing/LIVE-BACKENDS.md`: the scope line; the config's extra backends,
  models, caps and the `live-media` key; the cost note; a vLLM bullet; the
  self-test's vLLM-Omni fakes (and why 2.5 s clips catch per-item rounding); 22
  check rows, the media endpoints in `endpoint-not-served`'s; new section **Speech
  and images on vLLM-Omni** (flags, the venv install, both start commands with a
  GPU-sharing note, the run command, which PASS lines matter, `speech-to-image` as a
  record); five Reading failures notes (PCM refused by a model, seconds not matching,
  estimated whole clips, the image model mismatch, `image-to-speech` answering
  otherwise).
- `docs/DEPLOYMENT.md`:
  - Clients: the media endpoints on the API port; a vLLM-Omni model reachable
    through them only; vLLM-Omni's image `503` among the endpoint-missing causes.
  - Config for many hosts: `vllm-omni` in the types; the two caps in the slot
    paragraph and the caps bullet; five counters per scope (the example: 35 000);
    `vllm-omni` among the types passing `service_tier` (as GATEWAY.md says).
  - New section **Speech and images** (after Rerankers): the endpoints and the type
    serving them alone (and why); one backend per model and the readings; what the
    gateway accepts (WAV/PCM, PCM's `sample_rate` and which models take one, inline
    reference audio and its body size, `b64_json` only, `lora`, the caps against
    `tts_batch_max_items`); timeouts; the output limit and Qwen3-TTS Base's cost;
    usage; limits (reservations, overshoot, applicability, the `429` shape); where
    the units show (records, request line, metrics, the sample's page); installing
    vLLM-Omni; serving Qwen3-TTS and Qwen-Image; the config; the live kit before
    go-live.
  - Observability: the cardinality formula's media counter; the request line's media
    fields. Starter alerts: the image model mismatch and an image model on a speech
    server named in the two rows they show in.
  - Upgrades: **Next release** (protocol 6, format 6) — both halves together and
    `format_version` 6; the new endpoints and type; the optional fields; five
    counters per scope; the new error codes and readings; the new metrics, label and
    request-line fields (no starter alert changes); host apps on `kaiak-control`.
    "Config format 5 (this release, …)" now names its release, 0.11.0.
- `README.md`: the intro (rerank, speech and images; vLLM-Omni; images and seconds
  counted); status at version 6; the doc map's live-test row; Client APIs, Limits and
  Usage in Concepts; quick start step 3's hosts (the two vLLM-Omni ones) and types,
  rewrapped.
- `docs/ARCHITECTURE.md`: the mermaid client and backend boxes; the request-path
  bullet; `accounting` (the media formats, audio measured, images counted);
  `limits` (the hour counts); `fakebackend` and its command's flags; the live kit's
  paragraph (every kind, the servers beside the run).
- `docs/architecture/gateway.html`: the listener's client APIs; Inbound's formats and
  refusals; the limits paragraph (five counters) and window table (media rows);
  reservations and units bullets; the plug-points figure's client box (Speech,
  Images; the legend moved down); the seams table (① formats, ③ `vllm-omni`, ④ the
  media units and next plugs, ⑦ the media limit types); the client API × backend
  figure — a vLLM-Omni column and a Speech · Images row (rendered and checked in
  Chrome) — its label and caption; the footer.
- `docs/architecture/control-plane.html`: `Kaiak-Protocol: 6` (both places); the
  footer.
- Endpoint lists checked with `git grep` (`responses/input_tokens`, `v1/rerank`,
  `/v1/embeddings`, "client APIs", version 5 mentions, type lists): `docs/kaiak.md`
  and the specs already list the media endpoints; nothing else listed the gateway's
  endpoints.

### Design choices

- **Flags beside a run of any kind, not a `vllm-omni` kind.** A vLLM-Omni server
  serves one model and the media endpoints alone, so a kind of its own would have no
  chat through which the kit's core checks run (auth, models, ceiling, rate limit,
  metrics, the drain) — the check sequence would need a second shape. The kit
  already adds servers beside the chat run (embeddings, reranker); one run checks the
  chat server and both vLLM-Omni servers together, and either can come alone. The
  backends are `vllm-omni` whatever `-kind` is, as the embeddings backend is
  `openai-compatible`.
- **Audio measured on this machine, compared exactly.** The kit's own WAV walk
  (separate code from the gateway's: the kit imports nothing of it) and byte counts;
  exact lengths as fractions (`math/big`), a batch's summed before the one rounding;
  the check is equality with the exact length rounded up, which is what "within
  rounding" means for a count rounded once. The self-test's 2.5 s clips and two-item
  batch tell the roundings apart (3 s per clip; 5 s for the batch, 6 if rounded per
  item).
- **The limit checks wait for the first request's log line**: audio generated
  reserves nothing, so a second clip sent before the first settled would be
  admitted.
- **`image-to-speech` and `media-metrics` beyond the brief**: the first is the live
  check of the image `503` reading step 1's notes asked for (step 12 confirms its
  text); the second shows the media counters and refusals in `/metrics` as the
  chat run's `metrics` does for tokens.
- **Cheap requests**: one sentence per clip; 512×512 images at 10 steps (the checks
  need an image, not a good one); no `voice` (the model's default), PCM at 24 kHz
  (Qwen3-TTS's and Breeze-TTS-2's rate). No `-speech-params`/`-image-params`
  flags: nothing the step-12 models need.
- **The media model metadata**: `live-tts` context 4096 — the talker's
  `max_model_len` in vLLM-Omni's bundled `qwen3_tts.yaml` — with vLLM-Omni's
  `max_new_tokens` default and bound as its output limit; `live-image` context 1024,
  Qwen-Image's prompt bound.
- **The self-test runs the media checks on the vllm kind only**: they do not depend
  on the run's kind, and every kind would cost time without covering more.

### Sources verified (2026-10-09)

- **vLLM-Omni v0.30.0** — the latest release (`v0.31.0rc1` is a pre-release), tag
  commit `a8576ccb725c4e21cd13c3eb5f9a546b21149d2b`, read with `gh api
  repos/vllm-project/vllm-omni/contents/<path>?ref=v0.30.0`:
  - `docs/serving/speech_api.md`: `vllm serve <model> --omni`, one model per
    server; Qwen3-TTS CustomVoice's command (`--deploy-config
    vllm_omni/deploy/qwen3_tts.yaml --omni --port 8091 --trust-remote-code
    --enforce-eager`), its checkpoints (1.7B and 0.6B CustomVoice and Base,
    VoiceDesign), `voice` default `vivian`, `sample_rate` 8000 or 24000, native
    24 kHz; usage headers absent on diffusion-mode speech servers; the batch's 1–32
    items and `tts_batch_max_items` (engine kwarg, default 32); `--stage-overrides`.
  - `docs/serving/image_generation_api.md`: `vllm serve Qwen/Qwen-Image --omni
    --port 8000`; `n` 1–10; `response_format` `b64_json` or `file`; `size`,
    `negative_prompt`, `num_inference_steps`, `guidance_scale`, `true_cfg_scale`,
    `seed`, the model's defaults when absent.
  - `README.md` (it points to the docs for installing),
    `docs/getting_started/quickstart.md`, `installation/gpu.md`,
    `installation/gpu/cuda.inc.md`, `installation/python_env_setup.inc.md`: Linux,
    Python 3.12; `uv venv --python 3.12 --seed`; `uv pip install vllm==0.30.0
    --torch-backend=auto`; the same vLLM major and minor (a warning on import
    otherwise); vLLM's CLI hands `--omni` to vLLM-Omni; the `vllm/vllm-omni` image
    with no default entrypoint. `requirements/common.txt` names vLLM 0.30.0.
  - The released wheel (`vllm_omni-0.30.0-py3-none-any.whl` on PyPI, uploaded
    2026-09-25; listed, its files grepped, nothing run): it ships
    `vllm_omni/deploy/*.yaml`; `model_executor/models/qwen3_tts/pipeline.py` makes
    `qwen3_tts.yaml` Qwen3-TTS's default deploy config, so a pip install needs no
    `--deploy-config`; `qwen3_tts.yaml` gives each of its two stages 0.3 of GPU 0's
    memory and the talker `max_model_len` 4096; `pipeline_qwen_image.py`
    `tokenizer_max_length` 1024; `tts_adapters/qwen3_tts.py` rates {8000, 24000};
    `omni_base.py` reads `tts_batch_max_items` (default 32) from engine kwargs, no
    flag.
  - PyPI has `vllm-omni` 0.30.0 (the install docs' "until 0.30.0 artifacts are
    published" note is stale); Docker Hub has `vllm/vllm-omni:v0.30.0` (2026-09-27).

### Notes for review

- **`examples/config.json`'s `qwen3-tts` context** is 32768 (the checkpoint's
  `max_position_embeddings`, step 2), while vLLM-Omni runs the talker with
  `max_model_len` 4096 in its bundled deploy config; the kit declares 4096. Both
  hold the 4096 ceiling; not changed.
- **Not built**: a live check of the image `400 Model mismatch` text (step 12 asks
  for it: a deployment naming another model, or `curl` by hand); request-parameter
  flags for speech and images.

### Mutation checks (each reverted)

- The gateway rounding seconds down: `usage-log/speech-wav`, `-pcm`, `-raw`, `-sse`
  fail (`2 s, want 3`); `usage-log/batch` passes, its 5 s being exact.
- The gateway passing `lora`: `image-refused` fails (`200` from the fake).
- Flag errors, by hand: `-speech-base-url` without `-speech-model`,
  `-image-api-key-env` without `-image-base-url`, an unset key variable — each a
  one-line `live:` error before anything starts.

### Suite (2026-10-09)

- `go -C scripts/live run . -self-test`: passed for vllm (59 passed, 0 skipped),
  llama-server (37 / 2 skipped: `chat-to-reranker`, `speech, image`), openai 22 / 1,
  azure-openai 21 / 1, anthropic 16 / 3, azure-anthropic 16 / 3, vllm with two
  backends 34 / 2 — 0 failed anywhere.
- `scripts/check-gateway.sh` (2 m 06 s): gofmt, vet, staticcheck 2026.2.1 on go1.27.1,
  telemetry boundary; `go test -race -count=1 ./...` all 20 packages `ok` (`e2e`
  121.1 s); the kit's gofmt, vet, staticcheck and self-test as above; `gateway checks
  passed`, exit 0.
- `scripts/check-all.sh`, run 1 (3 m 37 s): the gateway stages as above (`e2e`
  119.7 s), the self-test passed for every kind, `gateway checks passed`; control
  `npm test`: tests 655, pass 654, fail 0, skipped 1; `npm run lint`: `boundaries
  ok`; cross-half `ok kaiak/e2e 75.5s`; `all checks passed`, exit 0.
- `scripts/check-all.sh`, run 2 (3 m 38 s, after this record was written): every
  gateway package `ok` (`e2e` 124.0 s), the self-test passed for every kind, `gateway
  checks passed`; control tests 655, pass 654, fail 0, skipped 1; `boundaries ok`;
  cross-half `ok kaiak/e2e 75.9s`; `all checks passed`, exit 0.
