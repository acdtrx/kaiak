# Media generation — research and direction

Research done 2026-10-09, before planning. The plan (`OVERVIEW.md` and step files)
comes after the rerank plan merges. This file holds the direction settled with the
user, the facts it rests on, and the questions planning must answer.

## Direction (settled with the user, 2026-10-09)

- **Backends: vLLM-Omni only** — a new backend type, `vllm-omni` (a type per server).
  No cloud backends, no other self-hosted servers for now.
- **Phase A — first release:**
  - speech: `POST /v1/audio/speech` — non-stream (raw audio), SSE stream and
    raw-audio stream — and vLLM-Omni's `POST /v1/audio/speech/batch`;
  - image generation: `POST /v1/images/generations`.
- **Phase B — after phase A is released:** uploads (`multipart/form-data`), then image
  edits (`POST /v1/images/edits`; Qwen-Image-Edit is the model of interest) and
  transcription (`POST /v1/audio/transcriptions`).
- **Not now:** video (vLLM-Omni's `/v1/videos` job API — researched below, for the
  record); vLLM-Omni's WebSocket endpoints; the voices endpoints.
- **Limits:** requests/min is enough for images and transcription for now. Token limits
  keep working for speech, which reports tokens.
- **Units:** `images` and `audio_seconds` enter the protocol in phase A's version bump,
  so phase B needs none (the user's suggestion; agreed). `audio_seconds` is first
  produced in phase B.
- **Billing focus:** local models — usage counted and limited in tokens, units or
  requests. Prices stay optional (an unpriced model costs 0).

## vLLM-Omni (v0.30.0, 2026-09-25; source read 2026-10-09)

Launched as `vllm serve <model> --omni`; a separate package (`vllm-project/vllm-omni`)
pinned to the matching vLLM minor; one model per process; routes depend on the model
loaded (its output modalities), like vLLM's. It serves vLLM's own routes too (chat,
transcription when the model supports it). A `model` that does not match the served
name is refused with `400`.

| Endpoint | In | Out | Usage reported |
|---|---|---|---|
| `POST /v1/audio/speech` (non-stream) | JSON | raw audio, `audio/wav` by default (wav, mp3, flac, pcm, opus; no aac) | **response headers**: `x-vllm-omni-input-tokens`, `-output-tokens`, `-total-tokens`, `-input-text-tokens`, `-input-audio-tokens`; none on diffusion-mode speech servers |
| same, `stream: true` or `stream_format: "sse"` | JSON | SSE `speech.audio.delta` (base64 chunk), `speech.audio.done` (usage), `speech.audio.error` | `speech.audio.done.usage`: `{input_tokens, output_tokens, total_tokens, input_token_details{text_tokens, audio_tokens}}` |
| same, `stream_format: "audio"` | JSON | raw audio bytes as decoded | **none** |
| `POST /v1/audio/speech/batch` (vLLM-Omni's own) | JSON, `items` 1–32 with shared defaults | JSON, `results[]` each `{index, status, audio_data (base64), media_type, usage}` or `{status: "error", error}` | per item, same usage shape |
| `POST /v1/images/generations` | JSON | `{created, data[{b64_json}]}`; `response_format` `b64_json` (default) or `file` (raw bytes, non-OpenAI); `url` refused | **none** |
| `POST /v1/images/edits` | **multipart only** | same as generations, plus `metrics{stage_durations, peak_memory_mb}` | **none** |
| `POST /v1/audio/transcriptions` (vLLM's) | multipart (audio file) | JSON or text | vLLM: non-stream JSON `usage {type: "duration", seconds}`; `verbose_json` none; streamed with include-usage: token counts |
| `POST /v1/videos`, `GET /v1/videos/{id}`, `/content`, `/v1/videos/sync` | multipart | job object; MP4 | none |

- **Speech request fields:** OpenAI's `input`, `model`, `voice`, `response_format`,
  `speed`, `instructions`, `stream_format`; vLLM-Omni's `stream` (bool, SSE),
  `task_type`, `language`, `sample_rate`, `max_new_tokens` (default 2048 — the output
  limit), `ref_audio`, `ref_text`, `x_vector_only_mode`, and more. Both stream modes
  need `response_format` pcm or wav and `speed` 1.0.
- **Speech usage is tokens:** `input_tokens` = text tokens (`input` + `instructions`)
  + reference-audio codec frames (voice cloning only); `output_tokens` = generated
  codec tokens. Model-specific, like any model's tokens.
- **Image request fields:** OpenAI's `prompt`, `model`, `n` (1–10), `size`,
  `response_format`, `output_format`, `output_compression`, `background`, `user`,
  `stream` (edits, multi-stage pipelines only, with vLLM-Omni's own `image.edit.chunk`
  events); vLLM-Omni's `negative_prompt`, `num_inference_steps`, `guidance_scale`,
  `true_cfg_scale`, `seed`; edits add `image`, `url`, `reference_image`, `mask_image`.
- **Fields that make the server fetch or read things:** edits `url`,
  `reference_image`, `mask_image` (URLs); speech `ref_audio` ("HTTP URL, base64 data
  URL, or `file://` URI with `--allowed-local-media-path`"); video `image_reference`,
  `audio_reference`.
- **WebSocket endpoints:** `/v1/audio/speech/stream` (text streamed in, audio out;
  splits by sentence or clause), `/v1/realtime`, `/v1/duplex`, `/v1/video/chat/stream`,
  `/v1/realtime/video`, `/v1/realtime/robot/openpi`; plus HTTP realtime sessions
  (`/v1/realtime/sessions…`).
- **Video jobs** live in the process's memory plus local disk with a time-to-live;
  the server refuses more than one API worker; a restart loses jobs; `GET /v1/videos`
  lists every job unscoped.
- **Models:** images — Qwen-Image (incl. Edit, Edit-2509/2511, Layered), FLUX.1/2,
  SD3.5, SDXL, Z-Image, HunyuanImage-3, GLM-Image, BAGEL; speech — Qwen3-TTS
  (CustomVoice, VoiceDesign, Base), Fish S2-Pro, Voxtral-TTS, CosyVoice3, and more;
  video — Wan2.1/2.2, LTX-2.x, HunyuanVideo-1.5, Cosmos3.

## Counting usage

| API | From vLLM-Omni | kaiak counts |
|---|---|---|
| speech, non-stream / SSE / batch | tokens | `tokens_in`, `tokens_out` as reported — no new unit |
| speech, raw-audio stream | nothing | estimated from the input and flagged — or the mode is refused (planning decides) |
| image generation (and edits) | nothing | `images`: the answer's `data[]` count, exact |
| transcription | `seconds` | `audio_seconds` |

Token limits and the output-limit reservation apply to speech as to chat
(`max_new_tokens` as the output-limit key). Images and transcription are limited by
requests/min for now; per-unit limit types, or a per-model token weight that lets
token limits cover them, wait for a need.

## What it means in kaiak

- **New formats:** speech (JSON in, raw audio or SSE out), speech batch (JSON both
  ways), images (JSON; multi-MB base64 answers). Each is a format as rerank was —
  owned fields, a stream format, a usage reader, a row in the endpoint table.
- **Usage from response headers** (non-stream speech) — the meter reads only bodies
  and events today.
- **The usage-record units and price units are closed sets** (`usage-record.schema.json`,
  `usd_per_million`): `images` and `audio_seconds` are a protocol and config-format
  bump on both halves (precedent: `tokens_cache_write`, protocol 4).
- **The image answer's `data[]` must be counted, not kept**: naming it the content
  member would hold up to 4 MiB of base64 per answer.
- **Refuse what makes the server fetch or read:** URLs and `file://` paths in the
  fields above (inline data stays), as hosted tools are refused; `url` as an image
  `response_format` (vLLM-Omni refuses it anyway); vLLM-Omni's `file` format is a
  planning question (raw image bytes — no `data[]` to count).
- **`vllm-omni` type with no core endpoints:** it creates routes from the loaded model,
  so the wrong-endpoint reading built in the rerank plan applies (a chat request to an
  image model is the endpoint missing, neutral). Which of vLLM's endpoints it claims
  besides the media ones is a planning question.
- **Model metadata:** `context_length` and `capabilities` are required today and mean
  little for image and speech models.
- **`gen_ai.operation.name`:** OpenTelemetry's well-known `generate_content`
  ("Multimodal content generation operation") likely applies to image and speech
  generation, with `gen_ai.output.type` naming the kind — check at the pinned
  semantic-conventions commit before choosing custom values.
- **Phase B — uploads:** the inbound stage reads JSON only; the request content type
  is never read and `Content-Type: application/json` is sent upstream. Uploads need a
  multipart reader (Go's `mime/multipart`), rules for repeated and owned parts, the
  `model` part spliced in the raw bytes, the client's `Content-Type` (boundary)
  forwarded, an estimate over parts, and an upload cap and budget: OpenAI allows 16
  images of 50 MB per edit and 25 MB of audio per transcription, against today's 4 MiB
  body cap, 512 MiB body budget and 60 s body-read deadline.

## Questions for planning

1. Speech raw-audio stream: accept with estimated usage, or refuse?
2. Image `response_format: "file"` (vLLM-Omni's raw bytes): pass with the count taken
   from `n`, or refuse?
3. The endpoints `vllm-omni` claims (its media endpoints only, or vLLM's too).
4. `gen_ai.operation.name` values for speech and images.
5. Model metadata for non-text models.
6. Speech `ref_audio`: accept base64 data only?
7. A cap on images `n` and on batch `items` (vLLM-Omni allows 10 and 32).
8. Phase B: the upload cap, budget and read deadline; multipart owned-part rules.

## Landscape, for the record (checked 2026-10-09)

- **OpenAI:** the Videos API and every Sora model shut down 2026-09-24, no
  replacement; DALL·E removed 2026-05-12 and `/v1/images/variations` gone; current
  image models (`gpt-image-2`, `gpt-image-2.5-*`) return base64 only and report usage
  in tokens split by text and image; `tts-1`, `tts-1-hd` and `gpt-4o-mini-tts` shut
  down 2027-01-06 (replacement Realtime-only); `whisper-1` and the `gpt-4o-*-transcribe`
  models shut down 2027-02-26 (replacements `gpt-transcribe`, `gpt-live-transcribe`).
  Source: https://developers.openai.com/api/docs/deprecations
- **Azure OpenAI:** images, speech and transcription on the v1 *preview* API; image
  answers add content-filter results per item; the last Sora version retires
  2026-10-15.
- **Other self-hosted servers:** vLLM core and llama-server serve transcription only;
  SGLang Diffusion serves images and video jobs (URL answers by default, client-set
  server paths); stable-diffusion.cpp's `sd-server` images (base64, synchronous);
  LocalAI images and speech (URL answers, zero usage); Kokoro-FastAPI and Speaches
  speech (no real usage). None reports image usage.
- **Video, if it returns:** only self-hosted job servers (vLLM-Omni, SGLang Diffusion,
  FastVideo) speak OpenAI's job shape; jobs are process-local, so follow-ups must reach
  the same server. Within a stateless gateway that means a sealed job ID (deployment,
  owning key and expiry encrypted into the ID with a secret from the environment),
  routing pinned to that deployment with no failover, the list endpoint refused,
  billing at create (seconds), and no retry once a create was sent. It runs against
  the rulings that keep stateful APIs out (`docs/kaiak.md`, Never planned;
  `docs/specs/GATEWAY.md`, Responses is stateless).
