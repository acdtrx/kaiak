# Step 8 — end to end

**Status:** done — reviewed and committed 2026-10-11

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
  - a wrong `base_url` answered `502 upstream_path_missing` at once, the circuit
    untouched and no retry.
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

## Result

### What was added

- **`gateway/e2e/uploads_test.go`** (new) — the upload scenarios' shared parts:
  - a form written as a client writes it (Go's `mime/multipart` writer: `writeForm`,
    `textPart`, `filePart`), sent with its length declared (`postForm`) or chunked
    (`postUpload`);
  - `readForm` reads a form as a backend's server does; `wantRebuilt` checks what the
    backend got: every part in order with its name, filename, content type and bytes,
    `model` the deployment's, a boundary of the gateway's, an exact `Content-Length`;
  - `wantRefusal`, an OpenAI-shaped refusal; `pngImage`, an input image holding every
    byte value, CR, LF and a boundary's dashes.
- **`gateway/e2e/image_edits_test.go`** (new), `TestImageEdits` — `editor` (vllm-omni,
  lists `images_edits` alone) and `painter` (`images_generations` alone), one backend
  each on the fake; `max_upload_bytes` 64 KiB:
  - *an edit through the pipeline*: two `image` file parts (one filename non-ASCII), an
    inline `url[]`, OpenAI's `mask`, `n` 2, an unknown part. The fake gets the form
    rebuilt (`wantRebuilt`), the answer comes back in vLLM-Omni's shape and is never
    flushed as a stream. The request line shows the record: `images` 2, `tokens_in`
    the estimate of the prompt and 3 input images, output 0, estimated, free,
    `generate_content` / `image`;
  - *refused before any backend*: OpenAI's JSON form and a form without its boundary
    (`400 unsupported_content_type`); over the cap, declared and chunked (`413
    request_too_large`, the cap in the message); an edit to `painter` and a generation
    to `editor` (`400 endpoint_not_served`, the message). The fake got none of them;
  - *images per minute* (5): `n` is reserved — 3 settled, then 3 refused `429 images`
    with 3 used, 2 admitted, 1 refused with 5 used; 2 rejections counted; the refused
    edits never sent;
  - the image usage metrics per key, the route's request count, and nothing of the
    prompts, filenames or images in the log.
- **`gateway/e2e/transcription_test.go`** (new):
  - `TestTranscription` — `whisper` on `vllm`, `asr` on `llama-server` (each lists
    `audio_transcriptions` alone, each on its own fake playing its server), `chat`
    (vllm, `chat_completions` alone); `max_upload_bytes` 64 KiB; the group `listen`
    with `audio_seconds_in_per_minute` 5:
    - *vllm/json*, *vllm/verbose_json*: an 8 s WAV. The record's `audio_seconds_in`
      is the answer's — `usage.seconds` 3 (2.5 s), `verbose_json`'s `duration` 4.2
      rounded up to 5 — never the measured 8. Tokens are the prompt's estimate,
      flagged. The form is rebuilt (`timestamp_granularities[]` repeated) and the
      answer relayed;
    - *llama-server/WAV, MP3, FLAC*: the gateway's measurement — 2.5 s → 3, 250 MP3
      frames → 6, a FLAC declaring 30.5 s → 31 — with the answer's tokens (375 in, 12
      out), nothing estimated; the form rebuilt, llama-server's answer relayed;
    - *refused before any backend*: a JSON body and no content type
      (`unsupported_content_type`); over the cap, declared and chunked (`413`); an M4A
      to `asr` (`400 audio_format_unsupported`, `param: "file"`, the filename not
      named); a transcription to `chat` and a chat to `asr` (`endpoint_not_served`).
      Neither fake got any of them;
    - *audio processed per minute*: 3 s on `whisper`, then 3 s on `asr` admitted with
      2 s left — nothing is reserved — taking the window to 6. The next on each is
      refused `429 audio_seconds` with 6 used and never sent; 2 rejections counted;
      chat in the group passes;
    - `kaiak_usage_audio_input_duration_seconds_total` per key and model, and nothing
      of the prompt, filenames or text in the log.
  - `TestTranscriptionWrongBaseURL` — `whisper` on two vllm deployments: `lost`,
    whose `base_url` leaves out `/v1` (its server answers FastAPI's 404), and `vl`;
    circuit threshold 2. Of 8 transcriptions:
    - each one routed to `lost` is answered `502 upstream_path_missing` without the
      backend's text, in one attempt, with no request to `vl`;
    - `vl` serves its share;
    - `lost` received exactly as many as were refused, more than the threshold, and
      its circuit stays closed;
    - `path_missing` attempts equal the refused ones.
- **`gateway/e2e/sample_test.go`** (cross-half): a subtest *an edit and a transcription
  settle at the sample; the hourly audio-processed total limits the other gateway*.
  - The scenario: on gw-a, the key of a new group `transcribers`
    (`audio_seconds_in_per_hour` 5, no image limit) sends an edit of one image, and a
    transcription of a 4.5 s WAV on `asr`, a llama-server deployment.
  - The sample accepts both records with their units: the edit's `images` 1 and
    `tokens_in` estimated; the transcription's `audio_seconds_in` 5 (the gateway's
    measurement) and 7/4 tokens reported.
  - The sample's hourly totals count them: images, audio processed, tokens.
  - gw-b's probes (`asr-probe` on the refusing backend) turn into `429 audio_seconds`
    once it holds the totals, with the rejection counted; chat in the group is still
    served on gw-b.
  - The supporting changes:
    - the fake plays llama-server's speech-to-text;
    - `images` (the hourly image total) is now held at the test's scope, since both
      media subtests add to it;
    - `crossHalfConfig` gains `ls-refuser`, `asr`, `asr-probe`, the group and its key
      (`transcribersKey`).
- **Fake backend command:** no new flags. The e2e drives the fake in process, and
  step 6's `-transcription-shape` and `-non-audio-models` serve the live kit.

### Decisions made here

- **The record in file mode is read from the request line** (its units and flags) and
  the usage metrics, as phase A's e2e reads it; the record itself is checked across the
  halves, as the sample accepted it (the proxy).
- **Scenario configs:**
  - each model lists its endpoints explicitly, one server per model; the two
    vllm-omni backends are at the same fake;
  - `max_upload_bytes` is 64 KiB, so the over-cap bodies stay small.
- **The wrong `base_url` scenario runs on speech-to-text**, so the "not found" reading
  is exercised on an upload and its rebuilt body, with two deployments to show no
  retry. The chat case stays `TestWrongBaseURLOnVLLM`.
- **Across the halves:**
  - the transcription is on `llama-server`, so the record the sample counts carries
    the gateway's own measurement;
  - a new group, because eval's hourly images are spent by the speech-and-image
    subtest.
- **The cross-half transcription is 5 s, not 3 s:** break 11 below first ran with
  3 s. The sample's audio-processed total then matched by coincidence — the speech
  subtest's 3 s of audio generated counted into it under the break — and only gw-b's
  refusal caught it. At 5 s the totals check catches it on its own.
- **One refusal check, `wantRefusal`, shared by both endpoints' tests** (the same
  purpose).

### Break-it checks

Each a one-line change in the code under test, run against the test it targets, then
reverted with `git checkout` (each file was clean). Every one failed its test:

| # | Family | Line broken | Failure |
|---|---|---|---|
| 1 | an edit through the pipeline | `internal/accounting/meter.go:179` `case provider.FormatImages, provider.FormatImageEdits:` → `case provider.FormatImages:` | `TestImageEdits`: `kaiak.usage.images` absent, want 2; images per minute admitted the second edit. Across the halves: the edit record's `images` 0, the hourly image total not seen |
| 2 | the rebuilt form | `internal/provider/form.go:62` `if part.Name == "model" {` → `… && false {` | `TestImageEdits`: part 0 (`model`) 6 bytes, want 25. `TestTranscription`: vLLM answered 404 "The model `whisper` does not exist."; llama-server's part 0 the client's |
| 3 | vllm: the answer's seconds | `internal/accounting/transcription_usage.go:112` `…answerSeconds(); ok {` → `ok && false {` | `vllm/json` 8 s, want 3; `vllm/verbose_json` 8 s, want 5 |
| 4 | llama-server: the measured upload | `transcription_usage.go:115` `if u.request.UploadMeasured {` → `… && false {` | `llama-server/WAV`, `MP3` and `FLAC`: no seconds, estimated; the per-minute window never filled |
| 5 | the upload cap | `internal/server/uploads.go:55` `limit := min(rq.snapshot.MaxUploadBytes, uploads.Budget.Limit())` → `limit := uploads.Budget.Limit()` | both tests' over-the-cap cases, declared and chunked, answered 200 and reached the backends |
| 6 | a body that is no form | `uploads.go:52` `if apiErr != nil {` → `… && false {` | JSON, no boundary, no content type: `400 invalid_body`, want `unsupported_content_type` |
| 7 | an unmeasurable upload | `internal/server/inbound_transcription.go:93` `return errAudioFormatUnsupported()` → `return nil` | the M4A reached llama-server and was answered 200 |
| 8 | the image limit | `internal/server/inbound_image_edits.go:39` `images, apiErr = editImages(` → `_, apiErr = editImages(` (`n` not reserved) | `edits-limit-three-more` answered 200 |
| 9 | the audio-processed limit | `internal/config/snapshot.go:182` `return []Unit{UnitAudioSecondsIn}` → `{UnitAudioSecondsOut}` | `listen-refused-whisper` answered 200 |
| 10 | an unlisted endpoint | `internal/server/pipeline.go:323` `if !rq.snapshot.Models[…].Admits(…)` → `if false && …` | all four unlisted requests answered 200 and reached the backends |
| 11a | wrong `base_url`: the circuit | `internal/server/attempts.go:504` `routing.Neutral` → `routing.Failure` | `lost` got 2 transcriptions (the threshold); its circuit open |
| 11b | wrong `base_url`: no retry | `internal/metrics/ops.go:84` `AttemptPathMissing` added to `RetryableOutcomes` | every transcription retried on `vl` and served: 0 answered `upstream_path_missing`; 8 `path_missing` attempts, want 0 |
| 12 | across the halves: settled at the sample | `control/kaiak-control/src/usage/aggregate.ts:53` `return BigInt(units.audio_seconds_in);` → `units.audio_seconds_out` | "the hourly audio-processed total at 5 not seen within 15s" |
| 13 | across the halves: the second gateway | `internal/limits/shared.go:91` the pushed window's `setBase` behind `if w.Type != config.LimitAudioSecondsInPerHour` | "gw-b refusing transcribers' transcriptions: not seen within 15s" |

Gateway paths are under `gateway/`.

### Suite

`scripts/check-all.sh` — **all checks passed**, run on the final tree (expected reds:
none):
- gateway: gofmt, vet and staticcheck 2026.2.1 (crosshalf tag included), the telemetry
  boundary, `go test -race -count=1 ./...` — every package `ok`, `e2e` in 129 s;
- live-test kit: lint, and the self-test passed for every kind;
- control: `npm test` 675 tests, 674 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before); `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e: `ok` in 80 s.
