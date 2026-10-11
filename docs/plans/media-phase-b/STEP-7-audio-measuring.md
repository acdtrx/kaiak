# Step 7 — measuring uploaded audio

**Status:** done — reviewed and committed 2026-10-11

## Intent

Measure the length of an uploaded audio file — WAV, FLAC or MP3 — so a speech-to-text
request on a model whose backend reports no seconds (`llama-server`) counts its
`audio_seconds_in`. A request that must be measured and cannot be is refused before
it is sent (OVERVIEW decisions 8 and 30).

## Files likely touched

- `gateway/internal/accounting/`:
  - upload length readers, one bounded pass over the in-memory file each:
    - **WAV:** the format chunk's rate, channels and bit depth; the data chunk's
      bytes actually present (the declared size bounded by the file);
    - **FLAC:** STREAMINFO's total samples over its sample rate; a total of 0 is
      unmeasurable;
    - **MP3:** a frame scan — ID3v2 skipped by its syncsafe size, ID3v1 and APE tags
      at the end ignored; frame lengths from bitrate, sample rate and padding;
      samples per frame by MPEG version and layer; a stream with no valid frame is
      unmeasurable;
  - the format chosen by magic bytes, as llama-server's decoder does — never by the
    filename or the part's content type.
  - phase A's WAV header reader (`audio.go`) reused where it fits.
- `gateway/internal/server/`:
  - measuring the `file` part during the inbound parse;
  - after the endpoint check (the model's type known), before limits: a
    `llama-server` model's upload that cannot be measured is refused (the error code
    from step 1);
  - the measured seconds handed to the meter.
- `gateway/internal/accounting/`:
  - the record's `audio_seconds_in` on `llama-server`: the measured seconds;
  - on `vllm`, the fallback when an answer reports none (decision 29).
- `gateway/internal/fakebackend/`: llama-server's handler measures nothing; the tests
  pick files of known length.
- `docs/specs/GATEWAY.md`, the errors table: the `audio_format_unsupported` row,
  which lands with the code that answers it (step 1 wrote it, the review moved it
  here):

  ```
  | A transcription for a `llama-server` model whose upload the gateway cannot measure — not WAV, FLAC or MP3, or one whose length its header and frames do not give (`param: "file"`; settled 2026-10-11, Accounting → Media usage: speech-to-text) | 400 | `invalid_request_error` | `audio_format_unsupported` |
  ```
- Tests beside each, with small generated files (no binary fixtures beyond a few KiB).

## Decisions made during planning

- **Exact lengths, rounded up once per record,** as `audio_seconds_out`.
- **Hostile input:**
  - every size field is bounded by the bytes present;
  - a pass never revisits a byte;
  - a chunk or frame with a zero, absurd or overflowing size ends the pass as
    unmeasurable — never a loop, a panic or an allocation.
- **Measured on every type, used where needed:** the parse measures any upload it can
  (cheap, bounded); only a `llama-server` model needs the result and refuses without
  it.

## Acceptance criteria

- **Component tests:**
  - WAV: 8/16/24/32-bit, mono and stereo, odd chunks before the data, a declared size
    past the end;
  - FLAC: known length; total samples 0;
  - MP3: MPEG-1 and MPEG-2 Layer III, CBR and VBR, with ID3v2/ID3v1/APE tags;
  - each against an independent calculation;
  - truncated, zero-size, absurd-size and garbage files unmeasurable, never hanging or
    panicking;
  - a fuzz test per reader, run briefly in the suite.
- **Server tests:**
  - a `llama-server` transcription of a measurable file records its seconds;
  - an unmeasurable one (an M4A, garbage) refused before any backend;
  - a `vllm` transcription unaffected by measuring.
- `scripts/check-all.sh` green — **phase 3 ends green.**

## Result

### What changed

- **`gateway/internal/accounting`:**
  - `upload_audio.go` (new): `UploadAudioLength(file)`, the format by its magic bytes
    as llama-server tells them (`RIFF`…`WAVE`, `fLaC`, `ID3` or an MPEG frame sync),
    in its decoder's order:
    - WAV: phase A's `wavClip`, the whole file in one write — rate and block size
      from `fmt ` (PCM or extensible), the `data` chunk's bytes present, at most its
      declared size;
    - FLAC: STREAMINFO first and 34 bytes, total samples over the sample rate; a total
      or rate of 0, or a length of 9 223 372 036 s or more, unmeasurable; also after
      leading ID3v2 tags;
    - ID3v2 tag sizes read as the decoders read them (each size byte's low 7 bits,
      plus a footer when flagged), bounded by the file.
  - `mp3.go` (new): the frame scan — header validity as minimp3 judges it (free
    format excluded), frame bytes from bitrate, rate and padding, samples by version
    and layer, "same stream" by version, layer and rate; a forward search that
    resynchronizes over junk; the first frame's Xing/Info header not counted, its
    frame count taken when longer than the frames present.
- **`gateway/internal/config`:** `Model.Type()`, the one backend type of a model's
  deployments.
- **`gateway/internal/server`:**
  - `inbound_transcription.go`: the `file` part measured on every type, filling
    `Transcription.Upload` / `UploadMeasured` for the meter; `checkUploadMeasured`;
  - `pipeline.go`: a stage `upload_measured` after `endpoint_list`, before
    `model_params` and `limits`;
  - `errors.go`: `errAudioFormatUnsupported` — `400 audio_format_unsupported`,
    `param: "file"`, its message naming neither the filename nor the bytes.
- **`gateway/internal/fakebackend/transcription.go`:** comments only — llama-server's
  shape reads no audio and no `AudioMillis`; a test sends a file of the length it
  wants counted.
- **`docs/specs/GATEWAY.md`:**
  - the errors-table row, word for word from this step;
  - Accounting → Media usage, measuring an upload: lengths read where llama-server's
    decoder reads them; WAV's formats named (PCM or extensible, as audio generated);
    FLAC past leading ID3v2 tags and its bound (9 223 372 036 s, about 292 years); MP3's resynchronization,
    free format, the Xing/Info frame count, "no audio frame" unmeasurable; "a pass
    never revisits a byte" restated as "a pass only moves forward, each position
    tried as a frame's start at most once".
- **`docs/BACKLOG.md`:** an entry "WAV beyond PCM" — resolved and removed by the
  review follow-up below.
- **Tests:**
  - `accounting/upload_audio_test.go`:
    - WAV 8/16/24/32-bit mono and stereo, odd chunks before the data, a chunk
      between `fmt ` and `data`, a declared size past the end, the placeholder, a
      declared size below the bytes present, an empty data chunk;
    - unmeasurable WAVs (IEEE float, MP3-in-WAV, cut, no data, data before fmt, zero
      rate, a chunk past the end, AVI);
    - FLAC known lengths (one sample; 2^36−1 samples at 2^20−1 Hz; past two ID3v2
      tags, one with a footer and a frame sync in its text);
    - unmeasurable FLACs (total 0, rate 0, first block not STREAMINFO, 33/35 bytes,
      cut, past the bound, a tag past the end);
    - other formats (M4A, Ogg, WebM, text, nothing);
    - every expected length computed apart from the readers, with `math/big`;
    - `FuzzWAVUploadLength`: data bytes at most the file past its headers, and the
      length theirs;
    - `FuzzFLACUploadLength`: the length re-derived from STREAMINFO read as one
      64-bit word, as dr_flac reads it.
  - `accounting/mp3_test.go`:
    - frames built from the standards' formulas and tables (kept apart from the
      reader's minimp3-style arithmetic);
    - MPEG-1 and MPEG-2 Layer III CBR, VBR with and without a Xing header (and with
      a CRC), MPEG-2.5, Layers I and II, CRC frames;
    - Info without a count; a Xing count above and below the frames present;
    - an ID3v2 tag embedding a real frame chain (never counted), with a footer;
    - ID3v1, APE and ID3v1, junk at the end, after the sync, after the tag, and
      between frames; a cut last frame; a second file at 48 kHz after the first;
    - unmeasurable: a sync and no frame, a tag past the end or with nothing after it,
      a Xing frame alone, a cut frame, free format, reserved version, layer,
      bitrate and rate, MPEG-2.5 Layers I and II;
    - `FuzzMP3UploadLength`: counted frames never overlap and are each at least 24
      bytes, measured exactly when an audio frame was counted, every reader run on
      the input.
  - `server/transcription_test.go`:
    - `wavData` is now a real 8 s WAV (8-bit mono at 32 Hz) whose data still holds
      every byte value;
    - updated: llama-server records `wavSeconds` with its reported tokens, nothing
      estimated; a vLLM answer cut short counts the measured upload;
    - `TestLlamaServerTranscriptionCountsTheMeasuredUpload`: WAV, FLAC (30.5 s → 31)
      and MP3 (2.4 s → 3; 3 s exactly), whatever filename and content type they
      carry;
    - `TestUnmeasurableUploadRefusedOnLlamaServer`:
      - eight unmeasurable uploads (M4A, Ogg, text, a sync with no frame, empty, a
        cut WAV header, an ID3 tag alone, a FLAC of unknown length) refused `400
        audio_format_unsupported`, `param: "file"`, no filename in the message;
      - a llama-server model not listing speech-to-text answers
        `endpoint_not_served` first;
      - no backend request, no record; the group's one request a minute still free
        afterwards (before limits), then spent;
    - `TestVLLMTranscriptionAndTheMeasuredUpload`: the answer's seconds win whether
      the upload is measured or not; without them, the measured upload, else 0.
  - `server/metrics_test.go`: the new code's class and constructor.

### Decisions made here

- **Read the decoder llama-server runs, not only the format.** Read from llama.cpp
  b11526's `tools/mtmd/mtmd-helper.cpp` and its vendored miniaudio 0.11.25 (sources
  fetched to the scratchpad, read, deleted). llama-server allocates the length
  `ma_decoder_get_length_in_pcm_frames` gives and processes that many samples, zeros
  past those decoded. So:
  - WAV: dr_wav bounds the data size by the file and reads `0xFFFFFFFF` to the end —
    `wavClip`'s rule — so phase A's reader is reused unchanged;
  - FLAC: dr_flac's length is STREAMINFO's total (required first, 34 bytes); it skips
    any number of leading ID3v2 tags before `fLaC`, and miniaudio tries FLAC before
    MP3, so an ID3-prefixed FLAC is measured as FLAC — otherwise a crafted file
    could declare hours of FLAC behind a few MP3 frames;
  - MP3: dr_mp3 skips the first ID3v2 tag by its size, and with a Xing/Info frame
    count takes count × samples (less the LAME delay and padding) as the length.
    Hence the **Xing rule: the longer of the declared count and the frames scanned**
    — never less than llama-server processes, and never less than the frames
    present. The LAME delay/padding is not subtracted: at most about two frames
    more.
- **MP3 resynchronization:**
  - minimp3 resyncs over junk anywhere in the stream, so stopping at the first
    break would let junk hide frames the decoder plays;
  - the scan follows a chain while each header is of the same stream (version,
    layer, rate — minimp3's `hdr_compare`) and the frame fits;
  - otherwise it searches forward for a valid header whose frame fits and whose next
    header is of the same stream, or whose frame ends within 4 bytes of the end;
  - minimp3 asks ten headers in a row, so the scan counts a superset of what the
    decoder plays — a false sync in junk adds a frame at most;
  - trailing tags are not trimmed by their sizes: the search passes over them. A
    trimmed APE size could otherwise hide frames that dr_mp3 still plays (it reads
    APE's size from the wrong footer field, so it strips the footer alone);
  - work is linear: the search tries each position once, each candidate reads one
    more header. Measured: 25 MiB of hostile bytes ≤ 170 ms, a 25 MiB CBR file under
    1 ms.
- **VBRI is not skipped:** dr_mp3 plays that frame (silence), so it counts as a
  frame. Xing/Info frames are skipped, as dr_mp3 skips them.
- **Free format is not read:**
  - finding its length is a search no bound keeps short (a 25 MiB DoS);
  - a stream of free-format frames alone is unmeasurable (refused on llama-server);
  - free-format frames among normal ones are not counted. A crafted file could hide
    audio seconds there; llama-server's reported tokens are still counted.
- **"No audio frame" is unmeasurable,** so a Xing frame alone is (the decoder fails
  on it too).
- **FLAC's bound is explicit:** whole seconds ≥ `MaxInt64 / 1e9` is unmeasurable,
  rather than relying on `audioDuration`'s saturation.
- **WAV formats:** first kept to PCM and extensible, as phase A pinned them —
  superseded by the review follow-up below.
- **Where the refusal sits:** a stage of its own, `upload_measured`, between
  `endpoint_list` and `model_params` (the spec's stage-2 order). It reads the type
  from `Model.Type()` (the first deployment's — all share one type). A model that
  does not list the endpoint answers `endpoint_not_served` first.
- **Measured on every type, in the inbound parse,** whatever the filename or content
  type; only `llama-server` refuses without a result.

### Suite

`scripts/check-all.sh` — **all checks passed, phase 3 ends green** (run twice, the
second on the final tree; expected reds: none):
- gateway: gofmt, vet, staticcheck 2026.2.1, the telemetry boundary, `go test -race
  -count=1 ./...` — every package `ok`, `e2e` in 122 s;
- live-test kit: lint and self-test passed for every kind;
- control: `npm test` 675 tests, 674 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before); `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e: `ok` in 75 s.
- The three fuzz tests also ran 40 s each with `-fuzz` (11–18 M executions each, no
  failure), their corpora deleted from the Go cache afterwards.

### Review follow-up (2026-10-11)

The float-WAV gap is closed rather than backlogged:
- **One widened reader for both paths.** `wavClip` (`accounting/audio.go`) serves
  speech output's `audio_seconds_out` and the uploads. It now reads every format
  dr_wav (miniaudio 0.11.25) plays as sample frames: PCM (1), IEEE float (3), and
  A-law (6) and μ-law (7) of 8-bit samples, as dr_wav requires. It also reads
  `WAVE_FORMAT_EXTENSIBLE` whose subformat is one of these, taken from the GUID's
  first two bytes in a 22-byte extension, exactly as dr_wav reads it.
  - The length is still the data bytes present over rate × block size.
  - The block size must still be the channels' samples, and the rate non-zero, under
    `maxAudioByteRate`.
  - Anything else stays unmeasured: ADPCM, MP3-in-WAV, an unknown tag, and an
    extensible format naming an unknown subformat or none.
  - Nothing in the speech path's contract forbids the widening (vLLM-Omni sends
    16-bit PCM), so it applies there too.
- **Extensible is narrower than before.** Phase A measured the extensible format
  whatever its subformat; it now needs a subformat dr_wav plays.
- **Phase A tests changed:** they described the old limitation, and the contract
  widened.
  - `TestUnreadableWAVIsNotMeasured`: "IEEE float tag" now counts as measured. In
    its place are an ADPCM tag, extensible-of-ADPCM, extensible with no GUID, a cut
    extension, extensible with no extension, and 16-bit A-law.
  - `TestWAVLengthIsTheDataReceived`: the "extensible format" case's zero extension
    had no subformat, which dr_wav cannot play either, so it now carries a real one.
    Added extensible IEEE float, IEEE float with a fact chunk, and A-law.
- **Upload tests:**
  - Measured against the test's own rate × channels × bytes: IEEE float 32 and 64-bit
    (mono and stereo), A-law, μ-law, and extensible naming PCM, float, A-law and μ-law.
  - Still unmeasurable: an unknown tag, extensible naming an unknown subformat, ADPCM
    or extensible, a size-0 extension, no extension, 16-bit μ-law, and a float block
    of the wrong size.
  - New fuzz seeds in both WAV fuzz tests; each ran 40 s with `-fuzz` (15 M
    executions, no failure), corpora deleted.
- **Spec:** `docs/specs/GATEWAY.md`, Accounting → Media usage: the Audio length WAV
  rule now names these formats (settled 2026-10-11), and the WAV bullet under
  Measuring an upload points to it. The "WAV beyond PCM" backlog entry is removed.
- **Suite:** `scripts/check-all.sh` — all checks passed.
  - Gateway: every package `ok`, `e2e` in 121 s.
  - Live-test kit: self-test passed.
  - Control: 675 tests, 674 pass, 1 skipped; lint "boundaries ok".
  - Cross-half e2e: `ok` in 76 s.

**Phase 3 ends green** (main session's run after the review follow-up, 2026-10-11):
`scripts/check-all.sh` — `all checks passed`; control 675 tests, 674 pass, 1
skipped; gateway e2e and the cross-half e2e green.
