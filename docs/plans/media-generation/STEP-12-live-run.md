# Step 12 — live run

**Status:** done — reviewed and committed 2026-10-10

## Intent

The real vLLM-Omni server confirms what steps 1–10 built from its source: the speech
modes, the batch, image generation, their usage and units, the refusals, and the
`DEPLOYMENT.md` setup. Then the branch merges.

## Files likely touched

- This file: commands, versions, model revisions, the kit's output, captured real
  answers (no long base64).
- Fixes for anything the run shows, each with its test.
- `docs/plans/media-generation/OVERVIEW.md`: the verification status.

## Decisions made during planning

- **On the DGX:**
  - vLLM-Omni at its latest release, in a venv of its own (the existing vLLM 0.30.0
    venv stays untouched);
  - Qwen3-TTS (CustomVoice) and Qwen-Image.
- **The DGX rules of the rerank run apply:**
  - record `cria status --json` and `vllm.sh status` first;
  - stop the user's model only as needed;
  - restore and verify it at the end;
  - no sudo, no changes to cria's or vLLM's config;
  - keep the new venv and the downloaded weights, listed with sizes.

## Acceptance criteria

- The kit's speech and image checks pass against the real server.
- The real answers match the fake backend's shapes in every field the gateway reads.
  Otherwise the fake is fixed first.
- `scripts/check-all.sh` green after any fix.
- The user's model is restored and verified.
- The branch rebases onto `main` and merges ff, after the user's go.

## Result

### Starting state on the DGX (2026-10-09 21:58 UTC, read-only)

- `cria status --json` (exit 0): one server, `qwen38-flash` (llama, build
  `~/opt/llama.cpp/build-spark`, `llama-server --version` build 11538, commit
  `79e2e74eb`), picks `cache=q8 cache_ram=0 context=512k mtp=3 slots=2 vision=on`,
  `0.0.0.0:11434`, PID 787003, launched 2026-10-09T20:59:03Z, health 200; both
  slots idle (`/slots`, 262144 each). No broken entries, no router.
- `~/spark-setup/vllm.sh status`: `stopped` (exit 1).
- `nvidia-smi`: GB10, driver 595.91.07, CUDA 13.2; one compute process, the Flash
  `llama-server` (96,261 MiB).
- `free -h`: 121 GiB total, 102 GiB used, 19 GiB available; swap 90 MiB of 8 GiB used.
- Listening: 22 (ssh), 53 (resolver, loopback), 11434 (`llama-server`, PID 787003).
- Restore target: `cria start qwen38-flash cache=q8 context=512k mtp=3 vision=on
  slots=2 cache_ram=0 --wait`.

The Flash server was idle; the user had said the DGX was free for this run. It was
stopped through cria (`cria stop qwen38-flash`, 21:59:48 UTC) for the run and
restored at the end.

### Versions and models

- **vLLM-Omni 0.30.0** on **vLLM 0.30.0**, the prepared venv
  `~/opt/vllm-omni-kaiak/.venv` (its `README.md`: CPython 3.13.15, PyTorch
  2.13.0+cu130, transformers 5.14.1, diffusers 0.40.0), unchanged.
- **Speech:** `Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice`, revision
  `0c0e3051f131929182e2c023b9537f8b1c68adfe` (4.3 GiB), served under that name,
  `max_model_len` 4096 (`/v1/models`).
- **Images:** `Qwen/Qwen-Image`, revision `75e0b4be04f60ec59a75f475837eced720f823b6`
  (54 GiB, BF16).
- Both from the Hugging Face cache, served with `HF_HUB_OFFLINE=1` (an addition to the
  README's commands: it keeps the pinned revisions; no download).

### Commands

On the DGX, from `~/opt/vllm-omni-kaiak`, the README's commands (logs to a scratch
folder instead of `logs/`), TTS first:

```sh
OMNI=.venv/lib/python3.13/site-packages/vllm_omni
HF_HUB_OFFLINE=1 .venv/bin/vllm serve Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice --omni \
  --deploy-config $OMNI/deploy/qwen3_tts.yaml \
  --host 0.0.0.0 --port 8091 --trust-remote-code --enforce-eager \
  --stage-overrides '{"0":{"gpu_memory_utilization":0.1},"1":{"gpu_memory_utilization":0.1}}'
HF_HUB_OFFLINE=1 .venv/bin/vllm serve Qwen/Qwen-Image --omni \
  --host 0.0.0.0 --port 8092 --gpu-memory-utilization 0.6
```

**TTS startup** (21:59:57 → healthy 22:01:34, 97 s): two stages,
`Qwen3TTSTalkerForConditionalGeneration` (stage 0) and `Qwen3TTSCode2Wav` (stage 1).
Stage 0: weights 3.91 GiB in 25 s; "Free memory on device (116.52/121.69 GiB) on
startup. Desired GPU memory utilization is (0.1, 12.17 GiB). Actual usage is 4.7 GiB";
KV cache 7.5 GiB, 70,224 tokens (17× a 4096-token request). Stage 1: 0.22 GiB, no KV
cache. `nvidia-smi`: stage 0 12,036 MiB, stage 1 513 MiB; `free -h` 20 GiB used,
101 GiB available. It registers every vLLM-Omni route — speech, the speech batch,
voices, image generation and edits, chat, completions, Messages, Responses, videos,
realtime — whatever the model.

**Qwen-Image startup** (22:02:25 → healthy 22:09:11, 6 min 46 s, nearly all of it
reading the weights from disk): weights "took 269.43 seconds", "Model loading took
52.4801 GiB and 389.675225 seconds", "Process-scoped GPU memory after model loading:
52.69 GiB"; attention backend `CUDNN_ATTN` (the platform default on sm_121); the
transformer set for lazy regional `torch.compile`, which cost nothing visible (the
first 512×512 request took 7 s). No `[admission]` or `Requested memory` line at this
version. `nvidia-smi`: the diffusion worker 55,258 MiB after loading, 58,954 MiB after
a 1024×1024 generation; the answer's `metrics.peak_memory_mb` 55,034 at 512×512.
`free -h` with both up: 72 GiB used, 49 GiB available; swap rose from 90 MiB to
4.7 GiB during the load (page cache pressure), back to 99 MiB once everything stopped.
`/v1/models`: `Qwen/Qwen-Image`, `max_model_len` null.

**Chat model for the kit's core checks:** cria's `qwen35-2b-a` (vLLM 0.30.0,
`Qwen/Qwen3.5-2B` revision `15852e8c…`, served as `qwen3.5-2b`, port 8001,
`--gpu-memory-utilization 0.15`), as in the rerank run — it fit beside both:
`~/.local/bin/cria start qwen35-2b-a --wait`, running after 57 s, `VLLM::EngineCore`
18,192 MiB. With all three up: 98 GiB used, 23 GiB available.

On the Mac, from the worktree (run twice; the second without `-request-timeout` and
`-keep`):

```sh
go -C scripts/live run . -kind vllm -base-url http://dgx.local:8001/v1 -model qwen3.5-2b -max-output 4096 \
  -speech-base-url http://dgx.local:8091/v1 -speech-model Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice \
  -image-base-url http://dgx.local:8092/v1 -image-model Qwen/Qwen-Image \
  -request-timeout 5m -keep
```

### Kit output: 50 passed, 0 failed, 4 skipped (both runs)

The first run (22:12:19–22:13:54 UTC):

```
== vllm: http://dgx.local:8001/v1, model qwen3.5-2b, speech Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice on http://dgx.local:8091/v1, images Qwen/Qwen-Image on http://dgx.local:8092/v1
  PASS  auth-reject          no key → 401
  PASS  models               lists live-capped, live-chat, live-image, live-rpm, live-tts; endpoints chat_completions, completions, embeddings, messages, messages_count_tokens, rerank, responses; live-tts and live-image: audio_speech, audio_speech_batch, images_generations
  PASS  chat                 "A lighthouse is for guiding ships with their lights toward s…" (finish stop, 24+14 tokens)
  PASS  usage-log/chat       in 24 (cache read 0, cache write 0), out 14 (reasoning 0), cost 5.2e-05
  PASS  chat-stream          no usage chunk, 18 chunks, [DONE], "Lighthouses are structures built to guide ships safe out of …"
  PASS  usage-log/stream     in 24 (cache read 0, cache write 0), out 17 (reasoning 0), cost 5.8e-05
  PASS  chat-stream-usage    usage chunk relayed, 32 chunks, [DONE], "A lighthouse is a beacon used to guide ships and boats safel…"
  PASS  usage-log/stream-usg in 24 (cache read 0, cache write 0), out 30 (reasoning 0), cost 8.4e-05
  SKIP  embeddings           no -embeddings-model
  SKIP  usage-log/embeddings no -embeddings-model
  SKIP  rerank               no -rerank-base-url
  PASS  rerank-to-chat       rerank → 502 upstream_endpoint_missing, warned for live; a chat right after → 200
  PASS  messages             "A lighthouse serves as a navigational beacon to guide ships …" (stop end_turn, 24+18 tokens), max_tokens set by the gateway
  PASS  usage-log/messages   in 24 (cache read 0, cache write 0), out 18 (reasoning 0), cost 6e-05
  PASS  messages-stream      29 events, message_start … message_stop, "Lighthouses serve as navigational beacons that guide ships s…"
  PASS  usage-log/msg-stream in 24 (cache read 0, cache write 0), out 25 (reasoning 0), cost 7.4e-05
  SKIP  messages-cache       the server reported no cache read: its prefix cache is off or does not report it
  PASS  messages-count       24 input tokens, no usage record
  PASS  messages-models      Anthropic's shape: live-capped, live-chat, live-rpm
  PASS  messages-errors      wrong key → 401 authentication_error, unknown model → 404 not_found_error
  PASS  messages-hosted-tool web_search_20250305 → 400 hosted_tool_unsupported, never sent
  PASS  price-options        speed "fast" passed through: vllm prices nothing
  PASS  responses            "A lighthouse serves as a beacon of guidance that enables shi…" (status completed, 24+30 tokens)
  PASS  usage-log/responses  in 24 (cache read 0, cache write 0), out 30 (reasoning 0), cost 8.4e-05
  PASS  responses-stream     28 events, response.created … response.completed, "A lighthouse serves as a navigational beacon to illuminate t…"
  PASS  usage-log/resp-stream in 24 (cache read 0, cache write 0), out 21 (reasoning 0), cost 6.6e-05
  PASS  responses-stateful   previous_response_id → 400 stateful_responses_unsupported, never sent
  PASS  responses-hosted-tool web_search → 400 hosted_tool_unsupported, never sent
  PASS  speech-wav           audio/wav, WAV 24000 Hz, 1 channel(s), 16-bit, 3.28 s; no x-vllm-omni-* header
  PASS  usage-log/speech-wav 4 s of audio, as measured here; in 12, out 42, estimated false, cost 9.6e-05
  PASS  speech-pcm           audio/pcm, PCM 24000 Hz, 16-bit mono, 4.32 s; no x-vllm-omni-* header
  PASS  usage-log/speech-pcm 5 s of audio, as measured here; in 12, out 55, estimated false, cost 0.000122
  PASS  speech-raw           raw-audio stream, WAV 24000 Hz, 1 channel(s), 16-bit, 3.44 s
  PASS  usage-log/speech-raw 4 s of audio, as measured here; in 34, out 0, estimated true, cost 3.4e-05
  PASS  speech-sse           4 deltas, WAV 24000 Hz, 1 channel(s), 16-bit, 3.36 s; done with 12+43 tokens
  PASS  usage-log/speech-sse 4 s of audio, as measured here; in 12, out 43, estimated false, cost 9.8e-05
  PASS  speech-refused       mp3 → 400 response_format_unsupported; ref_audio URL → 400 external_reference_unsupported; pcm without sample_rate → 400 missing_required_parameter; none sent
  PASS  speech-batch         WAV 24000 Hz, 1 channel(s), 16-bit, 3.84 s; PCM 24000 Hz, 16-bit mono, 2.00 s; 5.84 s and 20+75 tokens in all
  PASS  usage-log/batch      6 s of audio, as measured here; in 20, out 75, estimated false, cost 0.00017
  PASS  speech-batch-cap     4 items over max_speech_batch_items 3 → 400 invalid_value; none sent
  PASS  image                1 image(s): PNG of 787271 bytes
  PASS  usage-log/image      1 image(s); in 33, out 0, estimated true, cost 3.3e-05
  PASS  image-cap            n 3 over max_images_per_request 2 → 400 invalid_value; none sent
  PASS  image-refused        response_format url → 400 response_format_unsupported; response_format file → 400 response_format_unsupported; lora → 400 external_reference_unsupported; extra_params → 400 external_reference_unsupported; none sent
  PASS  chat-to-image        chat to live-image → 400 endpoint_not_served; none sent
  PASS  audio-limit          first served, counted 5 s of audio; second → 429 audio_seconds, audio_seconds_out_per_minute 1, Retry-After 60
  PASS  image-limit          first served, counted 1 image(s); second → 429 images, images_per_minute 1, Retry-After 60
  PASS  image-to-speech      image → 502 upstream_endpoint_missing, warned for live-speech; a speech right after → 200
  PASS  speech-to-image      recorded: status 400, content type "application/json", error type BadRequestError, code 400: "TTS model did not produce audio output.", error.type=upstream_client_error, kaiak.upstream.error.code=400, kaiak.upstream.error.type=BadRequestError
  PASS  media-metrics        32 seconds of audio, 1 audio limit refusals, 2 images, 1 image limit refusals
  PASS  endpoint-not-served  /v1/responses/input_tokens, /v1/audio/speech, /v1/audio/speech/batch, /v1/images/generations → 400 endpoint_not_served
  PASS  output-ceiling       max_tokens 32768 → 16 tokens, finish length
  PASS  rate-limit           second request → 429, Retry-After 60, reset 1m0s
  PASS  metrics              live-chat: 12 usage records, 229 output tokens, 0.020572 USD; 3 rate-limited; 23 successful upstream attempts
  50 passed, 0 failed, 4 skipped
```

The second run (22:14:18–22:15:49 UTC, the default 2-minute request timeout): exit 0,
the same 54 lines, 50 passed, 0 failed, 4 skipped; its clips 3.52 s (WAV), 3.44 s
(PCM), 3.52 s (raw), 3.68 s (SSE, 12+47 tokens), the batch 4.40 + 2.32 s → 7 s,
each counted as measured. The skips are by design: no `-embeddings-model`, no
`-rerank-base-url`, vLLM's prefix cache reports no reads.

The kept gateway log (first run) shows each media record: `kaiak.backend.type`
`vllm-omni`, `gen_ai.operation.name` `generate_content`, `kaiak.usage.partial` false;
the audio limit's refusal `kaiak.limit.used` 5 against 1; `live-image-to-speech` → the
warning `the deployment's server does not serve an endpoint its type serves` and
`kaiak.upstream.error.message` "upstream_endpoint_missing: backend live-speech
answered 503: its server does not serve http://dgx.local:8091/v1/images/generations"
(0 tokens, `partial` true); `live-speech-to-image` → `upstream_client_error`, 0
tokens, cost 0, 57 s after the previous line. No `ERROR` or traceback in either
server's log; the servers' access logs: TTS 16 speech and 4 batch `200`, 3 image
`503`; image 5 generations `200`, 1 `400` (model mismatch), 3 speech and 1 batch
`400`.

### Real answers, sent with curl straight to the servers

Speech: `{"model": "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice", "input": "Hello, how are
you?", "voice": "vivian", "language": "English", …}`.

- **Whole clip, WAV** (`200`, 9.0 s — the server's first request): headers (as sent,
  lower case)

  ```
  x-vllm-omni-input-tokens: 6
  x-vllm-omni-output-tokens: 27
  x-vllm-omni-total-tokens: 33
  x-vllm-omni-input-text-tokens: 6
  x-vllm-omni-input-audio-tokens: 0
  content-length: 99884
  content-type: audio/wav
  ```

  A 44-byte header with the real sizes (RIFF 99,876, data 99,840), PCM tag 1, mono,
  24,000 Hz, byte rate 48,000, block 2, 16-bit: 2.08 s.
- **Whole clip, PCM, `sample_rate` 24000** (`200`): the same five headers (6+23
  tokens), `content-type: audio/pcm`, 84,480 bytes, no header: 1.76 s.
- **SSE stream, `stream: true`, WAV** (`200`, `content-type: text/event-stream;
  charset=utf-8`, chunked): each event `event: <type>\ndata: <json>\n\n`; no `[DONE]`.

  ```
  event: speech.audio.delta  data: {"type":"speech.audio.delta","audio":"UklGRv////9XQVZF…","response_format":"wav"}   (44 bytes: the WAV header, both sizes 0xFFFFFFFF)
  event: speech.audio.delta  data: {…, 3,840 bytes of audio}
  event: speech.audio.delta  data: {…, 61,440 bytes of audio}
  event: speech.audio.done   data: {"type":"speech.audio.done","usage":{"input_tokens":6,"output_tokens":18,"total_tokens":24,"input_token_details":{"text_tokens":6,"audio_tokens":0}}}
  ```

  Joined: a streamed WAV, 65,280 data bytes, 1.36 s.
- **Raw-audio stream, `stream_format: "audio"`, WAV** (`200`, `content-type:
  audio/wav`, chunked, no usage headers): a 44-byte header with RIFF and data sizes
  `0xFFFFFFFF`, then 76,800 bytes: 1.60 s.
- **Batch of 2** (`200`, `application/json`): an item of the batch's WAV and one with
  its own `pcm`/`sample_rate` 24000

  ```json
  {"id":"speech-batch-b4984cabd2077950",
   "results":[{"index":0,"status":"success","audio_data":"<base64, 119,084 bytes: WAV with real sizes, 2.48 s>","media_type":"audio/wav",
               "usage":{"input_tokens":8,"output_tokens":32,"total_tokens":40,"input_token_details":{"text_tokens":8,"audio_tokens":0}}},
              {"index":1,"status":"success","audio_data":"<base64, 115,200 bytes of PCM, 2.40 s>","media_type":"audio/pcm",
               "usage":{"input_tokens":8,"output_tokens":31,"total_tokens":39,"input_token_details":{"text_tokens":8,"audio_tokens":0}}}],
   "total":2,"succeeded":2,"failed":0}
  ```

- **Batch with failed items** (`200`): an item with `voice: "nobody"` →
  `{"index":0,"status":"error","error":"Invalid voice 'nobody'. Supported: aiden, dylan,
  eric, ono_anna, ryan, serena, sohee, uncle_fu, vivian"}`; an item asking `mp3`
  (sent straight, not through the gateway) → success, `media_type` `audio/mpeg`;
  `"total":2,"succeeded":1,"failed":1`.
- **Image, `n` 1** (`{"model": "Qwen/Qwen-Image", "prompt": "A lighthouse on a rocky
  coast at dusk.", "n": 1, "size": "512x512", "num_inference_steps": 10, "seed": 7}`;
  `200`, 7.0 s, `content-length: 1049960`, `application/json`):

  ```json
  {"created":1791583770,"data":[{"b64_json":"<1,049,696 chars: a 512×512 PNG of 787,271 bytes>","url":null,"revised_prompt":null}],
   "output_format":"png","size":"512x512","cot_output":null,
   "metrics":{"stage_durations":{"queue_wait_ms":0.32,"stage_0_gen_ms":6854.6},"peak_memory_mb":55034.0}}
  ```

  The picture is a lighthouse on a rocky coast at dusk.

### Cross-endpoint answers and the model mismatch

- **Speech sent to the image model** (`POST :8092/v1/audio/speech`, model
  `Qwen/Qwen-Image`): **`400`**, after **62 s**:

  ```json
  {"error": {"message": "TTS model did not produce audio output.", "type": "BadRequestError", "param": null, "code": 400}}
  ```

  The server logs `Diffusion TTS speech request …`, `Invalid output modality: audio,
  ignoring it`, then **generates a whole image at the pipeline's defaults**
  (`image_pixels` 1,048,576 — 1024×1024 — `diffusion_engine_exec_time_s` 61.97)
  before `_create_diffusion_speech`'s `ValueError` turns it into `400`. Through the
  gateway (`speech-to-image`): relayed `400`, `upstream_client_error`, neutral for
  the circuit; the record counts 0 tokens, 0 seconds, cost 0.
- **The speech batch sent to the image model:** `400` at once:
  `{"error":{"message":"Batch speech is not supported in diffusion mode","type":"Bad
  Request","param":null,"code":400}}`.
- **Image sent to the speech model** (`POST :8091/v1/images/generations`): **`503`**
  at once:
  `{"error":{"message":"No diffusion stage found in multi-stage pipeline.","type":"Service
  Unavailable","param":null,"code":503}}` — read by the gateway as
  `502 upstream_endpoint_missing`, warned, and the deployment kept serving speech
  (`image-to-speech`).
- **Model mismatch** (`POST :8092/v1/images/generations`, model `qwen-image`): `400`
  at once:
  `{"error":{"message":"Model mismatch: request specifies 'qwen-image' but server is
  running 'Qwen/Qwen-Image'.","type":"Bad Request","param":null,"code":400}}`.

### Against the fake backend and the spec

Every field the gateway or the kit reads matches the fake
(`fakebackend/speech.go`, `speech_batch.go`, `images.go`) and `GATEWAY.md`:

- **Usage headers:** the same five `x-vllm-omni-*` names, plain integers; vLLM-Omni
  sends them lower case, the fake upper case (its comment says names are
  case-insensitive; the gateway reads them through `http.Header`). Only on the whole
  clip; none on the raw-audio stream.
- **WAV:** the whole clip's header carries the real sizes; the raw-audio stream's and
  the SSE stream's carry `0xFFFFFFFF` in both size fields, 44 bytes, 24 kHz mono
  16-bit — as `WAVHeader` writes them.
- **SSE:** `event:` + `data:` lines; the WAV header a delta of its own, first; deltas
  `{type, audio, response_format}`; `speech.audio.done` last, its `usage`
  `{input_tokens, output_tokens, total_tokens, input_token_details: {text_tokens,
  audio_tokens}}`; no `[DONE]`.
- **Batch:** top-level members `id, results, total, succeeded, failed`, in that
  order; a success `index, status, audio_data, media_type, usage`, in that order
  (`audio_data` before `media_type`, as the fake); a failure `index, status, error`
  with `error` a string.
- **Images:** `created, data[{b64_json, url: null, revised_prompt: null}],
  output_format, size, cot_output, metrics`; no `model`, no `usage`.
- **Errors:** the speech-to-image `400` (`BadRequestError`, "TTS model did not
  produce audio output."), the batch's `400` (`Bad Request`, "Batch speech is not
  supported in diffusion mode"), the image `503` (`Service Unavailable`, "No
  diffusion stage found in multi-stage pipeline.") and the `400` model mismatch
  (`Bad Request`, the same wording) are the fake's status, type and message
  exactly.
- **Audio length:** in every mode the gateway's `audio_seconds_out` equals the audio
  decoded on the Mac, rounded up (kit, both runs).

Cosmetic differences, nothing reads them, no fix made:
- the SSE delta's members in the order `type, audio, response_format` (the fake's
  `json.Marshal` of a map sorts them: `audio, response_format, type`); the gateway
  and the kit decode the event whole;
- the image answer's `metrics` holds `stage_durations` and `peak_memory_mb` (the
  fake's `{}`);
- the speech-to-image error body is written with spaces after `:` and `,` (Python's
  `json.dumps`), the others compact;
- the batch `id` is `speech-batch-<hex>` (the fake's `speech-batch-fake`).

**No mismatch, no failing test written.**

**For the main session — two open points, not mismatches:**
- **`GATEWAY.md` → Providers (vLLM-Omni)** says what speech answers on an image
  model "the source does not decide; the live run records it": the answer is `400`
  `BadRequestError` "TTS model did not produce audio output." (relayed, neutral), as
  `SetNonSpeechModels` scripts it. The spec line can now say so.
- **A speech request to an image model costs a whole image generation and counts
  nothing.** vLLM-Omni renders a 1024×1024 image at the pipeline's defaults (62 s
  on the GB10) before answering `400`; the gateway settles 0 tokens, 0 images, 0
  seconds, cost 0, and no image limit sees it (its output type is speech). A key
  allowed an image model can keep its server busy within its request limits. The
  batch (`400` at once) and image-to-speech (`503` at once) cost nothing. Whether
  to note it, or refuse speech to a model the gateway knows draws images, is the
  main session's call.

### Restoration (2026-10-09 22:16–22:21 UTC)

- vLLM-Omni servers stopped with `SIGTERM` to their API processes (TTS 792872,
  image 794007): both shut down cleanly ("Application shutdown complete"), their stage
  and diffusion workers gone. cria's chat entry stopped through cria:
  `cria stop qwen35-2b-a`. Then: `cria status --json` no servers, the GPU held no
  process, listening 22 and 53 only, 118 GiB available.
- `~/.local/bin/cria start qwen38-flash cache=q8 context=512k mtp=3 vision=on slots=2
  cache_ram=0 --wait` → running after 1 min 9 s (the page cache was cold after
  Qwen-Image), PID 797852.
- Checked: `cria status --json` (exit 0) — `qwen38-flash`, the same picks,
  `0.0.0.0:11434`, running, health 200, its command line identical to the starting
  one (compared element by element), no broken entries, no router; `/health`
  `{"status":"ok"}`; `/v1/models` lists `unsloth/Qwen3.8-Flash-Next-GGUF:UD-Q4_K_XL`
  (262144); `/slots` two idle slots of 262144; a chat "What is 17*19? Answer with the
  number only." → `323` (finish `stop`); `nvidia-smi` one compute process, this
  `llama-server` (96,241 MiB); `vllm.sh status` `stopped` (exit 1); listening 22, 53,
  11434 only. No other vLLM, vLLM-Omni or Python process on the DGX; no kit, gateway
  or fake process and no `kaiak-live-*` folder left on the Mac.

### Left on the DGX

- `~/opt/vllm-omni-kaiak` as prepared — venv, `README.md`, its logs — unchanged; the
  run's server logs went to a scratch folder.
- The models in the Hugging Face cache: `models--Qwen--Qwen3-TTS-12Hz-1.7B-CustomVoice`
  4.3 GiB, `models--Qwen--Qwen-Image` 54 GiB (revisions above).
- Compile caches the run added beside the existing ones (they speed a re-run): 32
  entries in `~/.triton/cache` (6 MiB), two `vllm_omni-…-Qwen3TTS…json` files in
  `~/.cache/vllm/modelinfos` (under 1 KiB each).
- Removed: the scratch folder `~/kaiak-step12` (server logs, the start script, the
  status snapshots); the run's `/tmp` leftovers — 18 entries (vLLM and vLLM-Omni IPC
  sockets, `vllm_dist_*` files, `tmp*` folders, the two model locks,
  `vllm_omni_device_0_init.lock`) and the 29 MB the diffusion's compile wrote into the
  empty `/tmp/torchinductor_acdtrx`; the empty `~/.cache/vllm-omni/speakers` the
  servers created. The pre-existing `/tmp/…Qwen-Qwen3.5-2B.lock` stays (cria's vLLM
  touched it). cria's config, `~/spark-setup/vllm/config.json` and the existing venvs
  and builds are unchanged.
