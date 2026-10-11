# Step 7 — measuring uploaded audio

**Status:** not started

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
