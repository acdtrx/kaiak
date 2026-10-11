# Media phase B — research

Facts read from source on 2026-10-11, for planning. The installed code on the DGX
(`~/opt/vllm-omni-kaiak/.venv`, Python 3.13): vLLM 0.30.0, vLLM-Omni 0.30.0 (commit
a8576ccb), FastAPI 0.136.3, Starlette 1.7.0, soundfile 0.14.0, torchcodec 0.16.0,
PyAV 19.0.1, Pillow 12.3.0. llama.cpp read at tag `b11526` in `~/opt/llama.cpp` (no tag
`b11525` exists; `tools/` and `common/` are identical across b11524..b11526). OpenAI
from `openai/openai-openapi` `main/openapi.yaml` (commit e95c0fe6, 2026-10-09). Paths
below are relative to site-packages or the llama.cpp tree.

## vLLM 0.30.0 — `POST /v1/audio/transcriptions` (and translations)

- **Fields** (`vllm/entrypoints/speech_to_text/transcription/protocol.py:52-181`,
  form fields via `api_router.py:31-61`):
  - OpenAI's: `file`, `model`, `language`, `prompt` (""), `response_format` ("json"),
    `timestamp_granularities[]` (accepted, never used), `stream`, `temperature`.
  - vLLM's own: `hotwords`, `to_language`, `stream_include_usage`,
    `stream_continuous_usage_stats`, `vllm_xargs` (a JSON string handed to the
    model), `use_beam_search`, `n`, `length_penalty`, `include_stop_str_in_output`,
    `top_p`, `top_k`, `min_p`, `seed`, the penalties, `max_completion_tokens`.
  - Unknown fields are ignored (`extra="allow"`).
- **Model check** (`serve/engine/serving.py:40-68`): an unknown `model` → `404`
  `NotFoundError` "The model `X` does not exist.", `param: "model"`; an omitted one is
  filled with the served name.
- **Answers and usage** (`speech_to_text/base/serving.py`):
  - `json` and `text`: `{text, usage: {type: "duration", seconds}}` — `text` is JSON
    too, never plain text (`api_router.py:58-59`).
  - `seconds` = `int(math.ceil(duration_s))`, `duration_s` = decoded samples / sample
    rate after mono downmix and resampling (`:642-646`; `multimodal/audio.py:39-50`).
  - `verbose_json`: `{duration (float), language, text, segments[], words}`, **no
    usage** (`:677-686`); only models with segment timestamps (Whisper, FunASR,
    FireRedASR2), others `400`.
  - `diarized_json`: with duration usage; MOSS only.
  - `srt`, `vtt`: refused `400` (`:445-454`).
  - Translations: no usage at all (`:687-700`).
- **Streaming** (`:734-846`): vLLM's own chunks (`object: "transcription.chunk"`,
  `choices[].delta.content`), `[DONE]` at the end; usage only with
  `stream_include_usage` (or `--enable-force-include-usage`), **tokens only, never
  seconds**; split audio overwrites `prompt_tokens` per piece.
- **Limits:** upload `VLLM_MAX_AUDIO_CLIP_FILESIZE_MB` 25 → `400`; decode at most 600 s
  and 256 MiB (`envs.py`); audio over the model's clip length (30 s by default) is split
  at quiet points and transcribed piece by piece.
- **Decoding:** soundfile → torchcodec → PyAV (`multimodal/media/audio.py:403-495`):
  every FFmpeg container (MP3, M4A/MP4, WebM, Ogg, FLAC, WAV). Undecodable → `400`.
- **No token counts on non-stream answers.** `max_completion_tokens` bounds the output
  (per piece).
- **A model without transcription:** the routes exist only when the model's tasks
  include transcription (`launchers/api_server/routers.py:57-62`), so the server
  answers its route-missing `404 {"detail":"Not Found"}`.
- **Models** (`model_executor/models/`, implementing `SupportsTranscription`): Whisper,
  Voxtral, Qwen3-ASR, Qwen3-Omni (thinker), Granite Speech, Gemma 3n, GLM-ASR,
  Kimi-Audio, FunASR, FireRedASR2, Cohere ASR, MOSS Transcribe-Diarize.

## vLLM-Omni 0.30.0 — `POST /v1/images/edits` (`vllm_omni/entrypoints/openai/api_server.py:2260-2629`)

- **Multipart only:** a JSON body parses as an empty form → `400` (prompt missing).
  Generations are JSON only.
- **Fields:**
  - input images: `image` (repeated file parts), `image[]` (alias; ignored when `image`
    is present), `url` / `url[]` (strings); none at all → `422`;
  - `prompt` (required); `model` (a mismatch → `400 Model mismatch`);
  - OpenAI's: `n` (default 1, **no bound on edits**), `size` ("auto": the first input
    image's size), `response_format` (`b64_json` only, else `400`), `output_format`,
    `background`, `output_compression`, `stream`, `user`;
  - vLLM-Omni's: `mask_image`, `reference_image` (a file part or a string),
    `negative_prompt`, `num_inference_steps`, `guidance_scale`, `guidance_scale_2`,
    `strength`, `true_cfg_scale`, `seed`, `generator_device`, `lora` (a JSON string
    naming a server-side path), `layers`, `resolution`, `bot_task`, `sys_type`,
    `system_prompt`, `return_stage_metrics`. No `extra_params` on edits.
  - Ignored silently: OpenAI's `mask` (vLLM-Omni's is `mask_image`), `input_fidelity`,
    `quality`, `partial_images`.
- **Fetching:** `url`, `url[]`, `mask_image` and `reference_image` as strings: `http…`
  is fetched (60 s, no size cap), `data:image…` decoded, anything else `400`
  (`images/helpers.py:180-228`). `lora` carries a server-side path.
- **Answer:** `{created, data[{b64_json}], output_format, size, cot_output, metrics}`,
  **no usage**.
- **Streaming:** only for multi-stage pipelines (an AR stage then diffusion, e.g.
  GLM-Image), `image.edit.chunk` events; a single-stage pipeline (Qwen-Image-Edit)
  refuses `stream: true` with `400`.
- **Input image count** (`_get_max_edit_input_images`): 1 for plain pipelines; 4 for
  `QwenImageEditPlusPipeline` (Qwen-Image-Edit-2509/2511) and Flux2 Klein; 3 for
  HunyuanImage3; over → `400`. No byte limit per image; Starlette caps text parts at
  1 MiB and a form at 1000 parts.
- **Wrong model:** an edit sent to Qwen-Image runs plain text-to-image at the input
  image's size, ignoring the input (inferred from code); a generation sent to an edit
  model answers `500` (`EngineGenerateError`).
- **vLLM-Omni never serves transcription:** its engine reports the tasks `generate` and
  `speech` only, so the transcription router is never mounted (`404`). Plain
  `vllm serve` of Qwen3-Omni gets it.

## llama.cpp `llama-server` (b11526)

- **Routes:** `POST /v1/audio/transcriptions` and `/audio/transcriptions`
  (`tools/server/server.cpp:278-279`); the request becomes a chat completion
  (`server-chat.cpp:651-706`): the `file` part (required), `prompt`, `language`,
  `stream`, `max_tokens`, `temperature`; every other field copied into the chat body;
  `response_format` must be `json` (else `400`).
- **A model without audio:** `501 not_supported_error` "The current model does not
  support audio input."
- **Answer:** `{type: "transcript.text.done", text, usage: {type: "tokens",
  input_tokens, output_tokens, total_tokens, input_tokens_details}}` — **never
  seconds**. Streaming: `transcript.text.delta` events, then the `done` event with
  usage, then `[DONE]`.
- **Decoding:** miniaudio — **WAV, MP3 and FLAC only**, by magic bytes; anything else
  → `400` (`tools/mtmd/mtmd-helper.cpp:301-398`). Payload limit 100 MB (cpp-httplib's
  default).
- **Models with an audio encoder** (`tools/mtmd/mtmd.cpp`): Qwen2-Audio, Qwen2.5-Omni,
  Qwen3 audio (ASR/Omni), Voxtral, Ultravox, Granite Speech, Gemma 4 audio, LFM2-Audio,
  Parakeet and others.

## OpenAI (for the client shapes)

- **Transcriptions** (multipart only): `file` (flac, mp3, mp4, mpeg, mpga, m4a, ogg,
  wav, webm; up to 25 MB), `model`, `language`, `prompt`, `response_format` (json,
  text, srt, verbose_json, vtt, diarized_json), `temperature`, `include[]`,
  `timestamp_granularities[]`, `stream`, `chunking_strategy`, `known_speaker_*`.
  Usage is `{type: "tokens", …}` or `{type: "duration", seconds}`, seconds rounded up
  in the examples; `text`, `srt` and `vtt` answer `text/plain` with no usage.
  Streaming: `transcript.text.delta`, `transcript.text.done` (token usage).
- **Image edits:** multipart or JSON (`images[]` of `{image_url | file_id}`); `image`
  up to 16 files of 50 MB (GPT image models), `mask` (PNG, applied to the first
  image), `n` 1–10, `input_fidelity`, `stream`, `partial_images`; answers carry token
  usage.
