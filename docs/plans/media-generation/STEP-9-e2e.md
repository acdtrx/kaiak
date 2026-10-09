# Step 9 — end to end

**Status:** not started

## Intent

Prove phase A through a running gateway and across both halves.

## Files likely touched

- `gateway/e2e/`, on `vllm-omni`-typed fake backends:
  - each speech mode, the batch and image generation through the whole pipeline;
  - the usage records with their units;
  - the new limits refusing (minute windows) and the caps;
  - wrong endpoints neutral (chat to an image model; speech to a chat model);
  - a client gone mid-clip.
- `gateway/e2e/sample_test.go` (`crosshalf`):
  - a speech request and an image request settle at the sample;
  - the hourly image and audio totals reach a second gateway and limit it there.

## Decisions made during planning

- **The fake backend plays each answer shape** as step 1 recorded them. Live captures
  come in step 12.

## Acceptance criteria

- The tests pass under `-race`, uncached.
- `scripts/check-all.sh` green twice.

## Result

(filled in when the step is done)
