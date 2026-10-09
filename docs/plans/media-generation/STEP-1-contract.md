# Step 1 — contract

**Status:** not started

## Intent

Settle everything phase A builds in the documents that own it, before any code, with
the server behaviour each rule rests on read from vLLM-Omni's source at its latest
release, and the OpenTelemetry values checked at the pinned conventions.

## Files likely touched

- `docs/specs/GATEWAY.md`, each section dated:
  - **Client API:** the three endpoints and their formats, their owned fields and
    refusals (OVERVIEW decisions 5, 6, 7, 11, 13–15), and the error-table rows.
  - **Providers:** the `vllm-omni` type, its endpoint-support column, its paths, its
    wrong-endpoint reading (no core endpoints), and what passes and what is edited.
  - **Limits:**
    - the caps;
    - the six limit types: their windows, what each counts, and what is reserved
      before the request (images: `n`; audio generated: nothing, settled after);
    - the input estimate for speech (text) and images (prompt).
  - **Accounting:**
    - the three units, and what fills each (decisions 16, 17);
    - audio length — WAV, PCM, and the sample rate (decision 18);
    - estimation for raw-audio streams;
    - partial answers.
  - **Observability:**
    - the operation name and `gen_ai.output.type`;
    - the new metrics (decision 20);
    - the request line's new fields;
    - the cardinality notes.
- `docs/specs/CONTROL-PROTOCOL.md`:
  - config format 6 and protocol 6;
  - the units on the usage record;
  - the new totals windows (per-hour images and audio) and how the control plane sums
    them;
  - the new limit types and caps.
- `docs/kaiak.md`: v1 scope gains speech and image generation on `vllm-omni`.
- `protocol/schema/`:
  - `config.schema.json`: the backend type, the limit types, the caps, format 6;
  - `usage-record.schema.json`: the units;
  - `totals.schema.json`: the windows;
  - any schema that names protocol 5.
- `protocol/fixtures/`:
  - every fixture moves to format and protocol 6;
  - valid fixtures carry the new type, units, limits, caps and totals;
  - invalid cases for each new bound.
- `docs/BACKLOG.md`: "Pricing media units" (revisit trigger: a priced image or speech
  model is needed).

## Decisions made during planning

- **Read vLLM-Omni's source, not memory**, at its latest release (v0.31.0 if out,
  else v0.30.0). Record the version and commit of each file read:
  - `vllm_omni/entrypoints/openai/api_server.py`;
  - `protocol/audio.py` and `protocol/images.py`;
  - `serving_speech.py` and `speech_usage.py`;
  - the image serving code.
- **What to read there:**
  - the speech response headers;
  - the SSE events (`speech.audio.delta`, `.done`, `.error`) and their error codes;
  - the batch answer;
  - the WAV header vLLM-Omni writes when streaming (does it carry the length?);
  - the PCM sample rate and sample format per supported speech model;
  - the image answer and `file` format;
  - the route-missing answer;
  - how a `model` mismatch is answered (`400`?) — for the wrong-model rule.
- **Decision 18 is settled here, with evidence.** If every speech model vLLM-Omni lists
  natively produces 24 kHz 16-bit mono PCM (or resamples to the request's
  `sample_rate`), "24 kHz when the request sets none" is the rule, stated with its
  evidence. If any model differs, the rule is a declared rate per model, or PCM
  refused without `sample_rate` — say which and why.
- **Error classification for speech:**
  - `speech.audio.error` with `code` 500 is a backend failure after the first event.
  - Step 1 reads which other codes vLLM-Omni can send there and classifies each, as
    the Responses error events were.
- **The OpenTelemetry check:** at the pinned semantic-conventions commit, confirm
  `generate_content`, `gen_ai.output.type`'s values for image and speech, and that no
  image or audio-length metric exists. Choose the `kaiak.*` names in the existing
  style.

## Acceptance criteria

- Every rule is in its owning doc, dated, with what it rests on.
- Schemas and fixtures at format and protocol 6, with an invalid case for every new
  bound.
- `scripts/check-all.sh` run and recorded. Expected reds:
  - both halves' fixture and version tests, cleared by step 2 (`kaiak-control`) and
    step 3 (gateway);
  - the cross-half e2e, cleared by step 3.

## Result

(filled in when the step is done)
