# Step 9 — end to end

**Status:** done — reviewed and committed 2026-10-09

## Intent

Prove phase A through a running gateway and across both halves.

## Files likely touched

- `gateway/e2e/`, on `vllm-omni`-typed fake backends:
  - each speech mode, the batch and image generation through the whole pipeline;
  - the usage records with their units;
  - the new limits refusing (minute windows) and the caps;
  - chat to a model on `vllm-omni` refused with `400 endpoint_not_served`;
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

### What changed

- `gateway/e2e/speech_test.go` (new) — `TestSpeech`, one gateway on `omniConfig`: a
  `vllm-omni` backend and an `openai-compatible` one on one fake backend; `tts`
  (priced, output limit 300/1000), `painter` and `chat`; caps of 4 images and 3 batch
  items; the groups `w`, `audio` (`audio_seconds_out_per_minute` 5) and `images`
  (`images_per_minute` 5), each with its own key.
  - Speech in six modes — whole clip, raw-audio stream and SSE, each in WAV and PCM:
    the audio relayed byte for byte (the SSE deltas decoded and joined), the word
    timing relayed, no `x-vllm-omni-*` header, `X-Accel-Buffering: no`; sent with the
    model edit and `max_new_tokens` 300 alone; the request line's backend type,
    `generate_content`, `speech`, `gen_ai.request.stream`, tokens (7/4 from the
    headers or `done`; the input estimate, flagged, on the raw stream), 3 s, cost.
  - The batch: three items of 1.25 s, the second errored — results relayed as sent,
    WAV and PCM audio byte for byte, usage 14/8 summed, 2.5 s → 3 s.
  - A client gone mid-clip (raw PCM, paced): the backend's request cancelled; 2 s
    (1.25 s relayed), estimated, partial, `kaiak.relay_end` `client_closed`.
  - `audio_seconds_out_per_minute`: two clips take the window to 6 of 5, the third is
    `429 audio_seconds` (no `x-ratelimit-*`, the log line naming the limit, the
    rejection metric); chat and an image in the same group served.
  - `/v1/models/tts` lists the three media endpoints.
  - Refused before any backend: chat to `tts` and embeddings to `painter`
    (`400 endpoint_not_served`), speech to `chat`, mp3, PCM without its rate, a
    `ref_audio` URL, the batch cap, an item's format (`items[1].response_format`).
  - Metrics: `kaiak_usage_audio_output_duration_seconds_total` with
    `gen_ai_output_type="speech"` — complete 21, partial 2, the audio group's 6 — and
    the record count; no text, audio (`UklGR`), word timing or item error in the log.
- `gateway/e2e/images_test.go` (new) — `TestImages`, on the same config: two images
  of 2000 bytes relayed in vLLM-Omni's members, not flushed, sent with the model edit
  alone (`stream` kept), settled as 2 images, the input estimated, flagged, no
  output, free, never a stream; a layered answer (`n` 1, 3 layers) relayed and counted
  as 3; `url`, `file`, `lora`, `n` over the cap refused before any backend, the LoRA's
  path never echoed; `images_per_minute`: 3 settled, 3 more `429 images`, a layered
  answer admitted on `n` 1 settling 3 (window 6 of 5), the next refused, chat and
  speech in the group served; `kaiak_usage_images_total` with
  `gen_ai_output_type="image"` (5 and 6), none for speech; no prompt, LoRA path or
  image (`iVBORw0KGgo`) in the log.
- `gateway/e2e/sample_test.go` (`crosshalf`):
  - `crossHalfConfig`: `vllm-omni` backends `omni` (the working fake) and
    `omni-refuser` (the refusing one); the unpriced `tts`, `painter` and their probes
    `tts-probe`, `painter-probe`; eval's `images_per_hour` 2 and
    `audio_seconds_out_per_hour` 3 (also in the replicas test's config, which sends no
    media).
  - New subtest: a clip and 2 images on gw-a; the records the sample accepted carry
    their units (7/4/3 s; the estimate/2 images, estimated) on `omni`, free; the
    sample's global hourly image (2), audio (3) and token totals count them; gw-b's
    probes — to the refusing backend, spending nothing — turn `429 images` and
    `429 audio_seconds` once the totals reach it; gw-b's rejection metric counts both
    hour types; chat on gw-b served.
  - `servedTokens` → `servedAmounts`, the hourly type a field (tokens, images, audio),
    so the hour-edge reading of `counted` serves every hourly total;
    `replicas_test.go` follows.
- `gateway/internal/fakebackend/speech.go`: `wavHeader` exported as `WAVHeader`, so a
  test builds the exact audio the fake sends and checks the relay byte for byte.

### Design choices

- **The fake runs in-process**, as in every gateway e2e test (`fakebackend.New()`),
  so `cmd/fakebackend` needs no new flags here; steps 5 and 7's notes about its
  missing flags (`SetNonSpeechModels`, `AudioMillis`, `FailItems`, image setters)
  stay for step 10's live kit, if it needs them.
- **Each limited group has its own key**, as the existing e2e does (`rpmKey`,
  `budgetKey`): a spent window never leaks into another subtest.
- **Waits on events, no sleeps**: request lines (`settled`) before a limit is read
  or a metric asserted; the fake's `Canceled()` for the client gone; the paced piece
  handed over with a bounded select; the proxy's accepted records and the totals
  watch in the cross-half; `pollUntil` for gw-b, as the budget subtest does.
- **The hour edge in the cross-half**: an hourly limit reaching gw-b holds only within
  the UTC hour of the spend. With less than 30 s of the hour left the subtest waits
  for the next hour on the totals watch, whose top-of-hour wake-up the suite already
  has (the subtest takes about 5 s). The sample's totals checks use `counted`'s
  existing edge tolerance. Rejected: re-spending in the new hour — a spend straddling
  the top of the hour would find gw-a's own image window already spent and fail.

### Found in earlier steps

Nothing wrong: every expectation held on the first run through the binary and the
sample.

### Spec points

None new; no spec edit.

### Suite (2026-10-09)

Phase 4 continues; no expected reds. `TestSpeech` and `TestImages` 15 times in a row
under `-race` (`-count=15`): all pass. `scripts/check-gateway.sh` green (exit 0,
`gateway checks passed`; `e2e` ≈122 s). Then `scripts/check-all.sh` twice (the
second after this record was written), each `all checks passed`, exit 0:

- **Gateway:** gofmt, vet (crosshalf tag included), staticcheck 2026.2.1 on
  go1.27.1, telemetry boundary pass; `go test -race -count=1 ./...` — all 20 packages
  `ok`, `e2e` included (≈120 s).
- **Live-test kit:** gofmt, vet, staticcheck pass; `-self-test` passed for vllm,
  llama-server, openai, azure-openai, anthropic, azure-anthropic, vllm with two
  backends (37/36/22/21/16/16/34 passed, 0 failed).
- **Control `npm test`:** 655 tests, 654 pass, 0 fail, 0 cancelled, 1 skipped.
  **`npm run lint`:** `boundaries ok`.
- **Cross-half e2e:** `ok kaiak/e2e` (≈76 s; the new subtest ≈5 s). `TestAcrossHalves`
  once more on its own after the second run: pass.
