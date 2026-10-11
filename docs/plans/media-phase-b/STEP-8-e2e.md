# Step 8 — end to end

**Status:** not started

## Intent

Prove phase B through the real binary against the fake backend, and across the halves
against the sample control plane.

## Files likely touched

- `gateway/e2e/`:
  - an image edit and a transcription (on a `vllm` and on a `llama-server` model)
    through the whole pipeline, with their usage records;
  - an upload over the cap; a non-multipart body; an unmeasurable upload to a
    `llama-server` model;
  - the image and `audio_seconds_in` limits refusing;
  - an unlisted endpoint refused before any backend;
  - a wrong `base_url` read as a wrong path, the circuit opening and the traffic
    moving to the other backend.
- `gateway/e2e/` cross-half tests (build tag `crosshalf`):
  - an edit and a transcription settle at the sample;
  - the hourly `audio_seconds_in` total from one gateway's transcriptions limits a
    second gateway.
- `gateway/internal/fakebackend/cmd/fakebackend`: flags for the new handlers, if the
  e2e needs them.

## Decisions made during planning

- **The e2e configs are the documents a reader would write:** a list on every model,
  one type per model — they double as examples.

## Acceptance criteria

- The new e2e and cross-half tests pass under `-race`, and each fails when the
  behaviour it covers is broken (checked by breaking it once, recorded).
- `scripts/check-all.sh` run and recorded. Expected red: none.
