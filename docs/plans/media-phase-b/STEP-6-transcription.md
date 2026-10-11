# Step 6 — speech-to-text

**Status:** not started

## Intent

Serve `POST /v1/audio/transcriptions` on `vllm` and `llama-server` deployments. It is
an upload format producing `audio_seconds_in`:
- on `vllm`, the seconds the answer reports;
- on `llama-server`, the measured upload, which is step 7's.

Tokens follow OVERVIEW decision 9.

## Files likely touched

- **The names, on both halves at once:**
  - `protocol/schema/config.schema.json`: `audio_transcriptions` in the endpoint
    names;
  - `protocol/fixtures/backend-types/vllm.json` and `llama-server.json`:
    `audio_transcriptions` in their endpoints;
  - `control/kaiak-control`: `ENDPOINT_SUPPORT`, `MODEL_ENDPOINTS`, the types, the
    packaged schema, tests;
  - `gateway/internal/config`: the name and the support table.
- `gateway/internal/provider/`:
  - the endpoint (path `audio/transcriptions`), an upload format;
  - `vllm.go` and `llama_server.go` serve it;
  - vLLM's `404` naming the model read as the model missing (check it matches the
    existing reading);
  - the answer relayed as the backend sends it, its content type kept.
- `gateway/internal/server/`:
  - the `bodyEndpoints` row: `audio_transcriptions`, operation `generate_content`,
    output type `text`, an upload endpoint, no output-limit keys;
  - the owned parts (decision 26):
    - `file`: exactly one file part, required;
    - `stream` true refused;
    - `response_format` read;
    - `vllm_xargs` refused;
    - `timestamp_granularities[]` may repeat;
  - the request counts toward `audio_seconds_in` with nothing reserved, so its limits
    refuse once the window is spent and settle after.
- `gateway/internal/accounting/`:
  - a usage reader for the answer:
    - vLLM's `usage: {type: "duration", seconds}`;
    - `verbose_json`'s `duration`, rounded up;
    - llama-server's `usage: {type: "tokens", input_tokens, output_tokens}`;
  - the record:
    - `audio_seconds_in` from the reader (on `llama-server`, from step 7);
    - tokens as reported, else `tokens_in` the prompt's estimate, flagged;
  - the input estimate: the `prompt` part's text.
- `gateway/internal/fakebackend/`: transcription handlers in vLLM's shapes (`json`,
  `text`, `verbose_json`; `srt`/`vtt` refused) and llama-server's (`json` only, token
  usage), each parsing the rebuilt body.
- Metrics and the request line: `kaiak.usage.audio.input.duration` now produced;
  `gen_ai.output.type` `text`.
- Tests beside each.

## Decisions made during planning

- **No output-limit keys:** neither the gateway nor OpenAI's API sets one.
  `max_completion_tokens` (vLLM) and `max_tokens` (llama-server) pass untouched.
- **Usage members are read by exact name,** as phase A's speech usage is.
- **Until step 7, a `llama-server` transcription records 0 seconds, flagged
  `estimated`.** That is the decision 29 fallback, which step 7 replaces with the
  measured upload. Not a red: phase 3 ends after step 7.

## Acceptance criteria

- **Component and server tests:**
  - each owned part and refusal: no file, two files, `stream` true, `vllm_xargs`;
  - vLLM's seconds from `json`, `text` and `verbose_json`;
  - a missing or malformed usage;
  - llama-server's tokens;
  - `tokens_in` estimated and flagged where none is reported;
  - the `audio_seconds_in` limits refusing a spent window and settling after;
  - the operation and output type on the request line and metrics;
  - a transcription to a model whose list leaves it out refused before any backend.
- The shared fixtures pass on both halves with the new name.
- `scripts/check-all.sh` run and recorded. Expected red: none (phase 3 continues).
