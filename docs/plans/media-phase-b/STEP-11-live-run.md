# Step 11 — the live run on the DGX

**Status:** live run done 2026-10-12 — the kit passes on all three servers (twice each),
the real answers match the fake's shapes in every field the gateway reads, the seconds
counted are right, the user's model restored and verified. One finding, fixed
(cf2065e): llama-server's transcription answer reports its prompt-cache reads, which
the gateway counted as plain input (Result → Against the fake backend and the spec).

## Intent

Run the live-test kit against real servers on the DGX:
- Qwen-Image-Edit on vLLM-Omni;
- an ASR model on vLLM;
- an ASR model on llama-server.

Confirm that their real answers match the fake backend's shapes in every field the
gateway reads, and that the measured seconds match the servers' own.

## How

- **Ask the user when the DGX is free.** Then record `cria status --json` and
  `~/spark-setup/vllm.sh status`, stop the user's model through cria only, and restore
  exactly that state afterwards, verified.
- **No changes outside the test setup:** no sudo, no edits to cria or vLLM configs or
  existing venvs.
- **Servers:**
  - vLLM-Omni 0.30.0 in `~/opt/vllm-omni-kaiak` with Qwen-Image-Edit-2511 (about
    60 GB with its text encoder; downloaded once and kept);
  - plain `vllm serve` from the same venv with an ASR model (Qwen3-ASR or Whisper
    large-v3, the smaller that the kit's checks need);
  - llama-server from the kept build in `~/opt/llama.cpp-kaiak-rerank` with a GGUF
    ASR model and its audio projector (Qwen3-ASR or Voxtral).
- **Per server:** the kit's kind run against kaiak in front of it. Captures of every
  answer shape the gateway reads, compared with the fake backend. Each finding fixed
  or put to the user.
- **Kept afterwards** for re-tests: the weights and builds.

## Acceptance criteria

- The kit's checks pass on all three servers (counts recorded).
- The fake backend matches every real answer shape the gateway reads, or is
  corrected (with tests).
- The measured seconds (llama-server) equal the decoded length, rounded up. vLLM's
  reported seconds equal the files' known lengths.
- The user's model restored and verified.
- `scripts/check-all.sh` green after any fix, 3× in a row if code changed.

## Result

### Starting state on the DGX (2026-10-12, read-only)

- Recorded before the run (`dgx-cria-before.json`, outside the repository) and
  re-read at 10:38:19 UTC, just before the stop: `cria status --json` (exit 0) — one
  server, `qwen38-flash` (llama, `unsloth/Qwen3.8-Flash-Next-GGUF` `UD-Q4_K_XL`, the
  binary `~/opt/llama.cpp-slotpick/build-spark/bin/llama-server`), picks `cache=q8
  cache_ram=0 context=512k mtp=3 slots=2 vision=on`, `0.0.0.0:11434`, PID 857277,
  launched 2026-10-11T18:56:25Z, health 200; no broken entries, no router. Both slots
  idle (`/slots`: `is_processing` false, 262,144 each).
- `~/spark-setup/vllm.sh status`: `stopped` (exit 1).
- `nvidia-smi`: one compute process, the Flash `llama-server` (96,023 MiB). `free -h`:
  121 GiB total, 100 GiB used, 20 GiB available. Listening: 22, 53 (loopback),
  11434.
- Stopped through cria (`cria stop qwen38-flash`, 10:38:22 UTC), the edit server
  started the second after.

### Versions and models

- **Gateway and kit:** branch `media-b` at `0577d1f`, built by the kit from the
  worktree.
- **vLLM-Omni 0.30.0 on vLLM 0.30.0**, the prepared venv `~/opt/vllm-omni-kaiak/.venv`
  (CPython 3.13.15, PyTorch 2.13.0+cu130), unchanged; plain `vllm serve` from the
  same venv for Whisper.
- **llama.cpp build 11525** (`609290be6`), the kept `~/opt/llama.cpp-kaiak-rerank`
  build, unchanged. Its source has the transcription route
  (`tools/server/server.cpp:278`), the Qwen3 audio projector (`tools/mtmd/models/qwen3a.cpp`)
  and the same `convert_transcriptions_to_chatcmpl` and `to_json_oaicompat_asr` as the
  b11526 the research read.
- **Models**, all from the Hugging Face cache, nothing downloaded:
  - image edits: `Qwen/Qwen-Image-Edit-2511`, revision
    `6f3ccc0b56e431dc6a0c2b2039706d7d26f22cb9` (54 GiB; `model_index.json`:
    `QwenImageEditPlusPipeline`);
  - speech-to-text on vLLM: `openai/whisper-large-v3-turbo`, revision
    `41f01f3fe87f28c78e2fbf8b568835947dd65ed9` (1.6 GiB);
  - speech-to-text on llama-server: `ggml-org/Qwen3-ASR-1.7B-GGUF`, revision
    `36a678687ba7d07a74ca70ccb0e36902e005fb80`, `Qwen3-ASR-1.7B-Q8_0.gguf` with
    `mmproj-Qwen3-ASR-1.7B-Q8_0.gguf` (2.4 GiB);
  - chat on vLLM: cria's `qwen35-2b-a` (`Qwen/Qwen3.5-2B`, revision `15852e8c…`, served
    as `qwen3.5-2b`, port 8001);
  - chat on llama-server: `unsloth/Qwen3.8-27B-GGUF` `UD-Q4_K_XL` (revision
    `4ca72078…`), alias `qwen38-27b`.
- **Served from the snapshot folders with `--served-model-name`:** the phase B
  download pinned each revision (`snapshot_download(repo, revision=…)`), which writes
  no `refs/main`, and with `HF_HUB_OFFLINE=1` a repository id resolves only through
  it. vLLM-Omni picks the pipeline from `model_index.json`'s class, so the folder
  path serves the same model; `/v1/models` lists the served names.

### Commands

On the DGX, server logs in the scratch folder `/tmp/kaiak-live-b/` (since deleted).
Round 1, the biggest first, each healthy before the next:

```sh
cd ~/opt/vllm-omni-kaiak
SNAP=~/.cache/huggingface/hub
# image edits (port 8093)
HF_HUB_OFFLINE=1 .venv/bin/vllm serve $SNAP/models--Qwen--Qwen-Image-Edit-2511/snapshots/6f3ccc0b… --omni \
  --served-model-name Qwen/Qwen-Image-Edit-2511 --host 0.0.0.0 --port 8093 --gpu-memory-utilization 0.6
# speech-to-text, plain vLLM (port 8006)
HF_HUB_OFFLINE=1 .venv/bin/vllm serve $SNAP/models--openai--whisper-large-v3-turbo/snapshots/41f01f3f… \
  --served-model-name openai/whisper-large-v3-turbo --host 0.0.0.0 --port 8006 --gpu-memory-utilization 0.1
# chat (port 8001)
~/.local/bin/cria start qwen35-2b-a --wait
```

Round 2, after round 1 stopped:

```sh
LS=~/opt/llama.cpp-kaiak-rerank/build-spark/bin/llama-server
# chat (port 8008): the rerank run's command
$LS -m $SNAP/models--unsloth--Qwen3.8-27B-GGUF/snapshots/4ca72078…/Qwen3.8-27B-UD-Q4_K_XL.gguf --alias qwen38-27b \
  --host 0.0.0.0 --port 8008 --gpu-layers all --load-mode dio --parallel 1 --flash-attn on --fit off --jinja \
  --temp 1.0 --top-p 0.95 --top-k 20 --min-p 0.0 --repeat-penalty 1.0 --presence-penalty 0.0 \
  --chat-template-kwargs '{"reasoning_effort":"medium"}' --cache-type-k q8_0 --cache-type-v q8_0 --ctx-size 65536
# speech-to-text (port 8007): LIVE-BACKENDS.md's command
$LS -m $ASR/Qwen3-ASR-1.7B-Q8_0.gguf --mmproj $ASR/mmproj-Qwen3-ASR-1.7B-Q8_0.gguf --host 0.0.0.0 --port 8007
```

- **Edit server** (10:38:23 → healthy 10:45:25, 7 min): weights "took 266.38
  seconds", "Model loading took 53.7431 GiB and 390.346916 seconds", "Process-scoped
  GPU memory after model loading: 53.99 GiB"; attention backend `CUDNN_ATTN`; lazy
  regional `torch.compile` on 60 `QwenImageTransformerBlock`s. `nvidia-smi`: the
  diffusion worker 57,826 MiB; the answers' `peak_memory_mb` 57,598 (one image) and
  57,986 (two). `/v1/models`: `Qwen/Qwen-Image-Edit-2511`, `max_model_len` null.
- **Whisper** (10:45:33 → healthy 10:47:26): weights 1.51 GiB in 11.8 s; "Free memory
  on device (57.35/121.69 GiB) on startup. Desired GPU memory utilization is (0.1,
  12.17 GiB)"; KV cache 8.33 GiB, 78,408 tokens; "Supported tasks: ['transcription']";
  `max_model_len` 448; `EngineCore` 10,676 MiB. (LIVE-BACKENDS.md's 0.2 does not fit
  beside the edit server and the chat model; 0.1 is ample.)
- **cria's chat entry:** running after 1 min 19 s, `EngineCore` 17,192 MiB. With the
  three up: 95 GiB used, 26 GiB available.
- **Round 2** (10:52:46 → both healthy 10:52:53): the 27B 18,671 MiB, one slot of
  65,536; Qwen3-ASR 10,121 MiB, four slots of 65,536, its log warning "audio input is
  in experimental stage and may have reduced quality". `/v1/models` lists the ASR model
  by its file's path, with `architecture.input_modalities` `["text", "audio"]`.

On the Mac, from the worktree (each round run twice; the second without `-v` and
`-keep`):

```sh
go -C scripts/live run . -kind vllm -base-url http://dgx.local:8001/v1 -model qwen3.5-2b -max-output 4096 \
  -asr-base-url http://dgx.local:8006/v1 -asr-model openai/whisper-large-v3-turbo \
  -edit-base-url http://dgx.local:8093/v1 -edit-model Qwen/Qwen-Image-Edit-2511 \
  -request-timeout 5m -v -keep
go -C scripts/live run . -kind llama-server -base-url http://dgx.local:8008/v1 -model qwen38-27b -max-output 4096 \
  -asr-base-url http://dgx.local:8007/v1 \
  -asr-model /home/acdtrx/.cache/huggingface/hub/models--ggml-org--Qwen3-ASR-1.7B-GGUF/snapshots/36a678687ba7d07a74ca70ccb0e36902e005fb80/Qwen3-ASR-1.7B-Q8_0.gguf \
  -request-timeout 5m -v -keep
```

### Kit output — round 1, vLLM: 45 passed, 0 failed, 6 skipped (both runs)

The first run (10:50:57–10:51:21 UTC, exit 0):

```
== vllm: http://dgx.local:8001/v1, model qwen3.5-2b, image edits Qwen/Qwen-Image-Edit-2511 on http://dgx.local:8093/v1, speech-to-text openai/whisper-large-v3-turbo on http://dgx.local:8006/v1
  PASS  auth-reject          no key → 401
  PASS  models               lists live-asr, live-capped, live-chat, live-edit, live-omni-path-missing, live-path-missing, live-rpm; live-asr: audio_transcriptions; live-chat: chat_completions, completions, messages, messages_count_tokens, responses; live-edit: images_edits
  PASS  chat                 "A lighthouse is for guiding ships with their lights toward s…" (finish stop, 24+14 tokens)
  PASS  usage-log/chat       in 24 (cache read 0, cache write 0), out 14 (reasoning 0), cost 5.2e-05
  PASS  chat-stream          no usage chunk, 18 chunks, [DONE], "Lighthouses are structures built to guide ships safe out of …"
  PASS  usage-log/stream     in 24 (cache read 0, cache write 0), out 17 (reasoning 0), cost 5.8e-05
  PASS  chat-stream-usage    usage chunk relayed, 32 chunks, [DONE], "A lighthouse is a beacon used to guide ships and boats safel…"
  PASS  usage-log/stream-usg in 24 (cache read 0, cache write 0), out 30 (reasoning 0), cost 8.4e-05
  SKIP  embeddings           no -embeddings-model
  SKIP  usage-log/embeddings no -embeddings-model
  SKIP  rerank               no -rerank-base-url
  PASS  rerank-to-chat       rerank → 400 endpoint_not_served by the chat model's endpoints list, never sent
  PASS  messages             "A lighthouse is a navigational aid that warns ships of dange…" (stop end_turn, 24+26 tokens), max_tokens set by the gateway
  PASS  usage-log/messages   in 24 (cache read 0, cache write 0), out 26 (reasoning 0), cost 7.6e-05
  PASS  messages-stream      22 events, message_start … message_stop, "A lighthouse serves as a navigational beacon to guide ships …"
  PASS  usage-log/msg-stream in 24 (cache read 0, cache write 0), out 18 (reasoning 0), cost 6e-05
  SKIP  messages-cache       the server reported no cache read: its prefix cache is off or does not report it
  PASS  messages-count       24 input tokens, no usage record
  PASS  messages-models      Anthropic's shape: live-capped, live-chat, live-rpm
  PASS  messages-errors      wrong key → 401 authentication_error, unknown model → 404 not_found_error
  PASS  messages-hosted-tool web_search_20250305 → 400 hosted_tool_unsupported, never sent
  PASS  price-options        speed "fast" passed through: vllm prices nothing
  PASS  responses            "A lighthouse serves as a critical warning beacon and navigat…" (status completed, 24+31 tokens)
  PASS  usage-log/responses  in 24 (cache read 0, cache write 0), out 31 (reasoning 0), cost 8.6e-05
  PASS  responses-stream     37 events, response.created … response.completed, "A lighthouse serves as a beacon of guidance that enables shi…"
  PASS  usage-log/resp-stream in 24 (cache read 0, cache write 0), out 30 (reasoning 0), cost 8.4e-05
  PASS  responses-stateful   previous_response_id → 400 stateful_responses_unsupported, never sent
  PASS  responses-hosted-tool web_search → 400 hosted_tool_unsupported, never sent
  SKIP  speech               no -speech-base-url
  SKIP  image                no -image-base-url
  PASS  edit                 a 512×512 PNG in, 1 image(s) out: PNG of 787271 bytes
  PASS  usage-log/edit       1 image(s); in 1007, out 0, estimated true, cost 0.001007
  PASS  edit-refused         stream=true → 400 invalid_value; url by https → 400 external_reference_unsupported; lora → 400 external_reference_unsupported; none sent
  PASS  generation-to-edit   image generation to live-edit → 400 endpoint_not_served; none sent
  PASS  transcribe-wav       wav of 115244 bytes, 3.6 s: "*repe|*"; the server counted 4 s
  PASS  usage-log/transcribe-wav 4 s of audio processed, the seconds the server reported; in 6, out 0, estimated true, cost 6e-06
  PASS  transcribe-flac      flac of 115379 bytes, 3.6 s: "*repe|*"; the server counted 4 s
  PASS  usage-log/transcribe-flac 4 s of audio processed, the seconds the server reported; in 6, out 0, estimated true, cost 6e-06
  PASS  transcribe-mp3       mp3 of 14400 bytes, 3.6 s: "\"I'm going to be a little bit of audio."; the server counted 4 s
  PASS  usage-log/transcribe-mp3 4 s of audio processed, the seconds the server reported; in 6, out 0, estimated true, cost 6e-06
  PASS  transcribe-verbose   duration 3.6 s, "*repe|*"
  PASS  usage-log/transcribe-verbose 4 s of audio processed, the server's duration rounded up; in 6, out 0, estimated true, cost 6e-06
  PASS  transcribe-refused   stream=true → 400 invalid_value; none sent
  PASS  transcribe-limit     first served, counted 4 s of audio processed; second → 429 audio_seconds, audio_seconds_in_per_minute 1, Retry-After 60
  PASS  media-metrics        1 edited images, 20 seconds of audio processed, 1 audio processed limit refusals
  PASS  endpoint-not-served  /v1/responses/input_tokens, /v1/audio/speech, /v1/audio/speech/batch, /v1/images/generations, /v1/images/edits, /v1/audio/transcriptions → 400 endpoint_not_served
  PASS  path-missing         6 requests to live-path-missing → 502 upstream_path_missing, one attempt each, zero usage; circuit of live closed (threshold 5), 6 path_missing attempts
  PASS  omni-path-missing    6 requests to live-omni-path-missing → 502 upstream_path_missing, one attempt each, zero usage; circuit of live-omni-wrong-path closed (threshold 5), 6 path_missing attempts; warned at config apply: live-omni-wrong-path has no models list at http://dgx.local:8093/v1/wrong-path
  PASS  output-ceiling       max_tokens 32768 → 16 tokens, finish length
  PASS  rate-limit           second request → 429, Retry-After 59, reset 59s
  PASS  metrics              live-chat: 10 usage records, 214 output tokens, 0.020518 USD; 2 rate-limited; 19 successful upstream attempts
  45 passed, 0 failed, 6 skipped
```

The second run (10:51:43–10:52:06, exit 0): the same verdicts on every line, the same
edit (787,271 bytes) and seconds (4 for each format and for `verbose_json`). The skips
are by design: no embeddings, reranker, speech or image server, and vLLM's prefix cache
reports no reads.

The kept gateway log: each transcription record `kaiak.backend.id` `live-transcriber`,
`gen_ai.output.type` `text`, `kaiak.usage.audio.input.duration` 4,
`gen_ai.usage.input_tokens` 6 (the `prompt` part's estimate), `kaiak.usage.estimated`
true, `partial` false; the edit's `kaiak.usage.images` 1, input 1,007 estimated (the
prompt and the input image). The "not found" lines: `kaiak.attempts` 1, `error.type`
`upstream_path_missing`, `kaiak.upstream.error.message` "upstream_path_missing:
backend live answered 404: no endpoint at http://dgx.local:8001/v1/embeddings (check
its base_url, and that its server serves the endpoint for the model)" (and the same
for `…:8093/v1/wrong-path/images/edits`), zero usage, `partial` true; the `WARN` "the
backend has no models list at its base_url" for `live-omni-wrong-path`. The drain
clean; no `ERROR` line.

### Kit output — round 2, llama-server: 40 passed, 0 failed, 4 skipped (both runs)

The first run (10:53:45–10:54:49 UTC, exit 0):

```
== llama-server: http://dgx.local:8008/v1, model qwen38-27b, speech-to-text /home/acdtrx/.cache/huggingface/hub/models--ggml-org--Qwen3-ASR-1.7B-GGUF/snapshots/36a678687ba7d07a74ca70ccb0e36902e005fb80/Qwen3-ASR-1.7B-Q8_0.gguf on http://dgx.local:8007/v1
  PASS  auth-reject          no key → 401
  PASS  models               lists live-asr, live-capped, live-chat, live-path-missing, live-rpm; live-asr: audio_transcriptions; live-chat: chat_completions, completions, messages, messages_count_tokens, responses, responses_input_tokens
  PASS  chat                 "A lighthouse is a tower with a powerful light that guides sh…" (finish stop, 22+81 tokens)
  PASS  usage-log/chat       in 22 (cache read 0, cache write 0), out 81 (reasoning 0), cost 0.000184
  PASS  chat-stream          no usage chunk, 87 chunks, [DONE], "A lighthouse uses its light to guide ships safely and warn t…"
  PASS  usage-log/stream     in 22 (cache read 18, cache write 0), out 88 (reasoning 0), cost 0.000198
  PASS  chat-stream-usage    usage chunk relayed, 80 chunks, [DONE], "A lighthouse is a tower with a powerful light that warns shi…"
  PASS  usage-log/stream-usg in 22 (cache read 18, cache write 0), out 80 (reasoning 0), cost 0.000182
  SKIP  embeddings           no -embeddings-model
  SKIP  usage-log/embeddings no -embeddings-model
  SKIP  rerank               no -rerank-base-url
  PASS  rerank-to-chat       rerank → 400 endpoint_not_served by the chat model's endpoints list, never sent
  PASS  messages             "A lighthouse is a tower with a powerful light that guides sh…" (stop end_turn, 4+83 tokens), max_tokens set by the gateway
  PASS  usage-log/messages   in 22 (cache read 18, cache write 0), out 83 (reasoning 0), cost 0.000188
  PASS  messages-stream      86 events, message_start … message_stop, "A lighthouse uses a powerful light to warn ships of dangerou…"
  PASS  usage-log/msg-stream in 22 (cache read 18, cache write 0), out 81 (reasoning 0), cost 0.000184
  PASS  messages-cache       first request wrote 0, second read 9943 of 9947 input tokens
  PASS  messages-count       22 input tokens, no usage record
  PASS  messages-models      Anthropic's shape: live-capped, live-chat, live-rpm
  PASS  messages-errors      wrong key → 401 authentication_error, unknown model → 404 not_found_error
  PASS  messages-hosted-tool web_search_20250305 → 400 hosted_tool_unsupported, never sent
  PASS  price-options        speed "fast" passed through: llama-server prices nothing
  PASS  responses            "A lighthouse is a tower with a powerful light that warns shi…" (status completed, 22+79 tokens)
  PASS  usage-log/responses  in 22 (cache read 18, cache write 0), out 79 (reasoning 0), cost 0.00018
  PASS  responses-stream     101 events, response.created … response.completed, "A lighthouse is a structure with a powerful light that warns…"
  PASS  usage-log/resp-stream in 22 (cache read 18, cache write 0), out 94 (reasoning 0), cost 0.00021
  PASS  responses-count      22 input tokens, no usage record
  PASS  responses-stateful   previous_response_id → 400 stateful_responses_unsupported, never sent
  PASS  responses-hosted-tool web_search → 400 hosted_tool_unsupported, never sent
  SKIP  speech, image, edit  no -speech-base-url, -image-base-url or -edit-base-url
  PASS  transcribe-wav       wav of 115244 bytes, 3.6 s: "language None<asr_text>"; 67+4 tokens reported, no seconds
  PASS  usage-log/transcribe-wav 4 s of audio processed, the upload's length rounded up, measured by the gateway; in 67, out 4, estimated false, cost 7.5e-05
  PASS  transcribe-flac      flac of 115379 bytes, 3.6 s: "language None<asr_text>"; 67+4 tokens reported, no seconds
  PASS  usage-log/transcribe-flac 4 s of audio processed, the upload's length rounded up, measured by the gateway; in 67, out 4, estimated false, cost 7.5e-05
  PASS  transcribe-mp3       mp3 of 14400 bytes, 3.6 s: "language Chinese<asr_text>嗯。"; 67+6 tokens reported, no seconds
  PASS  usage-log/transcribe-mp3 4 s of audio processed, the upload's length rounded up, measured by the gateway; in 67, out 6, estimated false, cost 7.9e-05
  PASS  transcribe-refused   stream=true → 400 invalid_value; an Ogg upload → 400 audio_format_unsupported; none sent
  PASS  transcribe-limit     first served, counted 4 s of audio processed; second → 429 audio_seconds, audio_seconds_in_per_minute 1, Retry-After 60
  PASS  media-metrics        16 seconds of audio processed, 1 audio processed limit refusals
  PASS  endpoint-not-served  /v1/audio/speech, /v1/audio/speech/batch, /v1/images/generations, /v1/images/edits, /v1/audio/transcriptions → 400 endpoint_not_served
  PASS  path-missing         6 requests to live-path-missing → 502 upstream_path_missing, one attempt each, zero usage; circuit of live-wrong-path closed (threshold 5), 6 path_missing attempts; warned at config apply: live-wrong-path has no models list at http://dgx.local:8008/v1/wrong-path
  PASS  output-ceiling       max_tokens 32768 → 16 tokens, finish length
  PASS  rate-limit           second request → 429, Retry-After 58, reset 58s
  PASS  metrics              live-chat: 10 usage records, 634 output tokens, 0.021338 USD; 2 rate-limited; 18 successful upstream attempts
  40 passed, 0 failed, 4 skipped
```

The second run (10:55:19–10:56:12, exit 0): the same verdicts on every line, the same
tokens (67+4, 67+4, 67+6) and seconds (4). The skips are by design: no embeddings,
reranker or vLLM-Omni server.

The kept gateway log: each transcription record `kaiak.usage.audio.input.duration` 4
(the gateway's measurement), `estimated` false, `gen_ai.usage.input_tokens` 67,
`cache_read.input_tokens` 0, output 4 or 6, `kaiak.attempts` 1; the Ogg upload
`audio_format_unsupported`, never sent; the "not found" lines as in round 1, at
`…:8008/v1/wrong-path/chat/completions`. The ASR server's log shows the kit's four
transcriptions each reusing 66 of its 67 prompt tokens from the slot's cache
(`n_past was set to 66`) — see the finding below.

### Real answers, sent with curl straight to the servers

The kit's own uploads, written by a copy of `scripts/live/uploads.go`: the 3.6-second
440 Hz tone as 16-bit 16 kHz mono WAV (115,244 bytes) and FLAC (115,379 bytes, 57,600
samples), 3.6 s of MP3 silence (14,400 bytes, MPEG-1 Layer III, 32 kHz, 100 frames),
the 512×512 PNG (4,643 bytes), and the kit's Ogg (`OggS` and 60 zero bytes). Bodies
trimmed: no image bytes. Every JSON body below was sent compact (no spaces).

**Whisper on vLLM 0.30.0** (`POST :8006/v1/audio/transcriptions`, `model`
`openai/whisper-large-v3-turbo`):

- `json` (absent): WAV `{"text":" *repe|*","usage":{"type":"duration","seconds":4}}`;
  FLAC the same; MP3 `{"text":" \"I'm going to be a little bit of audio.","usage":
  {"type":"duration","seconds":4}}` (`200`, `application/json`, 0.3–0.6 s).
- `text`: JSON as well, `{"text":" . . .","usage":{"type":"duration","seconds":4}}`.
- `verbose_json`: `{"duration":3.6,"language":"en","text":" .","segments":[{"id":0,
  "avg_logprob":…,"compression_ratio":0.2,"end":3.6,"no_speech_prob":null,"seek":0,
  "start":0.0,"temperature":0.0,"text":" .","tokens":[2411]}],"words":null}` — no
  usage.
- `diarized_json`: `400` `{"error":{"message":"Currently do not support diarized_json
  for openai/whisper-large-v3-turbo","type":"BadRequestError","param":null,"code":400}}`.
- `srt` and `vtt`: `400` `{"error":{"message":"Currently only support response_format:
  `text`, `json`, `verbose_json` or `diarized_json`","type":"BadRequestError",
  "param":null,"code":400}}`.
- `stream=true`: `200`, `text/event-stream`, `data: {"id":"transcribe-…","object":
  "transcription.chunk","created":…,"model":"openai/whisper-large-v3-turbo",
  "choices":[{"delta":{"content":" ."}}]}` events, the last with `finish_reason`
  `stop`, then `data: [DONE]`; no usage.
- `n=2`: one text, `seconds` 4 — `n` changes neither the answer nor its seconds.
- No `model` part: `200`, the served model filled in.
- `model` `whisper-nope`: `404` `{"error":{"message":"The model `whisper-nope` does not
  exist.","type":"NotFoundError","param":"model","code":404}}`.
- No `file` part, or a JSON body: `400` `{"error":{"message":"1 validation error:\n
  {'type': 'missing', 'loc': 'body.file', 'msg': 'Field required', …}","type":"Bad
  Request","param":"body.file","code":400}}`.
- The kit's Ogg (undecodable): `400` `{"error":{"message":"Invalid or unsupported audio
  file.","type":"BadRequestError","param":null,"code":400}}`.
- Sent chunked, without a `Content-Length`: `200`, the same answer.

**The vLLM chat model** (`:8001`, `qwen3.5-2b`): a transcription answers `404`
`{"detail":"Not Found"}` (no route for a model without transcription), embeddings the
same; `POST /v1/responses/input_tokens` `405` `{"detail":"Method Not Allowed"}`
(`allow: GET`).

**Qwen-Image-Edit-2511 on vLLM-Omni 0.30.0** (`POST :8093/v1/images/edits`, the kit's
form: `model`, `prompt` "Paint the lighthouse red.", `image` the PNG,
`num_inference_steps` 10, `seed` 7):

- `200`, 18.2 s (the first request, with the lazy compile; the next 15.8 s),
  `content-length: 1049960`:

  ```json
  {"created":1791802172,"data":[{"b64_json":"<1,049,696 chars: a 512×512 PNG of 787,271 bytes>","url":null,"revised_prompt":null}],
   "output_format":"png","size":"512x512","cot_output":null,
   "metrics":{"stage_durations":{"queue_wait_ms":30.64,"stage_0_gen_ms":17343.04},"peak_memory_mb":57598.0}}
  ```

  The kit's grey tower with a yellow lamp came back as a red lighthouse with a lit
  lantern: a real edit.
- `n=2`: two `data` elements, 45.8 s.
- `stream=true`: `400` `{"error":{"message":"stream=true is only supported for
  multi-stage image editing pipelines.","type":"Bad Request","param":null,"code":400}}`.
- `response_format=url`: `400` "Only response_format 'b64_json' is supported now.",
  type `Bad Request`.
- `model` `qwen-image-edit`: `400` `{"error":{"message":"Model mismatch: request
  specifies 'qwen-image-edit' but server is running 'Qwen/Qwen-Image-Edit-2511'.",
  "type":"Bad Request","param":null,"code":400}}`.
- No `prompt`, or a JSON body: `400` "1 validation error: … 'loc': 'body.prompt' …",
  type `Bad Request`, `param` `body.prompt`.
- No image: `422` `{"error":{"message":"Field 'image' or 'url' is required","type":
  "Unprocessable Content","param":null,"code":422}}`.
- Sent chunked, without a `Content-Length`: `200`, an image.
- **A generation sent to the edit model** (`POST /v1/images/generations`): `500` at
  once (0.16 s): `{"error":{"message":"Image generation failed: Missing preprocess
  images that should have been created by the preprocess function.","type":"Internal
  Server Error","param":null,"code":500}}` — the server logs the traceback ending in
  `RuntimeError` from `raise_client_error_or`. The research's `500`, confirmed; the
  kit's `generation-to-edit` shows the list stops it first.
- A transcription on the edit server: `404` `{"detail":"Not Found"}` (vLLM-Omni mounts
  no transcription route).

**Qwen3-ASR-1.7B on llama-server b11525** (`POST :8007/v1/audio/transcriptions`, the
model id the file's path):

- WAV `{"type":"transcript.text.done","text":"language None<asr_text>","usage":
  {"type":"tokens","input_tokens":67,"output_tokens":4,"total_tokens":71,
  "input_tokens_details":{"cached_tokens":0}}}`; FLAC the same text and counts,
  `cached_tokens` 9; MP3 `"text":"language Chinese<asr_text>嗯。"`, 67+6, `cached_tokens`
  9 (`200`, `application/json; charset=utf-8`, 0.1–0.3 s). The three uploads encode to
  the same 67 prompt tokens: the server heard the same length in each. The text is
  Qwen3-ASR's raw output format; any text passes the kit.
- No `prompt` part: the same answer (the server's ASR preset prompt).
- `verbose_json` or `text`: `400` `{"error":{"code":400,"message":"Only 'json'
  response_format is supported for transcription","type":"invalid_request_error"}}`.
- `stream=true`: `200`, `text/event-stream`, `data: {"type":"transcript.text.delta",
  "delta":"language"}` events, then `data: {"type":"transcript.text.done",…,"usage":
  {…}}`, then `data: [DONE]`.
- The kit's Ogg: `400` `{"error":{"code":400,"message":"Failed to load image or audio
  file","type":"invalid_request_error"}}` — the server logs that miniaudio, then the
  image decoder, then its video path (`ffprobe`, not installed) failed. Its video path
  only ever feeds frames to a vision model, so an audio upload is decoded as WAV, FLAC
  or MP3 or not at all, whatever the host has installed: the gateway's refusal of other
  formats on `llama-server` holds.
- No `file` part, or a JSON body: `400` "No input file found for transcription",
  `invalid_request_error`.
- Another `model` part: `200` (not checked: the server serves its one model).
- Sent chunked, without a `Content-Length`: `200`.

**The llama-server chat model** (`:8008`, no audio projector): a transcription answers
`501` `{"error":{"code":501,"message":"The current model does not support audio
input.","type":"not_supported_error"}}`.

**Paths the servers do not have:**
- vLLM (Whisper): `/wrong-path/v1/audio/transcriptions`, `/v1/embeddings` and
  `/v1/chat/completions` → `404` `{"detail":"Not Found"}` (`application/json`);
  `GET /v1/audio/transcriptions` → `405` `{"detail":"Method Not Allowed"}`
  (`allow: POST`).
- vLLM-Omni: `/wrong-path/v1/images/edits` and `/wrong-path/v1/models` → `404`
  `{"detail":"Not Found"}`.
- llama-server (both): `/wrong-path/v1/audio/transcriptions`, `/wrong-path/v1/models`,
  `/wrong-path/v1/chat/completions` → `404` `{"error":{"message":"File Not Found",
  "type":"not_found_error","code":404}}`.

The servers' access logs: Whisper 19 transcriptions `200`, 6 `400`, 1 `404`, the
three route-missing `404`s and the `405`; the edit server 5 edits `200`, 5 `400`,
1 `422`, the generation's `500`, 12 `POST /v1/wrong-path/images/edits` `404` (the
kit's 6, twice) and the `/v1/wrong-path/models` probes; cria's chat 13 embeddings `404` (the kit's 6 twice and
mine), the transcription `404`, the `405`. No `ERROR` in Whisper's log; the edit
server's only error is the generation's traceback.

### Against the fake backend and the spec

Every field the gateway reads matches the fake (`fakebackend/transcription.go`,
`form.go`, `images.go`, `fakebackend.go`) and `GATEWAY.md`:

- **vLLM transcription:** `{"text","usage":{"type":"duration","seconds"}}` for `json`
  and `text` (JSON, never plain text), `seconds` the decoded length rounded up;
  `verbose_json` `{duration, language, text, segments, words}` with no usage; `srt` and
  `vtt` refused `400` `BadRequestError`, relayed as the caller's; the model check's
  `404 NotFoundError` "The model `X` does not exist." with `param` `model`, word for
  word; an omitted model filled in; a model without transcription `404
  {"detail":"Not Found"}` (the fake's `SetNonAudioModels`).
- **llama-server transcription:** `{"type":"transcript.text.done","text","usage":
  {"type":"tokens","input_tokens","output_tokens","total_tokens",
  "input_tokens_details"}}`, never seconds; the model part not checked;
  `response_format` other than `json` `400 invalid_request_error`; a model without an
  audio encoder `501 not_supported_error` "The current model does not support audio
  input.", word for word.
- **vLLM-Omni edits:** `{"created","data":[{"b64_json","url":null,
  "revised_prompt":null}],"output_format","size","cot_output","metrics"}`, no
  `model`, no usage — the fake's member for member; `n` images in `data` (counted as
  `images`); the `400 Model mismatch` message word for word (read as the deployment's
  model missing); multipart only (a JSON body `400`); no image `422`; `stream=true`
  `400`.
- **"Not found":** FastAPI's `404 {"detail":"Not Found"}` on vLLM and vLLM-Omni, the
  `405 {"detail":"Method Not Allowed"}` on vLLM, llama-server's `404 File Not Found`
  `not_found_error` — the three readings in the providers, each answered `502
  upstream_path_missing` in one attempt by the kit's checks.

**Finding — llama-server reports its prompt-cache reads on transcriptions, and the
gateway counts them as plain input.**

- **What the server sends:** `usage.input_tokens_details` is `{"cached_tokens": N}`,
  the prompt tokens served from the slot's cache, included in `input_tokens` — llama.cpp
  writes it in `to_json_oaicompat_asr` (`tools/server/server-task.cpp:716-729`,
  `n_prompt_tokens_cache`), the same detail its Responses answer carries
  (`to_json_oaicompat_resp`, `:592`). Live:
  0, 9 and 66 of 67; through the gateway each of the kit's transcriptions reused 66 of
  67 tokens (the server's log).
- **What the fake sends:** `input_tokens_details` `{"text_tokens": 0, "audio_tokens":
  N}` (OpenAI's transcription detail), which llama-server never sends.
- **What the gateway records:** `transcriptionUsage.reported`
  (`gateway/internal/accounting/transcription_usage.go`) reads `input_tokens` →
  `tokens_in` and `output_tokens` → `tokens_out`, every other unit 0, as `GATEWAY.md`
  → Accounting → Media usage: speech-to-text, Tokens, says. So the kit's records carry
  `tokens_in` 67 and `tokens_cached` 0 where the server reported 66 cached. On chat,
  Messages and Responses on the same server the gateway counts cache reads
  (`usage-log/stream`: `cache read 18`), and `input_tokens_details.cached_tokens` is
  exactly the field it reads on Responses. A model priced with a cache-read price is
  charged the input price for those tokens on transcriptions.
- **No check failed:** the kit's `usage-log/transcribe-*` compared input and output
  only.
- **Fixed** (cf2065e; the user ruled it in, 2026-10-12): the gateway reads a
  llama-server transcription's usage as a Responses usage — cache reads (and
  writes) out of `tokens_in`, priced as cache reads and out of token limits; the
  fake sends llama-server's `{"cached_tokens"}`; the kit's `usage-log/transcribe-*`
  holds the record's cache read to the answer's. The spec's Tokens rule is updated.

Cosmetic differences, nothing reads them, no fix needed:
- error wording and types on answers the gateway relays or never causes: vLLM's
  missing-`file` `400` (`type` `Bad Request`, `param` `body.file`, a validation dump;
  the fake `BadRequestError` "file: Field required"); llama-server's `400` messages
  ("Only 'json' response_format is supported for transcription", "No input file found
  for transcription"); vLLM-Omni's `400`s typed `Bad Request` with their own messages
  for `stream=true` and `response_format` (the fake `BadRequestError` "Streaming is only
  supported for multi-stage pipelines", "the fake does not play …"), the missing
  prompt's validation dump, and the `422` typed `Unprocessable Content` with "Field
  'image' or 'url' is required" (the fake's `BadRequestError` "Either 'image' or 'url'
  must be provided");
- `verbose_json`'s segments carry Whisper's full segment members (`avg_logprob`,
  `compression_ratio`, `no_speech_prob`, `temperature`, `tokens`); the fake's carry
  `id, seek, start, end, text`; the gateway reads `duration` alone;
- member order: vLLM writes `usage` as `type, seconds`, the fake (a marshalled map)
  `seconds, type`;
- the edit answer's `metrics` holds `stage_durations` and `peak_memory_mb` (the
  fake's `{}`), as phase A found for generations;
- `stream=true` on transcriptions: both servers stream (vLLM's
  `transcription.chunk` events, no usage; llama-server's `transcript.text.delta`
  events, then `done` with usage, then `[DONE]`) where the fake answers `400`; the
  gateway refuses `stream=true` before routing and never sends it;
- `diarized_json`: Whisper refuses it (`400`, MOSS models alone answer it); the fake
  answers it as a model that does.

Observations, not mismatches:
- **Both servers take an upload without a `Content-Length`** (chunked): vLLM 0.30.0,
  vLLM-Omni and llama-server all answer `200`. The fake's refusal of such an upload is
  a guard that holds the gateway to sending one (decision 28), not any of these
  servers' behavior.
- **`n` on a vLLM transcription** changes neither the answer nor its seconds; the
  gateway's cap on the part and its reservation (none) stand.
- **Edit cost:** 15.8–18.2 s of the GPU per 512×512 image at 10 steps, 45.8 s for
  two; the gateway counts the answer's `data` (1, 2), and estimates the input from the
  prompt and the image (1,007 tokens here).

### Seconds counted

- **vLLM (Whisper):** the server reported `seconds` 4 for the WAV, FLAC and MP3
  uploads and `duration` 3.6 for `verbose_json` — the uploads' known length, 3.6 s,
  rounded up to 4. Each record's `kaiak.usage.audio.input.duration` is 4, taken from the
  answer (`usage-log/transcribe-*`, `-verbose`; both runs).
- **llama-server (Qwen3-ASR):** the answers carry no seconds; each record's
  `kaiak.usage.audio.input.duration` is 4, the gateway's own measurement of the upload
  (3.6 s rounded up), not estimated, for WAV, FLAC and MP3 (both runs). The server
  encoded each format to the same 67 prompt tokens: it decoded the same length the
  gateway measured.

### Restoration (2026-10-12 10:52–10:57 UTC)

- Round 1 stopped at 10:52:29: `SIGTERM` to the two API processes (edit 864168,
  Whisper 865558), both shut down cleanly ("Application shutdown complete"), their
  workers gone; `cria stop qwen35-2b-a`. Then no GPU process, ports 8001, 8006, 8093
  free, 118 GiB available.
- Round 2 stopped at 10:56:20: `SIGTERM` to both llama-servers. Then no GPU process, no
  vLLM or llama-server process, cria no servers, listening 22 and 53 only.
- `~/.local/bin/cria start qwen38-flash cache=q8 context=512k mtp=3 vision=on slots=2
  cache_ram=0 --wait` at 10:56:21 → running after 12 s, PID 868181. The user's model
  was down 18 min 12 s (10:38:22–10:56:34).
- Checked against the recorded JSON: `cria status --json` (exit 0) — one server,
  `qwen38-flash`, the same backend, repository, quant, picks, host `0.0.0.0`, port
  11434, phase running, and command line (the same binary
  `/home/acdtrx/opt/llama.cpp-slotpick/build-spark/bin/llama-server` and arguments,
  compared element by element), health 200 green, no broken entries, no router;
  `/health` `{"status":"ok"}`; `/v1/models` `unsloth/Qwen3.8-Flash-Next-GGUF:UD-Q4_K_XL`
  (262,144); `/slots` two idle slots of 262,144; a chat "What is 17*19? Answer with the
  number only." → `323` (finish `stop`); `nvidia-smi` one compute process, this
  `llama-server` (96,245 MiB, before 96,023); `vllm.sh status` `stopped` (exit 1);
  listening 22, 53, 11434 only; `free -h` 101 GiB used, 20 GiB available (before 100
  and 20).

### Left on the DGX

- The weights in the Hugging Face cache, as found (revisions above); the venv
  `~/opt/vllm-omni-kaiak` and the build `~/opt/llama.cpp-kaiak-rerank`, unchanged.
- Compile caches the run added inside the existing cache folders (they speed a
  re-run): `~/.cache/vllm/torch_compile_cache/79cdee7d49` (120 KiB) and
  `torch_aot_compile/6994f40d…` (17 MiB), the model-info file
  `vllm-model_executor-models-whisper-WhisperForConditionalGeneration.json` in
  `~/.cache/vllm/modelinfos`, one `~/.triton/cache` entry (148 KiB).
- Removed: the scratch folder `/tmp/kaiak-live-b` (start scripts, server logs, PID
  files); the run's `/tmp` leftovers — six vLLM IPC sockets, two `vllm_dist_*` files,
  two `tmp*` folders, two `perf-*.map` files — and the 40 entries (15 MiB) compiled into
  the empty `/tmp/torchinductor_acdtrx`; the new `~/.cache/torch` (a 104 KiB NVRTC kernel
  cache) and the empty `~/.cache/vllm-omni/speakers`. `/tmp` lists as before the run;
  the pre-existing `/tmp/…Qwen-Qwen3.5-2B.lock` stays (cria's vLLM touched it). cria's
  config, `~/spark-setup` and the existing venvs and builds are unchanged.
- On the Mac: no kit, gateway or fake process; both `-keep` workspaces deleted.

### Verification after the fix (2026-10-12)

`scripts/check-all.sh` on ce7231a (the fix, cf2065e, and this write-up), **3× in a row,
green**: 12:08:05–12:11:58, 12:11:58–12:15:45, 12:15:45–12:19:34 UTC (`all checks
passed` each).
