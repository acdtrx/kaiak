# Step 9 — the live-test kit and the docs

**Status:** not started

## Intent

Give the live-test kit checks for the new endpoints, so step 11 can run them against
real servers. Bring every operator-facing doc to phase B.

## Files likely touched

- `scripts/live/`:
  - **`vllm-omni` kind:** an image edit with a generated input image, its `images`
    count, the refusals (`stream`, a URL, `lora`);
  - **`vllm` kind:** a transcription (a generated WAV of known length, plus an MP3 or
    FLAC), its seconds against the known length, `verbose_json`;
  - **`llama-server` kind:** a transcription of WAV, FLAC and MP3, the measured
    seconds, the refusal of an unmeasurable upload;
  - every config it writes at protocol 7 with lists;
  - its self-test.
- `docs/testing/LIVE-BACKENDS.md`: the new checks; serving an ASR model on vLLM and on
  llama-server; Qwen-Image-Edit on vLLM-Omni.
- `docs/DEPLOYMENT.md`:
  - **Upgrade notes, "Next release" (protocol 7):**
    - both halves together;
    - `format_version` becomes `protocol_version: 7`;
    - a list on every model;
    - one type per model (split a model that mixed providers into two names);
    - `upstream_endpoint_missing` gone, now `upstream_path_missing` and a circuit
      failure — update alerts matching it;
    - the new config field and environment variables;
    - the new endpoints, codes, metrics and fields;
  - **Uploads:** the cap, the budget, the deadline, sizing `GOMEMLIMIT`;
  - **Image edits:** Qwen-Image-Edit on vLLM-Omni, the edit models' input image
    limits, `mask_image` (OpenAI's `mask` is ignored by vLLM-Omni);
  - **Speech-to-text:** vLLM (seconds reported, every format), llama-server (WAV, FLAC,
    MP3; seconds measured), streaming and translations not served;
  - **Endpoints per model:** now required — what to list per kind of model, and what
    a wrong list does (decision 22).
- `README.md`, `control/kaiak-control/GUIDE.md`, `docs/ARCHITECTURE.md` (uploads in the
  data flow; the endpoint memory gone), `docs/TECH-STACK.md` (only if a library
  choice is recorded).
- `docs/BACKLOG.md`: entries resolved by phase B removed; the open ones current.

## Decisions made during planning

- **Generated test media:** the kit writes its WAV, FLAC and MP3 inputs from code (a
  tone of known length) rather than shipping binary files. If MP3 or FLAC cannot be
  written without a dependency, it carries a few KiB of fixed bytes with their known
  length.

## Acceptance criteria

- The kit's lint and self-test pass (`scripts/check-gateway.sh`).
- The docs agree with the code and with each other: grep for `format_version`,
  `upstream_endpoint_missing`, "optional" endpoints and the endpoint memory turns up
  nothing stale.
- `scripts/check-all.sh` run and recorded. Expected red: none.
